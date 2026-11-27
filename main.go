// Command marina-wind — сервис «Ветер у марины».
//
// Один бинарник отдаёт и JSON-API (/api/...), и статический лендинг из web/
// (встроен через go:embed). Поэтому сервис одинаково работает локально,
// в Docker и как Go-сервер на Vercel (framework preset "go", слушает PORT).
package main

import (
	"embed"
	"encoding/json"
	"encoding/xml"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	_ "time/tzdata" // база часовых поясов внутри бинарника: на Vercel/alpine её нет

	"github.com/lerx11/marina-wind/internal/api"
	"github.com/lerx11/marina-wind/internal/booking"
	"github.com/lerx11/marina-wind/internal/journal"
	"github.com/lerx11/marina-wind/internal/meteo"
	"github.com/lerx11/marina-wind/internal/notify"
)

//go:embed web
var webFS embed.FS

// version подставляется при сборке: -ldflags "-X main.version=v1.0.0".
var version = "dev"

func main() {
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("встроенная статика: %v", err)
	}

	// Без UPSTASH_* сервис стартует как обычно: прогноз работает,
	// запись на выходы отвечает 503 с понятным пояснением.
	var store booking.Store
	if upstash, ok := booking.UpstashFromEnv(); ok {
		store = upstash
		log.Printf("бронирование: Upstash Redis подключён")
	} else {
		log.Printf("бронирование: UPSTASH_* не заданы, запись отключена")
	}

	// Уведомление владельцу: без переменных заявки всё равно сохраняются.
	var telegram booking.Notifier
	if tg, ok := notify.FromEnv(); ok {
		telegram = tg
		log.Printf("уведомления: Telegram подключён")
	} else {
		log.Printf("уведомления: TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID не заданы, отправка отключена")
	}

	book := booking.NewService(store)
	book.SetNotifier(telegram)

	a := api.New(meteo.New(8*time.Second), book)

	mux := http.NewServeMux()
	a.Register(mux) // /api/health, /api/spots, /api/forecast

	// Закрытый экран клуба: без cookie отдаём страницу входа, с ней — сам экран.
	mux.HandleFunc("GET /club", func(w http.ResponseWriter, r *http.Request) {
		page := "club-login.html"
		if api.ClubAuthorized(r) {
			page = "club.html"
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, static, page)
	})
	// Прямой доступ к файлу экрана закрываем: только через /club.
	mux.HandleFunc("GET /club.html", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/club", http.StatusFound)
	})

	// Журнал: тексты статей лежат в internal/journal/articles/*.md.
	articles := journal.All()
	pages, err := template.ParseFS(static, "journal.html", "journal_article.html")
	if err != nil {
		log.Fatalf("шаблоны журнала: %v", err)
	}

	mux.HandleFunc("GET /journal", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		data := struct{ Articles []journal.Article }{Articles: articles}
		if err := pages.ExecuteTemplate(w, "journal.html", data); err != nil {
			log.Printf("журнал: %v", err)
		}
	})

	mux.HandleFunc("GET /journal/{slug}", func(w http.ResponseWriter, r *http.Request) {
		article, ok := journal.Find(r.PathValue("slug"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		data := journalArticlePage{
			Article: article,
			Body:    template.HTML(article.BodyHTML),
			JSONLD:  articleJSONLD(article),
		}
		if err := pages.ExecuteTemplate(w, "journal_article.html", data); err != nil {
			log.Printf("статья %s: %v", article.Slug, err)
		}
	})

	mux.HandleFunc("GET /sitemap.xml", func(w http.ResponseWriter, r *http.Request) {
		writeSitemap(w, articles)
	})

	mux.Handle("GET /", http.FileServer(http.FS(static))) // лендинг и статика

	srv := &http.Server{
		Addr:              ":" + port(),
		Handler:           withLogging(mux),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("marina-wind %s запущен на %s", version, srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("сервер остановлен: %v", err)
	}
}

// journalArticlePage — данные шаблона статьи. Body и JSONLD уже безопасны:
// первый собран из Markdown с экранированием, второй — из encoding/json.
type journalArticlePage struct {
	Article journal.Article
	Body    template.HTML
	JSONLD  template.JS
}

// articleJSONLD собирает разметку Article + BreadcrumbList для поисковиков.
func articleJSONLD(a journal.Article) template.JS {
	const org = "Ветер у марины"
	graph := map[string]any{
		"@context": "https://schema.org",
		"@graph": []any{
			map[string]any{
				"@type":         "Article",
				"headline":      a.H1,
				"description":   a.Description,
				"datePublished": a.DateISO + "T09:00:00+03:00",
				"dateModified":  a.DateISO + "T09:00:00+03:00",
				"inLanguage":    "ru",
				"mainEntityOfPage": map[string]any{
					"@type": "WebPage",
					"@id":   a.URL,
				},
				"author":    map[string]any{"@type": "Organization", "name": org, "url": journal.SiteURL},
				"publisher": map[string]any{"@type": "Organization", "name": org, "url": journal.SiteURL},
			},
			map[string]any{
				"@type": "BreadcrumbList",
				"itemListElement": []any{
					map[string]any{"@type": "ListItem", "position": 1, "name": "Главная", "item": journal.SiteURL + "/"},
					map[string]any{"@type": "ListItem", "position": 2, "name": "Журнал", "item": journal.SiteURL + "/journal"},
					map[string]any{"@type": "ListItem", "position": 3, "name": a.H1, "item": a.URL},
				},
			},
		},
	}
	body, err := json.Marshal(graph)
	if err != nil {
		return ""
	}
	return template.JS(body)
}

// writeSitemap отдаёт sitemap.xml с главной, журналом и статьями.
func writeSitemap(w http.ResponseWriter, articles []journal.Article) {
	type entry struct {
		Loc     string `xml:"loc"`
		LastMod string `xml:"lastmod,omitempty"`
	}
	type urlSet struct {
		XMLName xml.Name `xml:"urlset"`
		Xmlns   string   `xml:"xmlns,attr"`
		URLs    []entry  `xml:"url"`
	}

	set := urlSet{
		Xmlns: "http://www.sitemaps.org/schemas/sitemap/0.9",
		URLs: []entry{
			{Loc: journal.SiteURL + "/"},
			{Loc: journal.SiteURL + "/journal"},
		},
	}
	for _, a := range articles {
		set.URLs = append(set.URLs, entry{Loc: a.URL, LastMod: a.DateISO})
	}

	body, err := xml.MarshalIndent(set, "", "  ")
	if err != nil {
		http.Error(w, "не удалось собрать sitemap", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
	_, _ = w.Write([]byte("\n"))
}

// port берёт PORT из окружения (обязателен на Vercel) с локальным дефолтом.
func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "3000"
}

// withLogging печатает строку на каждый запрос — удобно при отладке деплоя.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

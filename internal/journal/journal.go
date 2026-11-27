// Package journal — статьи «Журнала»: Markdown-файлы в репозитории.
//
// Тексты лежат в articles/*.md и встраиваются в бинарник. Чтобы добавить
// статью, достаточно положить новый файл с фронт-маттером — код менять не нужно.
package journal

import (
	"embed"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SiteURL — канонический адрес сайта для ссылок и sitemap.
const SiteURL = "https://regatta-go.vercel.app"

//go:embed articles/*.md
var files embed.FS

// Article — одна публикация журнала.
type Article struct {
	Slug        string
	Title       string
	Description string
	H1          string
	Question    string
	Date        time.Time
	DateLabel   string
	DateISO     string
	Reading     int
	Order       int
	Path        string // /journal/<slug>
	URL         string // абсолютный адрес для canonical и OG
	BodyHTML    string // готовый HTML без обёртки страницы
}

var (
	once     sync.Once
	articles []Article
)

// All возвращает статьи от новых к старым.
func All() []Article {
	once.Do(load)
	out := make([]Article, len(articles))
	copy(out, articles)
	return out
}

// Find ищет статью по slug.
func Find(slug string) (Article, bool) {
	All()
	for _, a := range articles {
		if a.Slug == slug {
			return a, true
		}
	}
	return Article{}, false
}

func load() {
	entries, err := files.ReadDir("articles")
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		raw, err := files.ReadFile("articles/" + entry.Name())
		if err != nil {
			continue
		}
		article, ok := parse(entry.Name(), string(raw))
		if ok {
			articles = append(articles, article)
		}
	}
	sort.Slice(articles, func(i, j int) bool {
		oi, oj := articles[i].Order, articles[j].Order
		if oi == 0 {
			oi = 999
		}
		if oj == 0 {
			oj = 999
		}
		if oi != oj {
			return oi < oj
		}
		if !articles[i].Date.Equal(articles[j].Date) {
			return articles[i].Date.After(articles[j].Date)
		}
		return articles[i].Slug < articles[j].Slug
	})
}

var (
	months = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
)

func parse(filename, raw string) (Article, bool) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	if !strings.HasPrefix(raw, "---\n") {
		return Article{}, false
	}
	end := strings.Index(raw[4:], "\n---\n")
	if end < 0 {
		return Article{}, false
	}
	meta := raw[4 : 4+end]
	body := raw[4+end+5:]

	fields := map[string]string{}
	for _, line := range strings.Split(meta, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}

	a := Article{
		Slug:        first(fields["slug"], strings.TrimSuffix(filename, ".md")),
		Title:       fields["title"],
		Description: fields["description"],
		H1:          first(fields["h1"], fields["title"]),
		Question:    fields["question"],
	}
	if a.Slug == "" || a.Title == "" {
		return Article{}, false
	}

	if d, err := time.Parse("2006-01-02", fields["date"]); err == nil {
		a.Date = d
		a.DateLabel = strconv.Itoa(d.Day()) + " " + months[d.Month()-1] + " " + strconv.Itoa(d.Year())
		a.DateISO = d.Format("2006-01-02")
	}

	a.BodyHTML = renderMarkdown(body)
	a.Reading = readingTime(body)
	a.Order, _ = strconv.Atoi(fields["order"])
	a.Path = "/journal/" + a.Slug
	a.URL = SiteURL + a.Path
	return a, true
}

func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func readingTime(body string) int {
	words := len(strings.Fields(stripMarkup(body)))
	minutes := (words + 179) / 180
	if minutes < 3 {
		minutes = 3
	}
	return minutes
}

var (
	linkPattern   = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	boldPattern   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	italicPattern = regexp.MustCompile(`\*([^*]+)\*`)
	codePattern   = regexp.MustCompile("`([^`]+)`")
	markupStrip   = regexp.MustCompile(`[#*_>` + "`" + `\[\]()\-]`)
)

func stripMarkup(s string) string { return markupStrip.ReplaceAllString(s, " ") }

// renderMarkdown поддерживает заголовки ##/###, абзацы, списки, цитаты,
// **жирный**, *курсив*, `код` и ссылки. Этого достаточно для статей журнала.
func renderMarkdown(body string) string {
	lines := strings.Split(body, "\n")
	var out strings.Builder
	var paragraph []string
	var list string
	var quote []string

	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}
		out.WriteString("<p>")
		out.WriteString(inline(strings.Join(paragraph, " ")))
		out.WriteString("</p>\n")
		paragraph = paragraph[:0]
	}
	flushList := func() {
		if list == "" {
			return
		}
		out.WriteString("</" + list + ">\n")
		list = ""
	}
	flushQuote := func() {
		if len(quote) == 0 {
			return
		}
		out.WriteString("<blockquote><p>")
		out.WriteString(inline(strings.Join(quote, " ")))
		out.WriteString("</p></blockquote>\n")
		quote = quote[:0]
	}
	flushAll := func() {
		flushParagraph()
		flushList()
		flushQuote()
	}

	openList := func(kind string) {
		if list == kind {
			return
		}
		flushList()
		out.WriteString("<" + kind + ">\n")
		list = kind
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flushAll()
		case strings.HasPrefix(trimmed, "### "):
			flushAll()
			out.WriteString("<h3>" + inline(strings.TrimPrefix(trimmed, "### ")) + "</h3>\n")
		case strings.HasPrefix(trimmed, "## "):
			flushAll()
			out.WriteString("<h2>" + inline(strings.TrimPrefix(trimmed, "## ")) + "</h2>\n")
		case strings.HasPrefix(trimmed, "- "), strings.HasPrefix(trimmed, "* "):
			flushParagraph()
			flushQuote()
			openList("ul")
			out.WriteString("<li>" + inline(trimmed[2:]) + "</li>\n")
		case isOrdered(trimmed):
			flushParagraph()
			flushQuote()
			openList("ol")
			item := trimmed[strings.Index(trimmed, ". ")+2:]
			out.WriteString("<li>" + inline(item) + "</li>\n")
		case strings.HasPrefix(trimmed, "> "):
			flushParagraph()
			flushList()
			quote = append(quote, strings.TrimPrefix(trimmed, "> "))
		default:
			flushList()
			flushQuote()
			paragraph = append(paragraph, trimmed)
		}
	}
	flushAll()
	return out.String()
}

func isOrdered(s string) bool {
	i := strings.Index(s, ". ")
	if i <= 0 {
		return false
	}
	if _, err := strconv.Atoi(s[:i]); err != nil {
		return false
	}
	return true
}

// inline экранирует текст и превращает разметку в теги.
func inline(s string) string {
	s = html.EscapeString(s)
	s = codePattern.ReplaceAllString(s, "<code>$1</code>")
	s = linkPattern.ReplaceAllStringFunc(s, func(m string) string {
		parts := linkPattern.FindStringSubmatch(m)
		href := parts[2]
		attrs := ""
		if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
			attrs = ` target="_blank" rel="noopener noreferrer"`
		} else if !strings.HasPrefix(href, "/") && !strings.HasPrefix(href, "#") {
			return parts[1] // неожиданная схема — оставляем только текст
		}
		return `<a href="` + href + `"` + attrs + `>` + parts[1] + `</a>`
	})
	s = boldPattern.ReplaceAllString(s, "<strong>$1</strong>")
	s = italicPattern.ReplaceAllString(s, "<em>$1</em>")
	return s
}

// String помогает в логах и тестах.
func (a Article) String() string {
	return fmt.Sprintf("%s (%s)", a.Title, a.Path)
}

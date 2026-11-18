// Command marina-wind — сервис «Ветер у марины».
//
// Один бинарник отдаёт и JSON-API (/api/...), и статический лендинг из web/
// (встроен через go:embed). Поэтому сервис одинаково работает локально,
// в Docker и как Go-сервер на Vercel (framework preset "go", слушает PORT).
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	_ "time/tzdata" // база часовых поясов внутри бинарника: на Vercel/alpine её нет

	"github.com/lerx11/marina-wind/internal/api"
	"github.com/lerx11/marina-wind/internal/meteo"
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

	a := api.New(meteo.New(8 * time.Second))

	mux := http.NewServeMux()
	a.Register(mux)                                       // /api/health, /api/spots, /api/forecast
	mux.Handle("GET /", http.FileServer(http.FS(static))) // лендинг

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

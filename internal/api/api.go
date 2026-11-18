// Package api — HTTP-слой сервиса. Тонкий: разбирает запрос, зовёт meteo и wind,
// сериализует ответ. Держит маленький TTL-кеш, чтобы не дёргать бесплатный
// Open-Meteo на каждый запрос.
package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/lerx11/marina-wind/internal/meteo"
	"github.com/lerx11/marina-wind/internal/spots"
	"github.com/lerx11/marina-wind/internal/wind"
)

// cacheTTL — сколько держим ответ в памяти.
const cacheTTL = 10 * time.Minute

// API — набор обработчиков сервиса.
type API struct {
	meteo *meteo.Client
	cache *ttlCache
}

// New создаёт HTTP-слой поверх клиента Open-Meteo.
func New(client *meteo.Client) *API {
	return &API{meteo: client, cache: newTTLCache(cacheTTL)}
}

// Register вешает маршруты на mux (Go 1.22+ pattern matching).
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/spots", a.spots)
	mux.HandleFunc("GET /api/scale", a.scale)
	mux.HandleFunc("GET /api/forecast", a.forecast)
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *API) spots(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"spots": spots.All()})
}

func (a *API) scale(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"scale": wind.Scale})
}

func (a *API) forecast(w http.ResponseWriter, r *http.Request) {
	slug := r.URL.Query().Get("spot")
	if slug == "" {
		slug = spots.DefaultSlug
	}
	spot, ok := spots.Find(slug)
	if !ok {
		writeError(w, http.StatusNotFound, "неизвестная акватория: "+slug)
		return
	}

	if body, hit := a.cache.get(spot.Slug); hit {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Cache", "HIT")
		_, _ = w.Write(body)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 9*time.Second)
	defer cancel()

	result, err := a.meteo.Fetch(ctx, spot.Lat, spot.Lon, spot.Coastal)
	if err != nil {
		writeError(w, http.StatusBadGateway, "не удалось получить прогноз: "+err.Error())
		return
	}

	body, err := json.Marshal(buildForecast(spot, result))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ошибка сериализации ответа")
		return
	}
	a.cache.set(spot.Slug, body)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(body)
}

// --- модель ответа ---

type forecastView struct {
	Spot      spots.Spot  `json:"spot"`
	Timezone  string      `json:"timezone"`
	Generated string      `json:"generated"`
	Current   currentView `json:"current"`
	Hours     []hourView  `json:"hours"`
	Checklist []string    `json:"checklist"`
}

type currentView struct {
	Time         string   `json:"time"`
	Date         string   `json:"date"`
	WindKnots    float64  `json:"wind_knots"`
	GustKnots    float64  `json:"gust_knots"`
	WindMS       float64  `json:"wind_ms"`
	Direction    float64  `json:"direction_deg"`
	Rumb         string   `json:"rumb"`
	RumbFull     string   `json:"rumb_full"`
	Beaufort     int      `json:"beaufort"`
	BeaufortName string   `json:"beaufort_name"`
	SeaState     string   `json:"sea_state"`
	SailAdvice   string   `json:"sail_advice"`
	SailorLevel  string   `json:"sailor_level"`
	Status       string   `json:"status"`
	WaveM        *float64 `json:"wave_m,omitempty"`
	WaveRumb     string   `json:"wave_rumb,omitempty"`
	WavePeriodS  *float64 `json:"wave_period_s,omitempty"`
}

type hourView struct {
	Time      string   `json:"time"`
	Label     string   `json:"label"`
	WindKnots float64  `json:"wind_knots"`
	GustKnots float64  `json:"gust_knots"`
	Direction float64  `json:"direction_deg"`
	Rumb      string   `json:"rumb"`
	Beaufort  int      `json:"beaufort"`
	Status    string   `json:"status"`
	WaveM     *float64 `json:"wave_m,omitempty"`
}

func buildForecast(spot spots.Spot, res meteo.Result) forecastView {
	b := wind.FromKnots(res.Current.WindKnots)

	cur := currentView{
		Time:         res.Current.Time.Format("15:04"),
		Date:         res.Current.Time.Format("02.01.2006"),
		WindKnots:    round1(res.Current.WindKnots),
		GustKnots:    round1(res.Current.GustKnots),
		WindMS:       round1(res.Current.WindKnots / wind.KnotsPerMS),
		Direction:    math.Round(res.Current.Direction),
		Rumb:         wind.Rumb(res.Current.Direction),
		RumbFull:     wind.RumbFull(res.Current.Direction),
		Beaufort:     b.Scale,
		BeaufortName: b.Name,
		SeaState:     b.SeaState,
		SailAdvice:   b.SailAdvice,
		SailorLevel:  b.SailorLevel,
		Status:       b.Status,
	}
	if res.WaveNow != nil {
		height := round1(res.WaveNow.HeightM)
		period := math.Round(res.WaveNow.PeriodS)
		cur.WaveM = &height
		cur.WavePeriodS = &period
		cur.WaveRumb = wind.Rumb(res.WaveNow.Direction)
	}

	hours := make([]hourView, 0, len(res.Hours))
	for _, h := range res.Hours {
		hb := wind.FromKnots(h.WindKnots)
		view := hourView{
			Time:      h.Time.Format(time.RFC3339),
			Label:     h.Time.Format("15:04"),
			WindKnots: round1(h.WindKnots),
			GustKnots: round1(h.GustKnots),
			Direction: math.Round(h.Direction),
			Rumb:      wind.Rumb(h.Direction),
			Beaufort:  hb.Scale,
			Status:    hb.Status,
		}
		if h.WaveM != nil {
			wave := round1(*h.WaveM)
			view.WaveM = &wave
		}
		hours = append(hours, view)
	}

	return forecastView{
		Spot:      spot,
		Timezone:  res.Timezone,
		Generated: time.Now().UTC().Format(time.RFC3339),
		Current:   cur,
		Hours:     hours,
		Checklist: wind.Checklist(b.Scale),
	}
}

// --- утилиты ---

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// ttlCache — простейший потокобезопасный кеш готовых JSON-ответов.
type ttlCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]cacheEntry
}

type cacheEntry struct {
	body    []byte
	expires time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{ttl: ttl, entries: make(map[string]cacheEntry)}
}

func (c *ttlCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.body, true
}

func (c *ttlCache) set(key string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = cacheEntry{body: body, expires: time.Now().Add(c.ttl)}
}

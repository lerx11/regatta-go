// Package api — HTTP-слой сервиса. Тонкий: разбирает запрос, зовёт meteo и wind,
// сериализует ответ. Держит маленький TTL-кеш, чтобы не дёргать бесплатный
// Open-Meteo на каждый запрос.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lerx11/marina-wind/internal/booking"
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
	book  *booking.Service
}

// New создаёт HTTP-слой поверх клиента Open-Meteo и сервиса бронирования.
func New(client *meteo.Client, book *booking.Service) *API {
	return &API{meteo: client, cache: newTTLCache(cacheTTL), book: book}
}

// Register вешает маршруты на mux (Go 1.22+ pattern matching).
func (a *API) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/spots", a.spots)
	mux.HandleFunc("GET /api/scale", a.scale)
	mux.HandleFunc("GET /api/forecast", a.forecast)
	mux.HandleFunc("GET /api/trips", a.trips)
	mux.HandleFunc("POST /api/trips/{id}/book", a.bookTrip)

	// Закрытый экран клуба: страницу /club отдаёт main.go, ниже — данные.
	mux.HandleFunc("POST /api/club/login", a.clubLogin)
	mux.HandleFunc("POST /api/club/logout", a.clubLogout)
	mux.HandleFunc("GET /api/club/trips", a.clubTrips)
	mux.HandleFunc("GET /api/club/trips/{id}", a.clubTrip)
	mux.HandleFunc("POST /api/club/trips", a.clubCreateTrip)
	mux.HandleFunc("PATCH /api/club/trips/{id}", a.clubUpdateTrip)
	mux.HandleFunc("POST /api/club/trips/{id}/cancel", a.clubCancelTrip)
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

// trips отдаёт расписание выходов и число занятых мест.
func (a *API) trips(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	list, booked, err := a.book.Trips(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "не удалось получить занятые места: "+err.Error())
		return
	}

	type tripView struct {
		booking.Trip
		Booked *int `json:"booked"`
		Free   *int `json:"free"`
	}
	views := make([]tripView, len(list))
	for i, t := range list {
		views[i] = tripView{Trip: t}
		if booked != nil {
			b := booked[i]
			f := max(0, t.Capacity-b)
			views[i].Booked, views[i].Free = &b, &f
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"trips":              views,
		"storage_configured": a.book.StoreConfigured(),
		"timezone":           "Europe/Moscow",
	})
}

// bookTrip записывает участника на выход.
func (a *API) bookTrip(w http.ResponseWriter, r *http.Request) {
	var crew booking.Crew
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	if err := dec.Decode(&crew); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: ожидаются поля name и contact")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	result, err := a.book.Reserve(ctx, r.PathValue("id"), crew)
	if err != nil {
		var invalid booking.InvalidError
		switch {
		case errors.Is(err, booking.ErrNotConfigured):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		case errors.Is(err, booking.ErrFull):
			writeError(w, http.StatusConflict, "на этот выход уже нет свободных мест")
		case errors.Is(err, booking.ErrTripStarted):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "Запись закрыта. Выход уже начался",
				"code":  "trip_started",
			})
		case errors.Is(err, booking.ErrTripCancelled):
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "Запись закрыта. Выход отменён",
				"code":  "trip_cancelled",
			})
		case errors.Is(err, booking.ErrTripNotFound):
			writeError(w, http.StatusNotFound, "выход не найден или уже прошёл")
		case errors.As(err, &invalid):
			writeError(w, http.StatusBadRequest, invalid.Error())
		default:
			writeError(w, http.StatusBadGateway, "не удалось сохранить запись: "+err.Error())
		}
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, result)
}

// --- закрытый экран клуба ---

const clubCookie = "regatta_club"

// clubPassword читает пароль клуба из окружения при каждом запросе.
func clubPassword() string { return strings.TrimSpace(os.Getenv("CLUB_PASSWORD")) }

// clubToken — HMAC от пароля: в cookie не лежит сам пароль, а при его смене
// старые cookie автоматически перестают действовать.
func clubToken(password string) string {
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte("regatta-club-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

// ClubAuthorized проверяет cookie клуба. Используется и в main.go при отдаче
// страницы /club, и в обработчиках данных.
func ClubAuthorized(r *http.Request) bool {
	password := clubPassword()
	if password == "" {
		return false
	}
	c, err := r.Cookie(clubCookie)
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(c.Value), []byte(clubToken(password)))
}

func (a *API) clubLogin(w http.ResponseWriter, r *http.Request) {
	password := clubPassword()
	if password == "" {
		writeError(w, http.StatusServiceUnavailable, "вход в клуб не настроен: задайте CLUB_PASSWORD")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON: ожидается поле password")
		return
	}
	if !hmac.Equal([]byte(body.Password), []byte(password)) {
		writeError(w, http.StatusUnauthorized, "Неверный пароль")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     clubCookie,
		Value:    clubToken(password),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(r),
		MaxAge:   12 * 60 * 60,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) clubLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     clubCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isSecure(r),
		MaxAge:   -1,
	})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *API) clubTrips(w http.ResponseWriter, r *http.Request) {
	if !requireClub(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	current, next, err := a.book.ClubTrips(ctx)
	if err != nil {
		writeClubError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"current":  current,
		"next":     next,
		"timezone": "Europe/Moscow",
	})
}

func (a *API) clubTrip(w http.ResponseWriter, r *http.Request) {
	if !requireClub(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	detail, err := a.book.ClubTrip(ctx, r.PathValue("id"))
	if err != nil {
		switch {
		case errors.Is(err, booking.ErrTripNotFound):
			writeError(w, http.StatusNotFound, "выход не найден")
		default:
			writeClubError(w, err)
		}
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, detail)
}

// clubCreateTrip добавляет разовый выход в расписание.
func (a *API) clubCreateTrip(w http.ResponseWriter, r *http.Request) {
	if !requireClub(w, r) {
		return
	}
	var in booking.TripInput
	if !decodeClubBody(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	trip, err := a.book.CreateTrip(ctx, in)
	if err != nil {
		writeTripError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, trip)
}

// clubUpdateTrip переносит выход или меняет вместимость.
func (a *API) clubUpdateTrip(w http.ResponseWriter, r *http.Request) {
	if !requireClub(w, r) {
		return
	}
	var in booking.TripUpdate
	if !decodeClubBody(w, r, &in) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	trip, err := a.book.UpdateTrip(ctx, r.PathValue("id"), in)
	if err != nil {
		writeTripError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, trip)
}

// clubCancelTrip закрывает запись, сохраняя карточку и экипаж.
func (a *API) clubCancelTrip(w http.ResponseWriter, r *http.Request) {
	if !requireClub(w, r) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	detail, err := a.book.CancelTrip(ctx, r.PathValue("id"))
	if err != nil {
		writeTripError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, detail)
}

// decodeClubBody читает JSON-тело изменения; при ошибке сам пишет 400.
func decodeClubBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "некорректный JSON в запросе")
		return false
	}
	return true
}

// writeTripError переводит ошибки управления выходами в HTTP-коды.
func writeTripError(w http.ResponseWriter, err error) {
	var invalid booking.InvalidError
	switch {
	case errors.Is(err, booking.ErrNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "управление выходами недоступно: не настроен Upstash Redis")
	case errors.Is(err, booking.ErrTimeConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, booking.ErrAlreadyCancelled), errors.Is(err, booking.ErrTripCancelled):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, booking.ErrTripStarted):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "Выход уже начался",
			"code":  "trip_started",
		})
	case errors.Is(err, booking.ErrTripNotFound):
		writeError(w, http.StatusNotFound, "выход не найден")
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Error())
	default:
		writeError(w, http.StatusBadGateway, "не удалось сохранить изменение: "+err.Error())
	}
}

// requireClub пускает дальше только с действующей cookie клуба.
func requireClub(w http.ResponseWriter, r *http.Request) bool {
	if ClubAuthorized(r) {
		return true
	}
	writeError(w, http.StatusUnauthorized, "нужен пароль клуба")
	return false
}

func writeClubError(w http.ResponseWriter, err error) {
	if errors.Is(err, booking.ErrNotConfigured) {
		writeError(w, http.StatusServiceUnavailable, "данные клуба недоступны: не настроен Upstash Redis")
		return
	}
	writeError(w, http.StatusBadGateway, "не удалось загрузить данные клуба")
}

func isSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
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

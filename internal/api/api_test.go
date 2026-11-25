package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lerx11/marina-wind/internal/booking"
	"github.com/lerx11/marina-wind/internal/meteo"
)

func newTestServer() http.Handler {
	book := booking.NewService(nil)
	book.SetNow(func() time.Time { return time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC) })
	mux := http.NewServeMux()
	New(meteo.New(time.Second), book).Register(mux)
	return mux
}

func TestSpotsEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/spots", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("статус = %d, ожидали 200", rec.Code)
	}

	var body struct {
		Spots []struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"spots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не разобрался как JSON: %v", err)
	}
	if len(body.Spots) == 0 || body.Spots[0].Slug == "" {
		t.Fatalf("неожиданный список акваторий: %+v", body.Spots)
	}
}

func TestScaleEndpointIsSerializable(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/scale", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("статус = %d, ожидали 200", rec.Code)
	}

	var body struct {
		Scale []struct {
			Scale      int     `json:"scale"`
			Name       string  `json:"name"`
			Range      string  `json:"range"`
			UpperKnots float64 `json:"upper_knots"`
		} `json:"scale"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("шкала Бофорта не сериализуется в JSON: %v", err)
	}
	if len(body.Scale) != 13 {
		t.Fatalf("ожидали 13 ступеней, получили %d", len(body.Scale))
	}
}

func TestForecastRejectsUnknownSpot(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/forecast?spot=atlantis", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("статус = %d, ожидали 404", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ об ошибке должен быть JSON: %v", err)
	}
	if body["error"] == "" {
		t.Error("ожидали поле error с пояснением")
	}
}

func TestUnknownRouteIsNotFound(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("статус = %d, ожидали 404", rec.Code)
	}
}

func TestHealthEndpoint(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("статус = %d, ожидали 200", rec.Code)
	}
}

func TestTripsWithoutStorage(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/trips", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("статус = %d, ожидали 200", rec.Code)
	}
	var body struct {
		Trips []struct {
			ID       string `json:"id"`
			SpotName string `json:"spot_name"`
			Boat     string `json:"boat"`
			Capacity int    `json:"capacity"`
			Booked   *int   `json:"booked"`
			Free     *int   `json:"free"`
		} `json:"trips"`
		StorageConfigured bool `json:"storage_configured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ответ не разобрался как JSON: %v", err)
	}
	if len(body.Trips) == 0 {
		t.Fatal("расписание пустое")
	}
	if body.StorageConfigured {
		t.Error("Redis не задан, но storage_configured = true")
	}
	for _, tr := range body.Trips {
		if tr.ID == "" || tr.SpotName == "" || tr.Boat == "" || tr.Capacity <= 0 {
			t.Errorf("неполный выход в расписании: %+v", tr)
		}
		if tr.Booked != nil || tr.Free != nil {
			t.Errorf("без Redis ожидали booked/free = null: %+v", tr)
		}
	}
}

func TestBookingWithoutStorageReturnsClearError(t *testing.T) {
	body := strings.NewReader(`{"name":"Иван","contact":"+7 900 000-00-00"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/trips/2026-11-23-strogino-10/book", body)
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("статус = %d, ожидали 503", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("ответ не JSON: %v", err)
	}
	if !strings.Contains(out["error"], "Upstash") {
		t.Errorf("ожидали упоминание Upstash в ошибке, получили %q", out["error"])
	}
}

func TestBookingRejectsBrokenJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/trips/x/book", strings.NewReader(`{`))
	rec := httptest.NewRecorder()
	newTestServer().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("статус = %d, ожидали 400", rec.Code)
	}
}

// memStore — хранилище в памяти для проверок HTTP-слоя.
type memStore struct {
	mu     sync.Mutex
	booked map[string]int
	crew   map[string][]booking.Member
}

func newMemStore() *memStore {
	return &memStore{booked: map[string]int{}, crew: map[string][]booking.Member{}}
}

func (m *memStore) Reserve(_ context.Context, tripID string, capacity int, pid, payload string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.booked[tripID] >= capacity {
		return 0, booking.ErrFull
	}
	m.booked[tripID]++
	var member booking.Member
	_ = json.Unmarshal([]byte(payload), &member)
	member.ParticipantID = pid
	m.crew[tripID] = append(m.crew[tripID], member)
	return m.booked[tripID], nil
}

func (m *memStore) Booked(_ context.Context, ids []string) ([]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = m.booked[id]
	}
	return out, nil
}

func (m *memStore) Crew(_ context.Context, tripID string) ([]booking.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]booking.Member, len(m.crew[tripID]))
	copy(out, m.crew[tripID])
	return out, nil
}

func newClubServer(now time.Time) http.Handler {
	store := newMemStore()
	book := booking.NewService(store)
	book.SetNow(func() time.Time { return now })
	mux := http.NewServeMux()
	New(meteo.New(time.Second), book).Register(mux)
	return mux
}

func TestBookingAfterStartReturnsTripStarted(t *testing.T) {
	start := time.Date(2026, 11, 25, 18, 0, 0, 0, time.FixedZone("MSK", 3*60*60))
	srv := newClubServer(start)

	req := httptest.NewRequest(http.MethodPost, "/api/trips/2026-11-25-strogino-18/book",
		strings.NewReader(`{"name":"Иван","contact":"+7 900 000-00-00"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("статус = %d, ожидали 409: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "trip_started" || !strings.Contains(body.Error, "Выход уже начался") {
		t.Errorf("тело ответа: %+v", body)
	}
}

func TestClubRequiresPassword(t *testing.T) {
	t.Setenv("CLUB_PASSWORD", "sail-2026")
	srv := newClubServer(time.Date(2026, 11, 25, 17, 59, 0, 0, time.FixedZone("MSK", 3*60*60)))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/club/trips", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("без cookie статус = %d, ожидали 401", rec.Code)
	}

	// Неверный пароль.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/club/login", strings.NewReader(`{"password":"nope"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("неверный пароль: статус = %d, ожидали 401", rec.Code)
	}

	// Верный пароль выдаёт cookie, с ней данные доступны.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/club/login", strings.NewReader(`{"password":"sail-2026"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("верный пароль: статус = %d, ожидали 200", rec.Code)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("cookie клуба не выдана")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/club/trips", nil)
	req.AddCookie(cookies[0])
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("с cookie статус = %d, ожидали 200: %s", rec.Code, rec.Body)
	}
	var body struct {
		Current []map[string]any `json:"current"`
		Next    []map[string]any `json:"next"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Current) == 0 || len(body.Next) == 0 {
		t.Fatalf("ожидали оба списка, получили current=%d next=%d", len(body.Current), len(body.Next))
	}
}

func TestClubLoginWithoutPasswordConfigured(t *testing.T) {
	t.Setenv("CLUB_PASSWORD", "")
	srv := newClubServer(time.Now())

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/club/login", strings.NewReader(`{"password":"x"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("статус = %d, ожидали 503", rec.Code)
	}
}

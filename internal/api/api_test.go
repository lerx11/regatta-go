package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

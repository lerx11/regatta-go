package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lerx11/marina-wind/internal/meteo"
)

func newTestServer() http.Handler {
	mux := http.NewServeMux()
	New(meteo.New(time.Second)).Register(mux)
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

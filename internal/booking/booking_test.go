package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStore — фейковый Redis: счётчики в памяти под мьютексом.
type fakeStore struct {
	mu     sync.Mutex
	booked map[string]int
	crew   map[string][]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{booked: map[string]int{}, crew: map[string][]string{}}
}

func (f *fakeStore) Reserve(_ context.Context, tripID string, capacity int, participantID, payload string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.booked[tripID] >= capacity {
		return 0, ErrFull
	}
	f.booked[tripID]++
	f.crew[tripID] = append(f.crew[tripID], payload)
	return f.booked[tripID], nil
}

func (f *fakeStore) Booked(_ context.Context, ids []string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(ids))
	for i, id := range ids {
		out[i] = f.booked[id]
	}
	return out, nil
}

func fixedNow() time.Time {
	return time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC) // 08:00 МСК
}

func newTestService(store Store) *Service {
	s := NewService(store)
	s.SetNow(fixedNow)
	return s
}

const spbTripID = "2026-11-20-spb-12" // ближайшая пятница 12:00 при fixedNow, вместимость 2

func TestReserveLastSeatIsAtomic(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store)
	crew := Crew{Name: "Иван", Contact: "+7 900 000-00-00"}

	const attempts = 8
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Reserve(context.Background(), spbTripID, crew)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var ok, full int
	for err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrFull):
			full++
		default:
			t.Fatalf("неожиданная ошибка: %v", err)
		}
	}
	if ok != 2 || full != attempts-2 {
		t.Fatalf("записались %d, отказано %d; ожидали 2 и %d", ok, full, attempts-2)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if got := len(store.crew[spbTripID]); got != 2 {
		t.Errorf("в экипаже %d человек, ожидали 2", got)
	}
}

func TestReserveValidation(t *testing.T) {
	cases := []struct {
		name string
		crew Crew
	}{
		{"пустое имя", Crew{Name: " ", Contact: "+7 900 000-00-00"}},
		{"короткое имя", Crew{Name: "И", Contact: "+7 900 000-00-00"}},
		{"короткий контакт", Crew{Name: "Иван", Contact: "123"}},
		{"длинный контакт", Crew{Name: "Иван", Contact: strings.Repeat("9", 121)}},
	}
	for _, c := range cases {
		store := newFakeStore()
		svc := newTestService(store)
		_, err := svc.Reserve(context.Background(), spbTripID, c.crew)
		var invalid InvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: ожидали InvalidError, получили %v", c.name, err)
		}
		store.mu.Lock()
		if store.booked[spbTripID] != 0 {
			t.Errorf("%s: место занято при невалидных данных", c.name)
		}
		store.mu.Unlock()
	}
}

func TestReserveUnknownTrip(t *testing.T) {
	svc := newTestService(newFakeStore())
	_, err := svc.Reserve(context.Background(), "nope", Crew{Name: "Иван", Contact: "+7 900 000-00-00"})
	if !errors.Is(err, ErrTripNotFound) {
		t.Fatalf("ожидали ErrTripNotFound, получили %v", err)
	}
}

func TestWithoutStore(t *testing.T) {
	svc := newTestService(nil)

	trips, booked, err := svc.Trips(context.Background())
	if err != nil {
		t.Fatalf("Trips без Redis: %v", err)
	}
	if len(trips) == 0 {
		t.Fatal("расписание пустое")
	}
	if booked != nil {
		t.Error("без Redis ожидали booked == nil")
	}

	_, err = svc.Reserve(context.Background(), trips[0].ID, Crew{Name: "Иван", Contact: "+7 900 000-00-00"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ожидали ErrNotConfigured, получили %v", err)
	}
}

func TestTripsReportBookedSeats(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store)
	crew := Crew{Name: "Иван", Contact: "+7 900 000-00-00"}

	if _, err := svc.Reserve(context.Background(), spbTripID, crew); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	trips, booked, err := svc.Trips(context.Background())
	if err != nil {
		t.Fatalf("Trips: %v", err)
	}
	for i, tr := range trips {
		want := 0
		if tr.ID == spbTripID {
			want = 1
		}
		if booked[i] != want {
			t.Errorf("%s: занято %d, ожидали %d", tr.ID, booked[i], want)
		}
	}
}

func TestScheduleIsConsistent(t *testing.T) {
	now := fixedNow()
	trips := Schedule(now)
	if len(trips) != len(slots) {
		t.Fatalf("выходов %d, ожидали %d", len(trips), len(slots))
	}
	idFormat := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-[a-z]+-\d{2}$`)
	seen := map[string]bool{}
	for _, tr := range trips {
		if seen[tr.ID] {
			t.Errorf("дубликат ID: %s", tr.ID)
		}
		seen[tr.ID] = true
		if !idFormat.MatchString(tr.ID) {
			t.Errorf("ID не в ожидаемом формате: %s", tr.ID)
		}
		if tr.Capacity <= 0 || tr.Boat == "" || tr.SpotName == "" {
			t.Errorf("неполный выход: %+v", tr)
		}
		start, err := time.ParseInLocation("2006-01-02 15:04", tr.Date+" "+tr.Time, moscow)
		if err != nil || !start.After(now) {
			t.Errorf("выход в прошлом или не разобрался: %+v", tr)
		}
	}
}

// ID вечернего Строгино должен быть одним и тем же от вечера накануне
// до минуты перед стартом — включая переход через полночь по Москве.
func TestEveningTripIDStableBeforeStart(t *testing.T) {
	const want = "2026-11-25-strogino-18"
	moments := []struct {
		name string
		now  time.Time
	}{
		{"вечер 24 ноября", time.Date(2026, 11, 24, 20, 0, 0, 0, moscow)},
		{"после полуночи", time.Date(2026, 11, 25, 0, 5, 0, 0, moscow)},
		{"25 ноября 17:59", time.Date(2026, 11, 25, 17, 59, 0, 0, moscow)},
	}
	for _, m := range moments {
		t.Run(m.name, func(t *testing.T) {
			trips := Schedule(m.now)
			assertNoDuplicateIDs(t, trips)

			var found int
			for _, tr := range trips {
				if tr.ID == want {
					found++
					if tr.Date != "2026-11-25" || tr.Time != "18:00" {
						t.Errorf("у %s дата/время %s %s", want, tr.Date, tr.Time)
					}
				}
			}
			if found != 1 {
				t.Fatalf("%s встречается %d раз, ожидали один", want, found)
			}
			if _, ok := FindTrip(m.now, want); !ok {
				t.Errorf("FindTrip не нашёл %s", want)
			}
		})
	}
}

func TestTripClosedAtStart(t *testing.T) {
	const id = "2026-11-25-strogino-18"
	start := time.Date(2026, 11, 25, 18, 0, 0, 0, moscow)
	crew := Crew{Name: "Иван", Contact: "+7 900 000-00-00"}

	if _, ok := FindTrip(start, id); ok {
		t.Errorf("в момент старта %s должен быть закрыт", id)
	}

	svc := newTestService(newFakeStore())
	svc.SetNow(func() time.Time { return start })
	if _, err := svc.Reserve(context.Background(), id, crew); !errors.Is(err, ErrTripNotFound) {
		t.Errorf("в 18:00 ожидали ErrTripNotFound, получили %v", err)
	}

	before := newTestService(newFakeStore())
	before.SetNow(func() time.Time { return start.Add(-time.Minute) })
	if _, err := before.Reserve(context.Background(), id, crew); err != nil {
		t.Errorf("в 17:59 запись должна проходить, получили %v", err)
	}
}

// Набор ID, опубликованный до полуночи, не должен меняться после исправления:
// к ним привязаны брони в Redis.
func TestPublishedIDsPreserved(t *testing.T) {
	now := time.Date(2026, 11, 24, 20, 0, 0, 0, moscow)
	want := []string{
		"2026-11-25-strogino-10",
		"2026-11-25-strogino-18",
		"2026-11-26-gelendzhik-11",
		"2026-11-27-spb-12",
		"2026-11-28-kazan-16",
		"2026-11-30-sochi-10",
	}
	trips := Schedule(now)
	assertNoDuplicateIDs(t, trips)

	got := make(map[string]bool, len(trips))
	for _, tr := range trips {
		got[tr.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("пропал опубликованный выход %s", id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("выходов %d, ожидали %d: %v", len(got), len(want), got)
	}
}

func assertNoDuplicateIDs(t *testing.T, trips []Trip) {
	t.Helper()
	seen := map[string]bool{}
	for _, tr := range trips {
		if seen[tr.ID] {
			t.Errorf("дубликат ID: %s", tr.ID)
		}
		seen[tr.ID] = true
	}
}

// upstashStub отвечает на REST-запросы как Upstash.
func upstashStub(t *testing.T, handler func(args []any) any) *Upstash {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("Authorization = %q", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var args []any
		if err := json.Unmarshal(raw, &args); err != nil {
			t.Fatalf("тело запроса не массив JSON: %s", raw)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": handler(args)})
	}))
	t.Cleanup(srv.Close)
	return NewUpstash(srv.URL, "token")
}

func TestUpstashReserveUsesEval(t *testing.T) {
	var got []any
	u := upstashStub(t, func(args []any) any {
		got = args
		return 3
	})
	n, err := u.Reserve(context.Background(), "trip-1", 4, "pid", `{"name":"Иван"}`)
	if err != nil || n != 3 {
		t.Fatalf("Reserve = %d, %v", n, err)
	}
	if len(got) != 8 || got[0] != "EVAL" || got[2] != "2" {
		t.Fatalf("ожидали EVAL с 2 ключами, получили %v", got)
	}
	if got[3] != "regatta:trip:trip-1:booked" || got[4] != "regatta:trip:trip-1:crew" {
		t.Errorf("ключи: %v", got[3:5])
	}
	if fmt.Sprint(got[7]) != `{"name":"Иван"}` {
		t.Errorf("payload = %v", got[7])
	}
}

func TestUpstashReserveFull(t *testing.T) {
	u := upstashStub(t, func([]any) any { return -1 })
	if _, err := u.Reserve(context.Background(), "trip-1", 1, "pid", "{}"); !errors.Is(err, ErrFull) {
		t.Fatalf("ожидали ErrFull, получили %v", err)
	}
}

func TestUpstashBookedParsesNulls(t *testing.T) {
	u := upstashStub(t, func(args []any) any {
		if args[0] != "MGET" {
			t.Errorf("ожидали MGET, получили %v", args[0])
		}
		// Реальный Upstash REST отдаёт значения счётчиков строками.
		return []any{nil, "2"}
	})
	got, err := u.Booked(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("Booked = %v", got)
	}
}

func TestUpstashReserveAcceptsStringResult(t *testing.T) {
	u := upstashStub(t, func([]any) any { return "1" })
	n, err := u.Reserve(context.Background(), "trip-1", 4, "pid", "{}")
	if err != nil || n != 1 {
		t.Fatalf("Reserve = %d, %v", n, err)
	}
}

func TestUpstashFromEnvSourceSelection(t *testing.T) {
	const (
		urlA = "https://upstash.example.io"
		urlB = "https://kv.example.io"
	)
	clear := func(t *testing.T) {
		t.Helper()
		for _, k := range []string{
			"UPSTASH_REDIS_REST_URL", "UPSTASH_REDIS_REST_TOKEN",
			"KV_REST_API_URL", "KV_REST_API_TOKEN", "KV_REST_API_READ_ONLY_TOKEN",
			"KV_URL", "REDIS_URL",
		} {
			t.Setenv(k, "")
		}
	}

	t.Run("ничего не задано", func(t *testing.T) {
		clear(t)
		if _, ok := UpstashFromEnv(); ok {
			t.Fatal("без переменных ожидали ok = false")
		}
	})

	t.Run("только read-only и KV_URL", func(t *testing.T) {
		clear(t)
		t.Setenv("KV_REST_API_READ_ONLY_TOKEN", "ro")
		t.Setenv("KV_URL", "redis://user:pass@host:6379")
		t.Setenv("REDIS_URL", "redis://user:pass@host:6379")
		if _, ok := UpstashFromEnv(); ok {
			t.Fatal("read-only токен и KV_URL не должны включать запись")
		}
	})

	t.Run("UPSTASH имеет приоритет", func(t *testing.T) {
		clear(t)
		t.Setenv("UPSTASH_REDIS_REST_URL", urlA)
		t.Setenv("UPSTASH_REDIS_REST_TOKEN", "token-a")
		t.Setenv("KV_REST_API_URL", urlB)
		t.Setenv("KV_REST_API_TOKEN", "token-b")
		u, ok := UpstashFromEnv()
		if !ok || u.url != urlA || u.token != "token-a" {
			t.Fatalf("ожидали UPSTASH (%s), получили %+v", urlA, u)
		}
	})

	t.Run("fallback на KV_REST_API", func(t *testing.T) {
		clear(t)
		t.Setenv("KV_REST_API_URL", urlB)
		t.Setenv("KV_REST_API_TOKEN", "token-b")
		u, ok := UpstashFromEnv()
		if !ok || u.url != urlB || u.token != "token-b" {
			t.Fatalf("ожидали KV (%s), получили %+v", urlB, u)
		}
	})

	t.Run("неполный UPSTASH падает на KV", func(t *testing.T) {
		clear(t)
		t.Setenv("UPSTASH_REDIS_REST_URL", urlA)
		t.Setenv("KV_REST_API_URL", urlB)
		t.Setenv("KV_REST_API_TOKEN", "token-b")
		u, ok := UpstashFromEnv()
		if !ok || u.url != urlB {
			t.Fatalf("ожидали fallback на KV, получили %+v", u)
		}
	})
}

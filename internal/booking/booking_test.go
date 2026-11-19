package booking

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

const spbTripID = "2026-11-23-spb-12" // вместимость 2

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
	seen := map[string]bool{}
	for _, tr := range trips {
		if seen[tr.ID] {
			t.Errorf("дубликат ID: %s", tr.ID)
		}
		seen[tr.ID] = true
		if tr.Capacity <= 0 || tr.Boat == "" || tr.SpotName == "" {
			t.Errorf("неполный выход: %+v", tr)
		}
		start, err := time.ParseInLocation("2006-01-02 15:04", tr.Date+" "+tr.Time, moscow)
		if err != nil || !start.After(now) {
			t.Errorf("выход в прошлом или не разобрался: %+v", tr)
		}
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
		return []any{nil, 2}
	})
	got, err := u.Booked(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("Booked = %v", got)
	}
}

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
	mu      sync.Mutex
	booked  map[string]int
	crew    map[string][]Member
	records map[string]TripRecord
}

func newFakeStore() *fakeStore {
	return &fakeStore{booked: map[string]int{}, crew: map[string][]Member{}, records: map[string]TripRecord{}}
}

func (f *fakeStore) TripRecords(context.Context) ([]TripRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]TripRecord, 0, len(f.records))
	for _, rec := range f.records {
		out = append(out, rec)
	}
	return out, nil
}

func (f *fakeStore) SaveTripRecord(_ context.Context, rec TripRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[rec.ID] = rec
	return nil
}

func (f *fakeStore) Reserve(_ context.Context, tripID string, capacity int, participantID, payload string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.booked[tripID] >= capacity {
		return 0, ErrFull
	}
	f.booked[tripID]++

	var m Member
	_ = json.Unmarshal([]byte(payload), &m)
	m.ParticipantID = participantID
	f.crew[tripID] = append(f.crew[tripID], m)
	return f.booked[tripID], nil
}

func (f *fakeStore) Crew(_ context.Context, tripID string) ([]Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Member, len(f.crew[tripID]))
	copy(out, f.crew[tripID])
	return out, nil
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
	// Клуб по-прежнему находит этот выход по стабильному ID.
	if _, ok := FindTripAny(start, id); !ok {
		t.Errorf("FindTripAny должен находить %s после старта", id)
	}

	svc := newTestService(newFakeStore())
	svc.SetNow(func() time.Time { return start })
	if _, err := svc.Reserve(context.Background(), id, crew); !errors.Is(err, ErrTripStarted) {
		t.Errorf("в 18:00 ожидали ErrTripStarted, получили %v", err)
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

// Клуб видит текущую и следующую неделю раздельно; начавшийся выход
// остаётся в текущей неделе и открывается по стабильному ID.
func TestClubTripsSplitByWeek(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store)
	svc.SetNow(func() time.Time { return time.Date(2026, 11, 25, 17, 59, 0, 0, moscow) })

	if _, err := svc.Reserve(context.Background(), "2026-11-25-strogino-18", Crew{Name: "Иван", Contact: "+7 900 000-00-00"}); err != nil {
		t.Fatalf("Reserve до старта: %v", err)
	}

	current, next, err := svc.ClubTrips(context.Background())
	if err != nil {
		t.Fatalf("ClubTrips: %v", err)
	}
	if len(current) != len(slots) || len(next) != len(slots) {
		t.Fatalf("текущая/следующая неделя: %d/%d, ожидали %d/%d", len(current), len(next), len(slots), len(slots))
	}

	cur := map[string]ClubTrip{}
	for _, tr := range current {
		cur[tr.ID] = tr
	}
	ten, ok := cur["2026-11-25-strogino-10"]
	if !ok || !ten.Started {
		t.Errorf("утренний выход должен быть в текущей неделе и уже начаться: %+v", ten)
	}
	if ten.Status != "finished" {
		t.Errorf("утренний выход закончился в 12:00, статус = %q", ten.Status)
	}
	sochiPast, ok := cur["2026-11-23-sochi-10"]
	if !ok {
		t.Fatal("выход Сочи 23 ноября должен быть в текущей неделе")
	}
	if sochiPast.Status == "live" {
		t.Error("Сочи 23 ноября не может быть «Идёт» 25 ноября")
	}
	if sochiPast.Status != "finished" {
		t.Errorf("Сочи 23 ноября: статус = %q, ожидали finished", sochiPast.Status)
	}
	for _, tr := range next {
		if tr.ID == "2026-11-30-sochi-10" && tr.Status == "closed" {
			t.Error("будущий выход 30 ноября не может быть «Запись закрыта»")
		}
	}
	evening, ok := cur["2026-11-25-strogino-18"]
	if !ok || evening.Started {
		t.Errorf("вечерний выход в 17:59 ещё не начался: %+v", evening)
	}
	if evening.Booked != 1 {
		t.Errorf("у вечернего выхода %d заявок, ожидали 1", evening.Booked)
	}
	if _, ok := cur["2026-11-26-gelendzhik-11"]; !ok {
		t.Error("выход четверга должен быть в текущей неделе")
	}

	for _, tr := range next {
		if tr.Started {
			t.Errorf("выход следующей недели не может быть начавшимся: %s", tr.ID)
		}
	}
	nextIDs := map[string]bool{}
	for _, tr := range next {
		nextIDs[tr.ID] = true
	}
	for _, id := range []string{"2026-11-30-sochi-10", "2026-12-02-strogino-18", "2026-12-05-kazan-16"} {
		if !nextIDs[id] {
			t.Errorf("в следующей неделе нет %s: %v", id, nextIDs)
		}
	}
}

func TestClubTripOpensAfterStart(t *testing.T) {
	store := newFakeStore()
	const id = "2026-11-25-strogino-18"

	before := newTestService(store)
	before.SetNow(func() time.Time { return time.Date(2026, 11, 25, 17, 59, 0, 0, moscow) })
	if _, err := before.Reserve(context.Background(), id, Crew{Name: "Иван", Contact: "+7 900 000-00-00"}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	after := newTestService(store)
	after.SetNow(func() time.Time { return time.Date(2026, 11, 25, 18, 0, 0, 0, moscow) })

	if _, ok := FindTrip(after.now(), id); ok {
		t.Error("публичное расписание не должно показывать начавшийся выход")
	}
	detail, err := after.ClubTrip(context.Background(), id)
	if err != nil {
		t.Fatalf("ClubTrip после старта: %v", err)
	}
	if !detail.Started || detail.Booked != 1 || len(detail.Crew) != 1 {
		t.Fatalf("после старта: %+v", detail)
	}
	if detail.Crew[0].Name != "Иван" {
		t.Errorf("имя участника = %q", detail.Crew[0].Name)
	}
	if _, err := after.ClubTrip(context.Background(), "2026-11-25-strogino-10"); err != nil {
		t.Errorf("утренний выход тоже должен открываться: %v", err)
	}
}

func TestTripStatus(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 11, 25, hour, minute, 0, 0, moscow)
	}
	cases := []struct {
		name string
		now  time.Time
		end  string
		want string
	}{
		{"до старта", at(9, 59), "12:00", ""},
		{"в момент старта", at(10, 0), "12:00", "live"},
		{"до окончания", at(11, 59), "12:00", "live"},
		{"в момент окончания", at(12, 0), "12:00", "finished"},
		{"после окончания", at(13, 0), "12:00", "finished"},
		{"без окончания до старта", at(9, 59), "", ""},
		{"без окончания после старта", at(10, 0), "", "closed"},
		{"без окончания на следующий день", at(23, 59), "", "closed"},
	}
	for _, c := range cases {
		trip := Trip{Date: "2026-11-25", Time: "10:00", End: c.end}
		if got := tripStatus(c.now, trip); got != c.want {
			t.Errorf("%s: статус = %q, ожидали %q", c.name, got, c.want)
		}
	}
}

type fakeNotifier struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (n *fakeNotifier) Send(_ context.Context, text string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.err != nil {
		return n.err
	}
	n.texts = append(n.texts, text)
	return nil
}

func TestReserveSendsOneNotification(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store)
	note := &fakeNotifier{}
	svc.SetNotifier(note)

	crew := Crew{Name: "Иван", Contact: "+7 900 000-00-00"}
	if _, err := svc.Reserve(context.Background(), spbTripID, crew); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	note.mu.Lock()
	defer note.mu.Unlock()
	if len(note.texts) != 1 {
		t.Fatalf("уведомлений %d, ожидали одно", len(note.texts))
	}
	for _, want := range []string{"Санкт-Петербург", "МСК", "Иван", crew.Contact} {
		if !strings.Contains(note.texts[0], want) {
			t.Errorf("в уведомлении нет %q:\n%s", want, note.texts[0])
		}
	}
}

func TestReserveSurvivesNotifierFailure(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store)
	svc.SetNotifier(&fakeNotifier{err: errors.New("telegram: HTTP 502")})

	res, err := svc.Reserve(context.Background(), spbTripID, Crew{Name: "Иван", Contact: "+7 900 000-00-00"})
	if err != nil {
		t.Fatalf("сбой уведомления не должен отменять заявку: %v", err)
	}
	if res.Booked != 1 {
		t.Errorf("занято %d мест, ожидали 1", res.Booked)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.crew[spbTripID]) != 1 {
		t.Errorf("участник не сохранён")
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

func TestUpstashCrewReadsHash(t *testing.T) {
	u := upstashStub(t, func(args []any) any {
		if args[0] != "HGETALL" || len(args) != 2 {
			t.Fatalf("ожидали HGETALL с одним ключом, получили %v", args)
		}
		if args[1] != "regatta:trip:trip-1:crew" {
			t.Errorf("ключ = %v", args[1])
		}
		return []any{
			"p2", `{"name":"Пётр","contact":"petr@example.com","at":"2026-11-25T15:01:00Z"}`,
			"p1", `{"name":"Иван","contact":"+7 900 000-00-00","at":"2026-11-25T15:00:00Z"}`,
		}
	})
	members, err := u.Crew(context.Background(), "trip-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("участников %d, ожидали 2", len(members))
	}
	if members[0].ParticipantID != "p1" || members[0].Name != "Иван" {
		t.Errorf("первый участник: %+v", members[0])
	}
	if members[1].Contact != "petr@example.com" {
		t.Errorf("второй участник: %+v", members[1])
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

func findTripByID(trips []Trip, id string) (Trip, bool) {
	for _, t := range trips {
		if t.ID == id {
			return t, true
		}
	}
	return Trip{}, false
}

func findClubTrip(t *testing.T, current, next []ClubTrip, id string) ClubTrip {
	t.Helper()
	for _, list := range [][]ClubTrip{current, next} {
		for _, ct := range list {
			if ct.ID == id {
				return ct
			}
		}
	}
	t.Fatalf("выход %s не найден в клубе", id)
	return ClubTrip{}
}

// Перенос меняет только выбранный выход: ID и заявки остаются, старый слот
// исчезает из публичного расписания, следующая неделя генерируется как обычно.
func TestMoveGeneratedTripKeepsIDAndCrew(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store)
	const id = "2026-11-25-strogino-18"
	crew := Crew{Name: "Иван", Contact: "+7 900 000-00-00"}

	if _, err := svc.Reserve(context.Background(), id, crew); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	moved, err := svc.UpdateTrip(context.Background(), id, TripUpdate{Start: "19:00", End: "21:00", Capacity: 4})
	if err != nil {
		t.Fatalf("UpdateTrip: %v", err)
	}
	if moved.ID != id || moved.Time != "19:00" || moved.End != "21:00" || moved.Booked != 1 {
		t.Fatalf("после переноса: %+v", moved)
	}

	trips, booked, err := svc.Trips(context.Background())
	if err != nil {
		t.Fatalf("Trips: %v", err)
	}
	var seen int
	for i, tr := range trips {
		if tr.ID != id {
			continue
		}
		seen++
		if tr.Time != "19:00" || tr.Date != "2026-11-25" {
			t.Errorf("перенесённый выход: %s %s", tr.Date, tr.Time)
		}
		if booked[i] != 1 {
			t.Errorf("занято %d мест, ожидали 1", booked[i])
		}
	}
	if seen != 1 {
		t.Errorf("%s встречается %d раз, ожидали один", id, seen)
	}
	for _, tr := range trips {
		if tr.Date == "2026-11-25" && tr.Spot == "strogino" && tr.Time == "18:00" {
			t.Errorf("старый слот 18:00 не должен оставаться в расписании: %+v", tr)
		}
	}

	// Следующая неделя не затронута: при наступлении следующей недели
	// генератор снова создаёт обычный слот 18:00.
	later := NewService(store)
	later.SetNow(func() time.Time { return time.Date(2026, 11, 26, 8, 0, 0, 0, moscow) })
	laterTrips, _, err := later.Trips(context.Background())
	if err != nil {
		t.Fatalf("Trips на следующей неделе: %v", err)
	}
	weekAfter, ok := findTripByID(laterTrips, "2026-12-02-strogino-18")
	if !ok || weekAfter.Time != "18:00" {
		t.Errorf("генератор не должен терять слот: %+v", weekAfter)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.crew[id]) != 1 {
		t.Errorf("экипаж потерян: %v", store.crew[id])
	}
	if rec := store.records[id]; rec.Start != "2026-11-25 19:00" {
		t.Errorf("запись переноса: %+v", rec)
	}
}

func TestMoveIntoAnotherSlotConflicts(t *testing.T) {
	svc := newTestService(newFakeStore())
	_, err := svc.UpdateTrip(context.Background(), "2026-11-25-strogino-18",
		TripUpdate{Start: "10:00", End: "12:00", Capacity: 4})
	if !errors.Is(err, ErrTimeConflict) {
		t.Fatalf("ожидали ErrTimeConflict, получили %v", err)
	}
}

func TestUpdateCapacityBelowBooked(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store)
	crew := Crew{Name: "Иван", Contact: "+7 900 000-00-00"}
	for i := 0; i < 2; i++ {
		if _, err := svc.Reserve(context.Background(), spbTripID, crew); err != nil {
			t.Fatalf("Reserve %d: %v", i+1, err)
		}
	}

	_, err := svc.UpdateTrip(context.Background(), spbTripID,
		TripUpdate{Start: "12:00", End: "14:00", Capacity: 1})
	var invalid InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("ожидали InvalidError, получили %v", err)
	}
	store.mu.Lock()
	if rec, ok := store.records[spbTripID]; ok && rec.Capacity == 1 {
		t.Error("неудачное изменение не должно сохраняться")
	}
	store.mu.Unlock()

	updated, err := svc.UpdateTrip(context.Background(), spbTripID,
		TripUpdate{Start: "12:00", End: "14:00", Capacity: 3})
	if err != nil {
		t.Fatalf("UpdateTrip до 3 мест: %v", err)
	}
	if updated.Capacity != 3 || updated.Booked != 2 {
		t.Errorf("после изменения: %+v", updated)
	}
}

// Сценарий из требований: создать выход, записать одного, перенести, отменить.
func TestCreateMoveCancelCustomTrip(t *testing.T) {
	store := newFakeStore()
	svc := newTestService(store)

	created, err := svc.CreateTrip(context.Background(), TripInput{
		Spot: "sochi", Boat: "Яхта 34 фута",
		Date: "2026-11-24", Start: "14:00", End: "17:00", Capacity: 4,
	})
	if err != nil {
		t.Fatalf("CreateTrip: %v", err)
	}
	if created.ID == "" || created.Time != "14:00" || created.Booked != 0 || created.SpotName != "Сочи" {
		t.Fatalf("созданный выход: %+v", created)
	}

	crew := Crew{Name: "Анна", Contact: "+7 900 123-45-67"}
	if _, err := svc.Reserve(context.Background(), created.ID, crew); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	moved, err := svc.UpdateTrip(context.Background(), created.ID,
		TripUpdate{Start: "15:00", End: "18:00", Capacity: 4})
	if err != nil {
		t.Fatalf("UpdateTrip: %v", err)
	}
	if moved.ID != created.ID || moved.Time != "15:00" || moved.Booked != 1 {
		t.Fatalf("после переноса: %+v", moved)
	}

	detail, err := svc.CancelTrip(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("CancelTrip: %v", err)
	}
	if detail.Status != "cancelled" || detail.Booked != 1 || len(detail.Crew) != 1 {
		t.Fatalf("отменённый выход: %+v", detail)
	}

	// Публично выхода больше нет, запись закрыта.
	trips, _, err := svc.Trips(context.Background())
	if err != nil {
		t.Fatalf("Trips: %v", err)
	}
	if _, ok := findTripByID(trips, created.ID); ok {
		t.Error("отменённый выход не должен попадать в публичное расписание")
	}
	if _, err := svc.Reserve(context.Background(), created.ID, crew); !errors.Is(err, ErrTripCancelled) {
		t.Errorf("запись на отменённый выход: ожидали ErrTripCancelled, получили %v", err)
	}

	// В клубе карточка и экипаж сохранены.
	current, next, err := svc.ClubTrips(context.Background())
	if err != nil {
		t.Fatalf("ClubTrips: %v", err)
	}
	card := findClubTrip(t, current, next, created.ID)
	if card.Status != "cancelled" || card.Booked != 1 {
		t.Errorf("карточка клуба: %+v", card)
	}
	if _, err := svc.CancelTrip(context.Background(), created.ID); !errors.Is(err, ErrAlreadyCancelled) {
		t.Errorf("повторная отмена: ожидали ErrAlreadyCancelled, получили %v", err)
	}
}

func TestCreateTripValidation(t *testing.T) {
	svc := newTestService(newFakeStore())
	cases := []struct {
		name string
		in   TripInput
		want error
	}{
		{"прошлое", TripInput{Spot: "sochi", Boat: "Яхта", Date: "2026-11-19", Start: "10:00", End: "12:00", Capacity: 4}, nil},
		{"занятый слот", TripInput{Spot: "strogino", Boat: "Лазер 4.7", Date: "2026-11-25", Start: "18:00", End: "20:00", Capacity: 4}, ErrTimeConflict},
	}
	for _, c := range cases {
		_, err := svc.CreateTrip(context.Background(), c.in)
		if c.want != nil {
			if !errors.Is(err, c.want) {
				t.Errorf("%s: ожидали %v, получили %v", c.name, c.want, err)
			}
			continue
		}
		var invalid InvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("%s: ожидали InvalidError, получили %v", c.name, err)
		}
	}

	if _, err := svc.CreateTrip(context.Background(), TripInput{
		Spot: "atlantis", Boat: "Яхта", Date: "2026-11-24", Start: "14:00", End: "17:00", Capacity: 4,
	}); !errors.As(err, new(InvalidError)) {
		t.Errorf("неизвестная акватория: %v", err)
	}
}

func TestStartedTripCannotBeChanged(t *testing.T) {
	start := time.Date(2026, 11, 25, 18, 0, 0, 0, moscow)
	svc := newTestService(newFakeStore())
	svc.SetNow(func() time.Time { return start })
	const id = "2026-11-25-strogino-18"

	if _, err := svc.UpdateTrip(context.Background(), id,
		TripUpdate{Start: "19:00", End: "21:00", Capacity: 4}); !errors.Is(err, ErrTripStarted) {
		t.Errorf("перенос начавшегося выхода: ожидали ErrTripStarted, получили %v", err)
	}
	if _, err := svc.CancelTrip(context.Background(), id); !errors.Is(err, ErrTripStarted) {
		t.Errorf("отмена начавшегося выхода: ожидали ErrTripStarted, получили %v", err)
	}
}

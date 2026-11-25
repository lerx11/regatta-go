// Package booking — расписание выходов на воду и запись в экипаж.
package booking

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lerx11/marina-wind/internal/spots"
)

// Trip — заранее подготовленный выход на воду.
type Trip struct {
	ID       string `json:"id"`
	Date     string `json:"date"`          // 2026-11-22
	Label    string `json:"label"`         // «сб, 22 ноября»
	Time     string `json:"time"`          // 10:00
	End      string `json:"end,omitempty"` // 13:00, если время окончания известно
	Spot     string `json:"spot"`
	SpotName string `json:"spot_name"`
	Boat     string `json:"boat"`
	Capacity int    `json:"capacity"`
}

// Crew — данные участника.
type Crew struct {
	Name    string `json:"name"`
	Contact string `json:"contact"`
}

// Member — участник, записавшийся на выход (для клуба).
type Member struct {
	ParticipantID string `json:"participant_id"`
	Name          string `json:"name"`
	Contact       string `json:"contact"`
	At            string `json:"at"`
}

// BookingResult — итог записи на выход.
type BookingResult struct {
	ParticipantID string `json:"participant_id"`
	TripID        string `json:"trip_id"`
	Booked        int    `json:"booked"`
	SeatsLeft     int    `json:"seats_left"`
}

// ClubTrip — выход в клубном списке: вместе со статусом и числом заявок.
//
// Status: "" — ещё не начался, "live" — идёт, "finished" — завершён,
// "closed" — начался, но время окончания неизвестно.
type ClubTrip struct {
	Trip
	Started bool   `json:"started"` // время старта наступило
	Status  string `json:"status"`
	Booked  int    `json:"booked"`
}

// ClubTripDetail — выход и его экипаж.
type ClubTripDetail struct {
	ClubTrip
	Crew []Member `json:"crew"`
}

var (
	// ErrFull — свободных мест на выходе не осталось.
	ErrFull = errors.New("мест не осталось")
	// ErrTripNotFound — выхода с таким ID нет в актуальном расписании.
	ErrTripNotFound = errors.New("выход не найден или уже прошёл")
	// ErrTripStarted — выход уже начался: новые заявки не принимаются.
	ErrTripStarted = errors.New("выход уже начался")
	// ErrNotConfigured — Upstash Redis не настроен.
	ErrNotConfigured = errors.New("бронирование недоступно: не настроен Upstash Redis (UPSTASH_REDIS_REST_URL и UPSTASH_REDIS_REST_TOKEN)")
)

// InvalidError — ошибка валидации данных участника.
type InvalidError string

func (e InvalidError) Error() string { return string(e) }

// Store — хранилище занятых мест.
//
// Reserve обязан быть атомарным: проверка свободных мест и их занятие —
// одна неделимая операция на стороне хранилища. В Upstash это делает Lua-скрипт.
type Store interface {
	// Reserve занимает одно место и возвращает число занятых мест после записи.
	// Если мест не осталось, возвращает ErrFull.
	Reserve(ctx context.Context, tripID string, capacity int, participantID, payload string) (int, error)
	// Booked возвращает число занятых мест по каждому ID выхода.
	Booked(ctx context.Context, tripIDs []string) ([]int, error)
	// Crew возвращает участников, записавшихся на выход.
	Crew(ctx context.Context, tripID string) ([]Member, error)
}

// Notifier отправляет владельцу сайта уведомление о новой заявке.
type Notifier interface {
	Send(ctx context.Context, text string) error
}

// Service — расписание и запись.
type Service struct {
	store    Store // nil — бронирование выключено, расписание всё равно отдаём
	notifier Notifier
	now      func() time.Time
}

// NewService создаёт сервис. store == nil означает «Redis не настроен».
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// SetNow подменяет часы (нужно тестам).
func (s *Service) SetNow(fn func() time.Time) { s.now = fn }

// SetNotifier включает уведомления о новых заявках.
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// StoreConfigured сообщает, доступна ли запись.
func (s *Service) StoreConfigured() bool { return s.store != nil }

// Trips отдаёт расписание; booked == nil, если хранилище не настроено.
func (s *Service) Trips(ctx context.Context) ([]Trip, []int, error) {
	trips := Schedule(s.now())
	if s.store == nil || len(trips) == 0 {
		return trips, nil, nil
	}
	ids := make([]string, len(trips))
	for i, t := range trips {
		ids[i] = t.ID
	}
	booked, err := s.store.Booked(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	return trips, booked, nil
}

// Reserve записывает участника на выход.
func (s *Service) Reserve(ctx context.Context, tripID string, crew Crew) (BookingResult, error) {
	if s.store == nil {
		return BookingResult{}, ErrNotConfigured
	}
	if err := Validate(crew); err != nil {
		return BookingResult{}, err
	}
	now := s.now()
	trip, ok := FindTrip(now, tripID)
	if !ok {
		// Публичное расписание уже не показывает начавшийся выход, но ID
		// остался прежним — отличаем «уже начался» от «такого нет».
		if _, started := FindTripAny(now, tripID); started {
			return BookingResult{}, ErrTripStarted
		}
		return BookingResult{}, ErrTripNotFound
	}

	payload, err := json.Marshal(map[string]string{
		"name":    strings.TrimSpace(crew.Name),
		"contact": strings.TrimSpace(crew.Contact),
		"at":      time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return BookingResult{}, err
	}

	pid := newID()
	booked, err := s.store.Reserve(ctx, trip.ID, trip.Capacity, pid, string(payload))
	if err != nil {
		return BookingResult{}, err
	}

	// Заявка уже сохранена: сбой уведомления не должен её отменять.
	if s.notifier != nil {
		if err := s.notifier.Send(ctx, NotificationText(trip, crew)); err != nil {
			// В лог — только ID выхода и текст ошибки: без токена и контактов.
			log.Printf("уведомление о заявке на выход %s не отправлено: %v", trip.ID, err)
		}
	}

	return BookingResult{
		ParticipantID: pid,
		TripID:        trip.ID,
		Booked:        booked,
		SeatsLeft:     max(0, trip.Capacity-booked),
	}, nil
}

// NotificationText собирает сообщение владельцу о новой заявке.
// В сообщении нет ничего, кроме выхода и данных участника.
func NotificationText(t Trip, c Crew) string {
	date := t.Date
	if parsed, err := time.Parse("2006-01-02", t.Date); err == nil {
		date = parsed.Format("02.01.2006")
	}
	return fmt.Sprintf(
		"Новая заявка на выход\nАкватория: %s\nДата: %s, %s МСК\nИмя: %s\nКонтакт: %s",
		t.SpotName, date, t.Time,
		strings.TrimSpace(c.Name), strings.TrimSpace(c.Contact),
	)
}

// Validate проверяет имя и контакт участника.
func Validate(c Crew) error {
	name := strings.TrimSpace(c.Name)
	if n := len([]rune(name)); n < 2 || n > 80 {
		return InvalidError("имя: от 2 до 80 символов")
	}
	contact := strings.TrimSpace(c.Contact)
	if n := len([]rune(contact)); n < 5 || n > 120 {
		return InvalidError("контакт: от 5 до 120 символов — телефон или e-mail")
	}
	return nil
}

// --- расписание ---

type slot struct {
	spot     string
	boat     string
	capacity int
	hour     int
	duration time.Duration // 0 — время окончания неизвестно
	weekday  time.Weekday
}

// Выходы повторяются раз в неделю: дата считается от дня недели, а не от
// «сегодня + N», поэтому карточка не переезжает на следующий день в полночь.
var slots = []slot{
	{spot: "strogino", boat: "Лазер 4.7", capacity: 4, hour: 10, duration: 2 * time.Hour, weekday: time.Wednesday},
	{spot: "strogino", boat: "Лазер 4.7", capacity: 4, hour: 18, duration: 2 * time.Hour, weekday: time.Wednesday},
	{spot: "gelendzhik", boat: "МХ700", capacity: 6, hour: 11, duration: 3 * time.Hour, weekday: time.Thursday},
	{spot: "spb", boat: "Оптимист", capacity: 2, hour: 12, duration: 2 * time.Hour, weekday: time.Friday},
	{spot: "kazan", boat: "МХ700", capacity: 6, hour: 16, duration: 3 * time.Hour, weekday: time.Saturday},
	{spot: "sochi", boat: "Яхта 34 фута", capacity: 8, hour: 10, duration: 3 * time.Hour, weekday: time.Monday},
}

var moscow = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		return time.FixedZone("MSK", 3*60*60)
	}
	return loc
}()

var (
	weekdays = [...]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}
	months   = [...]string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}
)

// Schedule возвращает ближайшие выходы.
//
// Выход остаётся с тем же ID до момента старта, в том числе после полуночи:
// дата привязана к дню недели, а не к «сегодня + N». После старта карточка
// закрывается, и на её месте появляется выход следующей недели.
func Schedule(now time.Time) []Trip {
	now = now.In(moscow)
	trips := make([]Trip, 0, len(slots))
	for _, s := range slots {
		sp, ok := spots.Find(s.spot)
		if !ok {
			continue
		}
		trips = append(trips, makeTrip(nextStart(now, s.weekday, s.hour), s, sp))
	}
	sortTrips(trips)
	return trips
}

// makeTrip собирает выход по времени старта и шаблону расписания.
func makeTrip(start time.Time, s slot, sp spots.Spot) Trip {
	start = start.In(moscow)
	trip := Trip{
		ID:       fmt.Sprintf("%s-%s-%02d", start.Format("2006-01-02"), sp.Slug, s.hour),
		Date:     start.Format("2006-01-02"),
		Label:    weekdays[start.Weekday()] + ", " + strconv.Itoa(start.Day()) + " " + months[start.Month()-1],
		Time:     start.Format("15:04"),
		Spot:     sp.Slug,
		SpotName: sp.Name,
		Boat:     s.boat,
		Capacity: s.capacity,
	}
	if s.duration > 0 {
		trip.End = start.Add(s.duration).Format("15:04")
	}
	return trip
}

func sortTrips(trips []Trip) {
	sort.Slice(trips, func(i, j int) bool {
		if trips[i].Date != trips[j].Date {
			return trips[i].Date < trips[j].Date
		}
		return trips[i].Time < trips[j].Time
	})
}

// nextStart — ближайшее наступление дня недели и часа по Москве.
// Если сегодняшний выход уже начался, берётся следующий на той же неделе.
func nextStart(now time.Time, weekday time.Weekday, hour int) time.Time {
	now = now.In(moscow)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, moscow)
	days := (int(weekday) - int(today.Weekday()) + 7) % 7
	start := today.AddDate(0, 0, days).Add(time.Duration(hour) * time.Hour)
	if !start.After(now) {
		start = start.AddDate(0, 0, 7)
	}
	return start
}

// FindTrip ищет актуальный выход по ID.
func FindTrip(now time.Time, id string) (Trip, bool) {
	for _, t := range Schedule(now) {
		if t.ID == id {
			return t, true
		}
	}
	return Trip{}, false
}

// FindTripAny ищет выход по стабильному ID, включая уже начавшийся.
//
// Клуб должен открыть экипаж во время выхода и после него, когда публичное
// расписание уже переключилось на следующую неделю.
func FindTripAny(now time.Time, id string) (Trip, bool) {
	for _, t := range clubTrips(now) {
		if t.ID == id {
			return t, true
		}
	}
	return Trip{}, false
}

// ClubTrips отдаёт выходы текущей и следующей недели для экрана клуба.
// Текущая неделя включает и уже начавшиеся выходы.
func (s *Service) ClubTrips(ctx context.Context) (current, next []ClubTrip, err error) {
	if s.store == nil {
		return nil, nil, ErrNotConfigured
	}
	now := s.now()
	curTrips, nextTrips := weekTrips(now)

	ids := make([]string, 0, len(curTrips)+len(nextTrips))
	for _, t := range curTrips {
		ids = append(ids, t.ID)
	}
	for _, t := range nextTrips {
		ids = append(ids, t.ID)
	}
	booked, err := s.store.Booked(ctx, ids)
	if err != nil {
		return nil, nil, err
	}

	current = make([]ClubTrip, len(curTrips))
	for i, t := range curTrips {
		current[i] = ClubTrip{Trip: t, Started: started(now, t), Status: tripStatus(now, t), Booked: booked[i]}
	}
	next = make([]ClubTrip, len(nextTrips))
	for i, t := range nextTrips {
		next[i] = ClubTrip{Trip: t, Started: started(now, t), Status: tripStatus(now, t), Booked: booked[len(curTrips)+i]}
	}
	return current, next, nil
}

// ClubTrip отдаёт выход и его экипаж по стабильному ID.
func (s *Service) ClubTrip(ctx context.Context, id string) (ClubTripDetail, error) {
	if s.store == nil {
		return ClubTripDetail{}, ErrNotConfigured
	}
	now := s.now()
	var found *Trip
	for _, t := range clubTrips(now) {
		if t.ID == id {
			trip := t
			found = &trip
			break
		}
	}
	if found == nil {
		return ClubTripDetail{}, ErrTripNotFound
	}

	booked, err := s.store.Booked(ctx, []string{id})
	if err != nil {
		return ClubTripDetail{}, err
	}
	crew, err := s.store.Crew(ctx, id)
	if err != nil {
		return ClubTripDetail{}, err
	}
	if crew == nil {
		crew = []Member{}
	}

	return ClubTripDetail{
		ClubTrip: ClubTrip{
			Trip:    *found,
			Started: started(now, *found),
			Status:  tripStatus(now, *found),
			Booked:  booked[0],
		},
		Crew: crew,
	}, nil
}

// weekTrips строит для каждого выхода его дату на текущей и следующей неделе.
// Неделя начинается в понедельник по Москве.
func weekTrips(now time.Time) (current, next []Trip) {
	now = now.In(moscow)
	ws := weekStart(now)
	current = make([]Trip, 0, len(slots))
	next = make([]Trip, 0, len(slots))
	for _, s := range slots {
		sp, ok := spots.Find(s.spot)
		if !ok {
			continue
		}
		offset := (int(s.weekday) - int(time.Monday) + 7) % 7
		start := ws.AddDate(0, 0, offset).Add(time.Duration(s.hour) * time.Hour)
		current = append(current, makeTrip(start, s, sp))
		next = append(next, makeTrip(start.AddDate(0, 0, 7), s, sp))
	}
	sortTrips(current)
	sortTrips(next)
	return current, next
}

// clubTrips — все выходы, доступные клубу: текущая и следующая неделя.
func clubTrips(now time.Time) []Trip {
	cur, next := weekTrips(now)
	all := make([]Trip, 0, len(cur)+len(next))
	all = append(all, cur...)
	all = append(all, next...)
	return all
}

// weekStart — понедельник 00:00 текущей недели по Москве.
func weekStart(now time.Time) time.Time {
	now = now.In(moscow)
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, moscow)
	offset := (int(day.Weekday()) + 6) % 7 // понедельник = 0
	return day.AddDate(0, 0, -offset)
}

// started сообщает, наступило ли время старта выхода.
func started(now time.Time, t Trip) bool {
	start, err := time.ParseInLocation("2006-01-02 15:04", t.Date+" "+t.Time, moscow)
	if err != nil {
		return false
	}
	return !start.After(now.In(moscow))
}

// tripStatus вычисляет метку выхода для клуба.
//
// Если время окончания известно, «Идёт» показывается только между стартом и
// окончанием, после — «Завершён». Если окончание неизвестно, с момента старта
// показывается «Запись закрыта». До старта метки нет.
func tripStatus(now time.Time, t Trip) string {
	now = now.In(moscow)
	start, err := time.ParseInLocation("2006-01-02 15:04", t.Date+" "+t.Time, moscow)
	if err != nil || now.Before(start) {
		return ""
	}
	if t.End != "" {
		end, err := time.ParseInLocation("2006-01-02 15:04", t.Date+" "+t.End, moscow)
		if err == nil {
			if now.Before(end) {
				return "live"
			}
			return "finished"
		}
	}
	return "closed"
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

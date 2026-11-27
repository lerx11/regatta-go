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
// "closed" — начался, но время окончания неизвестно, "cancelled" — отменён.
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

// TripRecord — сохранённое изменение выхода или выход, созданный клубом.
//
// Записи живут в Redis и переживают перезапуск сервера. ID выхода не меняется
// при переносе: к нему привязаны счётчик мест и экипаж.
type TripRecord struct {
	ID        string `json:"id"`
	Spot      string `json:"spot,omitempty"`
	Boat      string `json:"boat,omitempty"`
	Start     string `json:"start"`         // 2006-01-02 15:04 по Москве
	End       string `json:"end,omitempty"` // 15:04
	Capacity  int    `json:"capacity"`
	Custom    bool   `json:"custom,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// TripStore — хранилище изменений расписания и созданных выходов.
type TripStore interface {
	TripRecords(ctx context.Context) ([]TripRecord, error)
	SaveTripRecord(ctx context.Context, rec TripRecord) error
}

// TripInput — данные нового выхода от организатора.
type TripInput struct {
	Spot     string `json:"spot"`
	Boat     string `json:"boat"`
	Date     string `json:"date"`  // 2006-01-02
	Start    string `json:"start"` // 15:04
	End      string `json:"end"`   // 15:04, пусто — окончание неизвестно
	Capacity int    `json:"capacity"`
}

// TripUpdate — изменение времени и вместимости существующего выхода.
type TripUpdate struct {
	Start    string `json:"start"` // 15:04
	End      string `json:"end"`   // 15:04, пусто — окончание неизвестно
	Capacity int    `json:"capacity"`
}

const (
	minCapacity = 1
	maxCapacity = 50
)

var (
	// ErrFull — свободных мест на выходе не осталось.
	ErrFull = errors.New("мест не осталось")
	// ErrTripNotFound — выхода с таким ID нет в актуальном расписании.
	ErrTripNotFound = errors.New("выход не найден или уже прошёл")
	// ErrTripStarted — выход уже начался: новые заявки не принимаются.
	ErrTripStarted = errors.New("выход уже начался")
	// ErrTripCancelled — выход отменён организатором.
	ErrTripCancelled = errors.New("выход отменён")
	// ErrAlreadyCancelled — выход уже был отменён.
	ErrAlreadyCancelled = errors.New("выход уже отменён")
	// ErrTimeConflict — на это время уже есть другой выход.
	ErrTimeConflict = errors.New("на это время уже есть другой выход")
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
	store    Store     // nil — бронирование выключено, расписание всё равно отдаём
	repo     TripStore // nil — управление выходами недоступно
	notifier Notifier
	now      func() time.Time
}

// NewService создаёт сервис. store == nil означает «Redis не настроен».
func NewService(store Store) *Service {
	s := &Service{store: store, now: time.Now}
	if repo, ok := store.(TripStore); ok {
		s.repo = repo
	}
	return s
}

// SetNow подменяет часы (нужно тестам).
func (s *Service) SetNow(fn func() time.Time) { s.now = fn }

// SetNotifier включает уведомления о новых заявках.
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// StoreConfigured сообщает, доступна ли запись.
func (s *Service) StoreConfigured() bool { return s.store != nil }

// Trips отдаёт расписание; booked == nil, если хранилище не настроено.
func (s *Service) Trips(ctx context.Context) ([]Trip, []int, error) {
	trips, err := s.publicTrips(ctx)
	if err != nil {
		return nil, nil, err
	}
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
	trip, ok, err := s.publicTrip(ctx, tripID)
	if err != nil {
		return BookingResult{}, err
	}
	if !ok {
		// Публичное расписание уже не показывает начавшийся или отменённый
		// выход, но ID остался прежним — отличаем причины друг от друга.
		_, rec, found, err := s.findManagedTrip(ctx, tripID)
		if err != nil {
			return BookingResult{}, err
		}
		if found {
			if rec.Cancelled {
				return BookingResult{}, ErrTripCancelled
			}
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
		Label:    labelFor(start),
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
// Текущая неделя включает и уже начавшиеся, и отменённые выходы.
func (s *Service) ClubTrips(ctx context.Context) (current, next []ClubTrip, err error) {
	if s.store == nil {
		return nil, nil, ErrNotConfigured
	}
	now := s.now()
	trips, recs, err := s.effectiveClubTrips(ctx)
	if err != nil {
		return nil, nil, err
	}

	ids := make([]string, len(trips))
	for i, t := range trips {
		ids[i] = t.ID
	}
	booked, err := s.store.Booked(ctx, ids)
	if err != nil {
		return nil, nil, err
	}

	ws := weekStart(now)
	for i, t := range trips {
		status := tripStatus(now, t)
		if rec, ok := recs[t.ID]; ok && rec.Cancelled {
			status = "cancelled"
		}
		ct := ClubTrip{Trip: t, Started: started(now, t), Status: status, Booked: booked[i]}
		if weekStart(tripStart(t)).Equal(ws) {
			current = append(current, ct)
		} else {
			next = append(next, ct)
		}
	}
	return current, next, nil
}

// ClubTrip отдаёт выход и его экипаж по стабильному ID.
func (s *Service) ClubTrip(ctx context.Context, id string) (ClubTripDetail, error) {
	return s.clubTripDetail(ctx, id)
}

func (s *Service) clubTripDetail(ctx context.Context, id string) (ClubTripDetail, error) {
	if s.store == nil {
		return ClubTripDetail{}, ErrNotConfigured
	}
	now := s.now()
	trip, rec, found, err := s.findManagedTrip(ctx, id)
	if err != nil {
		return ClubTripDetail{}, err
	}
	if !found {
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

	status := tripStatus(now, trip)
	if rec.Cancelled {
		status = "cancelled"
	}
	return ClubTripDetail{
		ClubTrip: ClubTrip{
			Trip:    trip,
			Started: started(now, trip),
			Status:  status,
			Booked:  booked[0],
		},
		Crew: crew,
	}, nil
}

// --- управление выходами ---

// CreateTrip добавляет разовый выход в расписание.
func (s *Service) CreateTrip(ctx context.Context, in TripInput) (ClubTrip, error) {
	if s.store == nil || s.repo == nil {
		return ClubTrip{}, ErrNotConfigured
	}
	sp, ok := spots.Find(in.Spot)
	if !ok {
		return ClubTrip{}, InvalidError("неизвестная акватория")
	}
	boat := strings.TrimSpace(in.Boat)
	if n := len([]rune(boat)); n < 2 || n > 60 {
		return ClubTrip{}, InvalidError("лодка: от 2 до 60 символов")
	}
	if err := validateCapacity(in.Capacity); err != nil {
		return ClubTrip{}, err
	}
	start, err := parseTripTime(in.Date, in.Start)
	if err != nil {
		return ClubTrip{}, InvalidError("дата и время: ожидается дата ГГГГ-ММ-ДД и время ЧЧ:ММ")
	}
	now := s.now().In(moscow)
	if !start.After(now) {
		return ClubTrip{}, InvalidError("выход должен быть в будущем")
	}
	end, err := parseEnd(in.Date, start, in.End)
	if err != nil {
		return ClubTrip{}, err
	}

	recs, err := s.loadRecords(ctx)
	if err != nil {
		return ClubTrip{}, err
	}
	if s.timeBusy(in.Date, start, sp.Slug, "", recs) {
		return ClubTrip{}, ErrTimeConflict
	}

	id := customID(start, sp.Slug)
	for {
		if _, exists := recs[id]; !exists {
			break
		}
		id = customID(start, sp.Slug)
	}

	rec := TripRecord{
		ID:        id,
		Spot:      sp.Slug,
		Boat:      boat,
		Start:     start.Format("2006-01-02 15:04"),
		End:       end,
		Capacity:  in.Capacity,
		Custom:    true,
		UpdatedAt: now.UTC().Format(time.RFC3339),
	}
	if err := s.repo.SaveTripRecord(ctx, rec); err != nil {
		return ClubTrip{}, err
	}
	trip, _ := tripFromRecord(rec)
	return ClubTrip{Trip: trip}, nil
}

// UpdateTrip меняет время и вместимость конкретного выхода.
//
// ID не меняется: к нему привязаны заявки в Redis, поэтому экипаж сохраняется,
// а старый слот исчезает из расписания — запись просто применяется поверх него.
func (s *Service) UpdateTrip(ctx context.Context, id string, in TripUpdate) (ClubTrip, error) {
	if s.store == nil || s.repo == nil {
		return ClubTrip{}, ErrNotConfigured
	}
	now := s.now().In(moscow)
	trip, rec, found, err := s.findManagedTrip(ctx, id)
	if err != nil {
		return ClubTrip{}, err
	}
	if !found {
		return ClubTrip{}, ErrTripNotFound
	}
	if rec.Cancelled {
		return ClubTrip{}, ErrAlreadyCancelled
	}
	if started(now, trip) {
		return ClubTrip{}, ErrTripStarted
	}

	if err := validateCapacity(in.Capacity); err != nil {
		return ClubTrip{}, err
	}
	start, err := parseTripTime(trip.Date, in.Start)
	if err != nil {
		return ClubTrip{}, InvalidError("начало: ожидается время ЧЧ:ММ")
	}
	if !start.After(now) {
		return ClubTrip{}, InvalidError("нельзя перенести выход в прошлое")
	}
	end, err := parseEnd(trip.Date, start, in.End)
	if err != nil {
		return ClubTrip{}, err
	}

	booked, err := s.store.Booked(ctx, []string{id})
	if err != nil {
		return ClubTrip{}, err
	}
	if in.Capacity < booked[0] {
		return ClubTrip{}, InvalidError(fmt.Sprintf("вместимость не может быть меньше числа заявок (%d)", booked[0]))
	}

	recs, err := s.loadRecords(ctx)
	if err != nil {
		return ClubTrip{}, err
	}
	if s.timeBusy(trip.Date, start, trip.Spot, id, recs) {
		return ClubTrip{}, ErrTimeConflict
	}

	rec = ensureRecord(rec, trip)
	rec.Start = start.Format("2006-01-02 15:04")
	rec.End = end
	rec.Capacity = in.Capacity
	rec.Cancelled = false
	rec.UpdatedAt = now.UTC().Format(time.RFC3339)
	if err := s.repo.SaveTripRecord(ctx, rec); err != nil {
		return ClubTrip{}, err
	}

	updated, ok := tripFromRecord(rec)
	if !ok {
		return ClubTrip{}, ErrTripNotFound
	}
	return ClubTrip{Trip: updated, Booked: booked[0]}, nil
}

// CancelTrip отменяет выход: запись закрывается, карточка и экипаж остаются.
func (s *Service) CancelTrip(ctx context.Context, id string) (ClubTripDetail, error) {
	if s.store == nil || s.repo == nil {
		return ClubTripDetail{}, ErrNotConfigured
	}
	now := s.now().In(moscow)
	trip, rec, found, err := s.findManagedTrip(ctx, id)
	if err != nil {
		return ClubTripDetail{}, err
	}
	if !found {
		return ClubTripDetail{}, ErrTripNotFound
	}
	if rec.Cancelled {
		return ClubTripDetail{}, ErrAlreadyCancelled
	}
	if started(now, trip) {
		return ClubTripDetail{}, ErrTripStarted
	}

	rec = ensureRecord(rec, trip)
	rec.Cancelled = true
	rec.UpdatedAt = now.UTC().Format(time.RFC3339)
	if err := s.repo.SaveTripRecord(ctx, rec); err != nil {
		return ClubTripDetail{}, err
	}
	return s.clubTripDetail(ctx, id)
}

// ensureRecord дополняет запись неизменяемыми полями выхода.
func ensureRecord(rec TripRecord, t Trip) TripRecord {
	if rec.ID == "" {
		rec.ID = t.ID
	}
	if rec.Spot == "" {
		rec.Spot = t.Spot
	}
	if rec.Boat == "" {
		rec.Boat = t.Boat
	}
	if rec.Start == "" {
		rec.Start = t.Date + " " + t.Time
	}
	if rec.Capacity == 0 {
		rec.Capacity = t.Capacity
	}
	if rec.End == "" {
		rec.End = t.End
	}
	return rec
}

// findManagedTrip ищет выход в клубе: текущая и следующая неделя плюс любой
// созданный вручную выход.
func (s *Service) findManagedTrip(ctx context.Context, id string) (Trip, TripRecord, bool, error) {
	trips, recs, err := s.effectiveClubTrips(ctx)
	if err != nil {
		return Trip{}, TripRecord{}, false, err
	}
	for _, t := range trips {
		if t.ID == id {
			return t, recs[id], true, nil
		}
	}
	if rec, ok := recs[id]; ok && rec.Custom {
		if t, ok := tripFromRecord(rec); ok {
			return t, rec, true, nil
		}
	}
	return Trip{}, TripRecord{}, false, nil
}

func (s *Service) publicTrip(ctx context.Context, id string) (Trip, bool, error) {
	trips, err := s.publicTrips(ctx)
	if err != nil {
		return Trip{}, false, err
	}
	for _, t := range trips {
		if t.ID == id {
			return t, true, nil
		}
	}
	return Trip{}, false, nil
}

// publicTrips собирает публичное расписание: сгенерированные выходы с учётом
// переносов и отмен, плюс созданные вручную будущие выходы.
func (s *Service) publicTrips(ctx context.Context) ([]Trip, error) {
	now := s.now().In(moscow)
	recs, err := s.loadRecords(ctx)
	if err != nil {
		return nil, err
	}

	trips := make([]Trip, 0, len(slots)+len(recs))
	for _, sl := range slots {
		sp, ok := spots.Find(sl.spot)
		if !ok {
			continue
		}
		start := nextStart(now, sl.weekday, sl.hour)
		// Перенос может сдвинуть выход в прошлое, а отмена — убрать его:
		// тогда показываем следующий такой слот через неделю.
		for i := 0; i < 12; i++ {
			t := makeTrip(start, sl, sp)
			if rec, has := recs[t.ID]; has {
				if rec.Cancelled {
					start = start.AddDate(0, 0, 7)
					continue
				}
				t = applyRecord(t, rec)
			}
			if !tripStart(t).After(now) {
				start = start.AddDate(0, 0, 7)
				continue
			}
			trips = append(trips, t)
			break
		}
	}

	for _, rec := range recs {
		if !rec.Custom || rec.Cancelled {
			continue
		}
		t, ok := tripFromRecord(rec)
		if !ok || !tripStart(t).After(now) {
			continue
		}
		trips = append(trips, t)
	}

	sortTrips(trips)
	return trips, nil
}

// effectiveClubTrips собирает выходы текущей и следующей недели с учётом
// переносов и отмен. Отменённые остаются в списке.
func (s *Service) effectiveClubTrips(ctx context.Context) ([]Trip, map[string]TripRecord, error) {
	now := s.now()
	recs, err := s.loadRecords(ctx)
	if err != nil {
		return nil, nil, err
	}

	ws := weekStart(now)
	nextWS := ws.AddDate(0, 0, 7)
	trips := make([]Trip, 0, len(slots)*2+len(recs))
	for _, sl := range slots {
		sp, ok := spots.Find(sl.spot)
		if !ok {
			continue
		}
		offset := (int(sl.weekday) - int(time.Monday) + 7) % 7
		for w := 0; w < 2; w++ {
			start := ws.AddDate(0, 0, offset+7*w).Add(time.Duration(sl.hour) * time.Hour)
			t := makeTrip(start, sl, sp)
			if rec, has := recs[t.ID]; has {
				t = applyRecord(t, rec)
			}
			trips = append(trips, t)
		}
	}

	for _, rec := range recs {
		if !rec.Custom {
			continue
		}
		t, ok := tripFromRecord(rec)
		if !ok {
			continue
		}
		switch wk := weekStart(tripStart(t)); {
		case wk.Equal(ws), wk.Equal(nextWS):
			trips = append(trips, t)
		}
	}

	sortTrips(trips)
	return trips, recs, nil
}

func (s *Service) loadRecords(ctx context.Context) (map[string]TripRecord, error) {
	out := make(map[string]TripRecord)
	if s.repo == nil {
		return out, nil
	}
	list, err := s.repo.TripRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, rec := range list {
		out[rec.ID] = rec
	}
	return out, nil
}

// applyRecord накладывает сохранённые изменения на сгенерированный выход.
func applyRecord(t Trip, rec TripRecord) Trip {
	if st, err := parseRecordStart(rec.Start); err == nil {
		t.Date = st.Format("2006-01-02")
		t.Time = st.Format("15:04")
		t.Label = labelFor(st)
	}
	if rec.End != "" {
		if et, err := time.Parse("15:04", rec.End); err == nil {
			t.End = et.Format("15:04")
		} else {
			t.End = ""
		}
	} else {
		t.End = ""
	}
	if rec.Capacity > 0 {
		t.Capacity = rec.Capacity
	}
	return t
}

// tripFromRecord собирает выход из записи, созданной или изменённой клубом.
func tripFromRecord(rec TripRecord) (Trip, bool) {
	st, err := parseRecordStart(rec.Start)
	if err != nil || rec.Capacity < minCapacity {
		return Trip{}, false
	}
	sp, ok := spots.Find(rec.Spot)
	if !ok {
		return Trip{}, false
	}
	end := ""
	if rec.End != "" {
		if et, err := time.Parse("15:04", rec.End); err == nil {
			end = et.Format("15:04")
		}
	}
	return Trip{
		ID:       rec.ID,
		Date:     st.Format("2006-01-02"),
		Label:    labelFor(st),
		Time:     st.Format("15:04"),
		End:      end,
		Spot:     sp.Slug,
		SpotName: sp.Name,
		Boat:     rec.Boat,
		Capacity: rec.Capacity,
	}, true
}

func parseRecordStart(s string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02 15:04", s, moscow)
}

func parseTripTime(date, hhmm string) (time.Time, error) {
	if _, err := time.Parse("15:04", hhmm); err != nil {
		return time.Time{}, err
	}
	return time.ParseInLocation("2006-01-02 15:04", date+" "+hhmm, moscow)
}

func parseEnd(date string, start time.Time, hhmm string) (string, error) {
	if strings.TrimSpace(hhmm) == "" {
		return "", nil
	}
	end, err := parseTripTime(date, hhmm)
	if err != nil {
		return "", InvalidError("окончание: ожидается время ЧЧ:ММ")
	}
	if !end.After(start) {
		return "", InvalidError("окончание должно быть позже начала")
	}
	return end.Format("15:04"), nil
}

func validateCapacity(n int) error {
	if n < minCapacity || n > maxCapacity {
		return InvalidError(fmt.Sprintf("мест: от %d до %d", minCapacity, maxCapacity))
	}
	return nil
}

func tripStart(t Trip) time.Time {
	ts, _ := time.ParseInLocation("2006-01-02 15:04", t.Date+" "+t.Time, moscow)
	return ts
}

func labelFor(t time.Time) string {
	return weekdays[t.Weekday()] + ", " + strconv.Itoa(t.Day()) + " " + months[t.Month()-1]
}

// timeBusy проверяет, занято ли время у другой акватории/выхода.
// Слот, перенесённый на другое время или отменённый, освобождает своё время.
func (s *Service) timeBusy(date string, start time.Time, spot, exceptID string, recs map[string]TripRecord) bool {
	day, err := time.ParseInLocation("2006-01-02", date, moscow)
	if err != nil {
		return true
	}
	for _, sl := range slots {
		if sl.spot != spot || sl.hour != start.Hour() || sl.weekday != day.Weekday() {
			continue
		}
		id := fmt.Sprintf("%s-%s-%02d", date, spot, sl.hour)
		if id == exceptID {
			continue
		}
		if rec, ok := recs[id]; ok {
			if rec.Cancelled {
				continue
			}
			if ts, err := parseRecordStart(rec.Start); err == nil && !ts.Equal(start) {
				continue
			}
		}
		return true
	}

	for _, rec := range recs {
		if rec.ID == exceptID || rec.Cancelled || rec.Spot != spot {
			continue
		}
		ts, err := parseRecordStart(rec.Start)
		if err != nil {
			continue
		}
		if ts.Equal(start) {
			return true
		}
	}
	return false
}

func customID(start time.Time, slug string) string {
	return fmt.Sprintf("%s-%s-%02d-%s", start.Format("2006-01-02"), slug, start.Hour(), newID()[:4])
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

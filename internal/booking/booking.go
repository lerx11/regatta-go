// Package booking — расписание выходов на воду и запись в экипаж.
package booking

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lerx11/marina-wind/internal/spots"
)

// Trip — заранее подготовленный выход на воду.
type Trip struct {
	ID       string `json:"id"`
	Date     string `json:"date"`  // 2026-11-22
	Label    string `json:"label"` // «сб, 22 ноября»
	Time     string `json:"time"`  // 10:00
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

// BookingResult — итог записи на выход.
type BookingResult struct {
	ParticipantID string `json:"participant_id"`
	TripID        string `json:"trip_id"`
	Booked        int    `json:"booked"`
	SeatsLeft     int    `json:"seats_left"`
}

var (
	// ErrFull — свободных мест на выходе не осталось.
	ErrFull = errors.New("мест не осталось")
	// ErrTripNotFound — выхода с таким ID нет в актуальном расписании.
	ErrTripNotFound = errors.New("выход не найден или уже прошёл")
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
}

// Service — расписание и запись.
type Service struct {
	store Store // nil — бронирование выключено, расписание всё равно отдаём
	now   func() time.Time
}

// NewService создаёт сервис. store == nil означает «Redis не настроен».
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// SetNow подменяет часы (нужно тестам).
func (s *Service) SetNow(fn func() time.Time) { s.now = fn }

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
	trip, ok := FindTrip(s.now(), tripID)
	if !ok {
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
	return BookingResult{
		ParticipantID: pid,
		TripID:        trip.ID,
		Booked:        booked,
		SeatsLeft:     max(0, trip.Capacity-booked),
	}, nil
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
	spot      string
	boat      string
	capacity  int
	hour      int
	daysAhead int
}

var slots = []slot{
	{spot: "strogino", boat: "Лазер 4.7", capacity: 4, hour: 10, daysAhead: 0},
	{spot: "strogino", boat: "Лазер 4.7", capacity: 4, hour: 18, daysAhead: 1},
	{spot: "gelendzhik", boat: "МХ700", capacity: 6, hour: 11, daysAhead: 2},
	{spot: "spb", boat: "Оптимист", capacity: 2, hour: 12, daysAhead: 3},
	{spot: "kazan", boat: "МХ700", capacity: 6, hour: 16, daysAhead: 4},
	{spot: "sochi", boat: "Яхта 34 фута", capacity: 8, hour: 10, daysAhead: 6},
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

// Schedule возвращает ближайшие выходы; уже начавшиеся не показывает.
func Schedule(now time.Time) []Trip {
	now = now.In(moscow)
	trips := make([]Trip, 0, len(slots))
	for _, s := range slots {
		start := time.Date(now.Year(), now.Month(), now.Day()+s.daysAhead, s.hour, 0, 0, 0, moscow)
		if !start.After(now) {
			continue
		}
		sp, ok := spots.Find(s.spot)
		if !ok {
			continue
		}
		trips = append(trips, Trip{
			ID:       fmt.Sprintf("%s-%s-%02d", start.Format("2006-01-02"), sp.Slug, s.hour),
			Date:     start.Format("2006-01-02"),
			Label:    weekdays[start.Weekday()] + ", " + strconv.Itoa(start.Day()) + " " + months[start.Month()-1],
			Time:     start.Format("15:04"),
			Spot:     sp.Slug,
			SpotName: sp.Name,
			Boat:     s.boat,
			Capacity: s.capacity,
		})
	}
	return trips
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

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

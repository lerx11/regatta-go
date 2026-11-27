package booking

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// reserveScript — атомарная проверка и занятие места.
//
// Redis выполняет Lua-скрипт целиком, без параллельных вставок: между чтением
// счётчика и его увеличением никто другой не может вклиниться. Поэтому двое
// одновременно не займут последнее место — второй получит -1 и ErrFull.
const reserveScript = `
local booked = tonumber(redis.call('GET', KEYS[1]) or '0')
if booked >= tonumber(ARGV[1]) then return -1 end
redis.call('INCR', KEYS[1])
redis.call('HSET', KEYS[2], ARGV[2], ARGV[3])
return booked + 1`

// Upstash — хранилище поверх Upstash Redis REST API.
type Upstash struct {
	url   string
	token string
	hc    *http.Client
}

// UpstashFromEnv читает креды Upstash Redis.
//
// Сначала UPSTASH_REDIS_REST_URL / UPSTASH_REDIS_REST_TOKEN, затем — имена,
// которые создаёт Vercel-интеграция Upstash: KV_REST_API_URL / KV_REST_API_TOKEN.
// Read-only токен и KV_URL/REDIS_URL не используются: они не подходят для записи.
// ok == false, если подходящей пары нет: сервис стартует без Redis.
func UpstashFromEnv() (u *Upstash, ok bool) {
	pairs := [][2]string{
		{"UPSTASH_REDIS_REST_URL", "UPSTASH_REDIS_REST_TOKEN"},
		{"KV_REST_API_URL", "KV_REST_API_TOKEN"},
	}
	for _, p := range pairs {
		url := strings.TrimSpace(os.Getenv(p[0]))
		token := strings.TrimSpace(os.Getenv(p[1]))
		if url != "" && token != "" {
			return NewUpstash(url, token), true
		}
	}
	return nil, false
}

// NewUpstash создаёт клиент Upstash.
func NewUpstash(url, token string) *Upstash {
	return &Upstash{
		url:   strings.TrimRight(url, "/"),
		token: token,
		hc:    &http.Client{Timeout: 5 * time.Second},
	}
}

// Reserve выполняет атомарный Lua-скрипт: место занимается только если оно есть.
func (u *Upstash) Reserve(ctx context.Context, tripID string, capacity int, participantID, payload string) (int, error) {
	raw, err := u.do(ctx,
		"EVAL", reserveScript, "2",
		bookedKey(tripID), crewKey(tripID),
		capacity, participantID, payload,
	)
	if err != nil {
		return 0, err
	}
	var bookedRaw json.RawMessage
	if err := json.Unmarshal(raw, &bookedRaw); err != nil {
		return 0, fmt.Errorf("upstash: неожиданный ответ EVAL: %w", err)
	}
	booked, err := parseRedisInt(bookedRaw)
	if err != nil {
		return 0, fmt.Errorf("upstash: EVAL: %w", err)
	}
	if booked < 0 {
		return 0, ErrFull
	}
	return booked, nil
}

// Booked читает счётчики занятых мест одним MGET.
func (u *Upstash) Booked(ctx context.Context, tripIDs []string) ([]int, error) {
	if len(tripIDs) == 0 {
		return []int{}, nil
	}
	args := make([]any, 0, len(tripIDs)+1)
	args = append(args, "MGET")
	for _, id := range tripIDs {
		args = append(args, bookedKey(id))
	}
	raw, err := u.do(ctx, args...)
	if err != nil {
		return nil, err
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("upstash: неожиданный ответ MGET: %w", err)
	}
	if len(values) != len(tripIDs) {
		return nil, errors.New("upstash: MGET вернул неверное число значений")
	}
	out := make([]int, len(values))
	for i, v := range values {
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		n, err := parseRedisInt(v)
		if err != nil {
			return nil, fmt.Errorf("upstash: MGET: %w", err)
		}
		out[i] = n
	}
	return out, nil
}

// Crew читает экипаж выхода: hash participantID → JSON участника.
func (u *Upstash) Crew(ctx context.Context, tripID string) ([]Member, error) {
	raw, err := u.do(ctx, "HGETALL", crewKey(tripID))
	if err != nil {
		return nil, err
	}
	pairs, err := hashPairs(raw)
	if err != nil {
		return nil, fmt.Errorf("upstash: HGETALL: %w", err)
	}
	members := make([]Member, 0, len(pairs))
	for pid, payload := range pairs {
		var m Member
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			continue // одна битая запись не должна ломать весь экран клуба
		}
		m.ParticipantID = pid
		members = append(members, m)
	}
	sort.Slice(members, func(i, j int) bool {
		if members[i].At != members[j].At {
			return members[i].At < members[j].At
		}
		return members[i].Name < members[j].Name
	})
	return members, nil
}

// TripRecords читает сохранённые изменения расписания и созданные выходы.
func (u *Upstash) TripRecords(ctx context.Context) ([]TripRecord, error) {
	raw, err := u.do(ctx, "HGETALL", tripMetaKey)
	if err != nil {
		return nil, err
	}
	pairs, err := hashPairs(raw)
	if err != nil {
		return nil, fmt.Errorf("upstash: HGETALL: %w", err)
	}
	records := make([]TripRecord, 0, len(pairs))
	for _, payload := range pairs {
		var rec TripRecord
		if err := json.Unmarshal([]byte(payload), &rec); err != nil {
			continue // одна битая запись не должна ломать расписание
		}
		if rec.ID == "" {
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ID < records[j].ID })
	return records, nil
}

// SaveTripRecord сохраняет запись выхода: перенос, отмену или новый выход.
func (u *Upstash) SaveTripRecord(ctx context.Context, rec TripRecord) error {
	payload, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = u.do(ctx, "HSET", tripMetaKey, rec.ID, string(payload))
	return err
}

// hashPairs разбирает HGETALL: Upstash REST отдаёт либо плоский массив
// [поле, значение, ...], либо объект — поддерживаем оба вида.
func hashPairs(raw json.RawMessage) (map[string]string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return map[string]string{}, nil
	}
	var asMap map[string]string
	if err := json.Unmarshal(raw, &asMap); err == nil {
		return asMap, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, err
	}
	if len(arr)%2 != 0 {
		return nil, errors.New("нечётное число элементов")
	}
	out := make(map[string]string, len(arr)/2)
	for i := 0; i < len(arr); i += 2 {
		var field, value string
		if err := json.Unmarshal(arr[i], &field); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(arr[i+1], &value); err != nil {
			return nil, err
		}
		out[field] = value
	}
	return out, nil
}

// parseRedisInt разбирает число из ответа Upstash: REST может вернуть
// как JSON-число, так и строку ("2"), в зависимости от команды.
func parseRedisInt(raw json.RawMessage) (int, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.Atoi(s); err == nil {
			return v, nil
		}
	}
	return 0, fmt.Errorf("ожидали число, получили %s", raw)
}

// do отправляет команду Redis через REST: тело — JSON-массив аргументов.
func (u *Upstash) do(ctx context.Context, args ...any) (json.RawMessage, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstash: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("upstash: чтение ответа: %w", err)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("upstash: HTTP %d, ответ не JSON", resp.StatusCode)
	}
	if out.Error != "" {
		return nil, errors.New("upstash: " + out.Error)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstash: HTTP %d", resp.StatusCode)
	}
	return out.Result, nil
}

// tripMetaKey — хеш записей: поле — ID выхода, значение — JSON TripRecord.
const tripMetaKey = "regatta:trips:meta"

func bookedKey(tripID string) string { return "regatta:trip:" + tripID + ":booked" }
func crewKey(tripID string) string   { return "regatta:trip:" + tripID + ":crew" }

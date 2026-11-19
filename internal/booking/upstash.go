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

// UpstashFromEnv читает UPSTASH_REDIS_REST_URL и UPSTASH_REDIS_REST_TOKEN.
// ok == false, если хотя бы одна переменная пуста: сервис стартует без Redis.
func UpstashFromEnv() (u *Upstash, ok bool) {
	url := strings.TrimSpace(os.Getenv("UPSTASH_REDIS_REST_URL"))
	token := strings.TrimSpace(os.Getenv("UPSTASH_REDIS_REST_TOKEN"))
	if url == "" || token == "" {
		return nil, false
	}
	return NewUpstash(url, token), true
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
	var booked int
	if err := json.Unmarshal(raw, &booked); err != nil {
		return 0, fmt.Errorf("upstash: неожиданный ответ EVAL: %w", err)
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
	var values []*int
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("upstash: неожиданный ответ MGET: %w", err)
	}
	if len(values) != len(tripIDs) {
		return nil, errors.New("upstash: MGET вернул неверное число значений")
	}
	out := make([]int, len(values))
	for i, v := range values {
		if v != nil {
			out[i] = *v
		}
	}
	return out, nil
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

func bookedKey(tripID string) string { return "regatta:trip:" + tripID + ":booked" }
func crewKey(tripID string) string   { return "regatta:trip:" + tripID + ":crew" }

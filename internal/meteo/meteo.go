// Package meteo — тонкий клиент бесплатного Open-Meteo.
//
// Ключ API не нужен. Прогноз ветра и прогноз волн запрашиваются
// параллельно в двух горутинах и склеиваются в один результат.
package meteo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	weatherEndpoint = "https://api.open-meteo.com/v1/forecast"
	marineEndpoint  = "https://marine-api.open-meteo.com/v1/marine"
	userAgent       = "marina-wind/1.0 (+https://github.com/lerx11/marina-wind)"

	// hoursAhead — сколько часов прогноза отдаём наружу.
	hoursAhead = 24

	timeLayout = "2006-01-02T15:04"
)

// Client — потокобезопасный клиент Open-Meteo.
type Client struct {
	http *http.Client
}

// New создаёт клиент с общим таймаутом на запрос.
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	return &Client{http: &http.Client{Timeout: timeout}}
}

// Current — ветер «сейчас».
type Current struct {
	Time      time.Time
	WindKnots float64
	GustKnots float64
	Direction float64
}

// Hour — одна точка почасового прогноза.
type Hour struct {
	Time      time.Time
	WindKnots float64
	GustKnots float64
	Direction float64
	WaveM     *float64
}

// Wave — параметры волнения (только для морских акваторий).
type Wave struct {
	HeightM   float64
	Direction float64
	PeriodS   float64
}

// Result — собранный прогноз по одной точке.
type Result struct {
	Timezone string
	Current  Current
	Hours    []Hour
	WaveNow  *Wave
}

// Fetch параллельно забирает прогноз ветра и волн.
// Если волновой API недоступен (внутренние водоёмы), это не считается ошибкой:
// результат просто вернётся без волновых полей.
func (c *Client) Fetch(ctx context.Context, lat, lon float64, coastal bool) (Result, error) {
	var (
		wg      sync.WaitGroup
		weather *weatherResponse
		marine  *marineResponse

		weatherErr error
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		weather, weatherErr = c.fetchWeather(ctx, lat, lon)
	}()

	if coastal {
		wg.Add(1)
		go func() {
			defer wg.Done()
			marine, _ = c.fetchMarine(ctx, lat, lon)
		}()
	}
	wg.Wait()

	if weatherErr != nil {
		return Result{}, weatherErr
	}
	if weather == nil {
		return Result{}, fmt.Errorf("open-meteo: пустой ответ")
	}

	return assemble(weather, marine), nil
}

func assemble(w *weatherResponse, m *marineResponse) Result {
	loc := time.UTC
	if l, err := time.LoadLocation(w.Timezone); err == nil {
		loc = l
	}
	now := time.Now().In(loc)

	waves := map[string]*Wave{}
	if m != nil {
		for i, ts := range m.Hourly.Time {
			height, ok := pick(m.Hourly.WaveHeight, i)
			if !ok {
				continue
			}
			dir, _ := pick(m.Hourly.WaveDirection, i)
			period, _ := pick(m.Hourly.WavePeriod, i)
			waves[ts] = &Wave{HeightM: height, Direction: dir, PeriodS: period}
		}
	}

	res := Result{Timezone: w.Timezone}

	var firstWave *Wave
	floor := now.Truncate(time.Hour)
	for i, ts := range w.Hourly.Time {
		t, err := time.ParseInLocation(timeLayout, ts, loc)
		if err != nil || t.Before(floor) {
			continue
		}
		speed, ok := value(w.Hourly.WindSpeed, i)
		if !ok {
			continue
		}
		gust, _ := value(w.Hourly.WindGusts, i)
		dir, _ := value(w.Hourly.WindDirection, i)

		h := Hour{Time: t, WindKnots: speed, GustKnots: gust, Direction: dir}
		if wave, ok := waves[ts]; ok {
			height := wave.HeightM
			h.WaveM = &height
			if firstWave == nil {
				firstWave = wave
			}
		}
		res.Hours = append(res.Hours, h)
		if len(res.Hours) >= hoursAhead {
			break
		}
	}

	res.Current = Current{
		Time:      now,
		WindKnots: w.Current.WindSpeed,
		GustKnots: w.Current.WindGusts,
		Direction: w.Current.WindDirection,
	}
	if t, err := time.ParseInLocation(timeLayout, w.Current.Time, loc); err == nil {
		res.Current.Time = t
	}
	// Если текущий срез пуст (бывает на границе часа) — берём первый час прогноза.
	if len(res.Hours) > 0 && w.Current.Time == "" {
		first := res.Hours[0]
		res.Current = Current{Time: first.Time, WindKnots: first.WindKnots, GustKnots: first.GustKnots, Direction: first.Direction}
	}

	if wave, ok := waves[w.Current.Time]; ok {
		res.WaveNow = wave
	} else {
		res.WaveNow = firstWave
	}

	return res
}

// --- ответы Open-Meteo ---

type weatherResponse struct {
	Timezone string `json:"timezone"`
	Current  struct {
		Time          string  `json:"time"`
		WindSpeed     float64 `json:"wind_speed_10m"`
		WindGusts     float64 `json:"wind_gusts_10m"`
		WindDirection float64 `json:"wind_direction_10m"`
	} `json:"current"`
	Hourly struct {
		Time          []string  `json:"time"`
		WindSpeed     []float64 `json:"wind_speed_10m"`
		WindGusts     []float64 `json:"wind_gusts_10m"`
		WindDirection []float64 `json:"wind_direction_10m"`
	} `json:"hourly"`
}

type marineResponse struct {
	Hourly struct {
		Time          []string   `json:"time"`
		WaveHeight    []*float64 `json:"wave_height"`
		WaveDirection []*float64 `json:"wave_direction"`
		WavePeriod    []*float64 `json:"wave_period"`
	} `json:"hourly"`
}

// --- запросы ---

func (c *Client) fetchWeather(ctx context.Context, lat, lon float64) (*weatherResponse, error) {
	q := url.Values{}
	q.Set("latitude", coord(lat))
	q.Set("longitude", coord(lon))
	q.Set("current", "wind_speed_10m,wind_gusts_10m,wind_direction_10m")
	q.Set("hourly", "wind_speed_10m,wind_gusts_10m,wind_direction_10m")
	q.Set("wind_speed_unit", "kn") // сразу узлы — то, в чём ходят яхтсмены
	q.Set("timezone", "auto")
	q.Set("forecast_days", "2")

	var out weatherResponse
	if err := c.get(ctx, weatherEndpoint+"?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) fetchMarine(ctx context.Context, lat, lon float64) (*marineResponse, error) {
	q := url.Values{}
	q.Set("latitude", coord(lat))
	q.Set("longitude", coord(lon))
	q.Set("hourly", "wave_height,wave_direction,wave_period")
	q.Set("timezone", "auto")
	q.Set("forecast_days", "2")

	var out marineResponse
	if err := c.get(ctx, marineEndpoint+"?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) get(ctx context.Context, endpoint string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("open-meteo: создать запрос: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("open-meteo: запрос: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("open-meteo: статус %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("open-meteo: разбор ответа: %w", err)
	}
	return nil
}

// --- мелочи ---

func coord(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

// value безопасно берёт элемент числового среза.
func value(s []float64, i int) (float64, bool) {
	if i < 0 || i >= len(s) {
		return 0, false
	}
	return s[i], true
}

// pick безопасно берёт элемент среза указателей, отбрасывая nil.
func pick(s []*float64, i int) (float64, bool) {
	if i < 0 || i >= len(s) || s[i] == nil {
		return 0, false
	}
	return *s[i], true
}

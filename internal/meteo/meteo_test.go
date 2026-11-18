package meteo

import (
	"testing"
	"time"
)

func ptr(v float64) *float64 { return &v }

func TestAssembleSkipsPastHoursAndMapsWaves(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	stamp := func(offset time.Duration) string {
		return now.Add(offset).Format(timeLayout)
	}

	w := &weatherResponse{Timezone: "UTC"}
	w.Current.Time = stamp(0)
	w.Current.WindSpeed = 12.4
	w.Current.WindGusts = 18.1
	w.Current.WindDirection = 225

	w.Hourly.Time = []string{stamp(-2 * time.Hour), stamp(0), stamp(time.Hour), stamp(2 * time.Hour)}
	w.Hourly.WindSpeed = []float64{5, 12.4, 13, 14}
	w.Hourly.WindGusts = []float64{7, 18.1, 19, 20}
	w.Hourly.WindDirection = []float64{200, 225, 230, 240}

	m := &marineResponse{}
	m.Hourly.Time = w.Hourly.Time
	m.Hourly.WaveHeight = []*float64{nil, ptr(0.7), nil, nil}
	m.Hourly.WaveDirection = []*float64{nil, ptr(240), nil, nil}
	m.Hourly.WavePeriod = []*float64{nil, ptr(3.5), nil, nil}

	got := assemble(w, m)

	if len(got.Hours) != 3 {
		t.Fatalf("прошедшие часы должны отбрасываться: ожидали 3 точки, получили %d", len(got.Hours))
	}
	if !got.Hours[0].Time.Equal(now) {
		t.Errorf("первая точка = %v, ожидали %v", got.Hours[0].Time, now)
	}
	if got.Current.WindKnots != 12.4 || got.Current.Direction != 225 {
		t.Errorf("текущий ветер собран неверно: %+v", got.Current)
	}
	if got.WaveNow == nil || got.WaveNow.HeightM != 0.7 {
		t.Errorf("волна для текущего часа не найдена: %+v", got.WaveNow)
	}
	if got.Hours[0].WaveM == nil || *got.Hours[0].WaveM != 0.7 {
		t.Errorf("волна не привязалась к первому часу: %+v", got.Hours[0])
	}
	if got.Hours[2].WaveM != nil {
		t.Errorf("для часа без волновых данных ожидали nil, получили %v", *got.Hours[2].WaveM)
	}
}

func TestAssembleWithoutMarineData(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	w := &weatherResponse{Timezone: "UTC"}
	w.Current.Time = now.Format(timeLayout)
	w.Current.WindSpeed = 3
	w.Hourly.Time = []string{now.Format(timeLayout)}
	w.Hourly.WindSpeed = []float64{3}
	w.Hourly.WindGusts = []float64{4}
	w.Hourly.WindDirection = []float64{10}

	got := assemble(w, nil)

	if got.WaveNow != nil {
		t.Errorf("без волнового API волна должна быть nil, получили %+v", got.WaveNow)
	}
	if len(got.Hours) != 1 || got.Hours[0].WaveM != nil {
		t.Errorf("ожидали одну точку без волны, получили %+v", got.Hours)
	}
}

func TestAssembleIgnoresShortHourlySlices(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	w := &weatherResponse{Timezone: "UTC"}
	w.Current.Time = now.Format(timeLayout)
	// Данных меньше, чем временных меток: лишние часы должны молча отбрасываться.
	w.Hourly.Time = []string{now.Format(timeLayout), now.Add(time.Hour).Format(timeLayout)}
	w.Hourly.WindSpeed = []float64{9}

	got := assemble(w, nil)

	if len(got.Hours) != 1 {
		t.Fatalf("ожидали ровно одну валидную точку, получили %d", len(got.Hours))
	}
	if got.Hours[0].WindKnots != 9 {
		t.Errorf("скорость = %v, ожидали 9", got.Hours[0].WindKnots)
	}
}

package wind

import "testing"

func TestFromKnotsBoundaries(t *testing.T) {
	cases := []struct {
		knots float64
		want  int
	}{
		{-5, 0}, {0, 0}, {0.9, 0},
		{1, 1}, {3.9, 1},
		{4, 2}, {6.9, 2},
		{7, 3}, {10.9, 3},
		{11, 4}, {16.9, 4},
		{17, 5}, {21.9, 5},
		{22, 6}, {27.9, 6},
		{28, 7}, {33.9, 7},
		{34, 8}, {40.9, 8},
		{41, 9}, {47.9, 9},
		{48, 10}, {55.9, 10},
		{56, 11}, {63.9, 11},
		{64, 12}, {120, 12},
	}

	for _, c := range cases {
		if got := FromKnots(c.knots).Scale; got != c.want {
			t.Errorf("FromKnots(%v).Scale = %d, ожидали %d", c.knots, got, c.want)
		}
	}
}

func TestScaleCoversEveryBeaufortPoint(t *testing.T) {
	if len(Scale) != 13 {
		t.Fatalf("шкала должна содержать 13 ступеней (0..12), а содержит %d", len(Scale))
	}
	for i, step := range Scale {
		if step.Scale != i {
			t.Errorf("на позиции %d лежит ступень %d", i, step.Scale)
		}
		if step.Name == "" || step.Range == "" || step.SailAdvice == "" {
			t.Errorf("ступень %d заполнена не полностью: %+v", i, step)
		}
	}
}

func TestUpperBoundsAreStrictlyIncreasing(t *testing.T) {
	for i := 1; i < len(Scale); i++ {
		if Scale[i].UpperKnots <= Scale[i-1].UpperKnots {
			t.Fatalf("границы должны возрастать: ступень %d = %v, ступень %d = %v",
				i-1, Scale[i-1].UpperKnots, i, Scale[i].UpperKnots)
		}
	}
}

func TestRumb(t *testing.T) {
	cases := []struct {
		deg  float64
		want string
	}{
		{0, "С"}, {22.5, "ССВ"}, {45, "СВ"}, {90, "В"},
		{135, "ЮВ"}, {180, "Ю"}, {270, "З"}, {315, "СЗ"},
		{359, "С"}, {350, "С"}, {-90, "З"}, {405, "СВ"},
	}

	for _, c := range cases {
		if got := Rumb(c.deg); got != c.want {
			t.Errorf("Rumb(%v) = %q, ожидали %q", c.deg, got, c.want)
		}
	}
}

func TestRumbFullDiffersFromShort(t *testing.T) {
	if RumbFull(45) != "северо-восток" {
		t.Errorf("RumbFull(45) = %q", RumbFull(45))
	}
}

func TestChecklistGrowsWithWind(t *testing.T) {
	calm := Checklist(0)
	if len(calm) != 3 {
		t.Errorf("при штиле ожидали 3 базовых пункта, получили %d", len(calm))
	}

	fresh := Checklist(6)
	if !contains(fresh, "VHF-рация и свисток для сигналов") {
		t.Errorf("при 6 баллах в чек-листе должен быть пункт про рацию: %v", fresh)
	}
	if len(fresh) <= len(calm) {
		t.Errorf("чек-лист должен расти вместе с ветром")
	}

	storm := Checklist(11)
	if !contains(storm, "Выход на воду закрыт: остаёмся на берегу") {
		t.Errorf("при 11 баллах выход должен быть закрыт: %v", storm)
	}
	if len(storm) != 3 {
		t.Errorf("штормовой чек-лист заменяет базовый, ожидали 3 пункта, получили %d", len(storm))
	}
}

func TestKnotsConversions(t *testing.T) {
	if got := KnotsFromMS(1); got < 1.94 || got > 1.95 {
		t.Errorf("1 м/с ≈ 1.944 узла, получили %v", got)
	}
	if got := KnotsFromKmh(1.852); got < 0.99 || got > 1.01 {
		t.Errorf("1.852 км/ч ≈ 1 узел, получили %v", got)
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

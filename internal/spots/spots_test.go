package spots

import "testing"

func TestFind(t *testing.T) {
	cases := []struct {
		query string
		want  string
		ok    bool
	}{
		{"strogino", "Строгино", true},
		{"  MINSK-SEA ", "Минское море", true},
		{"gelendzhik", "Геленджик", true},
		{"atlantic", "", false},
		{"", "", false},
	}

	for _, c := range cases {
		got, ok := Find(c.query)
		if ok != c.ok {
			t.Errorf("Find(%q) ok = %v, ожидали %v", c.query, ok, c.ok)
			continue
		}
		if ok && got.Name != c.want {
			t.Errorf("Find(%q).Name = %q, ожидали %q", c.query, got.Name, c.want)
		}
	}
}

func TestCatalogIsValid(t *testing.T) {
	seen := map[string]bool{}
	for _, sp := range Catalog {
		if sp.Slug == "" || sp.Name == "" {
			t.Errorf("акватория без slug/name: %+v", sp)
		}
		if seen[sp.Slug] {
			t.Errorf("дубликат slug: %s", sp.Slug)
		}
		seen[sp.Slug] = true

		if sp.Lat < -90 || sp.Lat > 90 || sp.Lon < -180 || sp.Lon > 180 {
			t.Errorf("координаты вне диапазона у %s: %v, %v", sp.Slug, sp.Lat, sp.Lon)
		}
	}
	if len(Catalog) == 0 {
		t.Fatal("каталог акваторий пуст")
	}
	if _, ok := Find(DefaultSlug); !ok {
		t.Errorf("акватория по умолчанию %q отсутствует в каталоге", DefaultSlug)
	}
}

func TestAllReturnsCopy(t *testing.T) {
	got := All()
	got[0].Name = "изменено"
	if Catalog[0].Name == "изменено" {
		t.Error("All() должен возвращать копию, а не ссылку на исходный срез")
	}
}

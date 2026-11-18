// Package spots — справочник баз и акваторий, по которым строится прогноз.
package spots

import "strings"

// Spot — точка выхода на воду.
type Spot struct {
	Slug    string  `json:"slug"`    // идентификатор для API (?spot=strogino)
	Name    string  `json:"name"`    // человекочитаемое название
	Region  string  `json:"region"`  // регион / страна
	Lat     float64 `json:"lat"`     // широта
	Lon     float64 `json:"lon"`     // долгота
	Coastal bool    `json:"coastal"` // море/океан — доступен волновой прогноз
}

// Catalog — известные акватории. Координаты приблизительные и легко правятся.
var Catalog = []Spot{
	{Slug: "strogino", Name: "Строгино", Region: "Москва, Россия", Lat: 55.8033, Lon: 37.4025},
	{Slug: "pirogovo", Name: "Пирогово", Region: "Московская обл., Россия", Lat: 55.9658, Lon: 37.5151},
	{Slug: "minsk-sea", Name: "Минское море", Region: "Заславль, Беларусь", Lat: 54.0282, Lon: 27.2925},
	{Slug: "gelendzhik", Name: "Геленджик", Region: "Чёрное море, Россия", Lat: 44.5569, Lon: 38.0775, Coastal: true},
	{Slug: "bodrum", Name: "Бодрум", Region: "Эгейское море, Турция", Lat: 37.0330, Lon: 27.4290, Coastal: true},
}

// All возвращает копию каталога, безопасную для сериализации в JSON.
func All() []Spot {
	out := make([]Spot, len(Catalog))
	copy(out, Catalog)
	return out
}

// Find ищет акваторию по slug без учёта регистра.
func Find(slug string) (Spot, bool) {
	s := strings.ToLower(strings.TrimSpace(slug))
	for _, sp := range Catalog {
		if sp.Slug == s {
			return sp, true
		}
	}
	return Spot{}, false
}

// DefaultSlug — акватория по умолчанию, если ?spot= не передан.
const DefaultSlug = "strogino"

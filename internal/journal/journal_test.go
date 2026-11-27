package journal

import (
	"strings"
	"testing"
)

func TestArticles(t *testing.T) {
	all := All()
	if len(all) < 3 {
		t.Fatalf("статей %d, ожидали минимум 3", len(all))
	}

	seen := map[string]bool{}
	for _, a := range all {
		if a.Slug == "" || a.Title == "" || a.Description == "" || a.H1 == "" || a.Question == "" {
			t.Errorf("неполные метаданные: %+v", a)
		}
		if seen[a.Slug] {
			t.Errorf("повторяющийся slug: %s", a.Slug)
		}
		seen[a.Slug] = true
		if a.Path != "/journal/"+a.Slug || a.URL != SiteURL+a.Path {
			t.Errorf("неверные адреса: path=%s url=%s", a.Path, a.URL)
		}
		if a.Reading < 3 {
			t.Errorf("%s: время чтения %d", a.Slug, a.Reading)
		}
		if !strings.Contains(a.BodyHTML, "<h2>") {
			t.Errorf("%s: в статье нет подзаголовков", a.Slug)
		}
		if strings.Contains(a.BodyHTML, "**") || strings.Contains(a.BodyHTML, "](#") {
			t.Errorf("%s: осталась необработанная разметка", a.Slug)
		}
		if !strings.Contains(a.BodyHTML, "href=") {
			t.Errorf("%s: нет ни одной ссылки", a.Slug)
		}
	}

	if all[0].Order != 1 {
		t.Errorf("первой должна идти статья с order: 1, получили %d", all[0].Order)
	}
}

func TestFind(t *testing.T) {
	a, ok := Find("kak-chitat-prognoz-vetra")
	if !ok || a.Slug != "kak-chitat-prognoz-vetra" {
		t.Fatalf("статья не найдена: %+v", a)
	}
	if _, ok := Find("net-takoy"); ok {
		t.Error("несуществующая статья нашлась")
	}
}

func TestRenderMarkdown(t *testing.T) {
	got := renderMarkdown("## Заголовок\n\nТекст **жирный** и [сайт](https://example.com).\n\n- один\n- два\n\n<script>alert(1)</script>\n")

	if !strings.Contains(got, "<h2>Заголовок</h2>") {
		t.Errorf("нет заголовка: %s", got)
	}
	if !strings.Contains(got, "<strong>жирный</strong>") {
		t.Errorf("нет жирного текста: %s", got)
	}
	if !strings.Contains(got, `<a href="https://example.com" target="_blank" rel="noopener noreferrer">сайт</a>`) {
		t.Errorf("нет внешней ссылки: %s", got)
	}
	if !strings.Contains(got, "<ul>") || strings.Count(got, "<li>") != 2 {
		t.Errorf("нет списка: %s", got)
	}
	if strings.Contains(got, "<script>") {
		t.Errorf("HTML не экранирован: %s", got)
	}
}

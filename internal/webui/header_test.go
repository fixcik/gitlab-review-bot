package webui

import (
	"context"
	"strings"
	"testing"
)

func TestHeader(t *testing.T) {
	var n Nav
	page := n.Header(context.Background(), "/map")
	if p := HeaderProblems(page, "/map"); len(p) > 0 {
		t.Fatalf("header problems %v:\n%s", p, page)
	}
	for _, s := range []string{`<a class="brandlink" href="/"`, ">Ревью-бот<", `href="/usage"`, `href="/runs"`, `href="/problems"`, `href="/knowledge"`,
		`<a href="/map" aria-current="page">Карта системы</a>`, `<a href="/setup">Настройка</a>`, `id="theme"`, `class="progress"`} {
		if !strings.Contains(page, s) {
			t.Errorf("header lacks %q:\n%s", s, page)
		}
	}
	// Switched-off pages are left out; the problems badge comes from Badge.
	n = Nav{UsageOff: true, DiagOff: true, MapOff: true, Badge: func(context.Context) Badge { return Badge{Count: "12<", Bad: true} }}
	page = n.Header(context.Background(), "/problems")
	if strings.Contains(page, `href="/usage"`) || strings.Contains(page, `href="/runs"`) || strings.Contains(page, `href="/map"`) ||
		!strings.Contains(page, `<span class="cnt bad" title="активных проблем">12&lt;</span>`) || len(HeaderProblems(page, "/problems")) > 0 {
		t.Errorf("header:\n%s", page)
	}
	// A run page belongs to /runs; the badge is not asked when /problems is off.
	n = Nav{ProblemsOff: true, Badge: func(context.Context) Badge { t.Error("badge asked"); return Badge{} }}
	if page = n.Header(context.Background(), "/runs"); strings.Contains(page, `href="/problems"`) || len(HeaderProblems(page, "/runs")) > 0 {
		t.Errorf("header:\n%s", page)
	}
	if page = (Nav{}).HeaderWith("/", Badge{Count: "3"}); !strings.Contains(page, `<span class="cnt" title="активных проблем">3</span>`) {
		t.Errorf("badge:\n%s", page)
	}
	// The checker catches the old markup.
	old := `<header class="topbar"><nav class="crumbs"><a href="/usage" aria-current="page">x</a></nav></header>`
	if p := HeaderProblems(old, "/usage"); len(p) < 2 {
		t.Errorf("checker: %v", p)
	}
}

package webui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestAssets(t *testing.T) {
	css, js := Assets()
	// The filter bar's CSS is part of the shared CSS, after the tokens it uses.
	if !strings.Contains(css, "--accent-soft:") || !strings.Contains(css, ".rf-field") || strings.Index(css, ".rf-field") < strings.Index(css, "--accent-soft:") {
		t.Fatal("css: tokens or filter bar missing")
	}
	if strings.Contains(js, "{{") || !strings.Contains(js, "var D =") || !strings.Contains(js, "filters: filters") {
		t.Fatal("js: placeholder left or D.filters missing")
	}
	// No second combobox system and nothing from the design mockups.
	for _, s := range []string{css, js} {
		for _, bad := range []string{"TomSelect", "tomselect", ".ts-", "designs/", "rf--compact", "rf--bar"} {
			if strings.Contains(s, bad) {
				t.Errorf("assets contain %q", bad)
			}
		}
	}
	// Every kind has its label in the JS literal.
	for _, k := range core.AllJobKinds() {
		if !strings.Contains(js, `"`+string(k)+`":"`+KindLabel(k)+`"`) {
			t.Errorf("kind %s missing in KIND", k)
		}
	}
	if KindLabel(core.JobKind("no-such-kind")) != "no-such-kind" {
		t.Error("unknown kind must keep its raw value")
	}
	// Data is never parsed as HTML; no style attributes from JS.
	if m := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*"(style|on)`).FindString(js); m != "" {
		t.Errorf("common.js uses %s", m)
	}
}

func TestInlineViolations(t *testing.T) {
	page := `<html><head><style nonce="x">a[style] { color: red } .x { onclick: 1 }</style>
<script nonce="x">var onThemeChange = rebuild; el.setAttribute("x", 'style="y"');</script></head>
<body><div class="a" data-onx="1">ok</div><p data-style="1">ok</p>
<span style="color:red">bad</span><button type="button" onclick="x()">bad</button>
<a
  href="/" ONMOUSEOVER = "x">bad</a></body></html>`
	got := InlineViolations(page)
	if len(got) != 3 || !strings.HasPrefix(got[0], "<span") || !strings.HasPrefix(got[1], "<button") || !strings.HasPrefix(got[2], "<a") {
		t.Fatalf("violations: %q", got)
	}
	if v := InlineViolations(`<div class="rf" role="search"><input data-onchange="1"></div>`); len(v) != 0 {
		t.Fatalf("false positives: %q", v)
	}
}

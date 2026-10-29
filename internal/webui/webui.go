// Package webui holds the shared look of the bot's web pages (/, /usage,
// /runs, /problems, /knowledge, /map): the CSS (Pajamas tokens, layout,
// tables, the filter bar) and the JS helpers (theme, DOM, formatting,
// ECharts theming, D.filters). Pages inline both under their CSP nonce;
// the libraries (ECharts, the filter bar's filters.js) are served as
// versioned files by internal/webassets.
//
// header.go is the shared top bar of every page (Nav.Header).
//
// It is a leaf package (only core), so every page package can use it,
// including usageweb, which the other page packages import.
package webui

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

//go:embed common.css
var commonCSS string

//go:embed filters.css
var filtersCSS string

//go:embed common.js
var commonJS string

// kindLabelsJS is a JS object literal kind -> label, generated from
// core.AllJobKinds() (common.js: D.kindLabel).
var kindLabelsJS = buildKindLabels()

// KindLabel is the display name of a job kind (core.KindInfo.Label; its raw
// value when unknown).
func KindLabel(k core.JobKind) string {
	if info, ok := k.Info(); ok && info.Label != "" {
		return info.Label
	}
	return string(k)
}

func buildKindLabels() string {
	labels := map[string]string{}
	for _, k := range core.AllJobKinds() {
		labels[string(k)] = KindLabel(k)
	}
	// json.Marshal escapes <, > and &, so the literal is safe inside <script>.
	b, err := json.Marshal(labels)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// Assets returns the shared CSS (common.css + filters.css) and JS
// (common.js) that every page inlines into its <style nonce> / <script nonce>.
func Assets() (css, js string) {
	return commonCSS + "\n" + filtersCSS, strings.ReplaceAll(commonJS, "{{KIND_LABELS}}", kindLabelsJS)
}

var (
	scriptOrStyleRe = regexp.MustCompile(`(?is)(<(script|style)\b[^>]*>).*?</(script|style)>`)
	tagRe           = regexp.MustCompile(`(?s)<[a-zA-Z][^>]*>`)
	inlineAttrRe    = regexp.MustCompile(`(?i)\s(style|on[a-z]+)\s*=`)
)

// InlineViolations lists the tags of a rendered page that the pages' CSP
// blocks: a style="" attribute or an inline on*= handler. Script and style
// bodies are skipped (their code is not markup). Used by the page tests.
func InlineViolations(page string) []string {
	page = scriptOrStyleRe.ReplaceAllString(page, "$1")
	var bad []string
	for _, tag := range tagRe.FindAllString(page, -1) {
		if inlineAttrRe.MatchString(tag) {
			bad = append(bad, tag)
		}
	}
	return bad
}

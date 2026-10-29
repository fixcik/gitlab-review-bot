package webui

import (
	"context"
	"html"
	"regexp"
	"strings"
)

// The shared top bar of every page (/, /usage, /runs, /problems,
// /knowledge, /map, /setup): the logo and "Ревью-бот" lead to /, the sections as
// pills (the current one with aria-current="page"), the active problems
// badge on «Проблемы», the theme switch and the busy bar. Pages put the
// result of Nav.Header where their header was; a page's own breadcrumb
// (a run of /runs) stays inside the page.

// Nav is what the header shows: switched-off pages are left out (the zero
// value shows every section), Badge counts active problems (nil = none).
type Nav struct {
	UsageOff, DiagOff, ProblemsOff, MapOff bool
	// Badge returns the active problems badge; an empty Count shows none.
	// It must not fail the page: on an error it returns Badge{}.
	Badge func(ctx context.Context) Badge
}

// Badge is the active problems counter of «Проблемы» ("12", "1000+");
// Bad: some problem breaks reviews (red).
type Badge struct {
	Count string
	Bad   bool
}

// Section paths.
const (
	SectionHome      = "/"
	SectionUsage     = "/usage"
	SectionRuns      = "/runs"
	SectionProblems  = "/problems"
	SectionKnowledge = "/knowledge"
	SectionMap       = "/map"
	SectionSetup     = "/setup"
)

// Header renders the top bar with current highlighted, asking Badge for
// the problems counter.
func (n Nav) Header(ctx context.Context, current string) string {
	var b Badge
	if n.Badge != nil && !n.ProblemsOff {
		b = n.Badge(ctx)
	}
	return n.HeaderWith(current, b)
}

// HeaderWith renders the top bar with a known problems badge (the main page
// counts the problems anyway).
func (n Nav) HeaderWith(current string, b Badge) string {
	items := []struct {
		href, label string
		off         bool
	}{
		{SectionHome, "Главная", false}, {SectionUsage, "Расход токенов", n.UsageOff}, {SectionRuns, "Диагностика", n.DiagOff},
		{SectionProblems, "Проблемы", n.ProblemsOff}, {SectionKnowledge, "Знания", false}, {SectionMap, "Карта системы", n.MapOff},
		{SectionSetup, "Настройка", false},
	}
	var s strings.Builder
	s.WriteString(headerStart)
	for _, it := range items {
		if it.off && it.href != current {
			continue
		}
		s.WriteString(`<a href="` + it.href + `"`)
		if it.href == current {
			s.WriteString(` aria-current="page"`)
		}
		s.WriteString(">" + it.label)
		if it.href == SectionProblems && b.Count != "" {
			cls := "cnt"
			if b.Bad {
				cls += " bad"
			}
			s.WriteString(` <span class="` + cls + `" title="активных проблем">` + html.EscapeString(b.Count) + `</span>`)
		}
		s.WriteString("</a>\n      ")
	}
	s.WriteString(headerEnd)
	return s.String()
}

const headerStart = `<header class="topbar" id="topbar">
  <div class="topbar-in">
    <a class="brandlink" href="/" aria-label="Ревью-бот: главная">
      <span class="mark" aria-hidden="true"><svg width="14" height="14" viewBox="0 0 16 16" fill="currentColor"><path d="M8 1.5 14 5v6l-6 3.5L2 11V5l6-3.5Zm0 2.3L4 6.1v3.8l4 2.3 4-2.3V6.1L8 3.8Z"/></svg></span>
      <span class="brand">Ревью-бот</span>
    </a>
    <nav class="pills" aria-label="Разделы">
      `

const headerEnd = `</nav>
    <span class="spacer"></span>
    <div class="seg icons" id="theme" role="group" aria-label="Тема оформления">
      <button type="button" data-t="light" aria-label="Светлая тема" title="Светлая">
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" aria-hidden="true"><circle cx="8" cy="8" r="3"/><path d="M8 1.5v1.5M8 13v1.5M1.5 8H3M13 8h1.5M3.4 3.4l1.1 1.1M11.5 11.5l1.1 1.1M3.4 12.6l1.1-1.1M11.5 4.5l1.1-1.1"/></svg>
      </button>
      <button type="button" data-t="dark" aria-label="Тёмная тема" title="Тёмная">
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" aria-hidden="true"><path d="M13.5 9.6A5.5 5.5 0 0 1 6.4 2.5a5.5 5.5 0 1 0 7.1 7.1Z"/></svg>
      </button>
      <button type="button" data-t="system" aria-label="Как в системе" title="Как в системе">
        <svg width="16" height="16" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round" aria-hidden="true"><rect x="1.75" y="2.75" width="12.5" height="8.5" rx="1.5"/><path d="M5.5 14h5M8 11.25V14" stroke-linecap="round"/></svg>
      </button>
    </div>
  </div>
  <div class="progress" aria-hidden="true"></div>
</header>`

var (
	headerRe  = regexp.MustCompile(`(?s)<header class="topbar"[^>]*>.*?</header>`)
	currentRe = regexp.MustCompile(`<a href="([^"]*)" aria-current="page">`)
)

// HeaderProblems checks a rendered page for the shared header with current
// as the only highlighted section, «Главная» present and no old
// breadcrumb or link row (used by the page tests).
func HeaderProblems(page, current string) []string {
	var out []string
	hdr := headerRe.FindString(page)
	switch {
	case hdr == "":
		out = append(out, "no shared header")
	case strings.Count(page, `<header class="topbar"`) != 1:
		out = append(out, "more than one header")
	}
	if !strings.Contains(hdr, `<nav class="pills"`) || !strings.Contains(hdr, `class="brandlink" href="/"`) {
		out = append(out, "not the shared header")
	}
	if !strings.Contains(hdr, `>Главная`) {
		out = append(out, "no «Главная»")
	}
	if m := currentRe.FindAllStringSubmatch(hdr, -1); len(m) != 1 || m[0][1] != current {
		out = append(out, "aria-current is not exactly "+current)
	}
	if strings.Contains(page, `class="crumbs"`) || strings.Contains(page, `class="navs"`) {
		out = append(out, "old breadcrumb or link row")
	}
	return out
}

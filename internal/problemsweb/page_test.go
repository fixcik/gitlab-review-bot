package problemsweb

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

var t0 = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func rec(kind problems.Kind, project, subject string, count int64, ago time.Duration) problems.Record {
	return problems.Record{Fingerprint: problems.Fingerprint(kind, project, subject), Kind: kind, Source: problems.SourceBot,
		ProjectPath: project, Subject: subject, Count: count, FirstSeen: t0.Add(-ago - time.Hour), LastSeen: t0.Add(-ago)}
}

// impactHandler: one problem of every impact, a hidden one and a fixed one.
func impactHandler(t *testing.T, rows ...problems.Record) (http.Handler, *fakeStore) {
	t.Helper()
	if rows == nil {
		hidden := rec(problems.ReactionsFailed, "g/web", "403 на award emoji", 1, 3*time.Hour)
		hidden.AckedAt = t0.Add(-time.Hour)
		fixed := rec(problems.RepoConfigInvalid, "g/old", ".reviewbot.yaml", 2, 4*time.Hour)
		fixed.ResolvedAt = t0.Add(-time.Hour)
		rows = []problems.Record{
			rec(problems.LabelsFailed, "g/api", "метки нет: needs-review", 9, 5*time.Minute),
			rec(problems.DiffTruncated, "g/web", "дифф больше лимитов", 3, 30*time.Hour),
			rec(problems.MainPipelineFailed, "g/api", "ветка main: пайплайн упал", 1, 72*time.Hour),
			rec(problems.ContextUnavailable, "g/api", "g/payments", 2, 10*time.Second),
			hidden, fixed,
		}
		rows[1].LastMRIID = 12
	}
	st := &fakeStore{rows: rows}
	h := New(Options{Store: st, Enabled: true, GitLabURL: "https://gitlab.example.com", Location: time.UTC, Now: func() time.Time { return t0 }})
	return h, st
}

func get(t *testing.T, h http.Handler, target string) string {
	t.Helper()
	w := do(h, "GET", target, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", target, w.Code)
	}
	body := w.Body.String()
	if v := webui.InlineViolations(body); len(v) != 0 {
		t.Errorf("GET %s: inline style/handler (blocked by the CSP): %q", target, v)
	}
	return body
}

var rowRe = regexp.MustCompile(`<tbody class="p (\w+)" data-q="[^"]*" data-fp="([0-9a-f]{32})"( hidden)?>`)

// order lists the fingerprints of the rendered rows (+" hidden" when the
// search hides it).
func order(body string) []string {
	var out []string
	for _, m := range rowRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[2]+m[3])
	}
	return out
}

func fp(k problems.Kind, project, subject string) string {
	return problems.Fingerprint(k, project, subject)
}

var (
	fpLabels = fp(problems.LabelsFailed, "g/api", "метки нет: needs-review")
	fpDiff   = fp(problems.DiffTruncated, "g/web", "дифф больше лимитов")
	fpMain   = fp(problems.MainPipelineFailed, "g/api", "ветка main: пайплайн упал")
	fpCtx    = fp(problems.ContextUnavailable, "g/api", "g/payments")
	fpHidden = fp(problems.ReactionsFailed, "g/web", "403 на award emoji")
	fpFixed  = fp(problems.RepoConfigInvalid, "g/old", ".reviewbot.yaml")
)

// "Сначала важное": what breaks main or the review, then what narrows it
// (severity, then repeats), then what does not affect it; the summary
// counts the active problems per impact and links to the impact filter.
func TestPageOrderSummaryAndImpactFilter(t *testing.T) {
	h, _ := impactHandler(t)
	body := get(t, h, "/problems")
	if got, want := strings.Join(order(body), ","), strings.Join([]string{fpMain, fpDiff, fpCtx, fpLabels}, ","); got != want {
		t.Errorf("order\n got %s\nwant %s", got, want)
	}
	for _, want := range []string{
		`<a href="/problems" aria-current="true">`, `<span class="lbl">Активные</span><span class="val">4</span>`,
		`<a href="/problems?impact=breaks">`, `<span class="lbl">Ломают ревью или main</span><span class="val bad">1</span><span class="sub">чинить первыми</span>`,
		`<span class="lbl">Ревью проверило не всё</span><span class="val">2</span>`, `<span class="lbl">Не влияют на ревью</span><span class="val">1</span>`,
		`<select name="impact" data-label="Влияние">`, `<option value="">любое</option>`,
		`<option value="breaks" data-count="1">ломает ревью или main</option>`,
		`<option value="labels_failed" data-group="Шаги бота" data-count="1">Метки MR не проставлены</option>`,
		`<option value="model_other" data-group="Сообщила модель" data-count="0">`,
		`<option value="g/api" data-count="3">g/api</option>`,
		`<input type="search" name="q" value="" data-label="Поиск"`,
		`<h2 id="h-problems">Активные проблемы</h2><span class="count" data-total>4</span>`,
		"Как исправить: Почините main или откатите прямой пуш", "ломает ревью или main", "ревью проверило не всё",
		`<span class="g">g/</span><span class="mono">api</span>`, "5 мин назад", "30 ч назад", "3 дн назад", "только что",
		`src="` + webassets.ProblemsPath + `"`, `href="https://gitlab.example.com/g/web/-/merge_requests/12"`,
		`<form id="bulk-ack" class="inline" method="post" action="/problems/ack">`, `form="bulk-ack"`, "Скрыть выбранные",
		`<th scope="col" aria-sort="descending"><a class="sort" href="/problems?sort=-prio">Проблема`,
		`<a class="sort" href="/problems?sort=count">Раз`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "bulk-unack") || strings.Contains(body, fpHidden) || strings.Contains(body, fpFixed) {
		t.Error("the active list shows hidden or fixed problems")
	}
	// The impact filter: one class only; the summary keeps its numbers and
	// marks the chosen one; a click on «Активные» drops the filter.
	b := get(t, h, "/problems?impact=partial&project=g/api")
	if got := order(b); len(got) != 1 || got[0] != fpCtx {
		t.Errorf("impact filter rows %v", got)
	}
	for _, want := range []string{`<a href="/problems?impact=partial&amp;project=g%2Fapi" aria-current="true">`,
		`<a href="/problems?project=g%2Fapi">`, `<option value="partial" data-count="1" selected>`} {
		if !strings.Contains(b, want) {
			t.Errorf("impact page lacks %q", want)
		}
	}
	// An unknown impact is ignored.
	if got := order(get(t, h, "/problems?impact=bogus")); len(got) != 4 {
		t.Errorf("bogus impact filtered: %v", got)
	}
}

// Sorting by a column; a second click reverses it; ties fall back to the
// priority; unknown keys give the default.
func TestPageSort(t *testing.T) {
	h, _ := impactHandler(t)
	for target, want := range map[string][]string{
		"/problems?sort=count":    {fpLabels, fpDiff, fpCtx, fpMain},
		"/problems?sort=-count":   {fpMain, fpCtx, fpDiff, fpLabels},
		"/problems?sort=last":     {fpCtx, fpLabels, fpDiff, fpMain},
		"/problems?sort=-last":    {fpMain, fpDiff, fpLabels, fpCtx},
		"/problems?sort=project":  {fpMain, fpCtx, fpLabels, fpDiff},
		"/problems?sort=-prio":    {fpLabels, fpCtx, fpDiff, fpMain},
		"/problems?sort=bogus":    {fpMain, fpDiff, fpCtx, fpLabels},
		"/problems?sort=prio":     {fpMain, fpDiff, fpCtx, fpLabels},
		"/problems?sort=-project": {fpDiff, fpMain, fpCtx, fpLabels},
	} {
		if got := order(get(t, h, target)); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %v, want %v", target, got, want)
		}
	}
	b := get(t, h, "/problems?sort=count&kind=diff_truncated")
	for _, want := range []string{`<th scope="col" class="num" aria-sort="descending"><a class="sort" href="/problems?kind=diff_truncated&amp;sort=-count">Раз`,
		`<a class="sort" href="/problems?kind=diff_truncated">Проблема`, "Порядок — по колонке"} {
		if !strings.Contains(b, want) {
			t.Errorf("sorted page lacks %q", want)
		}
	}
	b = get(t, h, "/problems?sort=-project")
	if !strings.Contains(b, `aria-sort="descending"><a class="sort" href="/problems?sort=project">Проект<span class="ar" aria-hidden="true">↑</span>`) {
		t.Error("reversed project column")
	}
}

// The search marks rows that do not match as hidden (the page script can
// show them again without a reload); without JS the GET form applies it.
func TestPageSearch(t *testing.T) {
	h, _ := impactHandler(t)
	b := get(t, h, "/problems?q=!12")
	if got := order(b); strings.Join(got, ",") != fpMain+" hidden,"+fpDiff+","+fpCtx+" hidden,"+fpLabels+" hidden" {
		t.Errorf("search rows %v", got)
	}
	if !strings.Contains(b, `<span class="count" data-total>1</span>`) || !strings.Contains(b, `value="!12"`) ||
		!strings.Contains(b, `<div class="empty" data-qempty hidden>`) {
		t.Error("search count / value / hidden empty note")
	}
	b = get(t, h, "/problems?q=PAYMENTS")
	if !strings.Contains(b, `<span class="count" data-total>1</span>`) || !strings.Contains(b, `<span class="val">1</span><span class="sub">без скрытых`) {
		t.Errorf("case-insensitive search / summary follows the search")
	}
	b = get(t, h, "/problems?q=nothing-matches")
	if !strings.Contains(b, `<div class="empty" data-qempty>`) || !strings.Contains(b, `data-total>0</span>`) {
		t.Error("no match: the note is shown")
	}
	if !strings.Contains(get(t, h, "/problems?q="+strings.Repeat("я", 300)), `value="`+strings.Repeat("я", 200)+`"`) {
		t.Error("the search is capped")
	}
}

// Hidden problems: their own list, "Вернуть" per row and in bulk; the fixed
// one has neither; the status timeline uses the stored times only.
func TestPageHiddenAndFixedRows(t *testing.T) {
	h, _ := impactHandler(t)
	b := get(t, h, "/problems?status=acked")
	for _, want := range []string{`<form id="bulk-unack" class="inline" method="post" action="/problems/unack">`, "Вернуть выбранные",
		`value="` + fpHidden + `" form="bulk-unack"`, `<button type="submit" class="btn sm">Вернуть</button>`,
		"скрыта <b>28.10.2026 11:00</b> · вернётся, если повторится", "Скрытые проблемы"} {
		if !strings.Contains(b, want) {
			t.Errorf("hidden list lacks %q", want)
		}
	}
	if strings.Contains(b, `id="bulk-ack"`) {
		t.Error("hide form on the hidden list")
	}
	b = get(t, h, "/problems?status=all")
	if !strings.Contains(b, "исправлена <b>28.10.2026 11:00</b>") || strings.Contains(b, `value="`+fpFixed+`"`) ||
		!strings.Contains(b, `id="bulk-ack"`) || !strings.Contains(b, `id="bulk-unack"`) || !strings.Contains(b, "2 раз, последний") {
		t.Error("all: fixed row / both bulk forms / history")
	}
}

// Steps: kind-specific instructions with snippets; the placeholder becomes
// the subject when it is a safe project path; kinds without steps show the
// hint; backticks become <code>.
func TestPageSteps(t *testing.T) {
	h, _ := impactHandler(t)
	b := get(t, h, "/problems")
	for _, want := range []string{
		"<pre>context:\n  related:\n    - repo: g/payments</pre>",
		`<span class="f">.reviewbot.yaml</span><span class="w">корень репозитория, целевая ветка MR</span><span class="spacer"></span>`,
		`<span class="f">config.yaml</span><span class="w">конфиг бота</span><span class="ex">пример</span>`,
		"<pre>labels:\n  create_missing: true</pre>", `<li class="or">или</li>`, "<code>context.related</code>",
		`<button type="button" class="copy js-only" data-copy aria-label="Скопировать: config.yaml">Копировать</button>`,
		`aria-controls="fix-` + fpCtx + `" data-fix>`, `<tr class="fix" id="fix-` + fpCtx + `">`, "Подсказка целиком",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("steps lack %q", want)
		}
	}
	// An unsafe subject keeps the example placeholder; a kind without steps
	// (model_other) shows its hint as the only step; an unknown kind (an
	// old row) renders with the neutral fallback.
	bad := rec(problems.ContextUnavailable, "g/x", `"><script>x</script>`, 1, 0)
	other := rec(problems.ModelOther, "g/x", "что-то", 1, 0)
	gone := rec("gone_kind", "g/x", "старое", 1, 0)
	h2, _ := impactHandler(t, bad, other, gone)
	b = get(t, h2, "/problems")
	for _, want := range []string{"    - repo: group/project</pre>", "Подробности — в описании проблемы и в транскрипте запуска (/runs).",
		"gone_kind", "Как исправить: gone_kind"} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(b, "<script>x") {
		t.Error("unescaped subject")
	}
}

func TestPageEmptyStates(t *testing.T) {
	// Active list empty: reassuring, with the last time a problem was
	// recorded, links to the hidden and fixed ones.
	h, _ := impactHandler(t)
	b := get(t, h, "/problems?kind=model_other")
	if !strings.Contains(b, "Под фильтр ничего не попало") || !strings.Contains(b, "активных 0, скрытых 0, исправленных 0") ||
		!strings.Contains(b, `<a class="btn sm" href="/problems">Сбросить фильтры</a>`) {
		t.Error("filtered empty state")
	}
	quiet := []problems.Record{rec(problems.LabelsFailed, "g/api", "x", 1, 2*time.Hour), rec(problems.DiffTruncated, "g/api", "y", 1, 50*time.Hour)}
	quiet[0].AckedAt, quiet[1].ResolvedAt = t0, t0
	h, _ = impactHandler(t, quiet...)
	b = get(t, h, "/problems")
	for _, want := range []string{`<div class="empty calm">`, "Активных проблем нет", "С 10:00 бот не записал ни одной новой проблемы проверки.",
		"Скрытые (1) вернутся сюда, если повторятся.", `href="/problems?status=acked">Скрытые · 1</a>`, `href="/problems?status=resolved">Исправленные · 1</a>`,
		`<span class="val">0</span><span class="sub">новых нет</span>`, `<span class="val">0</span><span class="sub">нет</span>`} {
		if !strings.Contains(b, want) {
			t.Errorf("calm empty state lacks %q", want)
		}
	}
	if strings.Contains(b, "data-bulk") || strings.Contains(b, "<table") {
		t.Error("an empty list has the table or the selection bar")
	}
	// The last problem was on another day: with the date.
	older := rec(problems.LabelsFailed, "g/api", "x", 1, 30*time.Hour)
	older.AckedAt = t0
	h, _ = impactHandler(t, older)
	if b := get(t, h, "/problems"); !strings.Contains(b, "С 27.10.2026 06:00 бот") {
		t.Error("date of the last problem")
	}
	h2 := New(Options{Store: &fakeStore{}, Enabled: true, Location: time.UTC, Now: func() time.Time { return t0 }})
	for target, want := range map[string]string{
		"/problems":                 "Бот ещё не записал ни одной проблемы проверки",
		"/problems?status=acked":    "Скрытых нет",
		"/problems?status=resolved": "Исправленных нет",
		"/problems?status=all":      "Проблем нет",
	} {
		if b := get(t, h2, target); !strings.Contains(b, want) {
			t.Errorf("%s lacks %q", target, want)
		}
	}
}

func TestPageStoreErrors(t *testing.T) {
	h, st := impactHandler(t)
	st.listErr = errors.New("db down")
	b := get(t, h, "/problems")
	if !strings.Contains(b, "Не удалось прочитать проблемы") || strings.Contains(b, "Активных проблем нет") {
		t.Error("store error: banner, no empty state")
	}
}

// csrfOf reads the CSRF token of a page.
func csrfOf(t *testing.T, h http.Handler) string {
	t.Helper()
	m := csrfRe.FindStringSubmatch(do(h, "GET", "/problems?status=all", nil, "").Body.String())
	if m == nil {
		t.Fatal("no csrf token in the page")
	}
	return m[1]
}

var sameHost = map[string]string{"Origin": "http://example.com"}

func post(h http.Handler, path string, hdr map[string]string, v url.Values) (int, string) {
	w := do(h, "POST", path, hdr, v.Encode())
	return w.Code, w.Header().Get("Location")
}

// Bulk hide: several fingerprints in one POST, duplicates once, the
// redirect names what changed (for "Вернуть"); then the banner and its
// undo form; then "Вернуть" through /problems/unack.
func TestBulkHideAndReturn(t *testing.T) {
	h, st := impactHandler(t)
	csrf := csrfOf(t, h)
	code, loc := post(h, AckPath, sameHost, url.Values{"fingerprint": {fpDiff, fpLabels, fpDiff, fpHidden}, "csrf": {csrf},
		"back": {"status=new&impact=partial&q=x&sort=-count&evil=1"}})
	if code != http.StatusSeeOther || loc != "/problems?hidden="+fpDiff+"%2C"+fpLabels+"&impact=partial&q=x&sort=-count&status=new" {
		t.Fatalf("bulk ack: %d %q", code, loc)
	}
	if strings.Join(st.acked, ",") != strings.Join([]string{fpDiff, fpLabels, fpHidden}, ",") {
		t.Errorf("acked %v", st.acked)
	}
	b := get(t, h, loc)
	for _, want := range []string{`<div class="notice" role="status">`, "Скрыто проблем: 2. Они вернутся, если повторятся.",
		`<input type="hidden" name="fingerprint" value="` + fpDiff + `"><input type="hidden" name="fingerprint" value="` + fpLabels + `">`,
		`<button type="submit" class="linkbtn">Вернуть</button>`} {
		if !strings.Contains(b, want) {
			t.Errorf("banner lacks %q", want)
		}
	}
	code, loc = post(h, UnackPath, sameHost, url.Values{"fingerprint": {fpDiff, fpLabels, fpMain}, "csrf": {csrf}, "back": {"status=new"}})
	if code != http.StatusSeeOther || loc != "/problems?restored=2&status=new" {
		t.Fatalf("unack: %d %q", code, loc)
	}
	if strings.Join(st.unacked, ",") != strings.Join([]string{fpDiff, fpLabels, fpMain}, ",") {
		t.Errorf("unacked %v", st.unacked)
	}
	if b := get(t, h, loc); !strings.Contains(b, "Возвращено в активные: 2.") || len(order(b)) != 4 {
		t.Error("restored banner / rows back")
	}
	// Nothing changed: no banner parameter.
	if _, loc := post(h, UnackPath, sameHost, url.Values{"fingerprint": {fpMain}, "csrf": {csrf}}); loc != "/problems" {
		t.Errorf("no-op unack: %q", loc)
	}
	if _, loc := post(h, AckPath, sameHost, url.Values{"fingerprint": {fpHidden}, "csrf": {csrf}}); loc != "/problems" {
		t.Errorf("no-op ack: %q", loc)
	}
}

// More than maxUndo hidden at once: the banner gets the count and a link to
// the hidden list instead of an undo form.
func TestBulkHideManyGivesCount(t *testing.T) {
	var rows []problems.Record
	var fps []string
	for i := 0; i < maxUndo+1; i++ {
		r := rec(problems.LabelsFailed, "g/api", fmt.Sprintf("метка %c", 'a'+rune(i%26))+strings.Repeat("x", i/26), 1, 0)
		rows = append(rows, r)
		fps = append(fps, r.Fingerprint)
	}
	h, _ := impactHandler(t, rows...)
	code, loc := post(h, AckPath, sameHost, url.Values{"fingerprint": fps, "csrf": {csrfOf(t, h)}})
	if code != http.StatusSeeOther || loc != fmt.Sprintf("/problems?hidden=%d", maxUndo+1) {
		t.Fatalf("many: %d %q", code, loc)
	}
	b := get(t, h, loc)
	if !strings.Contains(b, fmt.Sprintf("Скрыто проблем: %d.", maxUndo+1)) || !strings.Contains(b, `<a class="navlink" href="/problems?status=acked">Скрытые</a>`) ||
		strings.Contains(b, `class="linkbtn">Вернуть`) {
		t.Error("count banner")
	}
}

// The banner parameters are validated strictly: anything else shows nothing.
func TestBannerValidation(t *testing.T) {
	h, _ := impactHandler(t)
	for _, q := range []string{"hidden=bogus", "hidden=" + fpDiff + ",../x", "hidden=0", "hidden=12345", "restored=0", "restored=-1",
		"restored=abc", "hidden=" + strings.TrimSuffix(strings.Repeat(fpDiff+",", maxUndo+1), ","), "hidden=" + strings.ToUpper(fpDiff)} {
		if b := get(t, h, "/problems?"+q); strings.Contains(b, `class="notice"`) {
			t.Errorf("%s: banner shown", q)
		}
	}
	if b := get(t, h, "/problems?hidden="+fpDiff); !strings.Contains(b, "Проблема скрыта до следующего появления.") {
		t.Error("single hide banner")
	}
}

// /problems/unack has exactly the protections of /problems/ack; both
// refuse bad bulk input as a whole.
func TestMutationProtections(t *testing.T) {
	h, st := testHandler(t, token)
	auth := map[string]string{"Authorization": "Bearer " + token}
	m := csrfRe.FindStringSubmatch(do(h, "GET", "/problems", auth, "").Body.String())
	if m == nil {
		t.Fatal("no csrf")
	}
	csrf := m[1]
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{"Authorization": "Bearer " + token}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	form := func(csrf string, fps ...string) string {
		return url.Values{"fingerprint": fps, "csrf": {csrf}}.Encode()
	}
	origin := with(sameHost)
	many := make([]string, maxBulk+1)
	for i := range many {
		many[i] = fpA
	}
	for _, path := range []string{AckPath, UnackPath} {
		for name, tc := range map[string]struct {
			hdr  map[string]string
			body string
			code int
		}{
			"no auth":           {sameHost, form(csrf, fpA), http.StatusUnauthorized},
			"no origin":         {with(nil), form(csrf, fpA), http.StatusForbidden},
			"null origin":       {with(map[string]string{"Origin": "null"}), form(csrf, fpA), http.StatusForbidden},
			"foreign origin":    {with(map[string]string{"Origin": "https://evil.example"}), form(csrf, fpA), http.StatusForbidden},
			"foreign referer":   {with(map[string]string{"Referer": "https://evil.example/problems"}), form(csrf, fpA), http.StatusForbidden},
			"origin javascript": {with(map[string]string{"Origin": "javascript://example.com"}), form(csrf, fpA), http.StatusForbidden},
			"bad csrf":          {origin, form(strings.Repeat("0", 64), fpA), http.StatusForbidden},
			"no csrf":           {origin, form("", fpA), http.StatusForbidden},
			"no fingerprint":    {origin, form(csrf), http.StatusBadRequest},
			"bad fingerprint":   {origin, form(csrf, "../x"), http.StatusBadRequest},
			"one bad of many":   {origin, form(csrf, fpA, strings.ToUpper(fpA)), http.StatusBadRequest},
			"too many":          {origin, form(csrf, many...), http.StatusBadRequest},
			"body too large":    {origin, form(csrf, fpA) + "&pad=" + strings.Repeat("a", maxBodySize), http.StatusBadRequest},
		} {
			if w := do(h, "POST", path, tc.hdr, tc.body); w.Code != tc.code {
				t.Errorf("%s %s: %d, want %d (%s)", path, name, w.Code, tc.code, w.Body.String())
			}
		}
	}
	if len(st.acked) != 0 || len(st.unacked) != 0 {
		t.Fatalf("changed despite protections: %v %v", st.acked, st.unacked)
	}
	// A whole table (maxBulk fingerprints) fits the body limit.
	if w := do(h, "POST", AckPath, origin, form(csrf, many[:maxBulk]...)); w.Code != http.StatusSeeOther {
		t.Errorf("maxBulk: %d %s", w.Code, w.Body.String())
	}
	// Store errors are 500 for both.
	st.ackErr = errors.New("db down")
	for _, path := range []string{AckPath, UnackPath} {
		if w := do(h, "POST", path, origin, form(csrf, fpA)); w.Code != http.StatusInternalServerError {
			t.Errorf("%s store error: %d", path, w.Code)
		}
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		if w := do(h, method, UnackPath, auth, ""); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
			t.Errorf("%s unack: %d %q", method, w.Code, w.Header().Get("Allow"))
		}
	}
}

func TestBackValues(t *testing.T) {
	v := backValues("kind=x&project=g/a&status=all&impact=none&q=y&sort=-last&hidden=1&evil=2&q2=z")
	if got := v.Encode(); got != "impact=none&kind=x&project=g%2Fa&q=y&sort=-last&status=all" {
		t.Errorf("back %s", got)
	}
	if len(backValues("%zz")) != 0 || len(backValues("q="+strings.Repeat("a", 4*maxQueryRunes+1))) != 0 {
		t.Error("bad back kept")
	}
}

func TestRegisterMountsUnack(t *testing.T) {
	mux := http.NewServeMux()
	Register(mux, Options{Store: &fakeStore{}, Enabled: true})
	if _, pat := mux.Handler(httptest.NewRequest("POST", UnackPath, nil)); pat == "" {
		t.Errorf("%s not mounted", UnackPath)
	}
}

// A project without a group, a row without times (old data) and a table
// longer than maxRows.
func TestPageEdgeRows(t *testing.T) {
	solo := problems.Record{Fingerprint: fp(problems.LabelsFailed, "solo", "x"), Kind: problems.LabelsFailed, ProjectPath: "solo", Subject: "x"}
	h, _ := impactHandler(t, solo)
	b := get(t, h, "/problems")
	if !strings.Contains(b, `<span class="g"></span><span class="mono">solo</span>`) || !strings.Contains(b, "появилась <b>—</b>") {
		t.Error("solo project / zero times")
	}
	var rows []problems.Record
	for i := 0; i <= maxRows; i++ {
		rows = append(rows, problems.Record{Fingerprint: fmt.Sprintf("%032x", i), Kind: problems.LabelsFailed, ProjectPath: "g/a", Subject: "s", LastSeen: t0})
	}
	h, _ = impactHandler(t, rows...)
	if b := get(t, h, "/problems"); !strings.Contains(b, fmt.Sprintf("data-total>%d+</span>", maxRows)) || len(order(b)) != maxRows {
		t.Error("truncated table")
	}
}

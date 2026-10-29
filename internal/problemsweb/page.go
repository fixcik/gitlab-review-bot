package problemsweb

import (
	"cmp"
	"context"
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

//go:embed page.html
var pageHTML string

//go:embed problems.css
var pageCSS string

var pageTmpl = template.Must(template.New("problems").Parse(pageHTML))

// option is an <option> of a filter field (data-count / data-group for the
// filter bar).
type option struct {
	Value, Label string
	Selected     bool
	Count        int
	Group        string
}

// seg is a piece of text: `code` spans of hints and steps become <code>.
type seg struct {
	Text string
	Code bool
}

type snipView struct {
	File, Where, Code string
	Example           bool
}

type stepView struct {
	Or      bool
	Segs    []seg
	Snippet *snipView
}

type rowView struct {
	Fingerprint string
	State       string // problems.StatusNew, StatusAcked, StatusResolved
	Acked       bool
	Resolved    bool // the bot saw the cause gone ("исправлена")
	StateBadge  bool // show "активна" (pages other than the active one)
	Severity    string
	SevLabel    string
	Impact      string
	ImpactLabel string
	Title       string
	Action      string
	Source      string
	Project     string
	Group       string // "group/sub/" of the project path
	Short       string // the last element
	Subject     string
	Detail      string
	Count       int64
	FirstSeen   string
	LastSeen    string
	Ago         string
	AckedAt     string
	ResolvedAt  string
	MRURL       string
	MRLabel     string
	RunURL      string
	Hint        []seg
	Steps       []stepView
	// Form is the bulk form the row's checkbox belongs to ("bulk-ack",
	// "bulk-unack" or "" for none).
	Form string
	// Text is the lower-case text the search looks in (data-q); Hidden: it
	// does not match the current search (the row is rendered hidden, so the
	// search can be changed without a reload).
	Text   string
	Hidden bool
}

type summaryItem struct {
	Label, Sub, Href string
	Value            int
	Current, Bad     bool
}

type sortLink struct {
	Href, Aria, Arrow string
}

type linkView struct{ Label, Href string }

type emptyView struct {
	Calm        bool
	Title, Text string
	Links       []linkView
}

type bannerView struct {
	Text string
	// Undo: the fingerprints the "Вернуть" form posts to /problems/unack.
	Undo []string
	// Link: where to see them when there are too many to undo here.
	Link *linkView
}

type pageData struct {
	// Header is the shared top bar (webui.Nav.Header).
	Header    template.HTML
	Nonce     string
	CommonCSS template.CSS
	CommonJS  template.JS
	Filters   string
	PageJS    string
	CSRF      string
	Back      string
	Kinds     []option
	Projects  []option
	Impacts   []option
	Status    string
	Impact    string
	Q         string
	Heading   string
	SortHint  string
	Summary   []summaryItem
	Sorts     map[string]sortLink
	Rows      []rowView
	Total     int // rows matching every filter (search included)
	Truncated bool
	HasAck    bool // some row can be hidden (bulk-ack form)
	HasUnack  bool // some row can be returned (bulk-unack form)
	Banner    *bannerView
	Empty     *emptyView
	Error     string
}

var sevLabels = map[problems.Severity]string{problems.SevError: "ошибка", problems.SevWarn: "внимание", problems.SevInfo: "инфо"}

// safeProjectRe: project paths that may be put into a GitLab URL (and into
// a snippet instead of its placeholder).
var safeProjectRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$`)

func safeProject(p string) bool { return safeProjectRe.MatchString(p) && !strings.Contains(p, "..") }

// query is the validated page parameters.
type query struct {
	Kind    problems.Kind
	Project string
	Status  string // new, acked, resolved, all
	Impact  problems.Impact
	Q       string
	Sort    string // "", "prio", "project", "count", "last", with an optional "-" (reversed)
}

var sortKeys = []string{"prio", "project", "count", "last"}

func parseQuery(q url.Values) query {
	p := query{Status: q.Get("status"), Kind: problems.Kind(strings.TrimSpace(q.Get("kind"))), Project: strings.TrimSpace(q.Get("project")),
		Impact: problems.Impact(q.Get("impact")), Q: strings.TrimSpace(q.Get("q")), Sort: q.Get("sort")}
	switch p.Status {
	case problems.StatusNew, problems.StatusAcked, problems.StatusResolved, "all":
	default:
		p.Status = problems.StatusNew
	}
	if _, ok := problems.Info(p.Kind); !ok {
		p.Kind = ""
	}
	if !p.Impact.Valid() {
		p.Impact = ""
	}
	if r := []rune(p.Q); len(r) > maxQueryRunes {
		p.Q = string(r[:maxQueryRunes])
	}
	if !slices.Contains(sortKeys, strings.TrimPrefix(p.Sort, "-")) || p.Sort == "prio" {
		p.Sort = ""
	}
	return p
}

// values is the query as URL parameters (defaults left out).
func (p query) values() url.Values {
	v := url.Values{}
	if p.Kind != "" {
		v.Set("kind", string(p.Kind))
	}
	if p.Project != "" {
		v.Set("project", p.Project)
	}
	v.Set("status", p.Status)
	if p.Impact != "" {
		v.Set("impact", string(p.Impact))
	}
	if p.Q != "" {
		v.Set("q", p.Q)
	}
	if p.Sort != "" {
		v.Set("sort", p.Sort)
	}
	return v
}

func (p query) href(change func(v url.Values)) string {
	v := p.values()
	if change != nil {
		change(v)
	}
	if v.Get("status") == problems.StatusNew {
		v.Del("status")
	}
	if len(v) == 0 {
		return PagePath
	}
	return PagePath + "?" + v.Encode()
}

func state(r problems.Record) string {
	switch {
	case !r.ResolvedAt.IsZero():
		return problems.StatusResolved
	case !r.AckedAt.IsZero():
		return problems.StatusAcked
	}
	return problems.StatusNew
}

// searchText is what the text search looks in: subject, detail, title,
// action, project and "!<MR>".
func searchText(r problems.Record) string {
	info := problems.InfoOf(r.Kind)
	t := r.Subject + " " + r.Detail + " " + info.Title + " " + info.Action + " " + r.ProjectPath
	if r.LastMRIID > 0 {
		t += " !" + strconv.FormatInt(r.LastMRIID, 10)
	}
	return strings.ToLower(t)
}

// match: does r pass the filters of p, except the ones skipped.
type skip struct{ status, kind, project, impact, q bool }

func (p query) match(r problems.Record, s skip) bool {
	switch {
	case !s.status && p.Status != "all" && state(r) != p.Status,
		!s.kind && p.Kind != "" && r.Kind != p.Kind,
		!s.project && p.Project != "" && !strings.EqualFold(r.ProjectPath, p.Project),
		!s.impact && p.Impact != "" && problems.InfoOf(r.Kind).Impact != p.Impact,
		!s.q && p.Q != "" && !strings.Contains(searchText(r), strings.ToLower(p.Q)):
		return false
	}
	return true
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	p := parseQuery(r.URL.Query())
	f := problems.Filter{Kind: p.Kind, Project: p.Project, Limit: maxRows + 1}
	if p.Status != "all" {
		f.Status = p.Status
	}
	nonce := usageweb.Nonce()
	css, js := webui.Assets()
	d := pageData{Header: template.HTML(h.o.Nav.Header(r.Context(), webui.SectionProblems)), Nonce: nonce, CommonCSS: template.CSS(css + "\n" + pageCSS), CommonJS: template.JS(js), Filters: webassets.FiltersPath,
		PageJS: webassets.ProblemsPath, CSRF: h.csrfToken(), Status: p.Status, Impact: string(p.Impact), Q: p.Q, Back: p.values().Encode()}
	rows, err := h.o.Store.ListProblems(r.Context(), f)
	if err != nil {
		h.o.Logger.Error("problems: list", "err", err)
		d.Error = "Не удалось прочитать проблемы (подробности в логах бота)."
	}
	if len(rows) > maxRows {
		rows, d.Truncated = rows[:maxRows], true
	}
	all := h.allProblems(r.Context())
	h.options(&d, p, all)
	h.summary(&d, p, all)
	shown := rows[:0:0]
	for _, x := range rows {
		if p.match(x, skip{status: true, kind: true, project: true, q: true}) {
			shown = append(shown, x)
		}
	}
	sortRows(shown, p.Sort)
	for _, x := range shown {
		v := h.rowView(x, p)
		d.HasAck = d.HasAck || v.Form == "bulk-ack"
		d.HasUnack = d.HasUnack || v.Form == "bulk-unack"
		if !v.Hidden {
			d.Total++
		}
		d.Rows = append(d.Rows, v)
	}
	d.Heading = map[string]string{problems.StatusNew: "Активные проблемы", problems.StatusAcked: "Скрытые проблемы",
		problems.StatusResolved: "Исправленные проблемы", "all": "Все проблемы"}[p.Status]
	d.SortHint = "Сначала то, что ломает main или ревью. «Скрыть» убирает проблему, пока она не появится снова."
	if p.Sort != "" {
		d.SortHint = "Порядок — по колонке. Вернуть «сначала важное» — клик по «Проблема»."
	}
	d.Sorts = sortLinks(p)
	d.Banner = h.banner(r.URL.Query(), p)
	if d.Error == "" && d.Total == 0 && len(d.Rows) == 0 {
		d.Empty = h.empty(p, all)
	}
	// form-action 'self': the filters (GET) and the hide/return forms (POST)
	// go to this origin only.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
		"'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	if err := pageTmpl.Execute(w, d); err != nil {
		h.o.Logger.Error("problems: render", "err", err)
	}
}

// allProblems lists every problem (any status) for the filter options,
// counts, the summary and the empty state; none when the store cannot be
// read.
func (h *handler) allProblems(ctx context.Context) []problems.Record {
	all, err := h.o.Store.ListProblems(ctx, problems.Filter{Limit: 5000})
	if err != nil {
		return nil
	}
	return all
}

func (h *handler) options(d *pageData, p query, all []problems.Record) {
	kindN, projN, impN := map[problems.Kind]int{}, map[string]int{}, map[problems.Impact]int{}
	var projects []string
	seen := map[string]bool{}
	for _, x := range all {
		if x.ProjectPath != "" && !seen[strings.ToLower(x.ProjectPath)] {
			seen[strings.ToLower(x.ProjectPath)] = true
			projects = append(projects, x.ProjectPath)
		}
		if p.match(x, skip{kind: true}) {
			kindN[x.Kind]++
		}
		if p.match(x, skip{project: true}) {
			projN[strings.ToLower(x.ProjectPath)]++
		}
		if p.match(x, skip{impact: true}) {
			impN[problems.InfoOf(x.Kind).Impact]++
		}
	}
	for _, k := range problems.Kinds() {
		g := "Шаги бота"
		if k.Model() {
			g = "Сообщила модель"
		}
		d.Kinds = append(d.Kinds, option{Value: string(k.Kind), Label: k.Title, Selected: k.Kind == p.Kind, Count: kindN[k.Kind], Group: g})
	}
	sort.Strings(projects)
	for _, pr := range projects {
		d.Projects = append(d.Projects, option{Value: pr, Label: pr, Selected: strings.EqualFold(pr, p.Project), Count: projN[strings.ToLower(pr)]})
	}
	for _, im := range problems.Impacts() {
		d.Impacts = append(d.Impacts, option{Value: string(im), Label: im.Label(), Selected: im == p.Impact, Count: impN[im]})
	}
}

// summary: four numbers of the active problems (the kind, project and
// search filters apply, the status and impact ones do not): all of them
// and one per impact; each is a link that sets the impact filter.
func (h *handler) summary(d *pageData, p query, all []problems.Record) {
	n := map[problems.Impact]int{}
	total := 0
	for _, x := range all {
		if state(x) == problems.StatusNew && p.match(x, skip{status: true, impact: true}) {
			n[problems.InfoOf(x.Kind).Impact]++
			total++
		}
	}
	set := func(im problems.Impact) string {
		return p.href(func(v url.Values) {
			v.Set("status", problems.StatusNew)
			if im == "" {
				v.Del("impact")
			} else {
				v.Set("impact", string(im))
			}
		})
	}
	sub := "новых нет"
	if total > 0 {
		sub = "без скрытых и исправленных"
	}
	breaks := "нет"
	if n[problems.ImpactBreaks] > 0 {
		breaks = "чинить первыми"
	}
	onActive := p.Status == problems.StatusNew
	d.Summary = []summaryItem{
		{Label: "Активные", Value: total, Sub: sub, Href: set(""), Current: onActive && p.Impact == ""},
		{Label: "Ломают ревью или main", Value: n[problems.ImpactBreaks], Sub: breaks, Href: set(problems.ImpactBreaks),
			Current: onActive && p.Impact == problems.ImpactBreaks, Bad: n[problems.ImpactBreaks] > 0},
		{Label: "Ревью проверило не всё", Value: n[problems.ImpactPartial], Sub: "ревью прошло, но неполное", Href: set(problems.ImpactPartial),
			Current: onActive && p.Impact == problems.ImpactPartial},
		{Label: "Не влияют на ревью", Value: n[problems.ImpactNone], Sub: "метки, реакции, советник, карта", Href: set(problems.ImpactNone),
			Current: onActive && p.Impact == problems.ImpactNone},
	}
}

// sortRows: "" is "сначала важное" (problems.ComparePriority); a column key
// sorts by it (project A→Z, count and last seen: most / newest first), a
// "-" prefix reverses; ties fall back to the priority.
func sortRows(rows []problems.Record, key string) {
	rev := strings.HasPrefix(key, "-")
	key = strings.TrimPrefix(key, "-")
	slices.SortStableFunc(rows, func(a, b problems.Record) int {
		var c int
		switch key {
		case "project":
			c = strings.Compare(strings.ToLower(a.ProjectPath), strings.ToLower(b.ProjectPath))
		case "count":
			c = cmp.Compare(b.Count, a.Count)
		case "last":
			c = b.LastSeen.Compare(a.LastSeen)
		default:
			c = problems.ComparePriority(a, b)
		}
		if rev {
			c = -c
		}
		if c == 0 {
			c = problems.ComparePriority(a, b)
		}
		return c
	})
}

// sortLinks: the header links; a click on the active column reverses it.
func sortLinks(p query) map[string]sortLink {
	cur, rev := strings.TrimPrefix(p.Sort, "-"), strings.HasPrefix(p.Sort, "-")
	if cur == "" {
		cur = "prio"
	}
	out := map[string]sortLink{}
	for _, k := range sortKeys {
		next := k
		l := sortLink{Arrow: "↓"}
		if k == cur {
			l.Aria = "descending"
			if k == "project" {
				l.Aria = "ascending"
			}
			if !rev {
				next = "-" + k
			} else {
				l.Arrow = "↑"
				l.Aria = map[string]string{"descending": "ascending", "ascending": "descending"}[l.Aria]
			}
		}
		if next == "prio" {
			next = ""
		}
		l.Href = p.href(func(v url.Values) {
			if next == "" {
				v.Del("sort")
			} else {
				v.Set("sort", next)
			}
		})
		out[k] = l
	}
	return out
}

var countRe = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)

// banner: the result of the last POST (?hidden=fp,fp… or a count,
// ?restored=N), strictly validated; anything else is ignored.
func (h *handler) banner(q url.Values, p query) *bannerView {
	if n := q.Get("restored"); countRe.MatchString(n) {
		return &bannerView{Text: "Возвращено в активные: " + n + "."}
	}
	raw := q.Get("hidden")
	if raw == "" {
		return nil
	}
	if countRe.MatchString(raw) {
		return &bannerView{Text: "Скрыто проблем: " + raw + ". Они вернутся, если повторятся.",
			Link: &linkView{Label: "Скрытые", Href: p.href(func(v url.Values) { v.Set("status", problems.StatusAcked) })}}
	}
	fps := strings.Split(raw, ",")
	if len(fps) > maxUndo {
		return nil
	}
	for _, fp := range fps {
		if !fingerprintRe.MatchString(fp) {
			return nil
		}
	}
	b := &bannerView{Undo: fps, Text: "Скрыто проблем: " + strconv.Itoa(len(fps)) + ". Они вернутся, если повторятся."}
	if len(fps) == 1 {
		b.Text = "Проблема скрыта до следующего появления."
	}
	return b
}

// empty: what to say when nothing is listed. On the active list without
// filters it is reassuring, with the time of the last recorded problem (a
// real fact of the store) when there is one.
func (h *handler) empty(p query, all []problems.Record) *emptyView {
	n := map[string]int{}
	var latest time.Time
	for _, x := range all {
		if p.match(x, skip{status: true}) {
			n[state(x)]++
		}
		if x.LastSeen.After(latest) {
			latest = x.LastSeen
		}
	}
	link := func(label, status string, c int) linkView {
		return linkView{Label: label + " · " + strconv.Itoa(c), Href: p.href(func(v url.Values) { v.Set("status", status) })}
	}
	filtered := p.Kind != "" || p.Project != "" || p.Impact != "" || p.Q != ""
	e := &emptyView{}
	switch {
	case filtered:
		e.Title = "Под фильтр ничего не попало"
		e.Text = "В других статусах: активных " + strconv.Itoa(n[problems.StatusNew]) + ", скрытых " + strconv.Itoa(n[problems.StatusAcked]) +
			", исправленных " + strconv.Itoa(n[problems.StatusResolved]) + "."
		e.Links = []linkView{{Label: "Сбросить фильтры", Href: query{Status: p.Status}.href(nil)}}
		return e
	case p.Status == problems.StatusNew:
		e.Calm = true
		e.Title = "Активных проблем нет"
		if latest.IsZero() {
			e.Text = "Бот ещё не записал ни одной проблемы проверки (или они старше срока хранения)."
		} else {
			e.Text = "С " + h.since(latest) + " бот не записал ни одной новой проблемы проверки."
		}
		if c := n[problems.StatusAcked]; c > 0 {
			e.Text += " Скрытые (" + strconv.Itoa(c) + ") вернутся сюда, если повторятся."
			e.Links = append(e.Links, link("Скрытые", problems.StatusAcked, c))
		}
		if c := n[problems.StatusResolved]; c > 0 {
			e.Links = append(e.Links, link("Исправленные", problems.StatusResolved, c))
		}
	case p.Status == problems.StatusAcked:
		e.Title = "Скрытых нет"
		e.Text = "«Скрыть» убирает проблему из активных до её следующего появления."
	case p.Status == problems.StatusResolved:
		e.Title = "Исправленных нет"
		e.Text = "«Исправлена» бот ставит сам, когда видит, что причины больше нет (сейчас — для невалидного .reviewbot.yaml)."
	default:
		e.Title = "Проблем нет"
		e.Text = "Бот ещё не записал ни одной проблемы проверки (или они старше срока хранения)."
	}
	return e
}

// since formats the time of the last problem: "14:06" today, else with
// the date.
func (h *handler) since(t time.Time) string {
	t, now := t.In(h.o.Location), h.o.Now().In(h.o.Location)
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("02.01.2006 15:04")
}

func (h *handler) rowView(x problems.Record, p query) rowView {
	info := problems.InfoOf(x.Kind)
	st := state(x)
	v := rowView{
		Fingerprint: x.Fingerprint, State: st, Acked: !x.AckedAt.IsZero(), Resolved: !x.ResolvedAt.IsZero(),
		StateBadge: p.Status != problems.StatusNew, Severity: string(info.Severity), SevLabel: sevLabels[info.Severity],
		Impact: string(info.Impact), ImpactLabel: info.Impact.Label(), Title: info.Title, Action: info.Action,
		Source: string(x.Source), Project: x.ProjectPath, Subject: x.Subject, Detail: x.Detail, Count: x.Count,
		FirstSeen: h.when(x.FirstSeen), LastSeen: h.when(x.LastSeen), Ago: h.ago(x.LastSeen), Hint: segments(info.Hint),
		Text: searchText(x), Hidden: !p.match(x, skip{status: true, kind: true, project: true, impact: true}),
	}
	if i := strings.LastIndex(x.ProjectPath, "/"); i > 0 {
		v.Group, v.Short = x.ProjectPath[:i+1], x.ProjectPath[i+1:]
	} else {
		v.Short = x.ProjectPath
	}
	switch st {
	case problems.StatusNew:
		v.Form = "bulk-ack"
	case problems.StatusAcked:
		v.Form = "bulk-unack"
		v.AckedAt = h.when(x.AckedAt)
	case problems.StatusResolved:
		v.ResolvedAt = h.when(x.ResolvedAt)
	}
	v.Steps = steps(info, x.Subject)
	if x.LastMRIID > 0 {
		iid := strconv.FormatInt(x.LastMRIID, 10)
		v.MRLabel = "!" + iid
		if h.o.GitLabURL != "" && safeProject(x.ProjectPath) {
			v.MRURL = h.o.GitLabURL + "/" + x.ProjectPath + "/-/merge_requests/" + iid
		}
	}
	if diag.ValidID(x.LastRunID) {
		v.RunURL = "/runs/" + x.LastRunID
	}
	return v
}

// steps: the kind's instructions (its hint as one step when it has none).
// A snippet placeholder becomes the problem's subject when that is a safe
// project path (context_unavailable: the related repository).
func steps(info problems.KindInfo, subject string) []stepView {
	if len(info.Steps) == 0 {
		if info.Hint == "" {
			return nil
		}
		return []stepView{{Segs: segments(info.Hint)}}
	}
	out := make([]stepView, 0, len(info.Steps))
	for _, s := range info.Steps {
		sv := stepView{Or: s.Or, Segs: segments(s.Text)}
		if sn := s.Snippet; sn != nil {
			v := &snipView{File: sn.File, Where: sn.Where, Code: sn.Code, Example: sn.Example}
			if sn.Placeholder != "" && safeProject(subject) && strings.Contains(subject, "/") {
				v.Code, v.Example = strings.ReplaceAll(sn.Code, sn.Placeholder, subject), false
			}
			sv.Snippet = v
		}
		out = append(out, sv)
	}
	return out
}

// segments splits text on backticks: odd pieces are code.
func segments(s string) []seg {
	var out []seg
	for i, part := range strings.Split(s, "`") {
		if part != "" {
			out = append(out, seg{Text: part, Code: i%2 == 1})
		}
	}
	return out
}

func (h *handler) when(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(h.o.Location).Format("02.01.2006 15:04")
}

// ago is a short relative time ("5 мин назад").
func (h *handler) ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := h.o.Now().Sub(t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " мин назад"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " ч назад"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + " дн назад"
}

package knowledgeweb

import (
	"cmp"
	_ "embed"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

//go:embed page.html
var pageHTML string

//go:embed knowledge.css
var pageCSS string

var pageTmpl = template.Must(template.New("knowledge").Parse(pageHTML))

type option struct {
	Value, Label string
	Selected     bool
}

// segLink is a tab of a segmented control (a plain link: works without JS).
type segLink struct {
	Label   string
	Count   int
	Href    string
	Current bool
}

// seg is a piece of text: `code` spans of finding excerpts become <code>.
type seg struct {
	Text string
	Code bool
}

// statusInfo: label and explanation of an outcome status.
type statusInfo struct{ Label, Why string }

var statuses = map[string]statusInfo{
	learn.OutcomeFixed:              {"Принято", "Код исправили по замечанию"},
	learn.OutcomeDismissed:          {"Отклонено", "Тред закрыли, код не меняли"},
	learn.OutcomeOpen:               {"Ждут решения", "Тред ещё открыт"},
	learn.OutcomeRejectedByVerifier: {"Не прошли проверку", "Проверка бота опровергла замечание, в MR оно не попало; в точность не входит"},
	learn.OutcomeResolvedByBot:      {"Закрыто ботом", "Некритичные замечания бот закрывает сам сразу после публикации; вердикта нет, в точность не входит"},
	learn.OutcomeClosed:             {"Закрыто иначе", "Не удалось классифицировать (например, после force push)"},
}

// statusOrder: the order of the outcome scale and of the stacked bars.
var statusOrder = []string{learn.OutcomeFixed, learn.OutcomeDismissed, learn.OutcomeOpen, learn.OutcomeRejectedByVerifier,
	learn.OutcomeResolvedByBot, learn.OutcomeClosed}

func count(st learn.OutcomeStats, status string) int {
	switch status {
	case learn.OutcomeFixed:
		return st.Fixed
	case learn.OutcomeDismissed:
		return st.Dismissed
	case learn.OutcomeOpen:
		return st.Open
	case learn.OutcomeRejectedByVerifier:
		return st.RejectedByVerifier
	case learn.OutcomeResolvedByBot:
		return st.ResolvedByBot
	default:
		return st.Closed
	}
}

func total(st learn.OutcomeStats) int {
	return st.Fixed + st.Dismissed + st.Open + st.RejectedByVerifier + st.ResolvedByBot + st.Closed
}

func addStats(a, b learn.OutcomeStats) learn.OutcomeStats {
	return learn.OutcomeStats{Open: a.Open + b.Open, Fixed: a.Fixed + b.Fixed, Dismissed: a.Dismissed + b.Dismissed,
		ResolvedByBot: a.ResolvedByBot + b.ResolvedByBot, Closed: a.Closed + b.Closed, RejectedByVerifier: a.RejectedByVerifier + b.RejectedByVerifier}
}

// distRow is a line of «Куда делись замечания».
type distRow struct {
	Status, Label, Why string
	Count, Width       int // Width: 0..100 of the largest count
}

// barPart is a rect of a stacked outcome bar (viewBox 0 0 100 10).
type barPart struct {
	Status   string
	X, Width string
}

// projectRow is a line of «По проектам».
type projectRow struct {
	Project, Href          string
	Precision              string
	Low                    bool
	Decided                int
	Fixed, Dismissed, Open int
	Rejected, ByBot        int
	Rules, Memory          int
	Empty                  bool // no decisions and nothing open
	Bar                    []barPart
	Tip                    string // tooltip lines
}

type decisionView struct {
	ID          int64
	When        string
	Status      string
	StatusLabel string
	Severity    string
	Title       string
	Project     string
	MR          string // "!12"
	MRURL       string
	File        string
	Excerpt     []seg
	RuleID      int64
	RuleText    string
	RuleDeleted bool
	RuleHref    string // "" when the rule is not listed (gone or beyond the limit)
}

// point is one decision of the precision chart (JSON for the page script).
type point struct {
	Status string `json:"s"`
	Title  string `json:"t"`
	Where  string `json:"w"`
	When   string `json:"d"`
	Day    string `json:"x"`
	Time   string `json:"h"`
}

type kbRow struct {
	Memory bool
	// rule
	ID        int64
	Global    bool
	Project   string
	Text      string
	Author    string
	SourceURL string // empty unless it points into the configured GitLab
	Created   string
	Updated   string
	Deleted   string // deleted rules: when
	CanLimit  bool   // a global rule that remembers its project
	NoProject bool   // cannot be limited to a project
	Highlight bool   // the rule a decision link points at (?rule=)
	// memory
	Title   string
	Bullets []string
	Source  string
}

type bannerView struct {
	Text string
	Warn bool
	// Undo: the rule ids the «Вернуть» form posts to /knowledge/rule/restore.
	Undo []int64
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
	Projects  []option
	Periods   []option
	Period    string // label: "30 дней"
	Project   string

	// precision
	Precision   string
	Fixed       int
	Decided     int
	Low         bool
	LowLimit    int
	Series      []point
	SeriesTrunc bool
	SeriesMax   int

	// distribution
	Dist      []distRow
	DistTotal int

	// decisions
	Outcome      string
	Outcomes     []segLink
	Decisions    []decisionView
	DecTotal     int
	DecFrom      int
	DecTo        int
	PrevHref     string
	MoreHref     string
	OutcomeLabel string

	// knowledge base
	KB          string // "", "rules", "memory", "deleted"
	KBTabs      []segLink
	KBRows      []kbRow
	KBTruncated bool
	HasRules    bool // some row can be selected
	MaxBulk     int
	Banner      *bannerView

	// per project
	ProjectRows []projectRow
	EmptyRows   int

	Error string
}

var periods = []struct {
	Value, Label string
	Days         int
}{{"7", "7 дней", 7}, {"30", "30 дней", 30}, {"90", "90 дней", 90}, {"all", "всё время", 0}}

var kbTabs = []struct{ Value, Label string }{{"", "всё"}, {"rules", "правила"}, {"memory", "память"}, {"deleted", "удалённые"}}

var decidedOrder = []string{learn.OutcomeFixed, learn.OutcomeDismissed, learn.OutcomeRejectedByVerifier}

// query is the validated page parameters.
type query struct {
	Project string
	Period  string // "7", "30", "90", "all"
	Days    int
	Label   string
	KB      string
	Outcome string
	Page    int // decisions page, 1-based
	Rule    int64
}

var pageRe = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)

func parseQuery(v url.Values) query {
	q := query{Project: strings.TrimSpace(v.Get("project")), Period: "30", Days: 30, Label: "30 дней", Page: 1}
	if r := []rune(q.Project); len(r) > 256 {
		q.Project = string(r[:256])
	}
	for _, p := range periods {
		if p.Value == v.Get("period") {
			q.Period, q.Days, q.Label = p.Value, p.Days, p.Label
		}
	}
	for _, t := range kbTabs {
		if t.Value == v.Get("kb") {
			q.KB = t.Value
		}
	}
	if o := v.Get("outcome"); slices.Contains(decidedOrder, o) {
		q.Outcome = o
	}
	if p := v.Get("dpage"); pageRe.MatchString(p) {
		q.Page, _ = strconv.Atoi(p)
	}
	if r := v.Get("rule"); idRe.MatchString(r) {
		q.Rule, _ = strconv.ParseInt(r, 10, 64)
	}
	return q
}

// values: the parameters (defaults left out).
func (q query) values() url.Values {
	v := url.Values{}
	if q.Project != "" {
		v.Set("project", q.Project)
	}
	if q.Period != "30" {
		v.Set("period", q.Period)
	}
	if q.KB != "" {
		v.Set("kb", q.KB)
	}
	if q.Outcome != "" {
		v.Set("outcome", q.Outcome)
	}
	if q.Page > 1 {
		v.Set("dpage", strconv.Itoa(q.Page))
	}
	return v
}

func (q query) href(change func(*query), anchor string) string {
	c := q
	c.Rule = 0
	change(&c)
	h := PagePath
	if v := c.values(); len(v) > 0 {
		h += "?" + v.Encode()
	}
	if c.Rule > 0 {
		sep := "?"
		if strings.Contains(h, "?") {
			sep = "&"
		}
		h += sep + "rule=" + strconv.FormatInt(c.Rule, 10)
	}
	return h + anchor
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	q := parseQuery(r.URL.Query())
	var since time.Time
	if q.Days > 0 {
		since = h.o.Now().AddDate(0, 0, -q.Days)
	}
	nonce := usageweb.Nonce()
	css, js := webui.Assets()
	d := pageData{Header: template.HTML(h.o.Nav.Header(r.Context(), webui.SectionKnowledge)), Nonce: nonce, CommonCSS: template.CSS(css + "\n" + pageCSS), CommonJS: template.JS(js), Filters: webassets.FiltersPath,
		PageJS: webassets.KnowledgePath, CSRF: h.csrfToken(), Back: q.values().Encode(), Period: q.Label, Project: q.Project,
		KB: q.KB, Outcome: q.Outcome, LowLimit: lowDecisions, SeriesMax: maxSeries, MaxBulk: maxBulk}
	for _, p := range periods {
		d.Periods = append(d.Periods, option{Value: p.Value, Label: p.Label, Selected: p.Value == q.Period})
	}

	ctx := r.Context()
	rules, err1 := h.o.Store.ListAllLearnings(ctx, maxRules+1)
	deleted, err2 := h.o.Store.ListDeletedLearnings(ctx, maxRules)
	mem, err3 := h.o.Store.ListAllMemory(ctx)
	stats, err4 := h.o.Store.OutcomeStatsAll(ctx, since)
	series, err5 := h.o.Store.DecisionSeries(ctx, q.Project, since, maxSeries+1)
	f := learn.DecisionFilter{ProjectPath: q.Project, Since: since, Status: q.Outcome, Limit: maxDecisions + 1, Offset: (q.Page - 1) * maxDecisions}
	decisions, err6 := h.o.Store.ListDecisions(ctx, f)
	if err := cmp.Or(err1, err2, err3, err4, err5, err6); err != nil {
		h.o.Logger.Error("knowledge: read", "rules", err1, "deleted", err2, "memory", err3, "stats", err4, "series", err5, "decisions", err6)
		d.Error = "Не удалось прочитать часть данных (подробности в логах бота)."
	}
	if len(rules) > maxRules {
		rules, d.KBTruncated = rules[:maxRules], true
	}

	display := h.projects(&d, q, rules, mem, stats)
	window := learn.OutcomeStats{}
	for k, st := range stats {
		if q.Project == "" || strings.EqualFold(k, q.Project) {
			window = addStats(window, st)
		}
	}
	h.precision(&d, window, series)
	h.dist(&d, window)
	h.decisions(&d, q, window, decisions, rules, deleted)
	h.knowledge(&d, q, rules, deleted, mem)
	h.perProject(&d, q, stats, display, rules, mem)
	d.Banner = banner(r.URL.Query())

	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
		"'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Method == http.MethodHead {
		return
	}
	if err := pageTmpl.Execute(w, d); err != nil {
		h.o.Logger.Error("knowledge: render", "err", err)
	}
}

// projects fills the project filter (every project with rules, memory or
// outcomes) and returns lower-cased path -> display path.
func (h *handler) projects(d *pageData, q query, rules []learn.Rule, mem []memory.Section, stats map[string]learn.OutcomeStats) map[string]string {
	seen := map[string]string{}
	note := func(p string) {
		if p != "" && seen[strings.ToLower(p)] == "" {
			seen[strings.ToLower(p)] = p
		}
	}
	for _, x := range rules {
		if x.Scope == learn.ScopeProject {
			note(x.ProjectPath)
		}
	}
	for _, m := range mem {
		note(m.ProjectPath)
	}
	for p := range stats {
		note(p)
	}
	var names []string
	for _, p := range seen {
		names = append(names, p)
	}
	slices.Sort(names)
	for _, p := range names {
		d.Projects = append(d.Projects, option{Value: p, Label: p, Selected: strings.EqualFold(p, q.Project)})
	}
	return seen
}

func pct(n, of int) string {
	return strconv.Itoa(int(float64(n)/float64(of)*100+0.5)) + "%"
}

// precision: the big number (from the counts of the window) and the chart
// series (the same window, in the order of the decisions).
func (h *handler) precision(d *pageData, st learn.OutcomeStats, series []learn.Outcome) {
	d.Fixed, d.Decided = st.Fixed, st.Fixed+st.Dismissed
	if d.Decided > 0 {
		d.Precision = pct(d.Fixed, d.Decided)
		d.Low = d.Decided < lowDecisions
	}
	if len(series) > maxSeries {
		series, d.SeriesTrunc = series[:maxSeries], true
	}
	for _, o := range series {
		p := point{Status: o.Status, Title: o.Title, Where: o.ProjectPath + " !" + strconv.FormatInt(o.MRIID, 10),
			When: h.when(o.UpdatedAt), Day: o.UpdatedAt.In(h.o.Location).Format("02.01"),
			Time: o.UpdatedAt.In(h.o.Location).Format("15:04")}
		if p.Title == "" {
			p.Title = "замечание без заголовка"
		}
		d.Series = append(d.Series, p)
	}
}

func (h *handler) dist(d *pageData, st learn.OutcomeStats) {
	d.DistTotal = total(st)
	most := 1
	for _, s := range statusOrder {
		most = max(most, count(st, s))
	}
	for _, s := range statusOrder {
		n := count(st, s)
		if s == learn.OutcomeClosed && n == 0 {
			continue
		}
		row := distRow{Status: s, Label: statuses[s].Label, Count: n, Width: n * 100 / most}
		if s == learn.OutcomeRejectedByVerifier || s == learn.OutcomeResolvedByBot {
			row.Why = statuses[s].Why
		}
		if n > 0 && row.Width == 0 {
			row.Width = 1
		}
		d.Dist = append(d.Dist, row)
	}
}

// severityRe: severities that may become a CSS class name.
var severityRe = regexp.MustCompile(`^[a-z]{1,16}$`)

// safeProjectRe: project paths that may be put into a GitLab URL.
var safeProjectRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$`)

func (h *handler) mrURL(project string, iid int64) string {
	if h.o.GitLabURL == "" || iid <= 0 || !safeProjectRe.MatchString(project) || strings.Contains(project, "..") {
		return ""
	}
	return h.o.GitLabURL + "/" + project + "/-/merge_requests/" + strconv.FormatInt(iid, 10)
}

func (h *handler) decisions(d *pageData, q query, st learn.OutcomeStats, rows []learn.Outcome, live, deleted []learn.Rule) {
	all := st.Fixed + st.Dismissed + st.RejectedByVerifier
	d.Outcomes = append(d.Outcomes, segLink{Label: "все", Count: all, Current: q.Outcome == "",
		Href: q.href(func(c *query) { c.Outcome, c.Page = "", 1 }, "#decisions")})
	labels := map[string]string{learn.OutcomeFixed: "принято", learn.OutcomeDismissed: "отклонено", learn.OutcomeRejectedByVerifier: "опровергнуто проверкой"}
	for _, s := range decidedOrder {
		d.Outcomes = append(d.Outcomes, segLink{Label: labels[s], Count: count(st, s), Current: q.Outcome == s,
			Href: q.href(func(c *query) { c.Outcome, c.Page = s, 1 }, "#decisions")})
	}
	d.DecTotal = all
	if q.Outcome != "" {
		d.DecTotal = count(st, q.Outcome)
		d.OutcomeLabel = labels[q.Outcome]
	}
	if len(rows) > maxDecisions {
		rows = rows[:maxDecisions]
		d.MoreHref = q.href(func(c *query) { c.Page++ }, "#decisions")
	}
	if q.Page > 1 {
		d.PrevHref = q.href(func(c *query) { c.Page-- }, "#decisions")
	}
	if len(rows) > 0 {
		d.DecFrom = (q.Page-1)*maxDecisions + 1
		d.DecTo = d.DecFrom + len(rows) - 1
		d.DecTotal = max(d.DecTotal, d.DecTo) // a decision recorded between the two reads
	}
	ruleByID := map[int64]learn.Rule{}
	for _, x := range deleted {
		ruleByID[x.ID] = x
	}
	for _, x := range live {
		ruleByID[x.ID] = x
	}
	for _, o := range rows {
		sev := o.Severity
		if !severityRe.MatchString(sev) {
			sev = ""
		}
		v := decisionView{ID: o.ID, When: h.when(o.UpdatedAt), Status: o.Status, StatusLabel: statuses[o.Status].Label, Severity: sev,
			Title: o.Title, Project: o.ProjectPath, MR: "!" + strconv.FormatInt(o.MRIID, 10), MRURL: h.mrURL(o.ProjectPath, o.MRIID),
			Excerpt: segments(o.BodyExcerpt), RuleID: o.RuleID}
		if v.Title == "" {
			v.Title = "замечание без заголовка"
		}
		if o.Path != "" {
			v.File = o.Path
			if o.Line > 0 {
				v.File += ":" + strconv.Itoa(o.Line)
			}
		}
		if x, ok := ruleByID[o.RuleID]; ok && o.RuleID > 0 {
			v.RuleText, v.RuleDeleted = x.Text, !x.DeletedAt.IsZero()
			kb := ""
			if v.RuleDeleted {
				kb = "deleted"
			}
			v.RuleHref = q.href(func(c *query) { c.KB, c.Rule = kb, o.RuleID }, "#rule-"+strconv.FormatInt(o.RuleID, 10))
		}
		d.Decisions = append(d.Decisions, v)
	}
}

// segments splits text at backticks: odd pieces are code.
func segments(s string) []seg {
	var out []seg
	for i, part := range strings.Split(s, "`") {
		if part != "" {
			out = append(out, seg{Text: part, Code: i%2 == 1})
		}
	}
	return out
}

func appliesTo(x learn.Rule, project string) bool {
	return project == "" || x.Scope == learn.ScopeGlobal || strings.EqualFold(x.ProjectPath, project)
}

func (h *handler) ruleRow(x learn.Rule, q query) kbRow {
	v := kbRow{ID: x.ID, Global: x.Scope == learn.ScopeGlobal, Project: x.ProjectPath, Text: x.Text, Author: x.Author,
		Created: h.when(x.CreatedAt), Updated: h.when(x.UpdatedAt), SourceURL: h.sourceURL(x.SourceURL), Highlight: q.Rule == x.ID}
	v.CanLimit = v.Global && x.ProjectPath != ""
	v.NoProject = x.ProjectPath == ""
	if !x.DeletedAt.IsZero() {
		v.Deleted = h.when(x.DeletedAt)
	}
	return v
}

func (h *handler) knowledge(d *pageData, q query, live, deleted []learn.Rule, mem []memory.Section) {
	var rules, dead []kbRow
	for _, x := range live {
		if appliesTo(x, q.Project) {
			rules = append(rules, h.ruleRow(x, q))
		}
	}
	for _, x := range deleted {
		if appliesTo(x, q.Project) {
			dead = append(dead, h.ruleRow(x, q))
		}
	}
	var memRows []kbRow
	for _, m := range mem {
		if q.Project != "" && !strings.EqualFold(m.ProjectPath, q.Project) {
			continue
		}
		v := kbRow{Memory: true, Project: m.ProjectPath, Title: memory.Title(m.Key), Updated: h.when(m.UpdatedAt), Source: m.SourceNote}
		if v.Title == "" {
			v.Title = m.Key
		}
		for _, line := range strings.Split(m.Content, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
			if line != "" {
				v.Bullets = append(v.Bullets, line)
			}
		}
		memRows = append(memRows, v)
	}
	counts := map[string]int{"": len(rules) + len(memRows), "rules": len(rules), "memory": len(memRows), "deleted": len(dead)}
	for _, t := range kbTabs {
		d.KBTabs = append(d.KBTabs, segLink{Label: t.Label, Count: counts[t.Value], Current: q.KB == t.Value,
			Href: q.href(func(c *query) { c.KB = t.Value }, "#kb")})
	}
	switch q.KB {
	case "rules":
		d.KBRows = rules
	case "memory":
		d.KBRows = memRows
	case "deleted":
		d.KBRows = dead
	default:
		d.KBRows = append(rules, memRows...)
	}
	d.HasRules = q.KB != "memory" && (len(rules) > 0 && q.KB != "deleted" || len(dead) > 0 && q.KB == "deleted")
}

func (h *handler) perProject(d *pageData, q query, stats map[string]learn.OutcomeStats, display map[string]string, rules []learn.Rule, mem []memory.Section) {
	nRules, nMem := map[string]int{}, map[string]int{}
	for _, x := range rules {
		if x.Scope == learn.ScopeProject {
			nRules[strings.ToLower(x.ProjectPath)]++
		}
	}
	for _, m := range mem {
		nMem[strings.ToLower(m.ProjectPath)]++
	}
	for k, st := range stats {
		if q.Project != "" && !strings.EqualFold(k, q.Project) {
			continue
		}
		name := cmp.Or(display[k], k)
		row := projectRow{Project: name, Decided: st.Fixed + st.Dismissed, Fixed: st.Fixed, Dismissed: st.Dismissed, Open: st.Open,
			Rejected: st.RejectedByVerifier, ByBot: st.ResolvedByBot, Rules: nRules[k], Memory: nMem[k],
			Href: q.href(func(c *query) { c.Project, c.Page = name, 1 }, "")}
		if row.Decided > 0 {
			row.Precision = pct(st.Fixed, row.Decided)
			row.Low = row.Decided < lowDecisions
		}
		row.Empty = row.Decided == 0 && st.Open == 0
		if row.Empty {
			d.EmptyRows++
		}
		tip := []string{name}
		x, all := 0, total(st)
		for _, s := range statusOrder {
			n := count(st, s)
			if n == 0 {
				continue
			}
			w := n * 1000 / max(all, 1)
			row.Bar = append(row.Bar, barPart{Status: s, X: tenths(x), Width: tenths(w)})
			x += w
			tip = append(tip, statuses[s].Label+": "+strconv.Itoa(n))
		}
		row.Tip = strings.Join(tip, "\n")
		d.ProjectRows = append(d.ProjectRows, row)
	}
	slices.SortFunc(d.ProjectRows, func(a, b projectRow) int {
		return cmp.Or(cmp.Compare(b.Decided, a.Decided), cmp.Compare(b.Open, a.Open), cmp.Compare(a.Project, b.Project))
	})
}

// tenths formats n/10 ("12.5").
func tenths(n int) string {
	return strconv.Itoa(n/10) + "." + strconv.Itoa(n%10)
}

var countRe = regexp.MustCompile(`^[1-9][0-9]{0,2}$`)

// idList parses "1,2,3": 1..maxBulk ids, each strictly valid; nil otherwise.
func idList(raw string) []int64 {
	parts := strings.Split(raw, ",")
	if raw == "" || len(parts) > maxBulk {
		return nil
	}
	ids := make([]int64, 0, len(parts))
	for _, s := range parts {
		if !idRe.MatchString(s) {
			return nil
		}
		id, _ := strconv.ParseInt(s, 10, 64)
		ids = append(ids, id)
	}
	return ids
}

// plural picks the Russian form for n (1 правило, 2 правила, 5 правил).
func plural(n int, one, few, many string) string {
	switch {
	case n%10 == 1 && n%100 != 11:
		return one
	case n%10 >= 2 && n%10 <= 4 && (n%100 < 12 || n%100 > 14):
		return few
	}
	return many
}

func ruleCount(n int) string {
	return strconv.Itoa(n) + " " + plural(n, "правило", "правила", "правил")
}

// banner: the result of the last POST (?deleted=1,2 / ?restored=N /
// ?scoped=N&scope=… / ?conflict=ID&live=ID&conflicts=N), strictly
// validated; anything else is ignored.
func banner(v url.Values) *bannerView {
	if ids := idList(v.Get("deleted")); ids != nil {
		text := "Удалено " + ruleCount(len(ids)) + ". Бот перестал их учитывать."
		if len(ids) == 1 {
			text = "Правило удалено, бот перестал его учитывать."
		}
		return &bannerView{Text: text, Undo: ids}
	}
	if n := v.Get("restored"); countRe.MatchString(n) {
		k, _ := strconv.Atoi(n)
		if k == 1 {
			return &bannerView{Text: "Правило возвращено, бот снова его учитывает."}
		}
		return &bannerView{Text: "Возвращено " + ruleCount(k) + ". Бот снова их учитывает."}
	}
	if n, s := v.Get("scoped"), v.Get("scope"); countRe.MatchString(n) && (s == learn.ScopeGlobal || s == learn.ScopeProject) {
		k, _ := strconv.Atoi(n)
		if s == learn.ScopeGlobal {
			return &bannerView{Text: "Теперь действуют во всех проектах: " + ruleCount(k) + "."}
		}
		return &bannerView{Text: "Теперь действуют только в своём проекте: " + ruleCount(k) + "."}
	}
	if c, l := v.Get("conflict"), v.Get("live"); idRe.MatchString(c) && idRe.MatchString(l) {
		text := "Ничего не возвращено: правило #" + c + " совпадает с действующим правилом #" + l + "."
		if n := v.Get("conflicts"); countRe.MatchString(n) && n != "1" {
			k, _ := strconv.Atoi(n)
			text = "Ничего не возвращено: " + ruleCount(k) + " совпадают с действующими (например, #" + c + " и #" + l + ")."
		}
		return &bannerView{Text: text + " Снимите их с выбора или удалите действующие.", Warn: true}
	}
	return nil
}

// sourceURL keeps only links into the configured GitLab (the value comes
// from a note and is untrusted).
func (h *handler) sourceURL(u string) string {
	if h.o.GitLabURL == "" || !strings.HasPrefix(u, h.o.GitLabURL+"/") {
		return ""
	}
	if p, err := url.Parse(u); err != nil || p.Host == "" {
		return ""
	}
	return u
}

func (h *handler) when(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(h.o.Location).Format("02.01.2006 15:04")
}

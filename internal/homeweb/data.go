package homeweb

import (
	"cmp"
	"context"
	"html/template"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag/diagindex"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

const (
	window = 24 * time.Hour
	// maxRows: rows per tab of «Требует внимания».
	maxRows = 5
	// maxProblems bounds the read of the active problems (the count says
	// "N+" when it is reached).
	maxProblems = 1000
	// accuracyDays is the window of the precision, the default of /knowledge.
	accuracyDays = 30
	// lowDecisions: fewer decisions make the precision unreliable (as on
	// /knowledge).
	lowDecisions = 10
	// maxRunning bounds the running tasks read to match the pending
	// automatic reviews of the queue.
	maxRunning = 100
)

// Links of the page (filters of the other pages).
const (
	failedHref    = "/runs?status=failed,interrupted&since=24h"
	postponedHref = "/runs?status=postponed&since=24h"
	breaksHref    = "/problems?impact=breaks"
)

// text is a piece of a line: plain, bold, a link, or marked bad / muted.
type text struct {
	Text, Href string
	Bold, Bad  bool
	Muted      bool
}

type kpi struct {
	ID, Label, Value string
	Sub              []text
	Spark            *spark
	Bar              *bar
}

type row struct {
	Href, Sev, SevTitle, Title, When, Project, Detail string
}

type tab struct {
	ID, Label string
	Count     string
	Checked   bool
	Rows      []row
	Empty     string // calm text when there are no rows
	Error     bool
	Foot      []text
}

type card struct {
	Href, Title, Text string
	Figs              []text
}

type pageData struct {
	Nonce     string
	CommonCSS template.CSS
	CommonJS  template.JS
	PageJS    string
	// Header is the shared top bar (webui.Nav.HeaderWith).
	Header template.HTML
	Status []text
	Live   bool
	KPIs   []kpi
	Tabs   []tab
	Cards  []card
}

// facts are the figures read from the sources; ok* = read without an error.
type facts struct {
	now, since time.Time

	usageOK bool
	totals  meter.RunStats
	hours   []meter.RunStats
	topProj *meter.RunGroup

	diagOK    bool
	summary   diagindex.Summary
	failed    []diagindex.Row
	postponed []diagindex.Row

	queueOK          bool
	running, waiting int

	probOK  bool
	active  []problems.Record
	breaks  int
	partial int

	knowOK  bool
	rules   int
	memProj int
	stats   learn.OutcomeStats

	mapOK      bool
	repos      int
	unresolved int
}

func (h *handler) build(ctx context.Context) pageData {
	f := h.read(ctx)
	d := pageData{}
	h.nav(&d, f)
	h.status(&d, f)
	h.kpis(&d, f)
	h.tabs(&d, f)
	h.cards(&d, f)
	return d
}

// read gathers the figures of the switched-on sources; a failed read is
// logged and its figures stay unknown.
func (h *handler) read(ctx context.Context) facts {
	now := h.o.Now()
	f := facts{now: now, since: now.Add(-window).Truncate(time.Minute)}
	log := h.o.Logger
	st := h.o.Store
	if h.o.Usage {
		var err1, err2, err3 error
		f.totals, err1 = st.RunTotals(ctx, meter.RunFilter{Since: f.since, Until: now})
		f.hours, err2 = st.RunsHourly(ctx, f.since, now)
		var byProj []meter.RunGroup
		byProj, err3 = st.RunsBy(ctx, meter.RunFilter{Since: f.since, Until: now}, meter.ByProject)
		if err := cmp.Or(err1, err2, err3); err != nil {
			log.Error("home: runs", "totals", err1, "hourly", err2, "projects", err3)
		} else {
			f.usageOK = true
			f.topProj = topByCost(byProj)
		}
	}
	var runningRows []diagindex.Row
	if h.o.Diag {
		var err1, err2, err3, err4 error
		f.summary, err1 = st.DiagRunSummary(ctx, f.since)
		var failed, interrupted []diagindex.Row
		failed, err2 = st.ListDiagRuns(ctx, diagindex.Filter{Since: f.since, Status: "failed", Limit: maxRows})
		interrupted, err3 = st.ListDiagRuns(ctx, diagindex.Filter{Since: f.since, Status: "interrupted", Limit: maxRows})
		f.postponed, err4 = st.ListDiagRuns(ctx, diagindex.Filter{Since: f.since, Status: "postponed", Limit: maxRows})
		if err := cmp.Or(err1, err2, err3, err4); err != nil {
			log.Error("home: diagnostics index", "summary", err1, "failed", err2, "interrupted", err3, "postponed", err4)
		} else {
			f.diagOK = true
			f.failed = append(failed, interrupted...)
			slices.SortStableFunc(f.failed, func(a, b diagindex.Row) int { return b.StartedAt.Compare(a.StartedAt) })
			if len(f.failed) > maxRows {
				f.failed = f.failed[:maxRows]
			}
		}
		if rows, err := st.ListDiagRuns(ctx, diagindex.Filter{Status: "running", Limit: maxRunning}); err == nil {
			runningRows = rows
		}
	}
	h.readQueue(ctx, &f, runningRows)
	if h.o.Problems {
		rows, err := st.ListProblems(ctx, problems.Filter{Status: problems.StatusNew, Limit: maxProblems})
		if err != nil {
			log.Error("home: problems", "err", err)
		} else {
			f.probOK = true
			slices.SortStableFunc(rows, problems.ComparePriority)
			f.active = rows
			for _, x := range rows {
				switch problems.InfoOf(x.Kind).Impact {
				case problems.ImpactBreaks:
					f.breaks++
				case problems.ImpactPartial:
					f.partial++
				}
			}
		}
	}
	rules, err1 := st.ListAllLearnings(ctx, 0)
	mem, err2 := st.ListAllMemory(ctx)
	stats, err3 := st.OutcomeStatsAll(ctx, now.AddDate(0, 0, -accuracyDays))
	if err := cmp.Or(err1, err2, err3); err != nil {
		log.Error("home: knowledge", "rules", err1, "memory", err2, "outcomes", err3)
	} else {
		f.knowOK, f.rules = true, len(rules)
		seen := map[int64]bool{}
		for _, m := range mem {
			seen[m.ProjectID] = true
		}
		f.memProj = len(seen)
		for _, s := range stats {
			f.stats.Fixed += s.Fixed
			f.stats.Dismissed += s.Dismissed
		}
	}
	if h.o.Map != nil {
		if x := h.o.Map.Index(); x != nil {
			f.mapOK, f.repos = true, len(x.Repos)
			for _, e := range x.Entries {
				if e.Status == sysmap.Unresolved {
					f.unresolved++
				}
			}
		}
	}
	return f
}

// readQueue counts the persisted queue: started jobs run; the other jobs
// and the pending automatic reviews wait, except a pending review whose task
// is running (it keeps its row while it runs, see diagweb.QueueData).
func (h *handler) readQueue(ctx context.Context, f *facts, runningRows []diagindex.Row) {
	jobs, err1 := h.o.Store.ListJobs(ctx)
	pending, err2 := h.o.Store.ListPending(ctx)
	if err := cmp.Or(err1, err2); err != nil {
		h.o.Logger.Error("home: queue", "jobs", err1, "pending", err2)
		return
	}
	f.queueOK = true
	for _, s := range jobs {
		switch {
		case s.Job.Kind == "":
			// unreadable row, dropped by the next restore
		case !s.StartedAt.IsZero():
			f.running++
		default:
			f.waiting++
		}
	}
	busy := map[string]bool{}
	for _, r := range runningRows {
		busy[strings.ToLower(r.ProjectPath)+"!"+strconv.FormatInt(r.MRIID, 10)] = true
	}
	for _, p := range pending {
		k := strings.ToLower(p.Job.ProjectPath) + "!" + strconv.FormatInt(p.Job.MRIID, 10)
		if busy[k] {
			f.running++
			delete(busy, k)
		} else {
			f.waiting++
		}
	}
}

// topByCost is the project with the largest spend (RunsBy orders by
// tokens); unattributed runs ("") do not count. Nil when nothing was spent.
func topByCost(gs []meter.RunGroup) *meter.RunGroup {
	var top *meter.RunGroup
	for i := range gs {
		g := &gs[i]
		if g.Key == "" || g.CostUSD <= 0 {
			continue
		}
		if top == nil || g.CostUSD > top.CostUSD {
			top = g
		}
	}
	return top
}

func (f facts) failedCount() int {
	return f.summary.ByStatus["failed"] + f.summary.ByStatus["interrupted"]
}

func (f facts) problemCount() string {
	s := strconv.Itoa(len(f.active))
	if len(f.active) >= maxProblems {
		s += "+"
	}
	return s
}

// nav renders the shared header: the sections this page knows are on, the
// problems badge from the problems it has just read.
func (h *handler) nav(d *pageData, f facts) {
	n := h.o.Nav
	n.UsageOff, n.DiagOff, n.ProblemsOff, n.MapOff = !h.o.Usage, !h.o.Diag, !h.o.Problems, h.o.Map == nil
	var b webui.Badge
	if f.probOK && len(f.active) > 0 {
		b = webui.Badge{Count: f.problemCount(), Bad: f.breaks > 0}
	}
	d.Header = template.HTML(n.HeaderWith(webui.SectionHome, b))
}

// status is the line under the title: the last task, tasks of the day, the
// queue.
func (h *handler) status(d *pageData, f facts) {
	var parts []text
	if f.diagOK {
		if f.summary.Last.IsZero() {
			parts = append(parts, text{Text: "Задач ещё не было"})
		} else {
			parts = append(parts, text{Text: "Последняя задача "}, text{Text: ago(f.summary.Last, f.now), Bold: true})
		}
		parts = append(parts, text{Text: " · "}, text{Text: pl(f.summary.Total, "задача", "задачи", "задач"), Bold: true}, text{Text: " за 24 ч"})
		d.Live = f.summary.Total > 0 || f.running > 0
	}
	if f.queueOK {
		if len(parts) > 0 {
			parts = append(parts, text{Text: " · "})
		}
		if f.waiting == 0 {
			parts = append(parts, text{Text: "очередь "}, text{Text: "пуста", Bold: true})
		} else {
			parts = append(parts, text{Text: strconv.Itoa(f.waiting) + " в очереди", Bold: true})
		}
		if f.running > 0 {
			parts = append(parts, text{Text: ", выполняется "}, text{Text: strconv.Itoa(f.running), Bold: true})
		}
		d.Live = d.Live || f.running > 0
	}
	d.Status = parts
}

// hourLabel is "14:00–15:00" of hour i of the window (the last one ends now).
func (h *handler) hourLabel(f facts, i int) string {
	from := f.since.Add(time.Duration(i) * time.Hour)
	to := from.Add(time.Hour)
	end := to.In(h.o.Location).Format("15:04")
	if !to.Before(f.now) {
		end = "сейчас"
	}
	return from.In(h.o.Location).Format("15:04") + "–" + end
}

func (h *handler) kpis(d *pageData, f facts) {
	if h.o.Usage {
		runs := kpi{ID: "runs", Label: "Запуски Claude за 24 ч", Value: "—"}
		cost := kpi{ID: "cost", Label: "Расход за 24 ч", Value: "—"}
		if f.usageOK {
			t := f.totals
			runs.Value, cost.Value = strconv.FormatInt(t.Runs, 10), usd(t.CostUSD)
			switch {
			case t.Failed > 0 && h.o.Diag:
				runs.Sub = []text{{Text: "с ошибкой: " + strconv.FormatInt(t.Failed, 10), Href: failedHref, Bad: true}}
			case t.Failed > 0:
				runs.Sub = []text{{Text: "с ошибкой: " + strconv.FormatInt(t.Failed, 10), Bad: true}}
			default:
				runs.Sub = []text{{Text: "без ошибок"}}
			}
			if tok := t.Usage.Total(); tok > 0 {
				cost.Sub = []text{{Text: "кэш — " + pct(int(t.Usage.CacheRead*1000/tok), 1000) + " токенов · " + short(tok) + " токенов"}}
			} else {
				cost.Sub = []text{{Text: "токенов не было"}}
			}
			n := len(f.hours)
			rv, cv := make([]float64, n), make([]float64, n)
			marks := make([]bool, n)
			rl, cl := make([]string, n), make([]string, n)
			for i, x := range f.hours {
				rv[i], cv[i], marks[i] = float64(x.Runs), x.CostUSD, x.Failed > 0
				lbl := h.hourLabel(f, i)
				rl[i] = lbl + " — " + pl(int(x.Runs), "запуск", "запуска", "запусков")
				if x.Failed > 0 {
					rl[i] += ", с ошибкой " + strconv.FormatInt(x.Failed, 10)
				}
				cl[i] = lbl + " — " + usd(x.CostUSD)
			}
			runs.Spark, cost.Spark = buildSpark(rv, marks, rl), buildSpark(cv, nil, cl)
		}
		d.KPIs = append(d.KPIs, runs, cost)
	}
	if h.o.Problems {
		k := kpi{ID: "problems", Label: "Активные проблемы", Value: "—"}
		if f.probOK {
			k.Value = f.problemCount()
			others := len(f.active) - f.breaks
			switch {
			case len(f.active) == 0:
				k.Sub = []text{{Text: "всё проверено"}}
			case f.breaks > 0:
				k.Sub = []text{{Text: strconv.Itoa(f.breaks) + " " + plural(f.breaks, "ломает", "ломают", "ломают") + " ревью или main", Href: breaksHref, Bad: true}}
				if others > 0 {
					k.Sub = append(k.Sub, text{Text: " · " + strconv.Itoa(others) + " " + plural(others, "другая", "другие", "других")})
				}
			default:
				k.Sub = []text{{Text: "ревью и main не ломают"}}
			}
			k.Bar = buildBar(
				part{"b-breaks", "ломают ревью или main: " + strconv.Itoa(f.breaks), f.breaks},
				part{"b-partial", "ревью проверило не всё: " + strconv.Itoa(f.partial), f.partial},
				part{"b-none", "не влияют на ревью: " + strconv.Itoa(len(f.active)-f.breaks-f.partial), len(f.active) - f.breaks - f.partial})
		}
		d.KPIs = append(d.KPIs, k)
	}
	k := kpi{ID: "rules", Label: "Выучено правил", Value: "—"}
	if f.knowOK {
		k.Value = strconv.Itoa(f.rules)
		decided := f.stats.Fixed + f.stats.Dismissed
		mem := "память: " + pl(f.memProj, "проект", "проекта", "проектов")
		if decided > 0 {
			acc := "точность замечаний " + pct(f.stats.Fixed, decided) + " за " + strconv.Itoa(accuracyDays) + " дн."
			if decided < lowDecisions {
				acc += " (мало решений)"
			}
			k.Sub = []text{{Text: acc + " · " + mem}}
			k.Bar = buildBar(part{"b-fixed", "исправлено по замечанию: " + strconv.Itoa(f.stats.Fixed), f.stats.Fixed},
				part{"b-dismissed", "отклонено: " + strconv.Itoa(f.stats.Dismissed), f.stats.Dismissed})
		} else {
			k.Sub = []text{{Text: "решений по замечаниям за " + strconv.Itoa(accuracyDays) + " дн. нет · " + mem}}
		}
	}
	d.KPIs = append(d.KPIs, k)
}

func impactSev(im problems.Impact) string {
	switch im {
	case problems.ImpactBreaks:
		return "error"
	case problems.ImpactNone:
		return "info"
	}
	return "warn"
}

// clock is "14:05" of t on the day of now; "вчера, 14:05" on an earlier
// day (the windows are 24 hours), "завтра, 14:05" on a later one (a retry).
func (h *handler) clock(t, now time.Time) string {
	l, n := t.In(h.o.Location), now.In(h.o.Location)
	s := l.Format("15:04")
	ld, nd := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC), time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	switch {
	case ld.Before(nd):
		s = "вчера, " + s
	case ld.After(nd):
		s = "завтра, " + s
	}
	return s
}

// taskRow is a row of a failed or postponed task: what, where, when,
// never its error or reason text.
func (h *handler) taskRow(r diagindex.Row, now time.Time) row {
	title := webui.KindLabel(core.JobKind(r.Kind))
	if r.ProjectPath != "" {
		title += " · " + r.ProjectPath
	}
	if r.MRIID > 0 {
		title += " !" + strconv.FormatInt(r.MRIID, 10)
	}
	x := row{Href: "/runs/" + url.PathEscape(r.ID), Title: title, Detail: r.MRTitle}
	switch r.Status {
	case "postponed":
		x.Sev, x.SevTitle, x.When = "warn", "отложена", h.clock(r.StartedAt, now)+" · отложена"
		if !r.RetryAt.IsZero() {
			x.Detail = strings.TrimPrefix(x.Detail+" · повтор в "+h.clock(r.RetryAt, now), " · ")
		}
	case "interrupted":
		x.Sev, x.SevTitle, x.When = "error", "прервана", h.clock(r.StartedAt, now)+" · прервана"
	default:
		x.Sev, x.SevTitle, x.When = "error", "ошибка", h.clock(r.StartedAt, now)+" · ошибка"
	}
	return x
}

func more(shown, total int) []text {
	if total > shown {
		return []text{{Text: "ещё " + strconv.Itoa(total-shown) + " не " + plural(total-shown, "показана", "показаны", "показаны"), Muted: true}}
	}
	return nil
}

func (h *handler) tabs(d *pageData, f facts) {
	if h.o.Problems {
		t := tab{ID: "problems", Label: "Проблемы", Count: "—", Empty: "Активных проблем нет — всё, что бот проверяет, проверено.", Error: !f.probOK}
		if f.probOK {
			t.Count = f.problemCount()
			for _, x := range f.active[:min(maxRows, len(f.active))] {
				info := problems.InfoOf(x.Kind)
				v := url.Values{"kind": {string(x.Kind)}}
				if x.ProjectPath != "" {
					v.Set("project", x.ProjectPath)
				}
				when := ago(x.LastSeen, f.now)
				if x.Count > 1 {
					when += " · " + pl(int(x.Count), "раз", "раза", "раз")
				}
				t.Rows = append(t.Rows, row{Href: "/problems?" + v.Encode(), Sev: impactSev(info.Impact), SevTitle: info.Impact.Label(),
					Title: info.Title, When: when, Project: x.ProjectPath, Detail: x.Subject})
			}
			if len(f.active) > 0 {
				t.Foot = append(t.Foot, text{Text: "Все активные (" + t.Count + ") →", Href: "/problems"})
				if f.breaks > 0 {
					t.Foot = append(t.Foot, text{Text: "Только ломающие (" + strconv.Itoa(f.breaks) + ") →", Href: breaksHref})
				}
				t.Foot = append(t.Foot, more(len(t.Rows), len(f.active))...)
			}
		}
		d.Tabs = append(d.Tabs, t)
	}
	if h.o.Diag {
		ft := tab{ID: "failed", Label: "Упавшие задачи", Count: "—", Empty: "За сутки ни одна задача не упала.", Error: !f.diagOK}
		pt := tab{ID: "postponed", Label: "Отложенные за сутки", Count: "—", Empty: "За сутки ни одна задача не откладывалась.", Error: !f.diagOK}
		if f.diagOK {
			nf, np := f.failedCount(), f.summary.ByStatus["postponed"]
			ft.Count, pt.Count = strconv.Itoa(nf), strconv.Itoa(np)
			for _, r := range f.failed {
				ft.Rows = append(ft.Rows, h.taskRow(r, f.now))
			}
			for _, r := range f.postponed {
				pt.Rows = append(pt.Rows, h.taskRow(r, f.now))
			}
			if nf > 0 {
				ft.Foot = append([]text{{Text: "Открыть в Диагностике →", Href: failedHref}}, more(len(ft.Rows), nf)...)
			}
			if np > 0 {
				pt.Foot = append([]text{{Text: "Открыть в Диагностике →", Href: postponedHref}}, more(len(pt.Rows), np)...)
			}
		}
		d.Tabs = append(d.Tabs, ft, pt)
	}
	if len(d.Tabs) > 0 {
		d.Tabs[0].Checked = true
	}
}

func (h *handler) cards(d *pageData, f facts) {
	if h.o.Usage {
		c := card{Href: "/usage?period=24h", Title: "Расход токенов", Text: "Токены и стоимость по дням, проектам, людям и видам задач."}
		if f.usageOK {
			c.Figs = []text{{Text: usd(f.totals.CostUSD) + " за 24 ч"}}
			if p := f.topProj; p != nil {
				c.Figs = append(c.Figs, text{Text: "больше всего " + p.Key + " — " + usd(p.CostUSD), Muted: true})
			}
		}
		d.Cards = append(d.Cards, c)
	}
	if h.o.Diag {
		c := card{Href: "/runs", Title: "Диагностика", Text: "Что бот делал в каждой задаче: таймлайн, промпты, транскрипты. Хранится 48 часов."}
		if f.diagOK {
			c.Figs = []text{{Text: pl(f.summary.Total, "задача", "задачи", "задач") + " за 24 ч"}}
			if n := f.failedCount(); n > 0 {
				c.Figs = append(c.Figs, text{Text: pl(n, "сбой", "сбоя", "сбоев"), Bad: true})
			} else {
				c.Figs = append(c.Figs, text{Text: "без сбоев", Muted: true})
			}
		}
		d.Cards = append(d.Cards, c)
	}
	if h.o.Problems {
		c := card{Href: "/problems", Title: "Проблемы проверки", Text: "Что бот не смог проверить: инструменты, доступы, обрезанный дифф, конфиг."}
		if f.probOK {
			c.Figs = []text{{Text: f.problemCount() + " " + plural(len(f.active), "активная", "активные", "активных")}}
			if f.breaks > 0 {
				c.Figs = append(c.Figs, text{Text: strconv.Itoa(f.breaks) + " " + plural(f.breaks, "ломает", "ломают", "ломают") + " ревью", Bad: true})
			}
		}
		d.Cards = append(d.Cards, c)
	}
	c := card{Href: "/knowledge", Title: "Знания бота", Text: "Правила команды, память о проектах и точность замечаний."}
	if f.knowOK {
		c.Figs = []text{{Text: pl(f.rules, "правило", "правила", "правил")}}
		if dec := f.stats.Fixed + f.stats.Dismissed; dec > 0 {
			c.Figs = append(c.Figs, text{Text: "точность " + pct(f.stats.Fixed, dec), Muted: true})
		}
	}
	d.Cards = append(d.Cards, c)
	if h.o.Map != nil {
		c := card{Href: "/map", Title: "Карта системы", Text: "Сервисы и адреса между репозиториями, контракты и неразрешённые вызовы."}
		if f.mapOK {
			c.Figs = []text{{Text: pl(f.repos, "репозиторий", "репозитория", "репозиториев")}}
			if f.unresolved > 0 {
				c.Figs = append(c.Figs, text{Text: pl(f.unresolved, "неразрешённый вызов", "неразрешённых вызова", "неразрешённых вызовов"), Muted: true})
			} else {
				c.Figs = append(c.Figs, text{Text: "все вызовы разрешены", Muted: true})
			}
		}
		d.Cards = append(d.Cards, c)
	}
}

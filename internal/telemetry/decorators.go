package telemetry

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
)

// ---------------------------------------------------------------------------
// Queue handler
// ---------------------------------------------------------------------------

// Handler wraps a queue handler: every call counts in jobs_total and
// job_duration_seconds. The status comes from diag (Run.Outcome), so it
// matches /runs; "skipped" needs the diagnostics Run in the context, so wrap
// INSIDE diag.Recorder.Handler: rec.Handler(m.Handler(h)). Without
// diagnostics a skipped job counts as ok. A panic counts as failed and is
// re-raised.
func (m *Metrics) Handler(h core.Handler) core.Handler {
	if m == nil {
		return h
	}
	return func(ctx context.Context, job core.Job) (err error) {
		start := time.Now()
		kind := meter.KindForJob(job.Kind)
		record := func(status string) {
			m.JobsTotal.With(kind, status).Inc()
			m.JobDuration.With(kind).Observe(time.Since(start).Seconds())
		}
		defer func() {
			if p := recover(); p != nil {
				record(diag.StatusFailed)
				panic(p)
			}
			record(diag.FromContext(ctx).Outcome(err))
		}()
		return h(ctx, job)
	}
}

// ---------------------------------------------------------------------------
// Claude runner
// ---------------------------------------------------------------------------

// Runner is the core.Runner decorator returned by WrapRunner. Run is
// measured; Exec passes through (tool runs are counted by the
// compat/lint checker wrappers, which know how each tool did).
type Runner struct {
	next core.Runner
	m    *Metrics
}

var _ core.Runner = (*Runner)(nil)

// WrapRunner returns r with every Claude run counted (runs, tokens, cost,
// duration, rate limits). The run kind is the meter attribution's kind.
func (m *Metrics) WrapRunner(r core.Runner) core.Runner {
	if m == nil || r == nil {
		return r
	}
	return &Runner{next: r, m: m}
}

func (d *Runner) Exec(ctx context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	return d.next.Exec(ctx, req)
}

func (d *Runner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	start := time.Now()
	res, err := d.next.Run(ctx, req)
	d.m.ObserveClaudeRun(ctx, req, res, err, time.Since(start))
	return res, err
}

// ObserveClaudeRun records one finished Claude run.
func (m *Metrics) ObserveClaudeRun(ctx context.Context, req core.RunRequest, res *core.RunResult, runErr error, wall time.Duration) {
	if m == nil {
		return
	}
	kind := meter.KindOf(ctx)
	used := meter.RunMetrics(res, runErr)
	model := RunModel(req.Model, used)
	result := ClaudeResult(runErr)
	m.ClaudeRuns.With(kind, model, result).Inc()
	m.ClaudeDuration.With(kind).Observe(wall.Seconds())
	if result == ClaudeRateLimited {
		m.ClaudeRateLimit.Inc()
	}
	if used == nil {
		return
	}
	if len(used.Models) == 0 {
		m.addTokens(kind, model, used.Usage)
		m.ClaudeCost.With(kind, model).Add(used.CostUSD)
		return
	}
	var modelCost float64
	for name, mu := range used.Models {
		nm := NormalizeModel(name)
		m.addTokens(kind, nm, mu.TokenUsage)
		m.ClaudeCost.With(kind, nm).Add(mu.CostUSD)
		modelCost += mu.CostUSD
	}
	if modelCost == 0 && used.CostUSD > 0 {
		m.ClaudeCost.With(kind, model).Add(used.CostUSD)
	}
}

func (m *Metrics) addTokens(kind, model string, u core.TokenUsage) {
	m.ClaudeTokens.With(kind, model, "input").Add(float64(u.Input))
	m.ClaudeTokens.With(kind, model, "output").Add(float64(u.Output))
	m.ClaudeTokens.With(kind, model, "cache_read").Add(float64(u.CacheRead))
	m.ClaudeTokens.With(kind, model, "cache_write").Add(float64(u.CacheWrite))
}

// ClaudeResult maps a Runner.Run error to ok | rate_limited | timeout |
// error (meter.ErrorClass, with canceled / max_turns / failed folded into
// error).
func ClaudeResult(err error) string {
	switch meter.ErrorClass(err) {
	case "":
		return ClaudeOK
	case "rate_limited":
		return ClaudeRateLimited
	case "timeout":
		return ClaudeTimeout
	default:
		return ClaudeError
	}
}

// RunModel is the model label of a run: the CLI-reported model with the
// largest share (cost, then output tokens) when the CLI gave a per-model
// breakdown, else the requested model; "" -> "default".
func RunModel(requested string, res *core.RunResult) string {
	best, bestCost, bestOut := "", -1.0, int64(-1)
	if res != nil {
		for name, mu := range res.Models {
			if mu.CostUSD > bestCost || mu.CostUSD == bestCost && (mu.Output > bestOut || mu.Output == bestOut && name < best) {
				best, bestCost, bestOut = name, mu.CostUSD, mu.Output
			}
		}
	}
	if best == "" {
		best = requested
	}
	return NormalizeModel(best)
}

// NormalizeModel makes a model id label-safe: lower case, trimmed, only
// [a-z0-9._-] kept (anything else, e.g. the "[1m]" context suffix, is
// dropped), at most 64 bytes; "" -> "default".
func NormalizeModel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexByte(s, '['); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return DefaultModel
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Limiter
// ---------------------------------------------------------------------------

type limiter struct {
	core.Limiter
	m *Metrics
}

// WrapLimiter returns l with denials counted in limit_denials_total.
// Scopes: CheckGlobal -> global (learn when the run being checked is a
// learning or memory run, by the meter attribution); CheckUser rerun ->
// user_rerun, or mr_cooldown when the per-MR cooldown alone denies it
// (found by re-checking without the cooldown, only on a denial); chat ->
// user_chat; learn -> learn.
func (m *Metrics) WrapLimiter(l core.Limiter) core.Limiter {
	if m == nil || l == nil {
		return l
	}
	return &limiter{Limiter: l, m: m}
}

func (l *limiter) CheckGlobal(ctx context.Context) (core.Decision, error) {
	dec, err := l.Limiter.CheckGlobal(ctx)
	if err == nil && !dec.Allowed {
		scope := ScopeGlobal
		if learningRun(ctx) {
			scope = ScopeLearn
		}
		l.m.LimitDenials.With(scope).Inc()
	}
	return dec, err
}

// learningRun: the run being checked is a learning or memory run (meter
// attribution); its global denials count as ScopeLearn.
func learningRun(ctx context.Context) bool {
	a, ok := meter.From(ctx)
	return ok && (a.Kind == meter.KindLearning || a.Kind == meter.KindMemory)
}

func (l *limiter) CheckUser(ctx context.Context, userID int64, username string, kind core.UsageKind, mrKey string, lim core.UserLimits) (core.Decision, error) {
	dec, err := l.Limiter.CheckUser(ctx, userID, username, kind, mrKey, lim)
	if err != nil || dec.Allowed {
		return dec, err
	}
	l.m.LimitDenials.With(l.userScope(ctx, userID, username, kind, mrKey, lim)).Inc()
	return dec, err
}

// Reserve counts a denial like the check that denied it: the per-user
// limits (re-checked only on a denial) or else the global one (learn for a
// learning run, including its per-kind cap).
func (l *limiter) Reserve(ctx context.Context, req core.ReserveRequest) (core.Decision, core.Reservation, error) {
	dec, res, err := l.Limiter.Reserve(ctx, req)
	if err != nil || dec.Allowed {
		return dec, res, err
	}
	u := req.Usage
	scope := ScopeGlobal
	if req.User != nil {
		if d, uerr := l.Limiter.CheckUser(ctx, u.UserID, req.Username, u.Kind, u.MRKey, *req.User); uerr == nil && !d.Allowed {
			scope = l.userScope(ctx, u.UserID, req.Username, u.Kind, u.MRKey, *req.User)
		}
	}
	// A learning booking (global limit or review.learn.max_per_hour) counts
	// as learn whatever job it runs in.
	if scope == ScopeGlobal && (learningRun(ctx) || u.Kind == core.UsageLearn) {
		scope = ScopeLearn
	}
	l.m.LimitDenials.With(scope).Inc()
	return dec, res, err
}

// userScope is the metric scope of a per-user denial.
func (l *limiter) userScope(ctx context.Context, userID int64, username string, kind core.UsageKind, mrKey string, lim core.UserLimits) string {
	scope := ScopeGlobal
	switch kind {
	case core.UsageRerun:
		scope = ScopeUserRerun
		if lim.MRRerunCooldown > 0 {
			noCD := lim
			noCD.MRRerunCooldown = 0
			if d2, err2 := l.Limiter.CheckUser(ctx, userID, username, kind, mrKey, noCD); err2 == nil && d2.Allowed {
				scope = ScopeMRCooldown
			}
		}
	case core.UsageChat:
		scope = ScopeUserChat
	case core.UsageLearn:
		scope = ScopeLearn
	}
	return scope
}

// ---------------------------------------------------------------------------
// Compat and lint checkers
// ---------------------------------------------------------------------------

// CompatChecker is compatcheck.Checker (implemented by *compat.Checker).
type CompatChecker interface {
	Check(ctx context.Context, req compat.Request) (*compat.Result, error)
}

// LintChecker is linters.Checker (implemented by *lint.Checker).
type LintChecker interface {
	Check(ctx context.Context, req lint.Request) (*lint.Result, error)
}

type compatChecker struct {
	next CompatChecker
	m    *Metrics
}

// WrapCompat returns c with every tool counted in tool_runs_total.
func (m *Metrics) WrapCompat(c CompatChecker) CompatChecker {
	if m == nil || c == nil {
		return c
	}
	return &compatChecker{next: c, m: m}
}

func (c *compatChecker) Check(ctx context.Context, req compat.Request) (*compat.Result, error) {
	res, err := c.next.Check(ctx, req)
	if err == nil && res != nil {
		for tool, result := range CompatToolResults(req, res) {
			c.m.ToolRuns.With(tool, result).Inc()
		}
	}
	return res, err
}

// CompatToolResults classifies one compat check per source (tool). The
// tools run are derived from the checked specs (oasdiff per OpenAPI spec,
// buf once for all proto files, graphql-inspector per GraphQL spec). Per
// tool: any "unavailable" issue -> unavailable; failed/skipped subjects ->
// failed when nothing was checked, partial otherwise; else findings when it
// reported breaking changes, ok otherwise.
func CompatToolResults(req compat.Request, res *compat.Result) map[string]string {
	subjects := map[string]int{}
	for _, s := range res.Specs {
		switch s.Kind {
		case compat.KindOpenAPI:
			subjects[compat.SourceOasdiff]++
		case compat.KindProto:
			subjects[compat.SourceBuf] = 1
		case compat.KindGraphQL:
			subjects[compat.SourceGraphQL]++
		}
	}
	unavailable := map[string]bool{}
	bad := map[string]map[string]bool{}
	for _, is := range res.Issues {
		if _, ok := subjects[is.Source]; !ok {
			subjects[is.Source] = 1 // an issue proves the tool was attempted
		}
		switch is.Kind {
		case compat.IssueUnavailable:
			unavailable[is.Source] = true
		default:
			if bad[is.Source] == nil {
				bad[is.Source] = map[string]bool{}
			}
			bad[is.Source][is.Path] = true
		}
	}
	changes := map[string]int{}
	for _, ch := range res.Changes {
		changes[ch.Source]++
		if _, ok := subjects[ch.Source]; !ok {
			subjects[ch.Source] = 1
		}
	}
	out := map[string]string{}
	for tool, n := range subjects {
		switch {
		case unavailable[tool]:
			out[tool] = ToolUnavailable
		case len(bad[tool]) > 0 && len(bad[tool]) >= n && changes[tool] == 0:
			out[tool] = ToolFailed
		case len(bad[tool]) > 0:
			out[tool] = ToolPartial
		case changes[tool] > 0:
			out[tool] = ToolFindings
		default:
			out[tool] = ToolOK
		}
	}
	return out
}

type lintChecker struct {
	next LintChecker
	m    *Metrics
}

// WrapLint returns c with every tool counted in tool_runs_total.
func (m *Metrics) WrapLint(c LintChecker) LintChecker {
	if m == nil || c == nil {
		return c
	}
	return &lintChecker{next: c, m: m}
}

func (c *lintChecker) Check(ctx context.Context, req lint.Request) (*lint.Result, error) {
	res, err := c.next.Check(ctx, req)
	if err == nil && res != nil {
		for _, st := range res.Tools {
			if r := LintToolResult(st); r != "" {
				c.m.ToolRuns.With(st.Tool, r).Inc()
			}
		}
	}
	return res, err
}

// LintToolResult maps a lint tool status to a tool_runs_total result
// ("" for a tool that had nothing to check: not counted).
func LintToolResult(st lint.ToolStatus) string {
	switch st.State {
	case lint.StateRan:
		if st.Found > 0 {
			return ToolFindings
		}
		return ToolOK
	case lint.StateUnavailable:
		return ToolUnavailable
	case lint.StateFailed:
		return ToolFailed
	case lint.StatePartial:
		return ToolPartial
	case lint.StateNotApplicable:
		return ""
	default:
		return ToolFailed
	}
}

// ---------------------------------------------------------------------------
// GitLab
// ---------------------------------------------------------------------------

// GitLabObserver returns the observer for gitlab.Client.SetObserver: every
// HTTP attempt (retries included) counts in gitlab_requests_total.
func (m *Metrics) GitLabObserver() func(method string, status int) {
	if m == nil {
		return nil
	}
	return func(method string, status int) {
		m.GitLabRequests.With(normMethod(method), StatusCodeLabel(status)).Inc()
	}
}

// StatusCodeLabel: exact code for 403, 404, 429 and every 5xx; the class
// (2xx, 3xx, 4xx, 1xx) otherwise; "error" when there was no response.
func StatusCodeLabel(status int) string {
	switch {
	case status <= 0:
		return "error"
	case status == 403 || status == 404 || status == 429 || status >= 500 && status <= 599:
		return fmt.Sprint(status)
	case status >= 100 && status <= 499:
		return fmt.Sprintf("%dxx", status/100)
	default:
		return "other"
	}
}

func normMethod(m string) string {
	switch m = strings.ToUpper(m); m {
	case "GET", "POST", "PUT", "DELETE", "PATCH", "HEAD":
		return m
	default:
		return "OTHER"
	}
}

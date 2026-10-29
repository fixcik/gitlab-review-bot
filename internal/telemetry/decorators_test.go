package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/limits"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// scrape renders the registry.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	var b strings.Builder
	if err := m.Registry.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// has fails unless the exposition contains the exact sample line.
func has(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(out, "\n"+l+"\n") {
			t.Errorf("missing %q", l)
		}
	}
	if t.Failed() {
		t.Logf("exposition:\n%s", out)
	}
}

func hasNot(t *testing.T, out string, prefixes ...string) {
	t.Helper()
	for _, p := range prefixes {
		if strings.Contains(out, "\n"+p) {
			t.Errorf("unexpected %q", p)
		}
	}
}

// fakeRunner returns scripted Claude results (never calls the real CLI).
type fakeRunner struct {
	runs  []func() (*core.RunResult, error)
	execs map[string]core.ExecResult // argv[0] -> result
	n     atomic.Int32
}

func (f *fakeRunner) Exec(_ context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	r := f.execs[req.Command[0]]
	return &r, nil
}
func (f *fakeRunner) Run(context.Context, core.RunRequest) (*core.RunResult, error) {
	i := int(f.n.Add(1)) - 1
	return f.runs[i]()
}

type fakeGitLabServer struct {
	*httptest.Server
	hits atomic.Int32
}

// newGitLab serves GET /user and GET MR (the first MR request answers 502
// once, to exercise retries) and 404 for anything else.
func newGitLab(t *testing.T) (*fakeGitLabServer, *gitlab.Client) {
	t.Helper()
	s := &fakeGitLabServer{}
	var mrCalls atomic.Int32
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		switch {
		case r.URL.Path == "/api/v4/user":
			fmt.Fprint(w, `{"id":99,"username":"reviewbot"}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/7"):
			if mrCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			fmt.Fprint(w, `{"iid":7,"project_id":42,"title":"feat: x","state":"opened","author":{"username":"alice"},"diff_refs":{"head_sha":"abc"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	c, err := gitlab.New(s.URL, "glpat-test-token-123456", gitlab.WithRetry(2, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func TestReviewJobThroughDecorators(t *testing.T) {
	m := New("1.2.3")
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rec, err := diag.New(diag.Options{Root: filepath.Join(t.TempDir(), "diag"), Index: st})
	if err != nil {
		t.Fatal(err)
	}
	_, gl := newGitLab(t)
	gl.SetObserver(m.GitLabObserver())

	lim := m.WrapLimiter(limits.New(st, 1, nil, time.Now)) // global: 1 run/hour
	fr := &fakeRunner{
		runs: []func() (*core.RunResult, error){
			func() (*core.RunResult, error) { // the review run
				return &core.RunResult{CostUSD: 0.5, Usage: core.TokenUsage{Input: 110, Output: 20, CacheRead: 1000, CacheWrite: 50},
					Models: map[string]core.ModelUsage{
						"claude-sonnet-4-5-20250929": {TokenUsage: core.TokenUsage{Input: 100, Output: 15, CacheRead: 1000, CacheWrite: 50}, CostUSD: 0.45},
						"claude-haiku-4-5[1m]":       {TokenUsage: core.TokenUsage{Input: 10, Output: 5}, CostUSD: 0.05},
					}}, nil
			},
			func() (*core.RunResult, error) { // rate limited, partial usage
				return nil, &core.FailedRun{Err: &core.ErrClaudeRateLimited{Message: "limit"},
					Partial: core.RunResult{CostUSD: 0.01, Usage: core.TokenUsage{Input: 7}}}
			},
		},
		execs: map[string]core.ExecResult{
			"gitleaks": {ExitCode: 1, Stdout: `[]`},
		},
	}
	runner := m.WrapRunner(fr)

	review := func(ctx context.Context, job core.Job) error {
		ctx = meter.With(ctx, meter.ForJob(job))
		if _, err := gl.GetMR(ctx, job.ProjectID, job.MRIID); err != nil {
			return err
		}
		dec, err := lim.CheckUser(ctx, job.UserID, job.Username, core.UsageRerun, job.MRKey(), core.UserLimits{RerunsPerHour: 5})
		if err != nil || !dec.Allowed {
			return fmt.Errorf("user limit: %v %v", dec, err)
		}
		if dec, err = lim.CheckGlobal(ctx); err != nil || !dec.Allowed {
			return &core.RetryLater{At: dec.RetryAt, Reason: dec.Reason}
		}
		if err := lim.Record(ctx, core.Usage{UserID: job.UserID, Kind: core.UsageRerun, MRKey: job.MRKey()}); err != nil {
			return err
		}
		if _, err := runner.Run(ctx, core.RunRequest{Model: "sonnet"}); err != nil {
			return err
		}
		// Verification run: rate limited.
		meter.Update(ctx, func(a *meter.Attribution) { a.Kind = meter.KindVerification })
		if _, err := runner.Run(ctx, core.RunRequest{Model: "opus"}); err != nil {
			var rl *core.ErrClaudeRateLimited
			if !errors.As(err, &rl) {
				return err
			}
		}
		return nil
	}
	h := rec.Handler(m.Handler(review))
	job := core.Job{Kind: core.JobReview, ProjectID: 42, MRIID: 7, UserID: 11, Username: "bob"}
	if err := h(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	// Second job: the global limit (1/hour) postpones it; the user limit
	// still allows.
	if err := h(context.Background(), job); err == nil {
		t.Fatal("want RetryLater")
	}
	// A job that fails and a skipped one.
	_ = rec.Handler(m.Handler(func(context.Context, core.Job) error { return errors.New("boom") }))(context.Background(), core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 1})
	logger := rec.Logger(slog.New(slog.DiscardHandler))
	_ = rec.Handler(m.Handler(func(ctx context.Context, j core.Job) error {
		logger.With("mr", j.MRKey()).Info("review: draft mr, skipping")
		return nil
	}))(context.Background(), core.Job{Kind: core.JobAutoReview, ProjectID: 2, MRIID: 2})

	out := scrape(t, m)
	has(t, out,
		`reviewbot_jobs_total{kind="review",status="ok"} 1`,
		`reviewbot_jobs_total{kind="review",status="postponed"} 1`,
		`reviewbot_jobs_total{kind="review",status="skipped"} 1`,
		`reviewbot_jobs_total{kind="chat",status="failed"} 1`,
		`reviewbot_job_duration_seconds_count{kind="review"} 3`,
		`reviewbot_claude_runs_total{kind="review",model="claude-sonnet-4-5-20250929",result="ok"} 1`,
		`reviewbot_claude_runs_total{kind="verification",model="opus",result="rate_limited"} 1`,
		`reviewbot_claude_rate_limited_total 1`,
		`reviewbot_claude_tokens_total{kind="review",model="claude-sonnet-4-5-20250929",type="input"} 100`,
		`reviewbot_claude_tokens_total{kind="review",model="claude-sonnet-4-5-20250929",type="cache_read"} 1000`,
		`reviewbot_claude_tokens_total{kind="review",model="claude-sonnet-4-5-20250929",type="cache_write"} 50`,
		`reviewbot_claude_tokens_total{kind="review",model="claude-haiku-4-5",type="output"} 5`,
		`reviewbot_claude_tokens_total{kind="verification",model="opus",type="input"} 7`,
		`reviewbot_claude_cost_usd_total{kind="review",model="claude-sonnet-4-5-20250929"} 0.45`,
		`reviewbot_claude_cost_usd_total{kind="review",model="claude-haiku-4-5"} 0.05`,
		`reviewbot_claude_cost_usd_total{kind="verification",model="opus"} 0.01`,
		`reviewbot_claude_run_duration_seconds_count{kind="review"} 1`,
		`reviewbot_claude_run_duration_seconds_count{kind="verification"} 1`,
		`reviewbot_limit_denials_total{scope="global"} 1`,
		// 502 then 200 for the first GetMR (retry counted), 200 for the second.
		`reviewbot_gitlab_requests_total{method="GET",code="502"} 1`,
		`reviewbot_gitlab_requests_total{method="GET",code="2xx"} 2`,
		`reviewbot_build_info{version="1.2.3",go_version="`+runtime.Version()+`"} 1`,
	)
	for _, p := range []string{"go_goroutines ", "go_memstats_alloc_bytes ", "go_memstats_sys_bytes ", "process_start_time_seconds "} {
		if !strings.Contains(out, "\n"+p) {
			t.Errorf("runtime metric %s missing", p)
		}
	}
	hasNot(t, out, `reviewbot_limit_denials_total{scope="user_rerun"}`)
}

func TestLimiterScopes(t *testing.T) {
	m := New("")
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	lim := m.WrapLimiter(limits.New(st, 0, nil, time.Now))
	_ = lim.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageRerun, MRKey: "1!1"})
	_ = lim.Record(ctx, core.Usage{UserID: 1, Kind: core.UsageChat, MRKey: "1!1"})

	// Cooldown alone denies (per-user hourly limit is fine).
	if d, _ := lim.CheckUser(ctx, 1, "u", core.UsageRerun, "1!1", core.UserLimits{RerunsPerHour: 5, MRRerunCooldown: time.Hour}); d.Allowed {
		t.Fatal("cooldown should deny")
	}
	// Hourly limit denies.
	if d, _ := lim.CheckUser(ctx, 1, "u", core.UsageRerun, "2!2", core.UserLimits{RerunsPerHour: 1, MRRerunCooldown: time.Hour}); d.Allowed {
		t.Fatal("hourly rerun should deny")
	}
	if d, _ := lim.CheckUser(ctx, 1, "u", core.UsageChat, "1!1", core.UserLimits{ChatPerHour: 1}); d.Allowed {
		t.Fatal("chat should deny")
	}
	if d, _ := lim.CheckUser(ctx, 1, "u", core.UsageChat, "1!1", core.UserLimits{ChatPerHour: 5}); !d.Allowed {
		t.Fatal("chat should allow")
	}
	// Global denial while a learning run is being checked -> learn.
	g := m.WrapLimiter(limits.New(st, 1, nil, time.Now))
	lctx := meter.With(ctx, meter.Attribution{Kind: meter.KindLearning})
	if d, _ := g.CheckGlobal(lctx); d.Allowed {
		t.Fatal("global should deny")
	}
	if d, _ := g.CheckGlobal(ctx); d.Allowed {
		t.Fatal("global should deny")
	}
	has(t, scrape(t, m),
		`reviewbot_limit_denials_total{scope="mr_cooldown"} 1`,
		`reviewbot_limit_denials_total{scope="user_rerun"} 1`,
		`reviewbot_limit_denials_total{scope="user_chat"} 1`,
		`reviewbot_limit_denials_total{scope="learn"} 1`,
		`reviewbot_limit_denials_total{scope="global"} 1`,
	)
}

// A learning booking denied by review.learn.max_per_hour (the per-kind
// cap) counts as learn even inside a review job (memory compaction).
func TestLimitDenialLearnCap(t *testing.T) {
	m := New("")
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := meter.With(context.Background(), meter.Attribution{Kind: meter.KindReview})
	lim := m.WrapLimiter(limits.New(st, 0, nil, time.Now))
	_ = lim.Record(ctx, core.Usage{Kind: core.UsageLearn, MRKey: "1!1"})
	req := core.ReserveRequest{Usage: core.Usage{Kind: core.UsageLearn, MRKey: "1!1"}, Global: true, KindPerHour: 1}
	if d, _, _ := lim.Reserve(ctx, req); d.Allowed {
		t.Fatal("learn cap should deny")
	}
	has(t, scrape(t, m), `reviewbot_limit_denials_total{scope="learn"} 1`)
}

func TestToolRuns(t *testing.T) {
	m := New("")
	ctx := context.Background()
	// Lint: statuses as the checker reports them (gitleaks is the only tool
	// the bot runs; one run per review).
	for _, st := range []lint.ToolStatus{
		{Tool: lint.ToolGitleaks, State: lint.StateRan, Found: 2},
		{Tool: lint.ToolGitleaks, State: lint.StateUnavailable},
		{Tool: lint.ToolGitleaks, State: lint.StateRan},
		{Tool: lint.ToolGitleaks, State: lint.StateNotApplicable},
		{Tool: lint.ToolGitleaks, State: lint.StatePartial},
	} {
		lc := m.WrapLint(lintFunc(func(context.Context, lint.Request) (*lint.Result, error) {
			return &lint.Result{Tools: []lint.ToolStatus{st}}, nil
		}))
		if _, err := lc.Check(ctx, lint.Request{}); err != nil {
			t.Fatal(err)
		}
	}
	// Compat: two OpenAPI specs (one failed), proto with a change, GraphQL
	// tool missing.
	cc := m.WrapCompat(compatFunc(func(context.Context, compat.Request) (*compat.Result, error) {
		return &compat.Result{
			Specs: []compat.Spec{{Kind: compat.KindOpenAPI, Path: "a.yaml"}, {Kind: compat.KindOpenAPI, Path: "b.yaml"},
				{Kind: compat.KindProto, Path: "x.proto"}, {Kind: compat.KindGraphQL, Path: "s.graphql"}},
			Changes: []compat.Change{{Source: compat.SourceBuf, Path: "x.proto"}},
			Issues: []compat.Issue{{Source: compat.SourceOasdiff, Path: "b.yaml", Kind: compat.IssueFailed},
				{Source: compat.SourceGraphQL, Path: "s.graphql", Kind: compat.IssueUnavailable}},
		}, nil
	}))
	if _, err := cc.Check(ctx, compat.Request{}); err != nil {
		t.Fatal(err)
	}
	// Checker error: nothing counted.
	if _, err := m.WrapLint(lintFunc(func(context.Context, lint.Request) (*lint.Result, error) {
		return nil, errors.New("git failed")
	})).Check(ctx, lint.Request{}); err == nil {
		t.Fatal("want error")
	}
	out := scrape(t, m)
	has(t, out,
		`reviewbot_tool_runs_total{tool="gitleaks",result="findings"} 1`,
		`reviewbot_tool_runs_total{tool="gitleaks",result="unavailable"} 1`,
		`reviewbot_tool_runs_total{tool="gitleaks",result="ok"} 1`,
		`reviewbot_tool_runs_total{tool="gitleaks",result="partial"} 1`,
		`reviewbot_tool_runs_total{tool="oasdiff",result="partial"} 1`,
		`reviewbot_tool_runs_total{tool="buf",result="findings"} 1`,
		`reviewbot_tool_runs_total{tool="graphql-inspector",result="unavailable"} 1`,
	)
	hasNot(t, out, `reviewbot_tool_runs_total{tool="gitleaks",result=""`, `reviewbot_tool_runs_total{tool="custom"`)
}

func TestCompatAllSubjectsFailed(t *testing.T) {
	got := CompatToolResults(compat.Request{}, &compat.Result{
		Specs:  []compat.Spec{{Kind: compat.KindOpenAPI, Path: "a.yaml"}},
		Issues: []compat.Issue{{Source: compat.SourceOasdiff, Path: "a.yaml", Kind: compat.IssueFailed}},
	})
	if got[compat.SourceOasdiff] != ToolFailed || len(got) != 1 {
		t.Fatalf("%v", got)
	}
}

func TestWebhookEvents(t *testing.T) {
	m := New("")
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("case") {
		case "405":
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		case "401":
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case "400":
			http.Error(w, "malformed json", http.StatusBadRequest)
		case "413":
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		case "accepted":
			w.Write([]byte("accepted\n"))
		default:
			w.Write([]byte("ignored: draft\n"))
		}
	})
	h := m.WrapWebhook(inner)
	for _, c := range []struct{ q, event string }{
		{"405", ""}, {"401", "Note Hook"}, {"400", "Merge Request Hook"}, {"413", "Note Hook"},
		{"accepted", "Merge Request Hook"}, {"accepted", "Merge Request Hook"}, {"accepted", "Note Hook"},
		{"ignored", "Note Hook"}, {"ignored", "Pipeline Hook"}, {"ignored", "Push Hook"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/webhook?case="+c.q, nil)
		r.Header.Set("X-Gitlab-Event", c.event)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
	}
	has(t, scrape(t, m),
		`reviewbot_webhook_events_total{event="other",result="method_not_allowed"} 1`,
		`reviewbot_webhook_events_total{event="note",result="unauthorized"} 1`,
		`reviewbot_webhook_events_total{event="merge_request",result="bad_request"} 1`,
		`reviewbot_webhook_events_total{event="note",result="bad_request"} 1`,
		`reviewbot_webhook_events_total{event="merge_request",result="accepted"} 2`,
		`reviewbot_webhook_events_total{event="note",result="accepted"} 1`,
		`reviewbot_webhook_events_total{event="note",result="ignored"} 1`,
		`reviewbot_webhook_events_total{event="other",result="ignored"} 1`,
		`reviewbot_webhook_events_total{event="pipeline",result="ignored"} 1`,
	)
}

func TestNilMetricsIsNoop(t *testing.T) {
	var m *Metrics
	r := &fakeRunner{}
	if m.WrapRunner(r) != core.Runner(r) || m.Handler(nil) != nil || m.WrapLimiter(nil) != nil || m.GitLabObserver() != nil {
		t.Fatal("nil metrics must pass through")
	}
	h := http.NotFoundHandler()
	if fmt.Sprint(m.WrapWebhook(h)) != fmt.Sprint(h) {
		t.Fatal("webhook passthrough")
	}
	m.ObserveQueue(nil)
	m.ObserveClaudeRun(context.Background(), core.RunRequest{}, nil, nil, 0)
}

func TestModelLabels(t *testing.T) {
	for in, want := range map[string]string{
		"":                           "default",
		"  Claude-Sonnet-4-5 ":       "claude-sonnet-4-5",
		"claude-opus-4-1[1m]":        "claude-opus-4-1",
		`x"y{z}`:                     "xyz",
		strings.Repeat("a", 100):     strings.Repeat("a", 64),
		"claude-haiku-4-5-20251001":  "claude-haiku-4-5-20251001",
		"us.anthropic.claude-3-7-v1": "us.anthropic.claude-3-7-v1",
	} {
		if got := NormalizeModel(in); got != want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", in, got, want)
		}
	}
	res := &core.RunResult{Models: map[string]core.ModelUsage{"b": {CostUSD: 1}, "a": {CostUSD: 2}}}
	if RunModel("sonnet", res) != "a" || RunModel("sonnet", nil) != "sonnet" || RunModel("", nil) != "default" {
		t.Fatal("RunModel")
	}
	for err, want := range map[error]string{
		nil:                                   ClaudeOK,
		context.DeadlineExceeded:              ClaudeTimeout,
		&core.ErrClaudeRateLimited{}:          ClaudeRateLimited,
		errors.New("x"):                       ClaudeError,
		fmt.Errorf("w: %w", context.Canceled): ClaudeError,
	} {
		if got := ClaudeResult(err); got != want {
			t.Errorf("ClaudeResult(%v) = %s", err, got)
		}
	}
	for code, want := range map[int]string{0: "error", 200: "2xx", 201: "2xx", 304: "3xx", 400: "4xx", 403: "403", 404: "404", 429: "429", 500: "500", 503: "503"} {
		if got := StatusCodeLabel(code); got != want {
			t.Errorf("StatusCodeLabel(%d) = %s", code, got)
		}
	}
}

type lintFunc func(context.Context, lint.Request) (*lint.Result, error)

func (f lintFunc) Check(ctx context.Context, r lint.Request) (*lint.Result, error) { return f(ctx, r) }

type compatFunc func(context.Context, compat.Request) (*compat.Result, error)

func (f compatFunc) Check(ctx context.Context, r compat.Request) (*compat.Result, error) {
	return f(ctx, r)
}

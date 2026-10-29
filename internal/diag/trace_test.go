package diag

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

type spanSink struct {
	mu    sync.Mutex
	spans []*tracing.SpanData
}

func (s *spanSink) Enqueue(d *tracing.SpanData) {
	s.mu.Lock()
	s.spans = append(s.spans, d)
	s.mu.Unlock()
}

func (s *spanSink) byName(t *testing.T, name string) *tracing.SpanData {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.spans {
		if d.Name == name {
			return d
		}
	}
	var names []string
	for _, d := range s.spans {
		names = append(names, d.Name)
	}
	t.Fatalf("no span %q in %v", name, names)
	return nil
}

func attr(d *tracing.SpanData, k string) string {
	v, ok := d.AttrValue(k)
	if !ok {
		return "<unset>"
	}
	return v.AsString()
}

// ctxGitLab records the span context each call receives.
type ctxGitLab struct {
	fakeGitLab
	mu  sync.Mutex
	got map[string]tracing.SpanContext
}

func (g *ctxGitLab) seen(op string, ctx context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.got == nil {
		g.got = map[string]tracing.SpanContext{}
	}
	g.got[op] = tracing.SpanFromContext(ctx).SpanContext()
}

func (g *ctxGitLab) GetMR(ctx context.Context, p, iid int64) (*core.MergeRequest, error) {
	g.seen("get_mr", ctx)
	return g.fakeGitLab.GetMR(ctx, p, iid)
}

func (g *ctxGitLab) GetRawFile(ctx context.Context, _ int64, _, _ string) ([]byte, error) {
	g.seen("get_raw_file", ctx)
	return nil, errors.New("GET https://gitlab.example.com/api/v4/x?private_token=" + glToken + ": connection reset")
}

// spyRunner returns a fixed Claude result and records the traceparent the
// runner saw (what the CLI would get as TRACEPARENT).
type spyRunner struct {
	mu          sync.Mutex
	traceparent string
}

func (r *spyRunner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	r.mu.Lock()
	r.traceparent = tracing.TraceParent(ctx)
	r.mu.Unlock()
	return &core.RunResult{Output: "{\"summary\":\"secret code here\"}", CostUSD: 0.42, NumTurns: 3, Duration: 2 * time.Second,
		Usage:  core.TokenUsage{Input: 100, Output: 50, CacheRead: 1000, CacheWrite: 10},
		Models: map[string]core.ModelUsage{"claude-sonnet-4-5": {}}}, nil
}

func (r *spyRunner) Exec(_ context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	return &core.ExecResult{ExitCode: 1, Stdout: "finding with " + glToken}, nil
}

// fakeReview is a job handler that does what the review flow does, through
// the decorated dependencies.
func fakeReview(gl core.GitLab, lim core.Limiter, repos core.Repos, run core.Runner, result error) core.Handler {
	return func(ctx context.Context, job core.Job) error {
		mr, err := gl.GetMR(ctx, job.ProjectID, job.MRIID)
		if err != nil {
			return err
		}
		_, _ = gl.GetRawFile(ctx, job.ProjectID, ".reviewbot.yaml", "main")
		co, err := repos.Checkout(ctx, job.ProjectPath, mr)
		if err != nil {
			return err
		}
		if _, err := co.Diff(ctx, mr.DiffRefs.BaseSHA, nil); err != nil {
			return err
		}
		if _, _, err := lim.Reserve(ctx, core.ReserveRequest{Usage: core.Usage{Kind: core.UsageAuto}, Global: true}); err != nil {
			return err
		}
		_, _ = run.Exec(ctx, core.ExecRequest{Command: []string{"/usr/bin/gitleaks", "detect", "--source", "/work"}})
		ctx = meter.With(ctx, meter.Attribution{Kind: meter.KindReview})
		if _, err := run.Run(ctx, core.RunRequest{Model: "sonnet", Prompt: "review this secret code", MCP: &core.MCPServer{URL: "http://mcp", Token: "mcp-token-123456"}}); err != nil {
			return err
		}
		if _, err := gl.CreateDraftNote(ctx, job.ProjectID, job.MRIID, "<!-- reviewbot:finding id=1 -->\nnote body with code", nil); err != nil {
			return err
		}
		end := Step(ctx, "review.publish")
		end(StepOK, Fields{"drafts": 1, "files": []string{"secret/path.go"}})
		return result
	}
}

func newTraced(t *testing.T) (*tracing.Tracer, *spanSink) {
	sink := &spanSink{}
	return tracing.New(tracing.Options{Exporter: sink, Scrub: ScrubFunc([]string{glToken})}), sink
}

// TestTraceWithoutDiagnostics: tracing on, diagnostics off (nil Recorder):
// the decorators still produce the span tree.
func TestTraceWithoutDiagnostics(t *testing.T) {
	tr, sink := newTraced(t)
	h := Hooks{Trace: true}
	gl := &ctxGitLab{}
	runner := &spyRunner{}
	handler := TraceJobs(tr, &tracing.Active{}, fakeReview(h.WrapGitLab(gl), h.WrapLimiter(fakeLimiter{}),
		h.WrapRepos(fakeRepos{dir: t.TempDir()}), h.WrapRunner(runner), nil))

	event := tracing.SpanContext{TraceID: tracing.TraceID{9}, SpanID: tracing.SpanID{8}, Sampled: true}
	job := core.Job{Kind: core.JobAutoReview, ProjectID: 42, ProjectPath: "group/app", MRIID: 7, HeadSHA: "abc1234",
		EnqueuedAt: time.Now().Add(-time.Second), TraceParent: event.TraceParent()}
	if err := handler(context.Background(), job); err != nil {
		t.Fatal(err)
	}

	root := sink.byName(t, "review.job")
	if root.Kind != tracing.KindConsumer || root.Parent.IsValid() {
		t.Fatalf("root: kind %v parent %v", root.Kind, root.Parent)
	}
	for k, want := range map[string]string{"gitlab.project": "group/app", "gitlab.mr.iid": "7", "job.kind": "auto_review",
		"job.manual": "false", "vcs.head_sha": "0123456789abcdef0123456789abcdef01234567", AttrJobResult: StatusOK, "gitlab.mr.state": "opened"} {
		if got := attr(root, k); got != want {
			t.Errorf("root %s = %q, want %q", k, got, want)
		}
	}
	if root.Status != tracing.StatusOK {
		t.Errorf("root status %v", root.Status)
	}
	if len(root.Links) != 1 || root.Links[0].SpanContext.SpanID != event.SpanID {
		t.Errorf("root not linked to the event: %+v", root.Links)
	}
	tid := root.SpanContext.TraceID
	for _, name := range []string{"gitlab.get_mr", "gitlab.get_raw_file", "git.checkout", "git.diff", "limiter.reserve",
		"exec gitleaks", "claude review", "gitlab.create_draft_note", "review.publish"} {
		d := sink.byName(t, name)
		if d.SpanContext.TraceID != tid || d.Parent != root.SpanContext.SpanID {
			t.Errorf("%s is not a child of the job span", name)
		}
	}
	// The wrapped call runs under its step span (HTTP spans nest there).
	if gl.got["get_mr"].SpanID != sink.byName(t, "gitlab.get_mr").SpanContext.SpanID {
		t.Error("GetMR did not receive its step span in ctx")
	}

	cl := sink.byName(t, "claude review")
	for k, want := range map[string]string{"gen_ai.system": "anthropic", "gen_ai.request.model": "sonnet",
		"gen_ai.response.model": "claude-sonnet-4-5", "gen_ai.usage.input_tokens": "1110", "gen_ai.usage.output_tokens": "50",
		"gen_ai.usage.cache_read.input_tokens": "1000", "reviewbot.claude.cost_usd": "0.42", "reviewbot.claude.kind": "review",
		"reviewbot.claude.turns": "3", "reviewbot.claude.mcp": "true"} {
		if got := attr(cl, k); got != want {
			t.Errorf("claude %s = %q, want %q", k, got, want)
		}
	}
	if cl.Kind != tracing.KindClient {
		t.Errorf("claude kind %v", cl.Kind)
	}
	if want := cl.SpanContext.TraceParent(); runner.traceparent != want {
		t.Errorf("runner saw traceparent %q, want the claude span %q", runner.traceparent, want)
	}

	if d := sink.byName(t, "limiter.reserve"); attr(d, "reviewbot.limiter.allowed") != "true" {
		t.Errorf("limiter allowed = %s", attr(d, "reviewbot.limiter.allowed"))
	}
	ex := sink.byName(t, "exec gitleaks")
	if attr(ex, "process.exit.code") != "1" || attr(ex, "process.executable.name") != "gitleaks" {
		t.Errorf("exec attrs: %v", ex.Attrs)
	}
	if d := sink.byName(t, "gitlab.create_draft_note"); attr(d, "reviewbot.marker") != "finding" || attr(d, "reviewbot.draft_note_id") != "101" {
		t.Errorf("draft note attrs: %v", d.Attrs)
	}
	if d := sink.byName(t, "review.publish"); attr(d, "reviewbot.drafts") != "1" || attr(d, "reviewbot.files") != "<unset>" {
		t.Errorf("step attrs: %v", d.Attrs)
	}
	rf := sink.byName(t, "gitlab.get_raw_file")
	if rf.Status != tracing.StatusError {
		t.Errorf("failed call status %v", rf.Status)
	}
	assertNoLeak(t, sink)
}

// assertNoLeak: no span carries secrets, prompt text, code, note bodies,
// file paths, argv or MCP tokens.
func assertNoLeak(t *testing.T, sink *spanSink) {
	t.Helper()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	check := func(where, v string) {
		for _, bad := range []string{glToken, "private_token", "secret code", "note body", "secret/path.go", ".reviewbot.yaml",
			"--source", "mcp-token", "review this"} {
			if strings.Contains(v, bad) {
				t.Errorf("%s leaks %q: %q", where, bad, v)
			}
		}
	}
	for _, d := range sink.spans {
		check(d.Name+" name", d.Name)
		check(d.Name+" status", d.StatusMsg)
		for _, a := range d.Attrs {
			check(d.Name+"."+a.Key, a.Value.AsString())
		}
		for _, e := range d.Events {
			for _, a := range e.Attrs {
				check(d.Name+" event "+a.Key, a.Value.AsString())
			}
		}
	}
}

// TestTraceWithDiagnostics: both sinks; the run record stores the trace id
// and the job span gets the /runs outcome.
func TestTraceWithDiagnostics(t *testing.T) {
	e := newEnv(t)
	tr, sink := newTraced(t)
	h := Hooks{Rec: e.rec, Trace: true}
	retry := &core.RetryLater{At: time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC), Reason: "global limit"}
	handler := TraceJobs(tr, nil, e.rec.Handler(fakeReview(h.WrapGitLab(fakeGitLab{}), h.WrapLimiter(fakeLimiter{}),
		h.WrapRepos(fakeRepos{dir: t.TempDir()}), h.WrapRunner(&spyRunner{}), retry)))
	if err := handler(context.Background(), core.Job{Kind: core.JobReview, ProjectID: 1, MRIID: 2}); !errors.As(err, &retry) {
		t.Fatal(err)
	}
	root := sink.byName(t, "review.job")
	if attr(root, AttrJobResult) != StatusPostponed || attr(root, AttrJobReason) != "global limit" {
		t.Fatalf("root outcome: %v", root.Attrs)
	}
	runs, err := e.st.ListDiagRuns(context.Background(), IndexFilter{})
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs: %v %v", runs, err)
	}
	_, rec := e.record(t, runs[0].ID)
	if rec.TraceID == "" || rec.TraceID != root.SpanContext.TraceID.String() {
		t.Fatalf("record trace id %q, span %s", rec.TraceID, root.SpanContext.TraceID)
	}
	if attr(root, "reviewbot.run_id") != rec.ID {
		t.Fatalf("run id attr %q", attr(root, "reviewbot.run_id"))
	}
	// The timeline is unchanged by tracing: same steps as without it.
	var kinds []string
	for _, s := range rec.Steps {
		if s.Kind != KindLog {
			kinds = append(kinds, s.Kind)
		}
	}
	if got := strings.Join(kinds, ","); got != "gitlab,gitlab,checkout,git,limiter,exec,claude,gitlab,step" {
		t.Fatalf("timeline kinds: %s", got)
	}
	assertNoLeak(t, sink)
}

func TestTraceJobsFailureAndPanic(t *testing.T) {
	tr, sink := newTraced(t)
	err := TraceJobs(tr, nil, func(context.Context, core.Job) error {
		return errors.New("claude run: token " + glToken + " rejected")
	})(context.Background(), core.Job{Kind: core.JobChat, ProjectID: 1, MRIID: 1})
	if err == nil {
		t.Fatal("error swallowed")
	}
	root := sink.byName(t, "review.job")
	if attr(root, AttrJobResult) != StatusFailed || root.Status != tracing.StatusError || attr(root, "job.manual") != "true" {
		t.Fatalf("failed job span: %v %v", root.Attrs, root.Status)
	}
	assertNoLeak(t, sink)

	tr2, sink2 := newTraced(t)
	func() {
		defer func() { _ = recover() }()
		TraceJobs(tr2, nil, func(context.Context, core.Job) error { panic("boom") })(context.Background(), core.Job{})
	}()
	if d := sink2.byName(t, "review.job"); attr(d, AttrJobResult) != StatusFailed {
		t.Fatalf("panic: %v", d.Attrs)
	}
	// Disabled: the handler is returned as is.
	var called bool
	h := func(context.Context, core.Job) error { called = true; return nil }
	TraceJobs(nil, nil, h)(context.Background(), core.Job{})
	if !called {
		t.Fatal("handler not called")
	}
}

func TestTraceJobsLogCorrelation(t *testing.T) {
	tr, _ := newTraced(t)
	active := &tracing.Active{}
	var during tracing.SpanContext
	var ok bool
	TraceJobs(tr, active, func(ctx context.Context, job core.Job) error {
		during, ok = active.Lookup("5!6")
		return nil
	})(context.Background(), core.Job{ProjectID: 5, MRIID: 6})
	if !ok || !during.IsValid() {
		t.Fatal("job not registered while running")
	}
	if _, ok := active.Lookup("5!6"); ok {
		t.Fatal("job still registered after it ended")
	}
}

func TestHooksOff(t *testing.T) {
	var h Hooks
	gl := core.GitLab(fakeGitLab{})
	if h.WrapGitLab(gl) != gl || h.WrapRunner(nil) != nil {
		t.Fatal("off hooks wrapped")
	}
	if (Hooks{Trace: true}).WrapGitLab(gl) == gl {
		t.Fatal("trace hooks did not wrap")
	}
	// Wrapped but neither a Run nor a span in ctx: plain pass-through.
	r := &spyRunner{}
	if _, err := (Hooks{Trace: true}).WrapRunner(r).Run(context.Background(), core.RunRequest{}); err != nil {
		t.Fatal(err)
	}
}

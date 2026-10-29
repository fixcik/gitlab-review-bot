package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// Fake secrets: a GitLab token known to the recorder by value, and Claude
// tokens that only the pattern rules can recognize.
const (
	glToken     = "glpat-SECRETgitlab12345"
	oauthToken  = "sk-ant-oat01-SECRETclaudeTOKENvalue1234567890"
	webhookSec  = "hook-secret-value-42"
	opaqueToken = "Zx9Kq2Lm7Np4Rt8Vw3Yb6Cd1Fg5Hj0Ks2Lm7Np4Rt8Vw3"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type env struct {
	rec   *Recorder
	st    *store.SQLite
	clock *clock
	root  string
}

func newEnv(t *testing.T, mut ...func(*Options)) *env {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c := &clock{t: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
	o := Options{Root: filepath.Join(t.TempDir(), "diag"), Index: st, Now: c.now, Secrets: []string{glToken, webhookSec, "short"},
		GitLabURL: "https://gitlab.example.com", MaxDiffBytes: 100}
	for _, f := range mut {
		f(&o)
	}
	rec, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return &env{rec: rec, st: st, clock: c, root: o.Root}
}

func (e *env) record(t *testing.T, id string) (*IndexRow, *Record) {
	t.Helper()
	row, rec, err := e.rec.Reader().Get(context.Background(), id)
	if err != nil || row == nil || rec == nil {
		t.Fatalf("get %s: %v %v %v", id, row, rec, err)
	}
	return row, rec
}

// allFiles returns the concatenated content of every file under root.
func allFiles(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			c, _ := os.ReadFile(p)
			b.Write(c)
		}
		return nil
	})
	return b.String()
}

var job = core.Job{Kind: core.JobReview, ProjectID: 42, ProjectPath: "g/api", MRIID: 7, Username: "bob", UserID: 11,
	Question: "review", EnqueuedAt: time.Date(2026, 10, 24, 11, 59, 0, 0, time.UTC)}

func TestLifecycleStatuses(t *testing.T) {
	e := newEnv(t)
	logger := e.rec.Logger(slog.New(slog.DiscardHandler))
	cases := []struct {
		name   string
		h      core.Handler
		status string
		check  func(*IndexRow, *Record)
	}{
		{"ok", func(ctx context.Context, j core.Job) error { return nil }, StatusOK, nil},
		{"skipped", func(ctx context.Context, j core.Job) error {
			logger.With("mr", j.MRKey()).Info("review: draft mr, skipping")
			return nil
		}, StatusSkipped, func(row *IndexRow, r *Record) {
			if !strings.Contains(r.Reason, "draft mr, skipping") || row.Reason != r.Reason {
				t.Errorf("skip reason: %q", r.Reason)
			}
		}},
		{"postponed", func(ctx context.Context, j core.Job) error {
			return &core.RetryLater{At: e.clock.now().Add(time.Hour), Reason: "global limit"}
		}, StatusPostponed, func(row *IndexRow, r *Record) {
			if r.RetryAt.IsZero() || r.Reason != "global limit" || row.RetryAt.IsZero() {
				t.Errorf("postponed: %+v", r)
			}
		}},
		{"failed", func(ctx context.Context, j core.Job) error {
			e.clock.add(3 * time.Second)
			return errors.New("checkout: boom token=" + glToken)
		}, StatusFailed, func(row *IndexRow, r *Record) {
			if !strings.Contains(r.Error, "checkout: boom") || strings.Contains(r.Error, glToken) || r.DurationMS != 3000 || row.DurationMS != 3000 {
				t.Errorf("failed: %+v", r)
			}
		}},
		{"interrupted", func(ctx context.Context, j core.Job) error { return context.Canceled }, StatusInterrupted, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var id string
			h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
				id = FromContext(ctx).ID()
				return tc.h(ctx, j)
			})
			_ = h(context.Background(), job)
			row, rec := e.record(t, id)
			if row.Status != tc.status || rec.Status != tc.status {
				t.Fatalf("status: %s / %s", row.Status, rec.Status)
			}
			if rec.Job.ProjectPath != "g/api" || rec.Job.MRIID != 7 || rec.Job.Username != "bob" || row.MRURL != "https://gitlab.example.com/g/api/-/merge_requests/7" {
				t.Fatalf("job: %+v / %+v", rec.Job, row)
			}
			if len(rec.Steps) == 0 || rec.Steps[0].Kind != KindQueue || rec.Steps[0].DurationMS < 60000 {
				t.Fatalf("queue step: %+v", rec.Steps)
			}
			if tc.check != nil {
				tc.check(row, rec)
			}
		})
	}
	// Retries of the same job count attempts.
	retry := job
	retry.EnqueuedAt = time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	var ids []string
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error { ids = append(ids, FromContext(ctx).ID()); return nil })
	_ = h(context.Background(), retry)
	_ = h(context.Background(), retry)
	if _, r := e.record(t, ids[1]); r.Attempt != 2 {
		t.Fatalf("attempt = %d", r.Attempt)
	}
	// Disabled diagnostics: a nil recorder changes nothing.
	var nilRec *Recorder
	called := false
	if err := nilRec.Handler(func(ctx context.Context, j core.Job) error { called = FromContext(ctx) == nil; return nil })(context.Background(), job); err != nil || !called {
		t.Fatal("nil recorder")
	}
	Step(context.Background(), "x")(StepOK, nil)
	Note(context.Background(), "x", nil)
}

func TestPanicIsRecorded(t *testing.T) {
	e := newEnv(t)
	var id string
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error { id = FromContext(ctx).ID(); panic("kaboom") })
	func() {
		defer func() { _ = recover() }()
		_ = h(context.Background(), job)
	}()
	if _, r := e.record(t, id); r.Status != StatusFailed || !strings.Contains(r.Error, "kaboom") {
		t.Fatalf("%+v", r)
	}
}

// ---------------------------------------------------------------------------
// Decorators
// ---------------------------------------------------------------------------

type fakeGitLab struct{ resolveErr error }

func (fakeGitLab) CurrentUser(context.Context) (int64, string, error) { return 1, "bot", nil }
func (fakeGitLab) GetMR(_ context.Context, p, iid int64) (*core.MergeRequest, error) {
	return &core.MergeRequest{ProjectID: p, IID: iid, Title: "feat: x", State: "opened", AuthorUsername: "alice",
		WebURL: "https://gitlab.example.com/g/api/-/merge_requests/7",
		DiffRefs: core.DiffRefs{BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", StartSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			HeadSHA: "0123456789abcdef0123456789abcdef01234567"}}, nil
}
func (fakeGitLab) GetMRDiffs(context.Context, int64, int64) ([]core.FileDiff, error) {
	return []core.FileDiff{{NewPath: "a.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}, nil
}
func (fakeGitLab) ListDiscussions(context.Context, int64, int64) ([]core.Discussion, error) {
	return []core.Discussion{{ID: "d1", Resolvable: true, Notes: []core.Note{{ID: 1}}}}, nil
}
func (fakeGitLab) GetDiscussion(context.Context, int64, int64, string) (*core.Discussion, error) {
	return nil, core.ErrNotFound
}
func (fakeGitLab) CreateDraftNote(context.Context, int64, int64, string, *core.Position) (int64, error) {
	return 101, nil
}
func (fakeGitLab) PublishDraftNotes(context.Context, int64, int64) error { return nil }
func (fakeGitLab) DeleteDraftNotes(context.Context, int64, int64) error  { return nil }
func (fakeGitLab) CreateNote(context.Context, int64, int64, string) (int64, error) {
	return 202, nil
}
func (fakeGitLab) UpdateNote(context.Context, int64, int64, int64, string) error { return nil }
func (fakeGitLab) ReplyToDiscussion(context.Context, int64, int64, string, string) error {
	return nil
}
func (f fakeGitLab) ResolveDiscussion(context.Context, int64, int64, string, bool) error {
	return f.resolveErr
}
func (fakeGitLab) AccessLevel(context.Context, int64, int64) (int, error) { return 30, nil }
func (fakeGitLab) GetRawFile(context.Context, int64, string, string) ([]byte, error) {
	return nil, core.ErrNotFound
}
func (fakeGitLab) ListMRCommits(context.Context, int64, int64) ([]core.Commit, error) {
	return nil, nil
}

type fakeLimiter struct{ deny bool }

func (f fakeLimiter) CheckUser(context.Context, int64, string, core.UsageKind, string, core.UserLimits) (core.Decision, error) {
	return core.Decision{Allowed: true}, nil
}
func (f fakeLimiter) CheckGlobal(context.Context) (core.Decision, error) {
	if f.deny {
		return core.Decision{Reason: "лимит 15/ч", RetryAt: time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)}, nil
	}
	return core.Decision{Allowed: true}, nil
}
func (fakeLimiter) Record(context.Context, core.Usage) error { return nil }
func (f fakeLimiter) Reserve(ctx context.Context, req core.ReserveRequest) (core.Decision, core.Reservation, error) {
	return core.ReserveByChecks(ctx, f, req)
}
func (fakeLimiter) Remaining(context.Context, int64, string, core.UserLimits) (core.Remaining, error) {
	return core.Remaining{}, nil
}

type fakeRepos struct{ dir string }

func (f fakeRepos) Checkout(context.Context, string, *core.MergeRequest) (core.Checkout, error) {
	return fakeCheckout{dir: f.dir}, nil
}

type fakeCheckout struct{ dir string }

func (f fakeCheckout) Dir() string { return f.dir }
func (fakeCheckout) Diff(context.Context, string, []string) (string, error) {
	return "diff --git a/a.go b/a.go\n" + strings.Repeat("+x\n", 100), nil
}
func (fakeCheckout) ChangedFiles(context.Context, string, []string) ([]string, error) {
	return []string{"a.go"}, nil
}
func (fakeCheckout) IsAncestor(context.Context, string) (bool, error)         { return true, nil }
func (fakeCheckout) ShowFile(context.Context, string, string) ([]byte, error) { return nil, nil }
func (fakeCheckout) ListFiles(context.Context, string) ([]string, error)      { return nil, nil }
func (fakeCheckout) Close() error                                             { return nil }

// fakeDocker writes a docker stand-in that prints a stream-json session and
// leaks the Claude token on stderr (it must not be stored).
func fakeDocker(t *testing.T, extraEvents string) string {
	t.Helper()
	bin := t.TempDir()
	p := filepath.Join(bin, "docker")
	script := `#!/bin/sh
for a in "$@"; do
  case "$a" in
    oasdiff) echo '[]'; echo "exec err ` + oauthToken + `" >&2; exit 1 ;;
  esac
done
cat >/dev/null
cat <<'EOF'
{"type":"system","subtype":"init","model":"claude-opus","tools":["Read"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"Looking at the code; I found ` + oauthToken + ` in config"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/work/a.go"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package a // password=hunter2hunter2"}]}}
` + extraEvents + `{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"summary":"ok"},"total_cost_usd":0.5,"num_turns":2,"duration_ms":1500,"usage":{"input_tokens":10,"output_tokens":20}}
EOF
echo "warning: leaking $CLAUDE_CODE_OAUTH_TOKEN" >&2
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newClaudeRunner(t *testing.T, docker string) core.Runner {
	t.Helper()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", oauthToken)
	t.Setenv("ANTHROPIC_API_KEY", "")
	r, err := claude.NewRunner(claude.Options{Mode: "docker", Image: "img", Network: "net", EgressProxy: "http://egress:3128", DockerBin: docker, User: "1000:1000"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDecoratorsBuildTimeline(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	runner := e.rec.WrapRunner(newClaudeRunner(t, fakeDocker(t, "")))
	gl := e.rec.WrapGitLab(fakeGitLab{resolveErr: core.ErrForbidden})
	lim := e.rec.WrapLimiter(fakeLimiter{deny: true})
	repos := e.rec.WrapRepos(fakeRepos{dir: work})

	var id string
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		id = FromContext(ctx).ID()
		ctx = meter.With(ctx, meter.ForJob(j))
		mr, _ := gl.GetMR(ctx, j.ProjectID, j.MRIID)
		_, _ = gl.ListDiscussions(ctx, j.ProjectID, j.MRIID)
		_, _ = gl.GetDiscussion(ctx, j.ProjectID, j.MRIID, "d9")
		_, _ = lim.CheckGlobal(ctx)
		co, err := repos.Checkout(ctx, j.ProjectPath, mr)
		if err != nil {
			return err
		}
		_, _ = co.Diff(ctx, mr.DiffRefs.BaseSHA, nil)
		res, err := runner.Exec(ctx, core.ExecRequest{Dir: work, Command: []string{"oasdiff", "breaking"}})
		if err != nil || res.ExitCode != 1 {
			t.Errorf("exec: %+v %v", res, err)
		}
		out, err := runner.Run(ctx, core.RunRequest{Dir: work, Prompt: "Review this. Config has " + oauthToken + " and " + glToken,
			SystemPrompt: "be strict", JSONSchema: `{"type":"object"}`, Model: "opus",
			ContextDirs: []core.ContextDir{{Name: "g/lib", SHA: "fedcba9876543210fedcba9876543210fedcba98"}}})
		if err != nil || out.Output != `{"summary":"ok"}` {
			t.Errorf("run: %+v %v", out, err)
		}
		_, _ = gl.CreateDraftNote(ctx, j.ProjectID, j.MRIID, "<!-- reviewbot:finding id=abc -->\nbody", &core.Position{NewPath: "a.go", NewLine: 3})
		_ = gl.PublishDraftNotes(ctx, j.ProjectID, j.MRIID)
		_, _ = gl.CreateNote(ctx, j.ProjectID, j.MRIID, "<!-- reviewbot:summary sha=x -->")
		_ = gl.ResolveDiscussion(ctx, j.ProjectID, j.MRIID, "d1", true)
		return nil
	})
	if err := h(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	row, r := e.record(t, id)
	if row.Status != StatusOK || row.ClaudeRuns != 1 || row.Tokens != 30 || row.CostUSD != 0.5 || row.MRTitle != "feat: x" {
		t.Fatalf("row: %+v", row)
	}
	byName := map[string]TimelineStep{}
	for _, s := range r.Steps {
		byName[s.Name] = s
	}
	want := map[string]string{
		"GET merge request": StepOK, "GET discussions": StepOK, "GET discussion": StepInfo, "глобальный лимит": StepDenied,
		"checkout": StepOK, "git diff": StepWarn,
		"exec: oasdiff breaking": StepWarn, "claude: review (opus)": StepOK, "POST draft note": StepOK,
		"POST publish draft notes": StepOK, "POST note": StepOK, "PUT resolve discussion (bot token)": StepWarn,
	}
	for name, st := range want {
		s, ok := byName[name]
		if !ok {
			t.Errorf("missing step %q in %v", name, names(r.Steps))
			continue
		}
		if s.Status != st {
			t.Errorf("step %q status %s, want %s (%+v)", name, s.Status, st, s)
		}
	}
	if s := byName["exec: oasdiff breaking"]; s.Fields["exit_code"] != float64(1) || !strings.Contains(s.Stdout, "[]") {
		t.Errorf("exec: %+v", s)
	}
	if s := byName["POST draft note"]; s.Fields["draft_note_id"] != float64(101) || s.Fields["marker"] != "finding" || s.Fields["position"] != "a.go:3" {
		t.Errorf("draft: %+v", s.Fields)
	}
	if s := byName["глобальный лимит"]; !strings.Contains(s.Detail, "лимит 15/ч") {
		t.Errorf("limiter: %+v", s)
	}
	if s := byName["git diff"]; s.Fields["capped_to_bytes"] != float64(100) {
		t.Errorf("diff cap: %+v", s.Fields)
	}
	// Claude run in full.
	if len(r.Claude) != 1 {
		t.Fatalf("claude: %+v", r.Claude)
	}
	c := r.Claude[0]
	if c.Purpose != "review" || c.Model != "opus" || c.SystemPrompt != "be strict" || c.JSONSchema != `{"type":"object"}` ||
		!strings.HasPrefix(c.Prompt, "Review this.") || c.Output != `{"summary":"ok"}` || c.Usage.Total() != 30 || c.NumTurns != 2 ||
		c.CostUSD != 0.5 || c.Events != 4 || c.ExitCode == nil || *c.ExitCode != 0 || c.Status != StepOK || strings.Join(c.AllowedTools, ",") != "Read(//work/**),Read(//repos/**)" ||
		len(c.Disallowed) == 0 || len(c.ContextDirs) != 1 || c.ContextDirs[0].SHA != "fedcba9876543210fedcba9876543210fedcba98" {
		t.Fatalf("claude run: %+v", c)
	}
	argv := strings.Join(c.Argv, " ")
	if !strings.Contains(argv, "--output-format stream-json --verbose") || !strings.Contains(argv, "<system prompt: 9 bytes>") || strings.Contains(argv, "be strict") {
		t.Errorf("argv: %s", argv)
	}
	if !strings.Contains(c.Stderr, "warning: leaking") {
		t.Errorf("stderr: %q", c.Stderr)
	}
	// Transcript: every event, scrubbed, valid JSON per line.
	tp := e.rec.Reader().TranscriptPath(row, 1)
	b, err := os.ReadFile(tp)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 4 {
		t.Fatalf("transcript lines: %d\n%s", len(lines), b)
	}
	for _, l := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatalf("bad line %q: %v", l, err)
		}
	}
	if !strings.Contains(string(b), `"tool_use"`) || !strings.Contains(string(b), `"tool_result"`) || !strings.Contains(string(b), "Looking at the code") {
		t.Errorf("transcript content: %s", b)
	}
	// SHAs stay readable.
	if !strings.Contains(allFiles(t, e.root), "0123456789abcdef0123456789abcdef01234567") {
		t.Error("head sha was scrubbed")
	}
}

func names(ss []TimelineStep) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.Name)
	}
	return out
}

func TestSecretsNeverStored(t *testing.T) {
	e := newEnv(t)
	work := t.TempDir()
	runner := e.rec.WrapRunner(newClaudeRunner(t, fakeDocker(t, "")))
	logger := e.rec.Logger(slog.New(slog.DiscardHandler))
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		logger.Warn("review: something", "mr", j.MRKey(), "url", "https://oauth2:"+glToken+"@gitlab.example.com/g/api.git", "secret", webhookSec)
		_, _ = runner.Exec(ctx, core.ExecRequest{Dir: work, Command: []string{"oasdiff", opaqueToken}})
		_, err := runner.Run(ctx, core.RunRequest{Dir: work, Prompt: "key: " + oauthToken + " gl " + glToken + " " + opaqueToken,
			SystemPrompt: "Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456", JSONSchema: "{}"})
		return err
	})
	j := job
	j.Question = "please use token=" + glToken
	if err := h(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	all := allFiles(t, e.root)
	for _, s := range []string{glToken, oauthToken, webhookSec, opaqueToken, "hunter2hunter2", "abcdefghijklmnopqrstuvwxyz123456", "oauth2:"} {
		if strings.Contains(all, s) {
			t.Errorf("secret %q stored", s)
		}
	}
	if !strings.Contains(all, Redacted) || !strings.Contains(all, "https://[скрыто]@gitlab.example.com") {
		t.Error("no redaction marker")
	}
	rows, _ := e.st.ListDiagRuns(context.Background(), IndexFilter{})
	for _, r := range rows {
		if strings.Contains(r.Error+r.Reason+r.MRTitle, glToken) {
			t.Error("secret in index")
		}
	}
}

func TestScrubber(t *testing.T) {
	s := newScrubber([]string{"literal-secret-1"})
	cases := map[string]string{
		"x literal-secret-1 y":                           "x [скрыто] y",
		"GITLAB_TOKEN=glpat-abcdefghij12":                "GITLAB_TOKEN=[скрыто]",
		"https://user:pa55@host/x":                       "https://[скрыто]@host/x",
		`"api_key": "abcdef123456"`:                      `"api_key": "[скрыто]"`,
		"sha 0123456789abcdef0123456789abcdef01234567":   "sha 0123456789abcdef0123456789abcdef01234567",
		"internal/review/flow_handle_review_test.go":     "internal/review/flow_handle_review_test.go",
		"TestHandleReviewPostponesWhenRateLimited2Times": "TestHandleReviewPostponesWhenRateLimited2Times",
	}
	for in, want := range cases {
		if got := s.str(in, nil); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	// JSON lines stay valid JSON after scrubbing, also inside keys.
	line := []byte(`{"type":"user","sk-ant-api03-aaaaaaaaaaaaaaaa":1,"text":"a \"quoted\" literal-secret-1"}`)
	out := s.jsonLine(line, nil)
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil || bytes.Contains(out, []byte("literal-secret-1")) || bytes.Contains(out, []byte("sk-ant")) {
		t.Fatalf("%s %v", out, err)
	}
	if got := s.jsonLine([]byte("not json literal-secret-1"), nil); !bytes.Contains(got, []byte("reviewbot_unparsed")) || bytes.Contains(got, []byte("literal-secret-1")) {
		t.Fatalf("%s", got)
	}
}

func TestTranscriptCapAndMarker(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.MaxRunBytes = 400 << 10 })
	big := strings.Repeat("x", 20<<10)
	var extra strings.Builder
	for i := 0; i < 40; i++ {
		extra.WriteString(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"` + big + `"}]}}` + "\n")
	}
	work := t.TempDir()
	runner := e.rec.WrapRunner(newClaudeRunner(t, fakeDocker(t, extra.String())))
	var id string
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		id = FromContext(ctx).ID()
		_, err := runner.Run(ctx, core.RunRequest{Dir: work, Prompt: strings.Repeat("p", 600<<10), JSONSchema: "{}"})
		return err
	})
	if err := h(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	row, r := e.record(t, id)
	c := r.Claude[0]
	if !c.TranscriptTruncated || c.Events != 44 {
		t.Fatalf("claude: truncated=%v events=%d", c.TranscriptTruncated, c.Events)
	}
	b, _ := os.ReadFile(e.rec.Reader().TranscriptPath(row, 1))
	if !bytes.Contains(b, []byte("reviewbot_truncated")) || !bytes.Contains(b, []byte(`"type":"result"`)) {
		t.Fatalf("marker/result missing (%d bytes)", len(b))
	}
	// The whole run stays within the cap (+ a little slack for the result).
	rj, _ := os.ReadFile(filepath.Join(e.root, row.Dir, RecordFile))
	if total := int64(len(b) + len(rj)); total > 400<<10+8<<10 {
		t.Fatalf("run size %d > cap", total)
	}
	if !r.Truncated || !strings.Contains(string(rj), "обрезано: превышен diag.max_run_mb") {
		t.Fatalf("run.json not truncated with marker: truncated=%v size=%d", r.Truncated, len(rj))
	}
	if row.SizeBytes == 0 {
		t.Fatal("size not indexed")
	}
}

func TestRetentionCleanup(t *testing.T) {
	e := newEnv(t)
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error { return nil })
	_ = h(context.Background(), job)
	e.clock.add(47 * time.Hour)
	_ = h(context.Background(), job)
	// An orphan directory (no index row) older than the retention.
	orphan := filepath.Join(e.root, "20261001", "20261001-000000-deadbeef")
	if err := os.MkdirAll(orphan, 0o750); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	_ = os.Chtimes(orphan, old, old)

	e.clock.add(2 * time.Hour) // first run is now 49h old
	e.rec.Cleanup(context.Background())
	rows, _ := e.st.ListDiagRuns(context.Background(), IndexFilter{})
	if len(rows) != 1 {
		t.Fatalf("rows after cleanup: %d", len(rows))
	}
	if _, err := os.Stat(filepath.Join(e.root, rows[0].Dir)); err != nil {
		t.Fatal("recent run removed")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("orphan kept")
	}
	if _, err := os.Stat(filepath.Join(e.root, "20261024")); !os.IsNotExist(err) {
		t.Fatal("empty day dir of the expired run kept")
	}
	// Rows left running by a previous process become interrupted.
	_ = e.st.SaveDiagRun(context.Background(), IndexRow{ID: "20261026-110000-00000001", StartedAt: e.clock.now(), Status: StatusRunning})
	if _, err := New(e.rec.o); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.rec.Run(ctx) // returns after one cleanup pass
	r, _ := e.st.GetDiagRun(context.Background(), "20261026-110000-00000001")
	if r == nil || r.Status != StatusInterrupted {
		t.Fatalf("interrupted: %+v", r)
	}
}

func TestTotalSizeEviction(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.MaxTotalBytes = 20 << 10 })
	work := t.TempDir()
	runner := e.rec.WrapRunner(newClaudeRunner(t, fakeDocker(t, "")))
	var ids []string
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		ids = append(ids, FromContext(ctx).ID())
		_, err := runner.Run(ctx, core.RunRequest{Dir: work, Prompt: strings.Repeat("q", 4<<10), JSONSchema: "{}"})
		return err
	})
	for i := 0; i < 5; i++ {
		if err := h(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		e.clock.add(time.Minute)
	}
	total, _ := e.st.DiagTotalSize(context.Background())
	if total > 20<<10 {
		t.Fatalf("total %d over cap", total)
	}
	rows, _ := e.st.ListDiagRuns(context.Background(), IndexFilter{})
	if len(rows) == 0 || len(rows) >= 5 || rows[0].ID != ids[4] {
		t.Fatalf("rows: %d (newest %v)", len(rows), rows)
	}
	if r, _ := e.st.GetDiagRun(context.Background(), ids[0]); r != nil {
		t.Fatal("oldest not evicted")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.root, "20261024")); len(entries) != len(rows) {
		t.Fatalf("dirs %d rows %d", len(entries), len(rows))
	}
}

func TestLogHandlerCapturesJobLines(t *testing.T) {
	e := newEnv(t)
	var buf bytes.Buffer
	logger := e.rec.Logger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	var id string
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		id = FromContext(ctx).ID()
		l := logger.With("mr", j.MRKey(), "kind", j.Kind)
		l.Debug("review: debug detail", "n", 3)
		l.Warn("review: setup failed, continuing without it", "err", errors.New("exit 1"))
		logger.Info("queue: job started", "mr", j.MRKey())    // queue lines are not part of the job
		logger.Info("review: other mr", "mr", "1!1")          // another MR
		logger.InfoContext(ctx, "compat: no mr attr but ctx") // context-carried run
		return nil
	})
	_ = h(context.Background(), job)
	_, r := e.record(t, id)
	var got []string
	for _, s := range r.Steps {
		if s.Kind == KindLog {
			got = append(got, s.Status+" "+s.Name)
		}
	}
	want := "info DEBUG: review: debug detail|warn WARN: review: setup failed, continuing without it|info INFO: compat: no mr attr but ctx"
	if strings.Join(got, "|") != want {
		t.Fatalf("log steps:\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}
	if strings.Contains(buf.String(), "debug detail") {
		t.Fatal("debug leaked to the real handler")
	}
	if r.Status != StatusOK {
		t.Fatalf("status %s", r.Status)
	}
}

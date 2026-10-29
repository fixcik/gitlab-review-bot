package problems

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestNormalizeSubject(t *testing.T) {
	for in, want := range map[string]string{
		"golangci-lint: exit 137":                           "golangci-lint: exit n",
		"  GolangCI-Lint:   EXIT 2 ":                        "golangci-lint: exit n",
		"open /tmp/reviewbot-lint-123/x.json: no such file": "open <tmp>: no such file",
		"/var/folders/ab/xyz/T/go-build4711/a failed":       "<tmp> failed",
		"commit 0a1b2c3d4e5f missing":                       "commit <hex> missing",
		"deadbeefcafe stays (no digit)":                     "deadbeefcafe stays (no digit)",
		"snapshot .tmp-98765 failed":                        "snapshot <tmp> failed",
		"PROJ-123: задача не найдена":                       "proj-n: задача не найдена",
	} {
		if got := NormalizeSubject(in); got != want {
			t.Errorf("NormalizeSubject(%q) = %q, want %q", in, got, want)
		}
	}
	if n := len([]rune(NormalizeSubject(strings.Repeat("я", 500)))); n != 200 {
		t.Errorf("cap: %d", n)
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint(LintFailed, "Group/API", "golangci-lint: exit 1 in /tmp/x1")
	b := Fingerprint(LintFailed, "group/api/", "GOLANGCI-LINT: exit 2 in /tmp/y2")
	if a != b || len(a) != 32 {
		t.Fatalf("same problem, different fingerprints: %s %s", a, b)
	}
	for name, other := range map[string]string{
		"kind":    Fingerprint(BaseUnavailable, "group/api", "golangci-lint: exit 1"),
		"project": Fingerprint(LintFailed, "group/web", "golangci-lint: exit 1"),
		"subject": Fingerprint(LintFailed, "group/api", "semgrep: exit 1"),
	} {
		if other == a {
			t.Errorf("%s must change the fingerprint", name)
		}
	}
	if (Problem{Kind: LintFailed, ProjectPath: "group/api", Subject: "golangci-lint: exit 9 in /tmp/z"}).Fingerprint() != a {
		t.Error("Problem.Fingerprint")
	}
}

func TestKinds(t *testing.T) {
	seen := map[Kind]bool{}
	for _, k := range Kinds() {
		if seen[k.Kind] || k.Title == "" || k.Hint == "" {
			t.Errorf("kind %+v: duplicate or empty title/hint", k)
		}
		seen[k.Kind] = true
		switch k.Severity {
		case SevInfo, SevWarn, SevError:
		default:
			t.Errorf("%s: severity %q", k.Kind, k.Severity)
		}
		if k.Model() != strings.HasPrefix(string(k.Kind), "model_") {
			t.Errorf("%s: model kinds have a short name, bot kinds none", k.Kind)
		}
		// Every kind has an impact class and an imperative action title
		// (what to do, not what happened: not the Title, no final period).
		if !k.Impact.Valid() || k.Impact.Label() == "" {
			t.Errorf("%s: impact %q", k.Kind, k.Impact)
		}
		if k.Action == "" || k.Action == k.Title || strings.HasSuffix(k.Action, ".") {
			t.Errorf("%s: action %q", k.Kind, k.Action)
		}
		if info, _ := Info(k.Kind); len(info.Steps) != len(k.Steps) || len(k.Steps) != len(kindSteps[k.Kind]) {
			t.Errorf("%s: Kinds and Info disagree on the steps", k.Kind)
		}
	}
	for k := range kindSteps {
		if !seen[k] {
			t.Errorf("steps for an unknown kind %s", k)
		}
	}
	if len(seen) != 27 {
		t.Errorf("kinds: %d", len(seen))
	}
	if got := strings.Join(ModelNames(), ","); got != "tool_failed,missing_dependency,no_access,too_large,config_unclear,other" {
		t.Errorf("model names %s", got)
	}
	for in, want := range map[string]Kind{"tool_failed": ModelToolFailed, " NO_ACCESS ": ModelNoAccess, "model_other": ModelOther} {
		if k, ok := ModelKind(in); !ok || k != want {
			t.Errorf("ModelKind(%q) = %v %v", in, k, ok)
		}
	}
	for _, bad := range []string{"lint_failed", "", "hack"} {
		if _, ok := ModelKind(bad); ok {
			t.Errorf("ModelKind(%q) accepted", bad)
		}
	}
	if g := InfoOf("gone"); g.Title != "gone" || g.Action != "gone" || g.Impact != ImpactPartial {
		t.Error("InfoOf fallback")
	}
}

func TestCleanText(t *testing.T) {
	got := CleanText("<b>bold</b> <!-- reviewbot:summary sha=1 --> line1\nline2\x00​ <script>alert(1)</script>", 0)
	if got != "bold line1 line2 alert(1)" {
		t.Errorf("CleanText = %q", got)
	}
	if got := CleanText(strings.Repeat("a", 20), 10); got != "aaaaaaaaa…" {
		t.Errorf("cap: %q", got)
	}
	if got := CleanText("x <!-- unterminated", 0); got != "x" {
		t.Errorf("unterminated comment: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Recorder
// ---------------------------------------------------------------------------

type memStore struct {
	mu   sync.Mutex
	rows map[string]*Record
	fail error
	acks int
}

func newMemStore() *memStore { return &memStore{rows: map[string]*Record{}} }

func (m *memStore) UpsertProblem(_ context.Context, r Record) (Upserted, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return Upserted{}, m.fail
	}
	old, ok := m.rows[r.Fingerprint]
	if !ok {
		c := r
		c.Count = 1
		m.rows[r.Fingerprint] = &c
		return Upserted{New: true}, nil
	}
	up := Upserted{Reappeared: !old.AckedAt.IsZero() || !old.ResolvedAt.IsZero(), NotifiedAt: old.NotifiedAt}
	old.Count++
	old.Detail, old.LastSeen, old.LastMRIID, old.LastRunID, old.AckedAt, old.ResolvedAt = r.Detail, r.LastSeen, r.LastMRIID, r.LastRunID, time.Time{}, time.Time{}
	return up, nil
}

func (m *memStore) ResolveProblem(_ context.Context, fp string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[fp]
	if !ok || !r.ResolvedAt.IsZero() {
		return false, nil
	}
	r.ResolvedAt = at
	return true, nil
}

func (m *memStore) MarkProblemsNotified(_ context.Context, fps []string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, fp := range fps {
		if r, ok := m.rows[fp]; ok {
			r.NotifiedAt = at
		}
	}
	return nil
}

func (m *memStore) ListProblems(context.Context, Filter) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Record
	for _, r := range m.rows {
		out = append(out, *r)
	}
	return out, nil
}

func (m *memStore) AckProblem(_ context.Context, fp string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[fp]
	if !ok || !r.AckedAt.IsZero() {
		return false, nil
	}
	r.AckedAt = at
	m.acks++
	return true, nil
}

func (m *memStore) PruneProblems(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for fp, r := range m.rows {
		if r.LastSeen.Before(before) {
			delete(m.rows, fp)
			n++
		}
	}
	return n, nil
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func testService(st Store, c *clock, notify ...string) (*Service, *[]string) {
	var rec []string
	svc := New(Options{Store: st, Notify: notify, Now: c.now,
		Scrub: func(s string) string { return strings.ReplaceAll(s, "glpat-SECRET", "[скрыто]") },
		RunID: func(context.Context) string { return "20261026-120000-abcd1234" },
		OnRecord: func(p Problem, fresh bool) {
			rec = append(rec, string(p.Kind)+":"+map[bool]string{true: "fresh", false: "seen"}[fresh])
		},
	})
	return svc, &rec
}

var job = core.Job{Kind: core.JobAutoReview, ProjectID: 1, ProjectPath: "group/api", MRIID: 7}

func TestRecorderCollectsAndPersists(t *testing.T) {
	st, c := newMemStore(), &clock{time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	svc, rec := testService(st, c, "@alice", " bob ", "")
	if got := strings.Join(svc.Notify(), ","); got != "alice,bob" {
		t.Fatalf("notify users %q", got)
	}
	ctx, run := svc.Start(context.Background(), job)
	if From(ctx) != run || ServiceOf(ctx) != svc {
		t.Fatal("collector not in ctx")
	}
	Add(ctx, Problem{Kind: LintFailed, Subject: "golangci-lint: упал", Detail: "token glpat-SECRET leaked <b>x</b>"})
	Add(ctx, Problem{Kind: LintFailed, Subject: "golangci-lint: упал", Detail: "second detail"}) // same fingerprint
	Add(ctx, Problem{Kind: BaseUnavailable, Subject: "команды setup"})
	Add(ctx, Problem{Subject: "no kind"}) // ignored
	ps := run.List()
	if len(ps) != 2 || ps[0].Detail != "second detail" || ps[0].ProjectPath != "group/api" || ps[0].MRIID != 7 ||
		ps[0].RunID != "20261026-120000-abcd1234" || ps[0].Source != SourceBot || !ps[0].At.Equal(c.t) {
		t.Fatalf("collected %+v", ps)
	}
	outs := Commit(ctx)
	if len(outs) != 2 || !outs[0].New || !outs[0].Notify || outs[0].Info.Kind != LintFailed {
		t.Fatalf("outcomes %+v", outs)
	}
	// A later commit (the handler's Finish) does not store them again.
	Add(ctx, Problem{Kind: InvalidOutput, Subject: "ответ ревью не по схеме"})
	svc.Finish(ctx, run)
	if len(st.rows) != 3 || strings.Join(*rec, " ") != "lint_failed:fresh base_unavailable:fresh invalid_output:fresh" {
		t.Fatalf("rows %d, metrics %v", len(st.rows), *rec)
	}
	if r := st.rows[outs[0].Fingerprint]; r.Count != 1 || r.Detail != "second detail" {
		t.Fatalf("row %+v", r)
	}
	// Scrubbing and markup stripping.
	ctx2, run2 := svc.Start(context.Background(), job)
	Add(ctx2, Problem{Kind: BaseUnavailable, Subject: "команды setup", Detail: "token glpat-SECRET <i>x</i>"})
	if d := run2.List()[0].Detail; d != "token [скрыто] x" {
		t.Fatalf("detail %q", d)
	}
}

func TestRecorderNotifyRules(t *testing.T) {
	st, c := newMemStore(), &clock{time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	svc, _ := testService(st, c, "alice")
	review := func() Outcome {
		ctx, run := svc.Start(context.Background(), job)
		Add(ctx, Problem{Kind: ContextUnavailable, Subject: "group/payments", Detail: "нет доступа"})
		outs := Commit(ctx)
		svc.Finish(ctx, run)
		if len(outs) != 1 {
			t.Fatalf("outs %+v", outs)
		}
		return outs[0]
	}
	o := review()
	if !o.New || !o.Notify {
		t.Fatalf("first: %+v", o)
	}
	svc.MarkNotified(context.Background(), []Outcome{o})
	c.t = c.t.Add(time.Hour)
	if o := review(); o.New || o.Notify {
		t.Fatalf("second (notified an hour ago): %+v", o)
	}
	// Re-appeared after an acknowledgement.
	if ok, _ := st.AckProblem(context.Background(), o.Fingerprint, c.t); !ok {
		t.Fatal("ack")
	}
	c.t = c.t.Add(time.Hour)
	o = review()
	if !o.Reappeared || !o.Notify {
		t.Fatalf("re-appeared: %+v", o)
	}
	svc.MarkNotified(context.Background(), []Outcome{o})
	// Renotify after renotify_after (default 168h).
	c.t = c.t.Add(DefaultRenotify - time.Minute)
	if o := review(); o.Notify {
		t.Fatalf("before renotify: %+v", o)
	}
	c.t = c.t.Add(2 * time.Minute)
	if o := review(); !o.Notify {
		t.Fatalf("after renotify: %+v", o)
	}
	// Never notified (e.g. the earlier summary failed): notify.
	ctx, _ := svc.Start(context.Background(), job)
	Add(ctx, Problem{Kind: JiraFailed, Subject: "X-1: задача не найдена"})
	Commit(ctx) // not marked notified
	ctx, _ = svc.Start(context.Background(), job)
	Add(ctx, Problem{Kind: JiraFailed, Subject: "X-2: задача не найдена"}) // same fingerprint (digits)
	if outs := Commit(ctx); outs[0].New || !outs[0].Notify {
		t.Fatalf("never notified: %+v", outs)
	}
	// Nobody to notify: never.
	quiet, _ := testService(newMemStore(), c)
	ctx, _ = quiet.Start(context.Background(), job)
	Add(ctx, Problem{Kind: JiraFailed, Subject: "x"})
	if outs := Commit(ctx); !outs[0].New || outs[0].Notify {
		t.Fatalf("no users: %+v", outs)
	}
	// A failing store: no mention, no "new" metric, the review goes on.
	bad := newMemStore()
	bad.fail = errors.New("disk full")
	failing, rec := testService(bad, c, "alice")
	ctx, _ = failing.Start(context.Background(), job)
	Add(ctx, Problem{Kind: JiraFailed, Subject: "x"})
	if outs := Commit(ctx); outs[0].Notify || outs[0].New {
		t.Fatalf("failing store: %+v", outs)
	}
	if strings.Join(*rec, " ") != "jira_failed:seen" {
		t.Fatalf("metrics %v", *rec)
	}
}

func TestAddModel(t *testing.T) {
	st, c := newMemStore(), &clock{time.Now()}
	svc, _ := testService(st, c)
	ctx, run := svc.Start(context.Background(), job)
	if _, err := AddModel(ctx, "hack", "x", ""); err == nil || !strings.Contains(err.Error(), "tool_failed") {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := AddModel(ctx, "other", "  <b></b> ", ""); err == nil {
		t.Fatal("empty subject accepted")
	}
	for i := 0; i < MaxModelPerRun; i++ {
		k, err := AddModel(ctx, "tool_failed", "golangci-lint "+strings.Repeat("x", i+1)+" "+strings.Repeat("я", 200), strings.Repeat("d", 900))
		if err != nil || k != ModelToolFailed {
			t.Fatalf("#%d: %v %v", i, k, err)
		}
	}
	if _, err := AddModel(ctx, "other", "sixth", ""); !errors.Is(err, ErrModelLimit) {
		t.Fatalf("cap: %v", err)
	}
	ps := run.List()
	if len(ps) != MaxModelPerRun {
		t.Fatalf("collected %d", len(ps))
	}
	for _, p := range ps {
		if p.Source != SourceModel || len([]rune(p.Subject)) > MaxModelSubjectRunes || len([]rune(p.Detail)) > MaxDetailRunes {
			t.Errorf("model problem not capped: %d %d", len([]rune(p.Subject)), len([]rune(p.Detail)))
		}
	}
	// Bot problems do not count toward the model's cap.
	Add(ctx, Problem{Kind: BaseUnavailable, Subject: "setup"})
	if len(run.List()) != MaxModelPerRun+1 {
		t.Fatal("bot problem dropped")
	}
	// Without a collector: validation only.
	if _, err := AddModel(context.Background(), "no_access", "group/x", ""); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerPersistsOnErrorAndNilService(t *testing.T) {
	st, c := newMemStore(), &clock{time.Now()}
	svc, _ := testService(st, c)
	h := svc.Handler(func(ctx context.Context, _ core.Job) error {
		Add(ctx, Problem{Kind: InvalidOutput, Subject: "ответ ревью не по схеме"})
		return errors.New("boom")
	})
	if err := h(context.Background(), job); err == nil {
		t.Fatal("error swallowed")
	}
	if len(st.rows) != 1 {
		t.Fatalf("rows %d", len(st.rows))
	}
	// A cancelled job context still persists.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = svc.Handler(func(ctx context.Context, _ core.Job) error {
		Add(ctx, Problem{Kind: BaseUnavailable, Subject: "setup"})
		return ctx.Err()
	})(ctx, job)
	if len(st.rows) != 2 {
		t.Fatalf("rows after cancel %d", len(st.rows))
	}
	// Nil service: the handler is unchanged and every hook is a no-op.
	var nilSvc *Service
	called := false
	_ = nilSvc.Handler(func(ctx context.Context, _ core.Job) error {
		called = true
		Add(ctx, Problem{Kind: BaseUnavailable, Subject: "x"})
		if Commit(ctx) != nil || From(ctx) != nil {
			t.Error("collector without a service")
		}
		return nil
	})(context.Background(), job)
	if !called || nilSvc.Notify() != nil || nilSvc.PageLink() != "" || nilSvc.NotifyMode() != NotifySummary {
		t.Fatal("nil service")
	}
	nilSvc.Record(context.Background(), Problem{Kind: BaseUnavailable})
	nilSvc.MarkNotified(context.Background(), nil)
}

func TestRecordBackgroundAndPrune(t *testing.T) {
	st, c := newMemStore(), &clock{time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	svc := New(Options{Store: st, Now: c.now, Retention: 48 * time.Hour, PageURL: "https://bot.example.com/"})
	if svc.PageLink() != "https://bot.example.com/problems" {
		t.Fatalf("link %q", svc.PageLink())
	}
	svc.Record(context.Background(), Problem{Kind: SysmapFailed, ProjectPath: "g/a", Subject: "scan"})
	if len(st.rows) != 1 {
		t.Fatal("not recorded")
	}
	// The next persist more than a day later prunes rows older than the
	// retention.
	c.t = c.t.Add(72 * time.Hour)
	svc.Record(context.Background(), Problem{Kind: SysmapFailed, ProjectPath: "g/b", Subject: "scan"})
	if len(st.rows) != 1 {
		t.Fatalf("rows after prune: %d", len(st.rows))
	}
}

// A problem the review model reported never mentions the notify users
// (only bot-side problems do): an injected answer could mint new
// fingerprints on every push.
func TestModelProblemsNeverNotify(t *testing.T) {
	st, c := newMemStore(), &clock{time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	svc, _ := testService(st, c, "alice")
	ctx, run := svc.Start(context.Background(), job)
	for i := 0; i < MaxModelPerRun; i++ {
		if _, err := AddModel(ctx, ModelNames()[0], fmt.Sprintf("injected subject %c", 'a'+i), "x"); err != nil {
			t.Fatal(err)
		}
	}
	Add(ctx, Problem{Kind: ContextUnavailable, Subject: "group/payments"})
	outs := Commit(ctx)
	svc.Finish(ctx, run)
	if len(outs) != MaxModelPerRun+1 {
		t.Fatalf("outs %+v", outs)
	}
	for _, o := range outs {
		if want := o.Problem.Source != SourceModel; o.Notify != want || !o.New {
			t.Errorf("%s (%s): notify %v, want %v", o.Problem.Subject, o.Problem.Source, o.Notify, want)
		}
	}
}

// Resolve marks the stored problem of the job's project fixed, unless the
// same job has just recorded it; a recurrence makes it active again.
func TestResolve(t *testing.T) {
	ctx := context.Background()
	c := &clock{t: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)}
	st := newMemStore()
	svc, _ := testService(st, c)
	fp := Fingerprint(RepoConfigInvalid, job.ProjectPath, ".reviewbot.yaml")

	// A job records it, the next job finds the config fine.
	ctx1, run1 := svc.Start(ctx, job)
	Add(ctx1, Problem{Kind: RepoConfigInvalid, Subject: ".reviewbot.yaml", Detail: "line 4"})
	Resolve(ctx1, RepoConfigInvalid, ".reviewbot.yaml") // same job: left alone
	svc.Finish(ctx1, run1)
	if !st.rows[fp].ResolvedAt.IsZero() {
		t.Fatal("a problem recorded by the same job was resolved")
	}
	ctx2, run2 := svc.Start(ctx, job)
	Resolve(ctx2, RepoConfigInvalid, ".reviewbot.yaml")
	svc.Finish(ctx2, run2)
	if st.rows[fp].ResolvedAt.IsZero() {
		t.Fatal("the fixed problem stayed active")
	}
	// Another project's job does not touch it; no collector is a no-op.
	other := job
	other.ProjectPath = "group/other"
	ctx3, run3 := svc.Start(ctx, other)
	Resolve(ctx3, RepoConfigInvalid, ".reviewbot.yaml")
	svc.Finish(ctx3, run3)
	Resolve(ctx, RepoConfigInvalid, ".reviewbot.yaml")
	// Seen again: active.
	ctx4, run4 := svc.Start(ctx, job)
	Add(ctx4, Problem{Kind: RepoConfigInvalid, Subject: ".reviewbot.yaml", Detail: "line 5"})
	svc.Finish(ctx4, run4)
	if !st.rows[fp].ResolvedAt.IsZero() {
		t.Fatal("a recurrence did not make the problem active")
	}
}

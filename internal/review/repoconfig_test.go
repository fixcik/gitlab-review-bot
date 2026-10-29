package review_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// cfgGitLab serves .reviewbot.yaml per ref and keeps the notes the bot
// creates as discussions (the base fake does not), so the config note can be
// found again.
type cfgGitLab struct {
	*reviewtest.FakeGitLab
	fmu   sync.Mutex
	files map[string]string // ref -> content; missing = not found
	fail  map[string]bool   // ref -> GetRawFile fails
	calls map[string]int
}

func (g *cfgGitLab) GetRawFile(_ context.Context, _ int64, path, ref string) ([]byte, error) {
	g.fmu.Lock()
	defer g.fmu.Unlock()
	g.calls[ref]++
	if g.fail[ref] {
		return nil, fmt.Errorf("gitlab: HTTP 500")
	}
	c, ok := g.files[ref]
	if path != config.RepoConfigPath || !ok {
		return nil, core.ErrNotFound
	}
	return []byte(c), nil
}

func (g *cfgGitLab) set(ref, content string) {
	g.fmu.Lock()
	defer g.fmu.Unlock()
	g.files[ref] = content
}

func (g *cfgGitLab) del(ref string) {
	g.fmu.Lock()
	defer g.fmu.Unlock()
	delete(g.files, ref)
}

func (g *cfgGitLab) CreateNote(ctx context.Context, p, i int64, body string) (int64, error) {
	id, err := g.FakeGitLab.CreateNote(ctx, p, i, body)
	if err == nil {
		g.Mu.Lock()
		g.Discussions = append(g.Discussions, core.Discussion{ID: fmt.Sprintf("n%d", id),
			Notes: []core.Note{{ID: id, AuthorID: reviewtest.BotID, AuthorUsername: reviewtest.BotName, Body: body}}})
		g.Mu.Unlock()
	}
	return id, err
}

func (g *cfgGitLab) UpdateNote(ctx context.Context, p, i, noteID int64, body string) error {
	if err := g.FakeGitLab.UpdateNote(ctx, p, i, noteID, body); err != nil {
		return err
	}
	g.Mu.Lock()
	defer g.Mu.Unlock()
	for di := range g.Discussions {
		for ni := range g.Discussions[di].Notes {
			if g.Discussions[di].Notes[ni].ID == noteID {
				g.Discussions[di].Notes[ni].Body = body
			}
		}
	}
	return nil
}

type cfgHarness struct {
	*reviewtest.Harness
	cg    *cfgGitLab
	skips []string
	now   time.Time
}

// newCfgHarness: opt-in on, the config cache on (5 min) with a movable clock.
func newCfgHarness(t *testing.T) *cfgHarness {
	t.Helper()
	h := &cfgHarness{Harness: reviewtest.New(t), now: reviewtest.TestNow}
	h.cg = &cfgGitLab{FakeGitLab: h.GL, files: map[string]string{}, fail: map[string]bool{}, calls: map[string]int{}}
	h.Cfg.Review.RequireRepoConfig = true
	h.Cfg.Review.Lint.Enabled = false
	h.Deps.RepoConfigTTL = review.DefaultRepoConfigTTL
	h.Deps.Now = func() time.Time { return h.now }
	h.Deps.OnSkip = func(reason string) { h.skips = append(h.skips, reason) }
	h.Env().GitLab = h.cg
	return h
}

func (h *cfgHarness) review(t *testing.T, job core.Job) {
	t.Helper()
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

// configNotes returns the bodies of the config notes, oldest first.
func (h *cfgHarness) configNotes() []string {
	h.GL.Mu.Lock()
	defer h.GL.Mu.Unlock()
	var out []string
	for _, id := range h.GL.CreatedNotes {
		if b := h.GL.Notes[id]; strings.HasPrefix(b, marker.ConfigNote) {
			out = append(out, b)
		}
	}
	return out
}

func (h *cfgHarness) push(sha string) {
	h.GL.Mu.Lock()
	defer h.GL.Mu.Unlock()
	h.GL.MR.DiffRefs.HeadSHA = sha
}

const (
	headSHA = "head111111111111"
	baseSHA = "base000000000000"
)

func TestOptInAutoReviewSkipsSilently(t *testing.T) {
	h := newCfgHarness(t)
	h.review(t, reviewtest.AutoJob())
	if len(h.Runner.Reqs) != 0 || len(h.GL.Replies) != 0 || len(h.GL.CreatedNotes) != 0 || len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("not opted in: runs=%d replies=%v notes=%v", len(h.Runner.Reqs), h.GL.Replies, h.GL.CreatedNotes)
	}
	if len(h.skips) != 1 || h.skips[0] != review.SkipNoRepoConfig {
		t.Fatalf("skips %v", h.skips)
	}
	// The next event within the TTL does not read the target branch again.
	h.push("head222222222222")
	h.review(t, reviewtest.AutoJob())
	if h.cg.calls["main"] != 1 {
		t.Fatalf("target branch read %d times, want 1 (cached)", h.cg.calls["main"])
	}
	// After the TTL it does; a config added meanwhile opts the repo in.
	h.now = h.now.Add(review.DefaultRepoConfigTTL + time.Second)
	h.cg.set("main", "")
	h.push("head333333333333")
	h.review(t, reviewtest.AutoJob())
	if h.cg.calls["main"] != 2 || len(h.Runner.Reqs) != 1 {
		t.Fatalf("after TTL: reads %d runs %d", h.cg.calls["main"], len(h.Runner.Reqs))
	}
	if len(h.configNotes()) != 0 {
		t.Fatalf("an empty valid config needs no note: %v", h.configNotes())
	}
}

func TestOptInOffReviewsWithoutConfig(t *testing.T) {
	h := newCfgHarness(t)
	h.Cfg.Review.RequireRepoConfig = false
	h.review(t, reviewtest.AutoJob())
	if len(h.Runner.Reqs) != 1 || len(h.skips) != 0 {
		t.Fatalf("require_repo_config: false must review every repository: runs %d skips %v", len(h.Runner.Reqs), h.skips)
	}
}

func TestOptInCommandsReplyOnce(t *testing.T) {
	for _, job := range []core.Job{reviewtest.ManualJob(core.JobReview), reviewtest.ManualJob(core.JobFullReview), reviewtest.ChatJob("что это?"),
		reviewtest.ManualJob(core.JobPause), reviewtest.ManualJob(core.JobConfig), reviewtest.ManualJob(core.JobCheck)} {
		t.Run(string(job.Kind), func(t *testing.T) {
			h := newCfgHarness(t)
			h.review(t, job)
			if len(h.GL.Replies) != 1 || len(h.Runner.Reqs) != 0 {
				t.Fatalf("replies %v runs %d", h.GL.Replies, len(h.Runner.Reqs))
			}
			r := h.GL.Replies[0].Body
			if !strings.Contains(r, "Репозиторий не подключён к ревью") || !strings.Contains(r, "`main`") ||
				!strings.Contains(r, ".reviewbot.example.yaml") {
				t.Fatalf("reply %q", r)
			}
			if len(h.configNotes()) != 0 {
				t.Fatal("not opted in: no config note")
			}
		})
	}
	h := newCfgHarness(t)
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobHelp)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL).Body; !strings.Contains(r, "Команды бота") || !strings.Contains(r, "Репозиторий не подключён к ревью") {
		t.Fatalf("help %q", r)
	}
}

func TestInvalidTargetConfigNoteLifecycle(t *testing.T) {
	h := newCfgHarness(t)
	h.cg.set("main", "languag: ru\nlint:\n  disabel: [x]\ntbd:\n  max_lines: -1\n")
	svc := problems.New(problems.Options{})
	ctx, run := svc.Start(context.Background(), reviewtest.AutoJob())
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(ctx, reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || len(h.GL.Replies) != 0 {
		t.Fatalf("invalid config: runs %d replies %v", len(h.Runner.Reqs), h.GL.Replies)
	}
	notes := h.configNotes()
	if len(notes) != 1 {
		t.Fatalf("config notes %d", len(notes))
	}
	n := notes[0]
	for _, want := range []string{"state=error", "ревью не выполняется", "`main`", "строка 1: `languag`", "language",
		"строка 3: `lint.disabel`", "tbd.max_lines must be >= 0", "validate-repo-config", "docs/configuration.md"} {
		if !strings.Contains(n, want) {
			t.Errorf("note misses %q:\n%s", want, n)
		}
	}
	if len(h.skips) != 1 || h.skips[0] != review.SkipRepoConfigInvalid {
		t.Fatalf("skips %v", h.skips)
	}
	ps := run.List()
	if len(ps) != 1 || ps[0].Kind != problems.RepoConfigInvalid || ps[0].Subject != config.RepoConfigPath ||
		!strings.Contains(ps[0].Detail, "ветка main") || !strings.Contains(ps[0].Detail, "lint.disabel") {
		t.Fatalf("problems %+v", ps)
	}

	// More pushes: the same note, no duplicate, no needless edit.
	for _, sha := range []string{"head222222222222", "head333333333333"} {
		h.push(sha)
		h.review(t, reviewtest.AutoJob())
	}
	if len(h.configNotes()) != 1 || len(h.GL.UpdatedNotes) != 0 {
		t.Fatalf("idempotent: notes %d updates %v", len(h.configNotes()), h.GL.UpdatedNotes)
	}

	// A command: the reply in its thread names the error; still one note.
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	r := reviewtest.LastReply(t, h.GL).Body
	if !strings.Contains(r, "Не удалось разобрать `.reviewbot.yaml`") || !strings.Contains(r, "languag") || !strings.Contains(r, "и ещё 2") {
		t.Fatalf("reply %q", r)
	}
	if len(h.configNotes()) != 1 {
		t.Fatal("a command must not duplicate the note")
	}

	// Fixed on the target branch: the next review edits the note and runs.
	h.cg.set("main", "language: ru\n")
	h.now = h.now.Add(review.DefaultRepoConfigTTL)
	h.push("head444444444444")
	h.review(t, reviewtest.AutoJob())
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("fixed config: runs %d", len(h.Runner.Reqs))
	}
	if len(h.GL.UpdatedNotes) == 0 {
		t.Fatal("note not updated")
	}
	n = h.GL.Notes[h.GL.UpdatedNotes[0]]
	if !strings.Contains(n, "state=ok") || !strings.Contains(n, "✅ Конфиг исправлен") || len(h.configNotes()) != 1 {
		t.Fatalf("fixed note:\n%s", n)
	}
	// Valid from now on: the fixed note is left alone.
	updates := len(h.GL.UpdatedNotes)
	h.push("head555555555555")
	h.review(t, reviewtest.AutoJob())
	if len(h.GL.UpdatedNotes) != updates+countSummaryUpdates(h, updates) {
		t.Fatalf("the fixed note must not be edited again: %v", h.GL.UpdatedNotes)
	}
}

// countSummaryUpdates counts summary note edits after the first n updates.
func countSummaryUpdates(h *cfgHarness, n int) int {
	c := 0
	for _, id := range h.GL.UpdatedNotes[n:] {
		if !strings.HasPrefix(h.GL.Notes[id], marker.ConfigNote) {
			c++
		}
	}
	return c
}

func TestMRChangingConfigValidatesHead(t *testing.T) {
	h := newCfgHarness(t)
	h.cg.set("main", "language: ru\n")
	h.cg.set(baseSHA, "language: ru\n")
	h.cg.set(headSHA, "language: ru\nauto_reviw: false\n")
	h.review(t, reviewtest.AutoJob())
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("a valid target config: the review runs (%d)", len(h.Runner.Reqs))
	}
	notes := h.configNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "в этом MR невалиден") || !strings.Contains(notes[0], "строка 2: `auto_reviw`") ||
		!strings.Contains(notes[0], "auto_review") || !strings.Contains(notes[0], "исправьте до мержа") {
		t.Fatalf("notes %v", notes)
	}
	var sum string
	for _, id := range h.GL.CreatedNotes {
		if strings.Contains(h.GL.Notes[id], summary.Title) {
			sum = h.GL.Notes[id]
		}
	}
	if !strings.Contains(sum, "`.reviewbot.yaml` в этом MR невалиден") {
		t.Fatalf("summary has no config line:\n%s", sum)
	}

	// Fixed in the MR: the note says so.
	h.push("head222222222222")
	h.cg.set("head222222222222", "language: ru\nauto_review: false\n")
	h.Repos.Ancestors = map[string]bool{headSHA: true}
	h.review(t, reviewtest.AutoJob())
	notes = h.configNotes()
	if len(notes) != 1 || len(h.GL.UpdatedNotes) == 0 {
		t.Fatalf("notes %v updates %v", notes, h.GL.UpdatedNotes)
	}
	if n := h.GL.Notes[h.GL.UpdatedNotes[0]]; !strings.Contains(n, "✅ Новый `.reviewbot.yaml` валиден.") || !strings.Contains(n, "state=ok") {
		t.Fatalf("fixed head note:\n%s", n)
	}
}

func TestMRNotTouchingConfigNoNote(t *testing.T) {
	h := newCfgHarness(t)
	// The target moved on; the MR's branch still has the old (base) file.
	h.cg.set("main", "language: en\n")
	h.cg.set(baseSHA, "language: ru\nfoo: 1\n")
	h.cg.set(headSHA, "language: ru\nfoo: 1\n")
	h.review(t, reviewtest.AutoJob())
	if len(h.Runner.Reqs) != 1 || len(h.configNotes()) != 0 {
		t.Fatalf("runs %d notes %v", len(h.Runner.Reqs), h.configNotes())
	}
	// The same file as the target: nothing to check either.
	h2 := newCfgHarness(t)
	h2.cg.set("main", "language: en\n")
	h2.cg.set(headSHA, "language: en\n")
	h2.review(t, reviewtest.AutoJob())
	if len(h2.configNotes()) != 0 || h2.cg.calls[baseSHA] != 0 {
		t.Fatalf("notes %v base reads %d", h2.configNotes(), h2.cg.calls[baseSHA])
	}
}

func TestOnboardingMRInRepoWithoutConfig(t *testing.T) {
	h := newCfgHarness(t)
	h.cg.set(headSHA, "language: ru\nlnt: {}\n")
	h.review(t, reviewtest.AutoJob())
	if len(h.Runner.Reqs) != 0 || len(h.GL.Replies) != 0 {
		t.Fatalf("not opted in yet: no review (runs %d)", len(h.Runner.Reqs))
	}
	notes := h.configNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "в этом MR невалиден") || !strings.Contains(notes[0], "подключится к ревью после мержа") ||
		!strings.Contains(notes[0], "`lnt`") {
		t.Fatalf("onboarding error note %v", notes)
	}
	h.push("head222222222222")
	h.cg.set("head222222222222", "language: ru\nlint: {}\n")
	h.review(t, reviewtest.AutoJob())
	if len(h.configNotes()) != 1 || len(h.GL.UpdatedNotes) != 1 {
		t.Fatalf("notes %d updates %v", len(h.configNotes()), h.GL.UpdatedNotes)
	}
	if n := h.GL.Notes[h.GL.UpdatedNotes[0]]; !strings.Contains(n, "✅ Новый `.reviewbot.yaml` валиден — ревью этого репозитория начнётся после мержа") {
		t.Fatalf("onboarding ok note:\n%s", n)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatal("still no review before the merge")
	}

	// A valid onboarding MR from the start: one "valid" note.
	h2 := newCfgHarness(t)
	h2.cg.set(headSHA, "")
	h2.review(t, reviewtest.AutoJob())
	if n := h2.configNotes(); len(n) != 1 || !strings.Contains(n[0], "начнётся после мержа MR в `main`") {
		t.Fatalf("notes %v", n)
	}
}

func TestRepoConfigCacheDoesNotKeepErrors(t *testing.T) {
	h := newCfgHarness(t)
	h.cg.fail["main"] = true
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("a GitLab error fails the job (retried): %v", err)
	}
	h.cg.fail["main"] = false
	h.cg.set("main", "")
	h.review(t, reviewtest.AutoJob())
	if h.cg.calls["main"] != 2 || len(h.Runner.Reqs) != 1 {
		t.Fatalf("an error must not be cached: reads %d runs %d", h.cg.calls["main"], len(h.Runner.Reqs))
	}
}

// cappedCfgGitLab also serves the size-capped read (*gitlab.Client).
type cappedCfgGitLab struct {
	*cfgGitLab
	maxes []int64
}

func (g *cappedCfgGitLab) GetRawFileMax(ctx context.Context, p int64, path, ref string, max int64) ([]byte, error) {
	b, err := g.cfgGitLab.GetRawFile(ctx, p, path, ref)
	g.fmu.Lock()
	g.maxes = append(g.maxes, max)
	g.fmu.Unlock()
	if err == nil && max > 0 && int64(len(b)) > max {
		return nil, fmt.Errorf("%w: raw file", gitlab.ErrTooLarge)
	}
	return b, err
}

// A .reviewbot.yaml over the validation limit is never kept in the cache:
// it is cached as "too big" and still reported as invalid.
func TestRepoConfigCacheKeepsNoOversizedBytes(t *testing.T) {
	h := newCfgHarness(t)
	big := "# " + strings.Repeat("x", config.MaxRepoConfigBytes) + "\n"
	h.cg.set("abc", big)
	for i := 0; i < 2; i++ {
		f, err := h.R().ReadRepoConfig(context.Background(), reviewtest.PID, "abc")
		if err != nil || !f.Found || !f.TooBig || f.Valid || len(f.Issues) != 1 || !strings.Contains(f.Issues[0].Message, "larger than 256 KiB") {
			t.Fatalf("read %d: %+v %v", i, f, err)
		}
	}
	if h.cg.calls["abc"] != 1 {
		t.Fatalf("not cached: %d calls", h.cg.calls["abc"])
	}
	e, cacheBytes := h.R().CfgCacheEntry(reviewtest.PID, "abc")
	if !e.TooBig || e.Raw != nil || cacheBytes != 0 {
		t.Fatalf("cache entry %+v bytes %d", e.Found, cacheBytes)
	}

	// With the capped read the body is not even read past the limit.
	h2 := newCfgHarness(t)
	cg := &cappedCfgGitLab{cfgGitLab: h2.cg}
	h2.Env().GitLab = cg
	cg.set("abc", big)
	cg.set("small", "enabled: true\n")
	if f, err := h2.R().ReadRepoConfig(context.Background(), reviewtest.PID, "abc"); err != nil || !f.TooBig || f.Valid {
		t.Fatalf("capped: %+v %v", f, err)
	}
	if f, err := h2.R().ReadRepoConfig(context.Background(), reviewtest.PID, "small"); err != nil || !f.Valid || string(f.Raw) != "enabled: true\n" {
		t.Fatalf("small: %+v %v", f, err)
	}
	if len(cg.maxes) != 2 || cg.maxes[0] != config.MaxRepoConfigBytes {
		t.Fatalf("caps %v", cg.maxes)
	}
}

// The repo_config_invalid problem is "fixed" once a later job of the project
// finds the target branch's config valid, and active again if it breaks again.
func TestRepoConfigProblemResolvedWhenConfigIsFixed(t *testing.T) {
	h := newCfgHarness(t)
	st := &reviewtest.ProbStore{Rows: map[string]*problems.Record{}}
	svc := problems.New(problems.Options{Store: st})
	run := func() {
		t.Helper()
		ctx, r := svc.Start(context.Background(), reviewtest.AutoJob())
		h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
		if err := h.R().Handle(ctx, reviewtest.AutoJob()); err != nil {
			t.Fatal(err)
		}
		svc.Finish(ctx, r)
	}
	row := func() *problems.Record {
		t.Helper()
		for _, r := range st.Rows {
			if r.Kind == problems.RepoConfigInvalid {
				return r
			}
		}
		t.Fatal("no repo_config_invalid problem")
		return nil
	}
	h.cg.set("main", "languag: ru\n")
	run()
	if !row().ResolvedAt.IsZero() {
		t.Fatal("a fresh problem is resolved")
	}
	h.cg.set("main", "language: ru\n")
	h.now = h.now.Add(10 * time.Minute) // past the config cache
	h.push("head222222222222")
	run()
	if row().ResolvedAt.IsZero() {
		t.Fatal("the config is fixed but the problem is still active")
	}
	h.cg.set("main", "languag: ru\n")
	h.now = h.now.Add(10 * time.Minute)
	h.push("head333333333333")
	run()
	if !row().ResolvedAt.IsZero() {
		t.Fatal("the config broke again but the problem stayed fixed")
	}
}

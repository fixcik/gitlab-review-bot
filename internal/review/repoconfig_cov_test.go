package review_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// faultCfgGitLab fails the config note's GitLab calls on demand: listing the
// discussions, and creating / updating a note that carries the config note
// marker (other notes, e.g. the summary, go through).
type faultCfgGitLab struct {
	*cfgGitLab
	failList   bool
	failCreate bool
	updateErr  error
	updates    []int64 // config note update attempts
	lists      int     // ListDiscussions calls
}

func (g *faultCfgGitLab) ListDiscussions(ctx context.Context, p, i int64) ([]core.Discussion, error) {
	g.lists++
	if g.failList {
		return nil, fmt.Errorf("gitlab: HTTP 502")
	}
	return g.cfgGitLab.ListDiscussions(ctx, p, i)
}

func (g *faultCfgGitLab) CreateNote(ctx context.Context, p, i int64, body string) (int64, error) {
	if g.failCreate && strings.HasPrefix(body, marker.ConfigNote) {
		return 0, fmt.Errorf("gitlab: HTTP 500")
	}
	return g.cfgGitLab.CreateNote(ctx, p, i, body)
}

func (g *faultCfgGitLab) UpdateNote(ctx context.Context, p, i, noteID int64, body string) error {
	if strings.HasPrefix(body, marker.ConfigNote) {
		g.updates = append(g.updates, noteID)
		if g.updateErr != nil {
			return g.updateErr
		}
	}
	return g.cfgGitLab.UpdateNote(ctx, p, i, noteID, body)
}

func newFaultCfgHarness(t *testing.T) (*cfgHarness, *faultCfgGitLab) {
	t.Helper()
	h := newCfgHarness(t)
	fg := &faultCfgGitLab{cfgGitLab: h.cg}
	h.Env().GitLab = fg
	return h, fg
}

func (h *cfgHarness) mr() *core.MergeRequest {
	h.GL.Mu.Lock()
	defer h.GL.Mu.Unlock()
	mr := h.GL.MR
	return &mr
}

// The repository config as the features see it (env.RepoConfig): a GitLab
// failure is an error everywhere (the job is retried) and never answered;
// a refusing config is a *repoConfigError (Effective), skipped silently
// (Auto) or answered once (Command).
func TestRepoConfigAccessOutcomes(t *testing.T) {
	const invalid = "languag: ru\n"
	cases := []struct {
		name    string
		setup   func(h *cfgHarness)
		effErr  string // "" = no error
		optedIn bool   // NotOptedIn(err) is false
		issues  string // key of the only issue unwrapped from the error
		gitlab  bool   // a GitLab failure: Auto and Command return it
		reply   string // Command's reply ("" = none)
		note    bool   // Command keeps a config note
	}{
		{name: "gitlab failure", setup: func(h *cfgHarness) { h.cg.fail["main"] = true },
			effErr: "read .reviewbot.yaml: gitlab: HTTP 500", optedIn: true, gitlab: true},
		{name: "not opted in", setup: func(*cfgHarness) {},
			effErr: "repository not opted in: no .reviewbot.yaml on main", reply: "Репозиторий не подключён к ревью"},
		{name: "invalid", setup: func(h *cfgHarness) { h.cg.set("main", invalid) },
			effErr: ".reviewbot.yaml: line 1", optedIn: true, issues: "languag", reply: "Не удалось разобрать `.reviewbot.yaml` в целевой ветке `main`", note: true},
		{name: "valid", setup: func(h *cfgHarness) { h.cg.set("main", "language: en\n") }, optedIn: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			h := newCfgHarness(t)
			c.setup(h)
			rc := h.Env().RepoConfig

			eff, err := rc.Effective(ctx, reviewtest.PID, h.mr())
			if c.effErr == "" {
				if err != nil || eff.Language != "en" {
					t.Fatalf("Effective: %+v %v", eff.Language, err)
				}
			} else if err == nil || !strings.HasPrefix(err.Error(), c.effErr) {
				t.Fatalf("Effective err %v, want prefix %q", err, c.effErr)
			}
			if got := rc.NotOptedIn(err); got == c.optedIn {
				t.Fatalf("NotOptedIn(%v) = %v", err, got)
			}
			if c.issues != "" {
				is := config.RepoIssuesOf(err) // through repoConfigError.Unwrap
				if len(is) != 1 || is[0].Key != c.issues || is[0].Line != 1 || err.Error() != is.Error() {
					t.Fatalf("issues %+v of %q", is, err)
				}
			}

			eff, ok, err := rc.Auto(ctx, reviewtest.PID, h.mr())
			switch {
			case c.effErr == "":
				if !ok || err != nil || eff.Language != "en" {
					t.Fatalf("Auto valid: %v %v %q", ok, err, eff.Language)
				}
			case c.gitlab:
				if ok || err == nil || !strings.Contains(err.Error(), "HTTP 500") {
					t.Fatalf("Auto on a GitLab failure: %v %v", ok, err)
				}
			default:
				if ok || err != nil {
					t.Fatalf("Auto on a refusing config skips silently: %v %v", ok, err)
				}
			}
			if len(h.GL.Replies) != 0 || len(h.configNotes()) != 0 {
				t.Fatalf("Auto answered: replies %v notes %v", h.GL.Replies, h.configNotes())
			}

			eff, ok, err = rc.Command(ctx, reviewtest.ManualJob(core.JobReview), h.mr())
			switch {
			case c.effErr == "":
				if !ok || err != nil || eff.Language != "en" {
					t.Fatalf("Command valid: %v %v %q", ok, err, eff.Language)
				}
			case c.gitlab:
				if ok || err == nil || !strings.Contains(err.Error(), "HTTP 500") {
					t.Fatalf("Command on a GitLab failure: %v %v", ok, err)
				}
			default:
				if ok || err != nil {
					t.Fatalf("Command on a refusing config: %v %v", ok, err)
				}
			}
			if c.reply == "" {
				if len(h.GL.Replies) != 0 {
					t.Fatalf("unexpected replies %v", h.GL.Replies)
				}
			} else if len(h.GL.Replies) != 1 || h.GL.Replies[0].DiscussionID != "cmd-disc" || !strings.Contains(h.GL.Replies[0].Body, c.reply) {
				t.Fatalf("replies %v, want one with %q", h.GL.Replies, c.reply)
			}
			if notes := h.configNotes(); (len(notes) == 1) != c.note || len(notes) > 1 {
				t.Fatalf("config notes %v", notes)
			}
		})
	}
}

// An MR without a head SHA has no own config to check: only the target
// branch is read, and the note lists the target branch's errors alone.
func TestDenyRepoConfigWithoutHeadSHA(t *testing.T) {
	h := newCfgHarness(t)
	h.cg.set("main", "lnt: {}\n")
	h.cg.set(baseSHA, "lint: {}\n")
	h.push("")
	_, ok, err := h.Env().RepoConfig.Command(context.Background(), reviewtest.ManualJob(core.JobReview), h.mr())
	if ok || err != nil {
		t.Fatalf("Command: %v %v", ok, err)
	}
	for ref, n := range h.cg.calls {
		if ref != "main" {
			t.Errorf("read %q %d times; only the target branch may be read", ref, n)
		}
	}
	notes := h.configNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "`lnt`") || strings.Contains(notes[0], "в этом MR") {
		t.Fatalf("notes %v", notes)
	}
}

// The merge base's file cannot be read: the MR's own config is not checked
// (logged), the rest goes on as if the MR did not touch it.
func TestHeadConfigBaseReadFails(t *testing.T) {
	t.Run("review", func(t *testing.T) {
		h := newCfgHarness(t)
		h.cg.set("main", "language: ru\n")
		h.cg.set(headSHA, "language: ru\nauto_reviw: false\n")
		h.cg.fail[baseSHA] = true
		h.review(t, reviewtest.AutoJob())
		if len(h.Runner.Reqs) != 1 || len(h.configNotes()) != 0 || h.cg.calls[baseSHA] != 1 {
			t.Fatalf("runs %d notes %v base reads %d", len(h.Runner.Reqs), h.configNotes(), h.cg.calls[baseSHA])
		}
		for _, id := range h.GL.CreatedNotes {
			if strings.Contains(h.GL.Notes[id], "в этом MR невалиден") {
				t.Fatalf("unchecked head config reported:\n%s", h.GL.Notes[id])
			}
		}
		// The failure is not cached: the next review checks the head.
		h.cg.fail[baseSHA] = false
		h.cg.set(baseSHA, "language: ru\n")
		h.review(t, reviewtest.AutoJob())
		if n := h.configNotes(); len(n) != 1 || !strings.Contains(n[0], "`auto_reviw`") {
			t.Fatalf("after the base is readable: notes %v", n)
		}
	})
	t.Run("command", func(t *testing.T) {
		h := newCfgHarness(t)
		h.cg.set("main", "lnt: {}\n")
		h.cg.set(headSHA, "tbd: 1\n")
		h.cg.fail[baseSHA] = true
		_, ok, err := h.Env().RepoConfig.Command(context.Background(), reviewtest.ManualJob(core.JobReview), h.mr())
		if ok || err != nil || len(h.GL.Replies) != 1 {
			t.Fatalf("Command: %v %v replies %v", ok, err, h.GL.Replies)
		}
		notes := h.configNotes()
		if len(notes) != 1 || !strings.Contains(notes[0], "`lnt`") || strings.Contains(notes[0], "тоже невалиден") || strings.Contains(notes[0], "`tbd`") {
			t.Fatalf("notes %v", notes)
		}
	})
}

// GitLab failures around the config note never fail the job: the command is
// still answered; an update that fails for another reason than a deleted
// note is not turned into a second note.
func TestConfigNoteGitLabFailures(t *testing.T) {
	const oldNote = marker.ConfigNote + " state=error -->\nold errors"
	seed := func(h *cfgHarness) {
		h.GL.Mu.Lock()
		defer h.GL.Mu.Unlock()
		h.GL.Discussions = append(h.GL.Discussions,
			core.Discussion{ID: "sys", Notes: []core.Note{{ID: 70, System: true, Body: oldNote}}},
			core.Discussion{ID: "empty"},
			core.Discussion{ID: "other", Notes: []core.Note{{ID: 71, AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: oldNote}}},
			core.Discussion{ID: "cfg", Notes: []core.Note{reviewtest.BotNote(77, oldNote, nil)}},
		)
		h.GL.Notes[77] = oldNote
	}
	cases := []struct {
		name    string
		seed    bool
		fault   func(g *faultCfgGitLab)
		updates int    // config note update attempts (of note 77)
		created bool   // a config note is created
		kept    string // body of note 77 afterwards
	}{
		{name: "list fails", seed: true, fault: func(g *faultCfgGitLab) { g.failList = true }, kept: oldNote},
		{name: "create fails", fault: func(g *faultCfgGitLab) { g.failCreate = true }},
		{name: "update fails", seed: true, fault: func(g *faultCfgGitLab) { g.updateErr = fmt.Errorf("gitlab: HTTP 500") }, updates: 1, kept: oldNote},
		{name: "note deleted", seed: true, fault: func(g *faultCfgGitLab) { g.updateErr = fmt.Errorf("gitlab: %w", core.ErrNotFound) }, updates: 1, created: true, kept: oldNote},
		{name: "update ok", seed: true, fault: func(*faultCfgGitLab) {}, updates: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fg := newFaultCfgHarness(t)
			h.cg.set("main", "lnt: {}\n")
			if c.seed {
				seed(h)
			}
			c.fault(fg)
			_, ok, err := h.Env().RepoConfig.Command(context.Background(), reviewtest.ManualJob(core.JobReview), h.mr())
			if ok || err != nil || len(h.GL.Replies) != 1 || !strings.Contains(h.GL.Replies[0].Body, "Не удалось разобрать") {
				t.Fatalf("Command: %v %v replies %v", ok, err, h.GL.Replies)
			}
			if len(fg.updates) != c.updates || (c.updates > 0 && fg.updates[0] != 77) {
				t.Fatalf("config note updates %v, want %d of note 77", fg.updates, c.updates)
			}
			if got := len(h.configNotes()) == 1; got != c.created || len(h.configNotes()) > 1 {
				t.Fatalf("created config notes %v", h.configNotes())
			}
			if c.created && !strings.Contains(h.configNotes()[0], "`lnt`") {
				t.Fatalf("created note:\n%s", h.configNotes()[0])
			}
			if c.seed {
				n := h.GL.Notes[77]
				if c.kept != "" && n != c.kept {
					t.Fatalf("note 77 changed:\n%s", n)
				}
				if c.kept == "" && (!strings.Contains(n, "`lnt`") || !strings.Contains(n, "state=error")) {
					t.Fatalf("note 77 not updated:\n%s", n)
				}
			}
		})
	}
}

// The oldest bot config note is the one kept up to date (a race made two).
func TestConfigNoteUpdatesOldest(t *testing.T) {
	h, fg := newFaultCfgHarness(t)
	h.cg.set("main", "lnt: {}\n")
	stale := marker.ConfigNote + " state=error -->\nstale"
	h.GL.Discussions = append(h.GL.Discussions,
		core.Discussion{ID: "b", Notes: []core.Note{reviewtest.BotNote(80, stale, nil)}},
		core.Discussion{ID: "a", Notes: []core.Note{reviewtest.BotNote(78, stale, nil)}},
		core.Discussion{ID: "c", Notes: []core.Note{reviewtest.BotNote(79, stale, nil)}},
	)
	if _, _, err := h.Env().RepoConfig.Command(context.Background(), reviewtest.ManualJob(core.JobReview), h.mr()); err != nil {
		t.Fatal(err)
	}
	if len(fg.updates) != 1 || fg.updates[0] != 78 || len(h.configNotes()) != 0 {
		t.Fatalf("updates %v created %v", fg.updates, h.configNotes())
	}
}

// The target branch's config is broken and the MR touches it too: the note
// says whether the MR fixes it or is broken as well; no review either way.
func TestInvalidTargetWithMRConfig(t *testing.T) {
	cases := []struct {
		name, head string
		want       []string
		not        []string
	}{
		{name: "mr fixes it", head: "lint: {}\n",
			want: []string{"state=error", "`lnt`", "✅ В этом MR `.reviewbot.yaml` исправлен — ревью заработает после его мержа."},
			not:  []string{"тоже невалиден"}},
		{name: "mr broken too", head: "tbd: 1\n",
			want: []string{"state=error", "`lnt`", "⚠️ `.reviewbot.yaml` в этом MR тоже невалиден:\n\n- строка 1: `tbd`", "Как исправить"},
			not:  []string{"исправлен —"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCfgHarness(t)
			h.cg.set("main", "lnt: {}\n")
			h.cg.set(baseSHA, "lnt: {}\n")
			h.cg.set(headSHA, c.head)
			h.review(t, reviewtest.AutoJob())
			if len(h.Runner.Reqs) != 0 {
				t.Fatalf("invalid target config: runs %d", len(h.Runner.Reqs))
			}
			notes := h.configNotes()
			if len(notes) != 1 {
				t.Fatalf("notes %v", notes)
			}
			for _, w := range c.want {
				if !strings.Contains(notes[0], w) {
					t.Errorf("note misses %q:\n%s", w, notes[0])
				}
			}
			for _, w := range c.not {
				if strings.Contains(notes[0], w) {
					t.Errorf("note has %q:\n%s", w, notes[0])
				}
			}
		})
	}
}

// A refused command: the reply names the first issue and counts the rest,
// the job is marked denied (🚫) and the problem lists every issue.
func TestDenyRepoConfigReplyAndProblem(t *testing.T) {
	cases := []struct {
		name, file, more string
	}{
		{name: "one issue", file: "languag: ru\n"},
		{name: "two issues", file: "languag: ru\nlnt: {}\n", more: " (и ещё 1)"},
		{name: "three issues", file: "languag: ru\nlnt: {}\ntbd: 1\n", more: " (и ещё 2)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCfgHarness(t)
			h.cg.set("main", c.file)
			job := reviewtest.ManualJob(core.JobReview)
			marks := &jobctx.Marks{}
			ctx, run := problems.New(problems.Options{}).Start(jobctx.WithMarks(context.Background(), marks), job)
			if _, ok, err := h.Env().RepoConfig.Command(ctx, job, h.mr()); ok || err != nil {
				t.Fatalf("Command: %v %v", ok, err)
			}
			_, issues := config.ValidateRepo([]byte(c.file))
			want := "Не удалось разобрать `.reviewbot.yaml` в целевой ветке `main`: " + issues[0].String() + c.more + ". Ревью не выполняется"
			if r := reviewtest.LastReply(t, h.GL).Body; !strings.HasPrefix(r, want) {
				t.Fatalf("reply %q, want prefix %q", r, want)
			}
			if denied, _ := marks.Get(); !denied {
				t.Fatal("refused command not marked denied")
			}
			parts := make([]string, len(issues))
			for i, is := range issues {
				parts[i] = is.String()
			}
			ps := run.List()
			if len(ps) != 1 || ps[0].Kind != problems.RepoConfigInvalid || ps[0].Detail != "ветка main: "+strings.Join(parts, "; ") {
				t.Fatalf("problems %+v", ps)
			}
		})
	}
}

// The review gate marks a refused manual review denied, never an automatic
// one; a repository that is not opted in and brings no config costs no
// discussion listing; a reviewer without an OnSkip hook skips fine.
func TestReviewConfigGateRefusals(t *testing.T) {
	cases := []struct {
		name   string
		job    core.Job
		file   string
		denied bool
		reply  string
		lists  int
	}{
		{name: "manual not opted in", job: reviewtest.ManualJob(core.JobReview), denied: true, reply: "не подключён"},
		{name: "manual invalid", job: reviewtest.ManualJob(core.JobReview), file: "languag: ru\n", denied: true, reply: "Не удалось разобрать", lists: 1},
		{name: "auto not opted in", job: reviewtest.AutoJob()},
		{name: "auto invalid", job: reviewtest.AutoJob(), file: "languag: ru\n", lists: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fg := newFaultCfgHarness(t)
			if c.file != "" {
				h.cg.set("main", c.file)
			}
			marks := &jobctx.Marks{}
			if err := h.R().HandleReview(jobctx.WithMarks(context.Background(), marks), c.job); err != nil {
				t.Fatal(err)
			}
			if denied, _ := marks.Get(); denied != c.denied {
				t.Fatalf("denied %v, want %v", denied, c.denied)
			}
			if c.reply == "" && len(h.GL.Replies) != 0 || c.reply != "" && (len(h.GL.Replies) != 1 || !strings.Contains(h.GL.Replies[0].Body, c.reply)) {
				t.Fatalf("replies %v", h.GL.Replies)
			}
			if fg.lists != c.lists || len(h.Runner.Reqs) != 0 {
				t.Fatalf("discussion listings %d (want %d), runs %d", fg.lists, c.lists, len(h.Runner.Reqs))
			}
		})
	}

	h := reviewtest.New(t)
	h.Cfg.Review.RequireRepoConfig = true
	if err := h.R().HandleReview(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatal("not opted in: no review")
	}
}

// A file of exactly config.MaxRepoConfigBytes is read and kept; one byte
// more is "too big".
func TestRepoConfigSizeBoundary(t *testing.T) {
	h := newCfgHarness(t)
	exact := "#" + strings.Repeat("x", config.MaxRepoConfigBytes-2) + "\n"
	h.cg.set("exact", exact)
	h.cg.set("over", exact+"\n")
	f, err := h.R().ReadRepoConfig(context.Background(), reviewtest.PID, "exact")
	if err != nil || !f.Found || f.TooBig || len(f.Raw) != config.MaxRepoConfigBytes {
		t.Fatalf("exact: found %v tooBig %v len %d err %v", f.Found, f.TooBig, len(f.Raw), err)
	}
	if _, bytes := h.R().CfgCacheEntry(reviewtest.PID, "exact"); bytes != config.MaxRepoConfigBytes {
		t.Fatalf("cached bytes %d", bytes)
	}
	f, err = h.R().ReadRepoConfig(context.Background(), reviewtest.PID, "over")
	if err != nil || !f.Found || !f.TooBig || f.Raw != nil || f.Valid {
		t.Fatalf("over: %+v %v", f.TooBig, err)
	}
}

// An MR without a merge base (no base or start SHA) still gets its own
// config checked; no read at an empty ref.
func TestHeadConfigWithoutMergeBase(t *testing.T) {
	h := newCfgHarness(t)
	h.GL.MR.DiffRefs.BaseSHA, h.GL.MR.DiffRefs.StartSHA = "", ""
	h.cg.set("main", "language: ru\n")
	h.cg.set(headSHA, "language: ru\nauto_reviw: false\n")
	h.review(t, reviewtest.AutoJob())
	if n := h.configNotes(); len(n) != 1 || !strings.Contains(n[0], "`auto_reviw`") {
		t.Fatalf("notes %v", n)
	}
	if n, ok := h.cg.calls[""]; ok {
		t.Fatalf("read at an empty ref %d times", n)
	}
}

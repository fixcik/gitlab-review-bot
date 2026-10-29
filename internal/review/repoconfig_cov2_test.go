package review_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// syncBuf is a log sink for the reviewer's logger.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// captureLog points the reviewer's logger at a buffer.
func captureLog(h *cfgHarness) *syncBuf {
	buf := &syncBuf{}
	h.Env().Log = slog.New(slog.NewTextHandler(buf, nil))
	return buf
}

// The gate's decision: go on only with a usable target config (merged into
// the effective config), with the MR's broken config named in the summary.
func TestReviewConfigGateDecision(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		fail      string
		job       core.Job
		ok        bool
		err       string
		lang      string
		summary   string
		headWarns bool
	}{
		{name: "target read fails", fail: "main", job: reviewtest.AutoJob(), err: "HTTP 500"},
		{name: "not opted in", job: reviewtest.AutoJob()},
		{name: "not opted in, manual", job: reviewtest.ManualJob(core.JobReview)},
		{name: "invalid", files: map[string]string{"main": "lnt: {}\n"}, job: reviewtest.AutoJob()},
		{name: "invalid, manual", files: map[string]string{"main": "lnt: {}\n"}, job: reviewtest.ManualJob(core.JobReview)},
		{name: "valid", files: map[string]string{"main": "language: en\n"}, job: reviewtest.AutoJob(), ok: true, lang: "en"},
		{name: "valid, mr config broken", files: map[string]string{"main": "language: en\n", baseSHA: "language: en\n", headSHA: "language: en\nlnt: {}\n"},
			job: reviewtest.AutoJob(), ok: true, lang: "en", summary: "⚠️ **`.reviewbot.yaml` в этом MR невалиден** (1 ошибка) — после мержа"},
		{name: "valid, mr config broken twice", files: map[string]string{"main": "language: en\n", headSHA: "language: en\nlnt: {}\ntbd: 1\n"},
			job: reviewtest.AutoJob(), ok: true, lang: "en", summary: "(2 ошибки)"},
		{name: "valid, base unreadable", files: map[string]string{"main": "language: en\n", headSHA: "lnt: {}\n"}, fail: baseSHA,
			job: reviewtest.AutoJob(), ok: true, lang: "en", headWarns: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newCfgHarness(t)
			logs := captureLog(h)
			for ref, s := range c.files {
				h.cg.set(ref, s)
			}
			if c.fail != "" {
				h.cg.fail[c.fail] = true
			}
			sum, lang, ok, err := h.R().ReviewConfigGate(context.Background(), c.job, h.mr())
			if ok != c.ok || (err == nil) != (c.err == "") || err != nil && !strings.Contains(err.Error(), c.err) {
				t.Fatalf("ok %v err %v, want %v %q", ok, err, c.ok, c.err)
			}
			if c.ok && lang != c.lang {
				t.Fatalf("language %q", lang)
			}
			if c.summary == "" && sum != "" || !strings.Contains(sum, c.summary) {
				t.Fatalf("summary line %q, want %q", sum, c.summary)
			}
			if got := strings.Contains(logs.String(), "read .reviewbot.yaml of the mr head"); got != c.headWarns {
				t.Fatalf("head read warning logged %v:\n%s", got, logs.String())
			}
		})
	}
}

// The config note's outcome is logged: created, updated, or why not.
func TestConfigNoteLogs(t *testing.T) {
	const oldNote = marker.ConfigNote + " state=error -->\nold errors"
	cases := []struct {
		name  string
		seed  bool
		fault func(g *faultCfgGitLab)
		log   string
		not   []string
	}{
		{name: "created", fault: func(*faultCfgGitLab) {}, log: `level=INFO msg="review: config note created"`},
		{name: "updated", seed: true, fault: func(*faultCfgGitLab) {}, log: `level=INFO msg="review: config note updated" mr=42!7 note=77`,
			not: []string{"config note created"}},
		{name: "list fails", fault: func(g *faultCfgGitLab) { g.failList = true }, log: `level=WARN msg="review: list discussions for the config note"`},
		{name: "create fails", fault: func(g *faultCfgGitLab) { g.failCreate = true }, log: `level=WARN msg="review: create config note"`,
			not: []string{"config note created"}},
		{name: "update fails", seed: true, fault: func(g *faultCfgGitLab) { g.updateErr = context.DeadlineExceeded }, log: `level=WARN msg="review: update config note"`,
			not: []string{"config note updated", "config note created"}},
		{name: "note deleted", seed: true, fault: func(g *faultCfgGitLab) { g.updateErr = core.ErrNotFound }, log: `level=INFO msg="review: config note created"`,
			not: []string{"config note updated", "update config note"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, fg := newFaultCfgHarness(t)
			logs := captureLog(h)
			h.cg.set("main", "lnt: {}\n")
			if c.seed {
				h.GL.Discussions = append(h.GL.Discussions, core.Discussion{ID: "cfg", Notes: []core.Note{reviewtest.BotNote(77, oldNote, nil)}})
			}
			c.fault(fg)
			if _, _, err := h.Env().RepoConfig.Command(context.Background(), reviewtest.ManualJob(core.JobReview), h.mr()); err != nil {
				t.Fatal(err)
			}
			out := logs.String()
			if !strings.Contains(out, c.log) {
				t.Fatalf("log misses %q:\n%s", c.log, out)
			}
			for _, n := range c.not {
				if strings.Contains(out, n) {
					t.Errorf("log has %q:\n%s", n, out)
				}
			}
		})
	}
}

// newUncachedCfgHarness is newCfgHarness without the config cache: every
// read goes to GitLab.
func newUncachedCfgHarness(t *testing.T) *cfgHarness {
	t.Helper()
	h := &cfgHarness{Harness: reviewtest.New(t), now: reviewtest.TestNow}
	h.cg = &cfgGitLab{FakeGitLab: h.GL, files: map[string]string{}, fail: map[string]bool{}, calls: map[string]int{}}
	h.Cfg.Review.RequireRepoConfig = true
	h.Env().GitLab = h.cg
	return h
}

// flakyCfgGitLab fails the reads of one ref after the first n.
type flakyCfgGitLab struct {
	*cfgGitLab
	ref string
	ok  int
}

func (g *flakyCfgGitLab) GetRawFile(ctx context.Context, p int64, path, ref string) ([]byte, error) {
	if ref == g.ref {
		if g.ok == 0 {
			return nil, context.DeadlineExceeded
		}
		g.ok--
	}
	return g.cfgGitLab.GetRawFile(ctx, p, path, ref)
}

// A refused command keeps the config note even when the second read of the
// target branch fails: the errors already known are listed. A command
// refused because the repository is not opted in records no problem and
// writes no note.
func TestDenyRepoConfigKeepsNoteOnRereadFailure(t *testing.T) {
	h := newUncachedCfgHarness(t)
	fg := &flakyCfgGitLab{cfgGitLab: h.cg, ref: "main", ok: 1}
	h.Env().GitLab = fg
	h.cg.set("main", "lnt: {}\n")
	if _, ok, err := h.Env().RepoConfig.Command(context.Background(), reviewtest.ManualJob(core.JobReview), h.mr()); ok || err != nil {
		t.Fatalf("Command: %v %v", ok, err)
	}
	notes := h.configNotes()
	if len(notes) != 1 || !strings.Contains(notes[0], "state=error") || !strings.Contains(notes[0], "`lnt`") {
		t.Fatalf("notes %v", notes)
	}

	h = newUncachedCfgHarness(t)
	job := reviewtest.ManualJob(core.JobReview)
	ctx, run := problems.New(problems.Options{}).Start(context.Background(), job)
	if _, ok, err := h.Env().RepoConfig.Command(ctx, job, h.mr()); ok || err != nil {
		t.Fatalf("Command: %v %v", ok, err)
	}
	if ps := run.List(); len(ps) != 0 || len(h.configNotes()) != 0 || h.cg.calls["main"] != 1 {
		t.Fatalf("not opted in: problems %+v notes %v target reads %d", ps, h.configNotes(), h.cg.calls["main"])
	}
}

// The MR's file equals the target branch's (the target moved on to it):
// the refusal's note lists the target's errors only, not the same errors
// again as the MR's.
func TestDenyRepoConfigHeadSameAsTarget(t *testing.T) {
	h := newCfgHarness(t)
	h.cg.set("main", "lnt: {}\n")
	h.cg.set(headSHA, "lnt: {}\n")
	h.cg.set(baseSHA, "lint: {}\n")
	if _, _, err := h.Env().RepoConfig.Command(context.Background(), reviewtest.ManualJob(core.JobReview), h.mr()); err != nil {
		t.Fatal(err)
	}
	notes := h.configNotes()
	if len(notes) != 1 || strings.Contains(notes[0], "в этом MR") || strings.Count(notes[0], "`lnt`") != 1 {
		t.Fatalf("notes %v", notes)
	}
}

// Without a TTL nothing is cached.
func TestRepoConfigNoCacheWithoutTTL(t *testing.T) {
	h := newUncachedCfgHarness(t)
	h.cg.set("main", "language: en\n")
	for i := 0; i < 2; i++ {
		if f, err := h.R().ReadRepoConfig(context.Background(), reviewtest.PID, "main"); err != nil || !f.Valid {
			t.Fatalf("read: %+v %v", f, err)
		}
	}
	if e, n := h.R().CfgCacheEntry(reviewtest.PID, "main"); e.Found || n != 0 || h.cg.calls["main"] != 2 {
		t.Fatalf("cached %+v (%d bytes), reads %d", e, n, h.cg.calls["main"])
	}
}

// A refused command whose MR config cannot be checked logs why.
func TestDenyRepoConfigLogsHeadReadFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		h := newCfgHarness(t)
		logs := captureLog(h)
		h.cg.set("main", "lnt: {}\n")
		h.cg.set(headSHA, "tbd: 1\n")
		h.cg.fail[baseSHA] = fail
		if _, _, err := h.Env().RepoConfig.Command(context.Background(), reviewtest.ManualJob(core.JobReview), h.mr()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(logs.String(), `level=WARN msg="review: read .reviewbot.yaml of the mr head" mr=42!7`); got != fail {
			t.Fatalf("base read fails %v: warning logged %v:\n%s", fail, got, logs.String())
		}
	}
}

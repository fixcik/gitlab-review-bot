package review_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestCoreMutIsBotNote(t *testing.T) {
	for _, tc := range []struct {
		name   string
		botID  int64
		botUN  string
		note   core.Note
		expect bool
	}{
		{"id match", reviewtest.BotID, reviewtest.BotName, core.Note{AuthorID: reviewtest.BotID, AuthorUsername: "someone"}, true},
		// The id wins over the username when both are known.
		{"same username, other id", reviewtest.BotID, reviewtest.BotName, core.Note{AuthorID: reviewtest.UserID, AuthorUsername: reviewtest.BotName}, false},
		{"no author id: username, case-insensitive", reviewtest.BotID, reviewtest.BotName, core.Note{AuthorUsername: "ReviewBot"}, true},
		{"no author id: other username", reviewtest.BotID, reviewtest.BotName, core.Note{AuthorUsername: "bob"}, false},
		{"bot id unknown: username", 0, reviewtest.BotName, core.Note{AuthorID: reviewtest.UserID, AuthorUsername: reviewtest.BotName}, true},
		{"nothing known", 0, "", core.Note{AuthorID: reviewtest.UserID}, false},
		{"bot username unknown, empty author", 0, "", core.Note{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := struct{ env *env.Env }{&env.Env{BotID: tc.botID, BotUsername: tc.botUN}}
			if got := r.env.IsBotNote(tc.note); got != tc.expect {
				t.Fatalf("isBotNote = %v, want %v", got, tc.expect)
			}
		})
	}
}

// A command whose earlier attempt was not admitted (a first attempt, or one
// postponed before any charge) must still pass the access check: only an
// already admitted and charged command skips it.
func TestCoreMutNotAdmittedCommandChecksAccess(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.Access = core.AccessReporter
	job := reviewtest.ManualJob(core.JobReview)
	run := &reviewtest.QueueRun{ForJob: job} // never charged
	if err := h.R().Handle(run.Attempt(context.Background()), job); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 {
		t.Fatalf("a Reporter must not run a review")
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "Недостаточно прав") {
		t.Fatalf("reply %q", r.Body)
	}
}

// An admitted attempt (core.RunState.Admitted: charged before, then
// postponed or replayed) skips the access check and the per-user limits;
// outside a queue job nothing is admitted.
func TestCoreMutAdmittedSkipsAccess(t *testing.T) {
	if core.RunAdmitted(context.Background()) {
		t.Fatal("admitted outside a queue job")
	}
	h := reviewtest.New(t)
	h.GL.Access = core.AccessReporter
	h.Lim.User = core.Decision{Reason: "denied"}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	job := reviewtest.ManualJob(core.JobReview)
	run := &reviewtest.QueueRun{ForJob: job, Charged: true}
	if err := h.R().Handle(run.Attempt(context.Background()), job); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 || len(h.Lim.UserCalls) != 0 {
		t.Fatalf("runs %d, user checks %v", len(h.Runner.Reqs), h.Lim.UserCalls)
	}
}

func TestCoreMutRetryAt(t *testing.T) {
	h := reviewtest.New(t)
	def := reviewtest.TestNow.Add(env.DefaultBackoff)
	for _, tc := range []struct {
		name string
		at   time.Time
		want time.Time
	}{
		{"zero", time.Time{}, def},
		{"past", reviewtest.TestNow.Add(-time.Minute), def},
		{"now", reviewtest.TestNow, def},
		{"future", reviewtest.TestNow.Add(5 * time.Minute), reviewtest.TestNow.Add(5 * time.Minute)},
	} {
		if got := h.Env().RetryAt(tc.at); !got.Equal(tc.want) {
			t.Errorf("%s: retryAt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCoreMutEnsureReviewerRespectsConfig(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	h.Cfg.Review.AssignReviewer = false
	h.R().EnsureReviewer(context.Background(), reviewtest.AutoJob())
	if len(m.Reviewers) != 0 {
		t.Fatalf("assign_reviewer=false must not add the bot: %v", m.Reviewers)
	}
	h.Cfg.Review.AssignReviewer = true
	h.Env().BotID = 0
	h.R().EnsureReviewer(context.Background(), reviewtest.AutoJob())
	if len(m.Reviewers) != 0 {
		t.Fatalf("unknown bot id must not be added: %v", m.Reviewers)
	}
}

func TestCoreMutBlockingThreshold(t *testing.T) {
	h := reviewtest.New(t)
	for _, tc := range []struct {
		min  string
		want map[core.Severity]bool
	}{
		{"critical", map[core.Severity]bool{core.SeverityCritical: true, core.SeverityMajor: false, core.SeverityMinor: false, core.SeverityNit: false}},
		{" Major ", map[core.Severity]bool{core.SeverityCritical: true, core.SeverityMajor: true, core.SeverityMinor: false, core.SeverityNit: false}},
		{"minor", map[core.Severity]bool{core.SeverityCritical: true, core.SeverityMajor: true, core.SeverityMinor: true, core.SeverityNit: false}},
		{"nit", map[core.Severity]bool{core.SeverityCritical: true, core.SeverityMajor: true, core.SeverityMinor: true, core.SeverityNit: true}},
	} {
		h.Cfg.Review.BlockMinSeverity = tc.min
		for s, want := range tc.want {
			if got := h.Env().Blocking(s); got != want {
				t.Errorf("block_min_severity=%q: blocking(%s) = %v, want %v", tc.min, s, got, want)
			}
		}
	}
}

// The summary note is reused, never duplicated: the remembered note when it
// still exists, otherwise the newest bot note carrying the summary marker.
func TestCoreMutSummaryNoteReused(t *testing.T) {
	sumNote := func(id int64) core.Discussion {
		return core.Discussion{ID: fmt.Sprintf("s%d", id), Notes: []core.Note{reviewtest.BotNote(id, marker.Summary("abc123")+"\n"+summary.Title+"\n\nold", nil)}}
	}
	for _, tc := range []struct {
		name  string
		known int64
		notes []int64
		want  int64
	}{
		{"remembered note deleted: marker", 500, []int64{600}, 600},
		{"no state: newest marker", 0, []int64{600, 700, 650}, 700},
		{"remembered note wins over newer marker", 500, []int64{500, 700}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := reviewtest.New(t)
			if tc.known != 0 {
				h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, SummaryNoteID: tc.known}
			}
			for _, id := range tc.notes {
				h.GL.Discussions = append(h.GL.Discussions, sumNote(id))
			}
			// A user quoting the marker is not the bot's summary.
			h.GL.Discussions = append(h.GL.Discussions, core.Discussion{ID: "u", Notes: []core.Note{
				{ID: 900, AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: marker.Summary("abc")}}})
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if len(h.GL.CreatedNotes) != 0 || len(h.GL.UpdatedNotes) != 1 || h.GL.UpdatedNotes[0] != tc.want {
				t.Fatalf("created=%v updated=%v, want update of %d", h.GL.CreatedNotes, h.GL.UpdatedNotes, tc.want)
			}
			if st, _ := h.St.Get(); st.SummaryNoteID != tc.want {
				t.Fatalf("state summary id = %d", st.SummaryNoteID)
			}
		})
	}
}

// An open finding on a removed line has only an old path: Claude still gets
// the file to re-check it.
func TestCoreMutOpenFindingOnRemovedLine(t *testing.T) {
	h := reviewtest.New(t)
	id := finding.ID("gone.go", "Dead code")
	h.GL.Discussions = []core.Discussion{{ID: "d", Resolvable: true, Notes: []core.Note{
		reviewtest.BotNote(1, finding.NoteBody(id, output.Finding{Severity: "minor", Title: "Dead code"}, false, 0), &core.Position{OldPath: "gone.go", OldLine: 3}),
	}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if p := h.Runner.Reqs[0].Prompt; !strings.Contains(p, "id: "+id+"\n  path: gone.go") {
		t.Fatalf("open finding path missing from prompt:\n%s", p)
	}
}

func TestCoreMutBrokenRepoConfig(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.RepoConfig = "enabled: [\n"
	if err := h.R().Handle(context.Background(), reviewtest.ManualJob(core.JobReview)); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "Не удалось разобрать `.reviewbot.yaml`") {
		t.Fatalf("reply %q", r.Body)
	}
	// Automatic review: nothing runs, no reply; only the config note
	// (repoconfig.go) tells the MR why.
	h2 := reviewtest.New(t)
	h2.GL.RepoConfig = "enabled: [\n"
	if err := h2.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs)+len(h2.Runner.Reqs) != 0 || len(h2.GL.Replies) != 0 || len(h2.GL.CreatedNotes) != 1 ||
		!strings.HasPrefix(h2.GL.Notes[h2.GL.CreatedNotes[0]], marker.ConfigNote) {
		t.Fatalf("broken config: runs=%d/%d replies=%v notes=%v", len(h.Runner.Reqs), len(h2.Runner.Reqs), h2.GL.Replies, h2.GL.CreatedNotes)
	}
}

// A full review starts at the merge base; without it, at the start SHA.
func TestCoreMutFullDiffBase(t *testing.T) {
	mr := &core.MergeRequest{DiffRefs: core.DiffRefs{BaseSHA: "base", StartSHA: "start"}}
	if got := diffcap.FullDiffBase(mr); got != "base" {
		t.Fatalf("base: %s", got)
	}
	mr.DiffRefs.BaseSHA = ""
	if got := diffcap.FullDiffBase(mr); got != "start" {
		t.Fatalf("fallback: %s", got)
	}
}

// A duplicate finding is skipped, the findings after it are still posted;
// line 1 is a valid anchor.
func TestCoreMutDuplicateDoesNotStopPublishing(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 1, Severity: "critical", Title: "First"},
		{Path: "cache.go", Line: 1, Severity: "critical", Title: "first"},
		{Path: "cache.go", Line: 4, Severity: "critical", Title: "After the duplicate"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Published) != 2 || !strings.Contains(h.GL.Published[1].Body, "After the duplicate") {
		t.Fatalf("published %+v", h.GL.Published)
	}
	if p := h.GL.Published[0].Pos; p == nil || p.NewLine != 1 {
		t.Fatalf("line 1 must be anchored: %+v", p)
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, "⛔ **Мерж заблокирован:** 2 блокирующих замечания") {
		t.Fatalf("gate:\n%s", sum)
	}
}

// Only the bot's own, still open threads of the just-posted non-blocking
// findings are resolved by the service token, even when other threads carry
// the same finding marker.
func TestCoreMutResolveNonBlockingOnlyOwnThreads(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.WithModerator(h)
	id := finding.ID("cache.go", "Naming")
	spoof := finding.NoteBody(id, output.Finding{Severity: "nit", Title: "Naming"}, false, 0)
	h.GL.Discussions = []core.Discussion{
		{ID: "user", Resolvable: true, Notes: []core.Note{{ID: 1, AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: spoof}}},
		{ID: "empty", Resolvable: true},
		{ID: "old", Resolvable: true, Resolved: true, Notes: []core.Note{reviewtest.BotNote(2, spoof, nil)}},
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 5, Severity: "nit", Title: "Naming"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(m.Resolved) != 1 || !strings.HasPrefix(m.Resolved[0], "pub") {
		t.Fatalf("resolved %v, want only the bot's new thread", m.Resolved)
	}
}

// An MR without a description is reviewed automatically (only the ignore
// marker in it stops that).
func TestCoreMutEmptyDescriptionIsNotIgnored(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.Description = "  \n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("auto review skipped for an empty description")
	}
}

func TestCoreMutChatRefusedEarly(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.MR.State = "merged"
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); r.Body != review.MsgNotOpened || len(h.Runner.Reqs) != 0 || len(h.Lim.Records()) != 0 {
		t.Fatalf("closed MR: reply %q runs %d", r.Body, len(h.Runner.Reqs))
	}
	h = reviewtest.New(t)
	h.GL.RepoConfig = "enabled: [\n"
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	if r := reviewtest.LastReply(t, h.GL); !strings.Contains(r.Body, "Не удалось разобрать `.reviewbot.yaml`") || len(h.Runner.Reqs) != 0 {
		t.Fatalf("broken config: reply %q runs %d", r.Body, len(h.Runner.Reqs))
	}
}

// A long thread keeps its first note (the finding) and the latest ones.
func TestCoreMutChatLongThreadKeepsFinding(t *testing.T) {
	h := reviewtest.New(t)
	d := reviewtest.FindingThread(false)
	d.Notes = d.Notes[:1]
	for i := 1; i <= 40; i++ {
		d.Notes = append(d.Notes, core.Note{ID: int64(100 + i), AuthorID: reviewtest.UserID, AuthorUsername: "bob", Body: fmt.Sprintf("reply-%02d", i)})
	}
	h.GL.Discussions = []core.Discussion{d}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ChatOutput{Reply: "ok"})}}
	if err := h.R().Handle(context.Background(), reviewtest.ChatJob("q")); err != nil {
		t.Fatal(err)
	}
	p := h.Runner.Reqs[0].Prompt
	// First note + the last maxThreadNotes-1 replies.
	first := 40 - (review.MaxThreadNotes - 1) + 1
	if !strings.Contains(p, "No lock.") || !strings.Contains(p, "reply-40") || !strings.Contains(p, fmt.Sprintf("reply-%02d", first)) ||
		strings.Contains(p, fmt.Sprintf("reply-%02d", first-1)) {
		t.Fatalf("thread window wrong:\n%s", p)
	}
}

// A finding without a line is never snapped onto a nearby line: a blocking
// one becomes a general thread naming the file.
func TestCoreMutFindingWithoutLineNotAnchored(t *testing.T) {
	h := reviewtest.New(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 0, Severity: "critical", Title: "Design issue"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Published) != 1 || h.GL.Published[0].Pos != nil || !strings.Contains(h.GL.Published[0].Body, "(вне диффа)") {
		t.Fatalf("published %+v", h.GL.Published)
	}
}

// Without a service token the bot resolves non-blocking threads with its own
// token.
func TestCoreMutNoModeratorResolvesWithBotToken(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.PublishMakesThreads = true
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 5, Severity: "nit", Title: "Naming"},
	}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.GL.Published) != 1 || len(h.GL.Resolved) != 1 || h.GL.Resolved[0] != "pub0" {
		t.Fatalf("published %d resolved %v", len(h.GL.Published), h.GL.Resolved)
	}
}

// An open finding on a renamed file is shown with its new path.
func TestCoreMutOpenFindingOnRenamedFile(t *testing.T) {
	h := reviewtest.New(t)
	id := finding.ID("new.go", "Typo")
	h.GL.Discussions = []core.Discussion{{ID: "d", Resolvable: true, Notes: []core.Note{
		reviewtest.BotNote(1, finding.NoteBody(id, output.Finding{Severity: "minor", Title: "Typo"}, false, 0), &core.Position{OldPath: "old.go", NewPath: "new.go", NewLine: 2}),
	}}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if p := h.Runner.Reqs[0].Prompt; !strings.Contains(p, "id: "+id+"\n  path: new.go\n  line: 2") {
		t.Fatalf("open finding path:\n%s", p)
	}
}

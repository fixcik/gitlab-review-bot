package publish_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

var errBoom = errors.New("boom")

type fixture struct {
	gl  *reviewtest.FaultGitLab
	st  *reviewtest.FaultStore
	env *env.Env
	p   *publish.Publisher
	idx *reviewtest.FakeIndex
}

func newFixture() *fixture {
	cfg := config.Defaults()
	f := &fixture{
		gl:  &reviewtest.FaultGitLab{FakeGitLab: reviewtest.NewFakeGitLab()},
		st:  &reviewtest.FaultStore{FakeStore: reviewtest.NewFakeStore()},
		idx: &reviewtest.FakeIndex{Lines: map[string]map[int]int{"cache.go": {1: 1, 2: 0, 3: 0}}},
	}
	f.env = &env.Env{GitLab: f.gl, Store: f.st, Cfg: &cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return reviewtest.TestNow }, BotID: reviewtest.BotID, BotUsername: reviewtest.BotName,
		Index: func([]core.FileDiff) env.DiffIndex { return f.idx }}
	f.p = publish.New(f.env)
	return f
}

func job() core.Job { return reviewtest.AutoJob() }

func TestDraftsPublishesTheBatch(t *testing.T) {
	f := newFixture()
	f.gl.Drafts = []reviewtest.DraftNote{{Body: "stale draft"}}
	f.gl.FailDraftFor = "bad"
	created, err := f.p.Drafts(context.Background(), job(), []publish.Draft{{Body: "one"}, {Body: "bad two"}, {Body: "three", Pos: &core.Position{NewPath: "a.go", NewLine: 1}}})
	if err != nil || len(created) != 3 || !created[0] || created[1] || !created[2] {
		t.Fatalf("created %v err %v", created, err)
	}
	if len(f.gl.Published) != 2 || f.gl.Published[0].Body != "one" || f.gl.Published[1].Pos.NewPath != "a.go" || f.gl.DeleteDrafts != 1 {
		t.Fatalf("published %+v deletes %d", f.gl.Published, f.gl.DeleteDrafts)
	}
	// An empty batch touches nothing.
	created, err = f.p.Drafts(context.Background(), job(), nil)
	if err != nil || len(created) != 0 || f.gl.DeleteDrafts != 1 || f.gl.Calls("PublishDraftNotes") != 1 {
		t.Fatalf("empty batch: %v %v deletes %d", created, err, f.gl.DeleteDrafts)
	}
}

// Every create fails: nothing is published (a publish would post the
// leftovers of another job).
func TestDraftsNothingCreated(t *testing.T) {
	f := newFixture()
	f.gl.Fail = reviewtest.FailOn(errBoom, "CreateDraftNote", "DeleteDraftNotes")
	created, err := f.p.Drafts(context.Background(), job(), []publish.Draft{{Body: "a"}, {Body: "b"}})
	if err != nil || created[0] || created[1] {
		t.Fatalf("created %v err %v", created, err)
	}
	if f.gl.Calls("PublishDraftNotes") != 0 || f.gl.Calls("CreateDraftNote") != 2 {
		t.Fatalf("publish %d creates %d", f.gl.Calls("PublishDraftNotes"), f.gl.Calls("CreateDraftNote"))
	}
}

// The publish fails: the batch's drafts are deleted again and the error is
// returned (the callers move the findings to the summary).
func TestDraftsPublishFails(t *testing.T) {
	for _, deleteFails := range []bool{false, true} {
		f := newFixture()
		f.gl.Fail = func(m string, n int) error {
			switch {
			case m == "PublishDraftNotes":
				return errBoom
			case m == "DeleteDraftNotes" && n == 2 && deleteFails:
				return errors.New("delete down")
			}
			return nil
		}
		created, err := f.p.Drafts(context.Background(), job(), []publish.Draft{{Body: "a"}})
		if !errors.Is(err, errBoom) || !created[0] {
			t.Fatalf("created %v err %v", created, err)
		}
		if f.gl.Calls("DeleteDraftNotes") != 2 || len(f.gl.Published) != 0 {
			t.Fatalf("deletes %d published %+v", f.gl.Calls("DeleteDraftNotes"), f.gl.Published)
		}
		if wantLeft := deleteFails; (len(f.gl.Drafts) == 1) != wantLeft {
			t.Fatalf("delete fails %v: drafts left %+v", deleteFails, f.gl.Drafts)
		}
	}
}

func TestIndex(t *testing.T) {
	ctx := context.Background()
	mr := reviewtest.NewFakeGitLab().MR
	f := newFixture()
	if _, ok := f.p.Index(ctx, job(), &mr, false).(env.EmptyIndex); !ok || f.gl.Calls("GetMRDiffs") != 0 {
		t.Fatal("need=false must not read the diffs")
	}
	if got := f.p.Index(ctx, job(), &mr, true); got != env.DiffIndex(f.idx) {
		t.Fatalf("index %T", got)
	}
	// The diffs or the fresh MR cannot be read, or the MR moved on: nothing is
	// anchored.
	for name, fail := range map[string]reviewtest.FaultFunc{
		"diffs": reviewtest.FailOn(errBoom, "GetMRDiffs"),
		"mr":    reviewtest.FailOn(errBoom, "GetMR"),
	} {
		f := newFixture()
		f.gl.Fail = fail
		if _, ok := f.p.Index(ctx, job(), &mr, true).(env.EmptyIndex); !ok {
			t.Errorf("%s: want the empty index", name)
		}
	}
	f = newFixture()
	f.gl.MR.DiffRefs.HeadSHA = "pushed0000000000"
	if _, ok := f.p.Index(ctx, job(), &mr, true).(env.EmptyIndex); !ok {
		t.Error("a push during the review: want the empty index")
	}
}

// failMod is a service-token client whose resolves fail for some threads.
type failMod struct {
	mu       sync.Mutex
	fail     map[string]error
	resolved []string
}

func (m *failMod) ResolveDiscussion(_ context.Context, _, _ int64, id string, resolved bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.fail[id]; err != nil {
		return err
	}
	if resolved {
		m.resolved = append(m.resolved, id)
	}
	return nil
}

func (m *failMod) AddReviewer(context.Context, int64, int64, int64) (bool, error) { return false, nil }

func TestMarkFixed(t *testing.T) {
	cases := []struct {
		name          string
		resolveErr    error
		comment       string
		done, pending bool
		lead, tail    string
	}{
		{name: "resolved", comment: "fixed in a.go:3", done: true, lead: "✅ Исправлено: fixed in a.go:3"},
		{name: "reporter bot", resolveErr: core.ErrForbidden, done: true, pending: true, lead: "✅ Похоже, исправлено.", tail: "_Тред можно закрыть._"},
		{name: "resolve error", resolveErr: errBoom, comment: "c", lead: "ℹ️ По моим данным это исправлено: c", tail: "закройте его вручную"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture()
			f.env.Mod = &failMod{fail: map[string]error{"d1": c.resolveErr}}
			comment := c.comment
			if comment != "" {
				comment += "\n/approve"
			}
			done, pending := f.p.MarkFixed(context.Background(), job(), "d1", comment)
			if done != c.done || pending != c.pending {
				t.Fatalf("done %v pending %v", done, pending)
			}
			r := reviewtest.LastReply(t, f.gl.FakeGitLab)
			if r.DiscussionID != "d1" || !strings.HasPrefix(r.Body, marker.Fixed+"\n"+c.lead) || !strings.Contains(r.Body, c.tail) {
				t.Fatalf("reply %+v", r)
			}
			if strings.Contains(r.Body, "\n/approve") {
				t.Fatalf("quick action not neutralized: %q", r.Body)
			}
		})
	}
	// Without a service token the bot's own token resolves; a failed reply
	// does not undo the resolve.
	f := newFixture()
	f.gl.Fail = reviewtest.FailOn(errBoom, "ReplyToDiscussion")
	if done, pending := f.p.MarkFixed(context.Background(), job(), "d2", ""); !done || pending {
		t.Fatalf("done %v pending %v", done, pending)
	}
	if strings.Join(f.gl.Resolved, ",") != "d2" || f.gl.Calls("ReplyToDiscussion") != 1 {
		t.Fatalf("resolved %v", f.gl.Resolved)
	}
}

func TestResolveNonBlocking(t *testing.T) {
	ctx := context.Background()
	fnote := func(id string, extra string) string { return marker.Finding(id) + extra + "\nbody" }
	// Skipped discussions first: they must not end the walk.
	ds := []core.Discussion{
		{ID: "r", Resolved: true, Notes: []core.Note{reviewtest.BotNote(4, fnote("aa", ""), nil)}},
		{ID: "h", Notes: []core.Note{{ID: 5, AuthorID: 7, Body: fnote("aa", "")}}},
		{ID: "e"},
		{ID: "x", Notes: []core.Note{reviewtest.BotNote(6, "no marker", nil)}},
		{ID: "c", Notes: []core.Note{reviewtest.BotNote(3, fnote("cc", ""), nil)}}, // not in ids
		{ID: "a", Notes: []core.Note{reviewtest.BotNote(1, fnote("aa", ""), nil)}},
		{ID: "b", Notes: []core.Note{reviewtest.BotNote(2, fnote("bb", ""), nil)}},
	}
	ids := map[string]bool{"aa": true, "bb": true}

	// Nothing to resolve: no read at all.
	f := newFixture()
	f.env.Mod = &failMod{}
	f.p.ResolveNonBlocking(ctx, job(), nil, "finding", marker.ParseFinding)
	if f.gl.Calls("ListDiscussions") != 0 {
		t.Fatalf("reads %d", f.gl.Calls("ListDiscussions"))
	}

	// No service token: the bot resolves them with its own token.
	f = newFixture()
	f.gl.Discussions = ds
	f.p.ResolveNonBlocking(ctx, job(), ids, "finding", marker.ParseFinding)
	if got := strings.Join(f.gl.Resolved, ","); got != "a,b" {
		t.Fatalf("resolved by the bot's token: %q", got)
	}

	// One resolve fails: the others still go through.
	f = newFixture()
	f.gl.Discussions = ds
	m := &failMod{}
	f.env.Mod = m
	f.p.ResolveNonBlocking(ctx, job(), ids, "finding", marker.ParseFinding)
	if strings.Join(m.resolved, ",") != "a,b" {
		t.Fatalf("resolved %v", m.resolved)
	}
	m = &failMod{fail: map[string]error{"a": errBoom}}
	f.env.Mod = m
	f.p.ResolveNonBlocking(ctx, job(), ids, "finding", marker.ParseFinding)
	if strings.Join(m.resolved, ",") != "b" {
		t.Fatalf("resolved %v", m.resolved)
	}

	// The discussions cannot be read: nothing is resolved.
	f = newFixture()
	f.gl.Discussions = ds
	f.gl.Fail = reviewtest.FailOn(errBoom, "ListDiscussions")
	m = &failMod{}
	f.env.Mod = m
	f.p.ResolveNonBlocking(ctx, job(), ids, "finding", marker.ParseFinding)
	if len(m.resolved) != 0 {
		t.Fatalf("resolved %v", m.resolved)
	}
}

// snapIndex snaps to a line it cannot anchor (an index out of step with
// itself must not produce a position).
type snapIndex struct{ *reviewtest.FakeIndex }

func (snapIndex) Snap(string, int, int) (int, bool) { return 42, true }

func TestAnchorAndPlace(t *testing.T) {
	f := newFixture()
	refs := core.DiffRefs{BaseSHA: "b", StartSHA: "s", HeadSHA: "h"}
	cases := []struct {
		name     string
		idx      env.DiffIndex
		fd       output.Finding
		kind     string
		exact    bool
		snapped  int
		line     int
		blocking bool
	}{
		{name: "first line", idx: f.idx, fd: output.Finding{Path: "cache.go", Line: 1, Severity: "minor"}, kind: publish.Inline, exact: true, line: 1},
		{name: "own line", idx: f.idx, fd: output.Finding{Path: "cache.go", Line: 2, Severity: "minor"}, kind: publish.Inline, exact: true, line: 2},
		{name: "snapped", idx: f.idx, fd: output.Finding{Path: "cache.go", Line: 5, Severity: "critical"}, kind: publish.Snapped, snapped: 5, line: 3, blocking: true},
		{name: "too far, blocking", idx: f.idx, fd: output.Finding{Path: "cache.go", Line: 9, Severity: "critical"}, kind: publish.General, blocking: true},
		{name: "unknown file", idx: f.idx, fd: output.Finding{Path: "other.go", Line: 2, Severity: "minor"}, kind: publish.Summary},
		{name: "no line", idx: f.idx, fd: output.Finding{Path: "cache.go", Severity: "minor"}, kind: publish.Summary},
		{name: "no index", idx: nil, fd: output.Finding{Path: "cache.go", Line: 2, Severity: "minor"}, kind: publish.Summary},
		{name: "snap target not anchorable", idx: snapIndex{f.idx}, fd: output.Finding{Path: "cache.go", Line: 7, Severity: "minor"}, kind: publish.Summary},
	}
	for _, c := range cases {
		pl := f.p.Place(c.idx, refs, c.fd)
		if pl.Kind != c.kind || pl.Exact != c.exact || pl.SnappedFrom != c.snapped || pl.Blocking != c.blocking {
			t.Errorf("%s: %+v", c.name, pl)
			continue
		}
		if (pl.Pos != nil) != (c.line != 0) || (pl.Pos != nil && (pl.Pos.NewLine != c.line || pl.Pos.HeadSHA != "h")) {
			t.Errorf("%s: pos %+v", c.name, pl.Pos)
		}
	}
}

func TestScanMR(t *testing.T) {
	ctx := context.Background()
	f := newFixture()
	f.gl.Discussions = []core.Discussion{
		{ID: "s1", Notes: []core.Note{reviewtest.BotNote(10, marker.Summary("abc")+"\nold", nil)}},
		{ID: "s2", Notes: []core.Note{reviewtest.BotNote(20, marker.Summary("def")+"\nnewer", nil)}},
	}
	th, err := f.p.ScanMR(ctx, job())
	if err != nil || th.SummaryID != 20 {
		t.Fatalf("no state: %+v %v", th, err)
	}
	f.st.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{SummaryNoteID: 10}
	if th, err := f.p.ScanMR(ctx, job()); err != nil || th.SummaryID != 10 {
		t.Fatalf("known summary: %+v %v", th, err)
	}
	f.st.Fail = reviewtest.FailOn(errBoom, "GetMRState")
	if _, err := f.p.ScanMR(ctx, job()); !errors.Is(err, errBoom) || !strings.HasPrefix(err.Error(), "get mr state:") {
		t.Fatalf("err %v", err)
	}
	f.st.Fail = nil
	f.gl.Fail = reviewtest.FailOn(errBoom, "ListDiscussions")
	if _, err := f.p.ScanMR(ctx, job()); !errors.Is(err, errBoom) || !strings.HasPrefix(err.Error(), "list discussions:") {
		t.Fatalf("err %v", err)
	}
}

func TestFixedReply(t *testing.T) {
	if got := publish.FixedReply("✅ Исправлено", ""); got != "✅ Исправлено." {
		t.Fatal(got)
	}
	if got := publish.FixedReply("✅ Исправлено", "x"); got != "✅ Исправлено: x" {
		t.Fatal(got)
	}
}

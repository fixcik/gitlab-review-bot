package review

// Per-job memo of GitLab reads (GetMR, GetMRDiffs, ListDiscussions).
//
// One review job used to read the same MR, its diffs and its discussions
// several times: the config gate, the lint / size / outcome thread scans,
// publish.Scan, coverage, the CI linters, the TBD block and the diff
// index each made their own call. memoGitLab wraps the reviewer's client
// (New) and answers repeated reads of the same MR from a memo that lives in
// the job's context (withGitLabMemo, installed by Handle), so it never
// outlives the job and never mixes jobs.
//
// Freshness rules:
//   - Any write of the bot to the MR (notes, draft notes, replies, thread
//     resolution — bot token and service token alike) drops the memoized
//     discussions of that MR before and after the call, so the next read
//     (publish.ResolveNonBlocking after a publish, the gate recount, the summary
//     status scan) sees the new threads. The MR and its diffs do not change
//     by the bot's writes and stay memoized.
//   - jobctx.WithFreshGitLab(ctx) forces a real call (and refreshes the memo):
//     publish.Index re-reads the MR to detect a push during the review.
//   - Errors are never memoized; concurrent misses of the same key (parallel
//     analyzers) share one call. A panic in the shared call is re-raised to
//     its caller; the waiters see a failed call and make their own.
//
// Callers get copies: nothing a caller does to a returned MR or slice can
// change what another caller sees.

import (
	"context"
	"errors"
	"sync"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
)

// Memoized operations (the op label of Deps.OnGitLabSaved).
const (
	memoGetMR       = "get_mr"
	memoGetMRDiffs  = "get_mr_diffs"
	memoDiscussions = "list_discussions"
)

type memoMR struct{ project, mr int64 }

type memoKey struct {
	op string
	memoMR
}

// memoEntry is one memoized read; done is closed when the call returned.
type memoEntry struct {
	done  chan struct{}
	gen   uint64
	mr    *core.MergeRequest
	diffs []core.FileDiff
	ds    []core.Discussion
	err   error
}

// glMemo is the memo of one job.
type glMemo struct {
	mu      sync.Mutex
	gen     map[memoKey]uint64 // bumped by every invalidation of the key
	entries map[memoKey]*memoEntry
}

type glMemoKey struct{}

// withGitLabMemo gives ctx (one job) its own memo.
func withGitLabMemo(ctx context.Context) context.Context {
	if memoFrom(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, glMemoKey{}, &glMemo{gen: map[memoKey]uint64{}, entries: map[memoKey]*memoEntry{}})
}

func memoFrom(ctx context.Context) *glMemo {
	m, _ := ctx.Value(glMemoKey{}).(*glMemo)
	return m
}

// invalidate drops the memoized discussions of the MR. The bot's writes
// are notes, draft notes, replies and thread resolution: they change the
// discussions, never the MR's refs or diffs.
func (m *glMemo) invalidate(project, mr int64) {
	if m == nil {
		return
	}
	k := memoKey{memoDiscussions, memoMR{project, mr}}
	m.mu.Lock()
	m.gen[k]++
	delete(m.entries, k)
	m.mu.Unlock()
}

// memoGitLab is the decorator. It embeds the client, so every other method
// is forwarded; Unwrap keeps optional capabilities reachable (core.GitLabAs).
type memoGitLab struct {
	core.GitLab
	onSaved func(op string)
}

// newMemoGitLab wraps gl (nil stays nil).
func newMemoGitLab(gl core.GitLab, onSaved func(op string)) core.GitLab {
	if gl == nil {
		return nil
	}
	return &memoGitLab{GitLab: gl, onSaved: onSaved}
}

func (g *memoGitLab) Unwrap() core.GitLab { return g.GitLab }

// read returns the memoized entry of key or makes the call (once for all
// concurrent callers). fill stores the call's result into the entry.
func (g *memoGitLab) read(ctx context.Context, key memoKey, fill func(e *memoEntry)) *memoEntry {
	m := memoFrom(ctx)
	if m == nil {
		e := &memoEntry{}
		fill(e)
		return e
	}
	fresh := jobctx.FreshGitLab(ctx)
	m.mu.Lock()
	if e, ok := m.entries[key]; ok && !fresh {
		m.mu.Unlock()
		select {
		case <-e.done:
		case <-ctx.Done():
			return &memoEntry{err: ctx.Err()}
		}
		if e.err == nil {
			if g.onSaved != nil {
				g.onSaved(key.op)
			}
			return e
		}
		// The shared call failed: try on our own (not memoized on failure).
		m.mu.Lock()
	}
	e := &memoEntry{done: make(chan struct{}), gen: m.gen[key]}
	m.entries[key] = e
	m.mu.Unlock()
	completed := false
	defer func() {
		if !completed {
			// fill panicked: waiters see a failed call and make their own;
			// the panic goes on to our caller.
			e.err = errMemoCallPanicked
		}
		m.mu.Lock()
		if cur, ok := m.entries[key]; ok && cur == e && (e.err != nil || m.gen[key] != e.gen) {
			// Failed, or a write happened during the call: the result may be
			// older than the write. Waiters still get it (they asked before).
			delete(m.entries, key)
		}
		m.mu.Unlock()
		close(e.done)
	}()
	fill(e)
	completed = true
	return e
}

// errMemoCallPanicked is what waiters of a shared call see when the call
// panicked (they retry on their own, like after any failed call).
var errMemoCallPanicked = errors.New("memoized GitLab call panicked")

func (g *memoGitLab) GetMR(ctx context.Context, projectID, mrIID int64) (*core.MergeRequest, error) {
	e := g.read(ctx, memoKey{memoGetMR, memoMR{projectID, mrIID}}, func(e *memoEntry) {
		e.mr, e.err = g.GitLab.GetMR(ctx, projectID, mrIID)
	})
	return cloneMR(e.mr), e.err
}

func (g *memoGitLab) GetMRDiffs(ctx context.Context, projectID, mrIID int64) ([]core.FileDiff, error) {
	e := g.read(ctx, memoKey{memoGetMRDiffs, memoMR{projectID, mrIID}}, func(e *memoEntry) {
		e.diffs, e.err = g.GitLab.GetMRDiffs(ctx, projectID, mrIID)
	})
	if e.diffs == nil {
		return nil, e.err
	}
	return append([]core.FileDiff(nil), e.diffs...), e.err
}

func (g *memoGitLab) ListDiscussions(ctx context.Context, projectID, mrIID int64) ([]core.Discussion, error) {
	e := g.read(ctx, memoKey{memoDiscussions, memoMR{projectID, mrIID}}, func(e *memoEntry) {
		e.ds, e.err = g.GitLab.ListDiscussions(ctx, projectID, mrIID)
	})
	return cloneDiscussions(e.ds), e.err
}

// writeMR runs a write to the MR (bot or service token) with the memo of
// the MR dropped before and after it.
func writeMR(ctx context.Context, projectID, mrIID int64, call func() error) error {
	m := memoFrom(ctx)
	m.invalidate(projectID, mrIID)
	err := call()
	m.invalidate(projectID, mrIID)
	return err
}

func (g *memoGitLab) CreateDraftNote(ctx context.Context, projectID, mrIID int64, body string, pos *core.Position) (id int64, err error) {
	err = writeMR(ctx, projectID, mrIID, func() error {
		id, err = g.GitLab.CreateDraftNote(ctx, projectID, mrIID, body, pos)
		return err
	})
	return id, err
}

func (g *memoGitLab) PublishDraftNotes(ctx context.Context, projectID, mrIID int64) error {
	return writeMR(ctx, projectID, mrIID, func() error { return g.GitLab.PublishDraftNotes(ctx, projectID, mrIID) })
}

func (g *memoGitLab) DeleteDraftNotes(ctx context.Context, projectID, mrIID int64) error {
	return writeMR(ctx, projectID, mrIID, func() error { return g.GitLab.DeleteDraftNotes(ctx, projectID, mrIID) })
}

func (g *memoGitLab) CreateNote(ctx context.Context, projectID, mrIID int64, body string) (id int64, err error) {
	err = writeMR(ctx, projectID, mrIID, func() error {
		id, err = g.GitLab.CreateNote(ctx, projectID, mrIID, body)
		return err
	})
	return id, err
}

func (g *memoGitLab) UpdateNote(ctx context.Context, projectID, mrIID, noteID int64, body string) error {
	return writeMR(ctx, projectID, mrIID, func() error { return g.GitLab.UpdateNote(ctx, projectID, mrIID, noteID, body) })
}

func (g *memoGitLab) ReplyToDiscussion(ctx context.Context, projectID, mrIID int64, discussionID, body string) error {
	return writeMR(ctx, projectID, mrIID, func() error {
		return g.GitLab.ReplyToDiscussion(ctx, projectID, mrIID, discussionID, body)
	})
}

func (g *memoGitLab) ResolveDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string, resolved bool) error {
	return writeMR(ctx, projectID, mrIID, func() error {
		return g.GitLab.ResolveDiscussion(ctx, projectID, mrIID, discussionID, resolved)
	})
}

// memoModerator drops the memo on the service token's writes too.
type memoModerator struct{ env.Moderator }

// newMemoModerator wraps m (nil stays nil: callers test r.mod == nil).
func newMemoModerator(m env.Moderator) env.Moderator {
	if m == nil {
		return nil
	}
	return &memoModerator{Moderator: m}
}

func (m *memoModerator) ResolveDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string, resolved bool) error {
	return writeMR(ctx, projectID, mrIID, func() error {
		return m.Moderator.ResolveDiscussion(ctx, projectID, mrIID, discussionID, resolved)
	})
}

func cloneMR(mr *core.MergeRequest) *core.MergeRequest {
	if mr == nil {
		return nil
	}
	c := *mr
	if mr.Pipeline != nil {
		p := *mr.Pipeline
		c.Pipeline = &p
	}
	return &c
}

func cloneDiscussions(ds []core.Discussion) []core.Discussion {
	if ds == nil {
		return nil
	}
	out := make([]core.Discussion, len(ds))
	for i, d := range ds {
		out[i] = d
		if d.Notes != nil {
			out[i].Notes = make([]core.Note, len(d.Notes))
			for j, n := range d.Notes {
				if n.Position != nil {
					p := *n.Position
					n.Position = &p
				}
				out[i].Notes[j] = n
			}
		}
	}
	return out
}

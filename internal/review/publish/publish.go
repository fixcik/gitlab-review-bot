// Package publish is how the review features write to an MR's threads:
// draft notes published as one batch, anchoring a finding in the diff
// (and the placement decision), marking a bot thread fixed, resolving the
// non-blocking ones and scanning the MR's discussions. It needs only the
// features' environment (review/env). It must not depend on internal/review
// or any of its feature packages.
package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// SnapDistance is how far (in lines) a finding may be moved to the closest
// commentable line.
const SnapDistance = 3

// Publisher publishes through the features' environment.
type Publisher struct{ env *env.Env }

// New is a Publisher on e.
func New(e *env.Env) *Publisher { return &Publisher{env: e} }

// Draft is one note of a Drafts batch; What names it in the log.
type Draft struct {
	Body string
	Pos  *core.Position
	What string
}

// Drafts is the only way the bot publishes threads through draft
// notes (findings, compat, lint, the size thread; the CI thread has its
// own). PublishDraftNotes publishes ALL of the bot's drafts on the MR, so the
// leftovers of an interrupted job are deleted first; then the batch is
// created and published. created[i] reports whether ds[i] was created (a
// failed one is left out and logged). err != nil: the publish failed and
// nothing of the batch is published (its drafts are deleted again). An
// empty batch, or one where every create failed, touches nothing.
func (p *Publisher) Drafts(ctx context.Context, job core.Job, ds []Draft) (created []bool, err error) {
	ctx, tsp := tracing.Start(ctx, "review.publish.drafts")
	defer func() {
		n := 0
		for _, c := range created {
			if c {
				n++
			}
		}
		tsp.SetAttr(tracing.Int("reviewbot.drafts", len(ds)), tracing.Int("reviewbot.drafts_created", n))
		if err != nil {
			tsp.RecordError(err, "publish")
		}
		tsp.End()
	}()
	created = make([]bool, len(ds))
	if len(ds) == 0 {
		return created, nil
	}
	log := p.env.Log.With("mr", job.MRKey())
	if err := p.env.GitLab.DeleteDraftNotes(ctx, job.ProjectID, job.MRIID); err != nil {
		log.Warn("review: delete leftover drafts", "err", err)
	}
	n := 0
	for i, d := range ds {
		if _, err := p.env.GitLab.CreateDraftNote(ctx, job.ProjectID, job.MRIID, d.Body, d.Pos); err != nil {
			log.Warn("review: create draft note", "what", d.What, "err", err)
			continue
		}
		created[i] = true
		n++
	}
	if n == 0 {
		return created, nil
	}
	if err := p.env.GitLab.PublishDraftNotes(ctx, job.ProjectID, job.MRIID); err != nil {
		log.Error("review: publish draft notes", "err", err)
		if derr := p.env.GitLab.DeleteDraftNotes(ctx, job.ProjectID, job.MRIID); derr != nil {
			log.Warn("review: delete unpublished drafts", "err", derr)
		}
		return created, err
	}
	return created, nil
}

// Index builds the line index of the MR diff for anchoring (need=false
// skips the API calls). The /diffs endpoint returns the LATEST MR version:
// if a push landed during the review, its line map does not match the
// reviewed head, and anchoring would put comments on wrong lines (or be
// rejected), so everything goes to general threads / the summary instead.
func (p *Publisher) Index(ctx context.Context, job core.Job, mr *core.MergeRequest, need bool) env.DiffIndex {
	ctx, tsp := tracing.Start(ctx, "review.diff_index")
	defer tsp.End()
	if !need {
		return env.EmptyIndex{}
	}
	log := p.env.Log.With("mr", job.MRKey())
	diffs, err := p.env.GitLab.GetMRDiffs(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: get mr diffs, nothing will be anchored", "err", err)
		return env.EmptyIndex{}
	}
	// A real read (not the job's memo, glmemo.go): it must see a push made
	// during the review. Diffs from the memo are of the same version when the
	// refs still match the job's start.
	if cur, err := p.env.GitLab.GetMR(jobctx.WithFreshGitLab(ctx), job.ProjectID, job.MRIID); err != nil || cur.DiffRefs != mr.DiffRefs {
		log.Warn("review: mr changed during review, nothing will be anchored", "err", err)
		return env.EmptyIndex{}
	}
	return p.env.Index(diffs)
}

// MarkFixed tells a bot thread it is fixed and resolves it (service token
// when available). done reports whether the thread no longer counts as open
// for the bot: resolved, or (Reporter bot, 403) marked fixed for a human to
// close; pending is set in the latter case: the thread is still unresolved
// in GitLab, so a blocking one still blocks the merge. On other resolve
// errors it says so and keeps the thread open (done=false).
func (p *Publisher) MarkFixed(ctx context.Context, job core.Job, discussionID, comment string) (done, pending bool) {
	log := p.env.Log.With("mr", job.MRKey())
	// Resolve first: only claim the finding is fixed once GitLab has
	// actually resolved the thread. If the resolve fails, still post the
	// comment (it may carry useful context) but say the thread was left
	// open rather than asserting "Исправлено", and do not count it.
	resolveErr := p.env.Resolve(ctx, job.ProjectID, job.MRIID, discussionID)
	pending = errors.Is(resolveErr, core.ErrForbidden)
	var body string
	switch {
	case pending:
		// Expected for a Reporter bot: it may comment but not resolve.
		body = FixedReply("✅ Похоже, исправлено", comment) + "\n\n_Тред можно закрыть._"
	case resolveErr != nil:
		log.Warn("review: resolve thread", "discussion", discussionID, "err", resolveErr)
		body = FixedReply("ℹ️ По моим данным это исправлено", comment) + "\n\n_Не удалось снять тред автоматически — закройте его вручную, если согласны._"
	default:
		body = FixedReply("✅ Исправлено", comment)
	}
	body = marker.Fixed + "\n" + body
	if err := p.env.GitLab.ReplyToDiscussion(ctx, job.ProjectID, job.MRIID, discussionID, mdtext.NeutralizeQuickActions(body)); err != nil {
		log.Warn("review: reply to fixed thread", "discussion", discussionID, "err", err)
	}
	return resolveErr == nil || pending, pending
}

// FixedReply is lead followed by ": comment", or by "." without a comment.
func FixedReply(lead, comment string) string {
	if comment == "" {
		return lead + "."
	}
	return lead + ": " + comment
}

// ResolveNonBlocking resolves the just-published threads of non-blocking
// results (findings, lint), so they don't hold the merge: the unresolved bot
// threads whose first note idOf maps to an id in ids. The service token when
// there is one, else the bot's own (GitLab lets the author of a thread resolve
// it, a Reporter included). what names the kind in the log.
func (p *Publisher) ResolveNonBlocking(ctx context.Context, job core.Job, ids map[string]bool, what string, idOf func(body string) (string, bool)) {
	if len(ids) == 0 {
		return
	}
	log := p.env.Log.With("mr", job.MRKey())
	ds, err := p.env.GitLab.ListDiscussions(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: list discussions to resolve non-blocking "+what+"s", "err", err)
		return
	}
	for _, d := range ds {
		if d.Resolved || len(d.Notes) == 0 || !p.env.IsBotNote(d.Notes[0]) {
			continue
		}
		id, ok := idOf(d.Notes[0].Body)
		if !ok || !ids[id] {
			continue
		}
		if err := p.env.Resolve(ctx, job.ProjectID, job.MRIID, d.ID); err != nil {
			log.Warn("review: resolve non-blocking "+what, "id", id, "err", err)
		}
	}
}

// Anchor finds a diff position for f. exact is true when the finding's own
// line is commentable (suggestions are only safe then). snappedFrom is the
// original line when the position was moved to a nearby line.
func (p *Publisher) Anchor(idx env.DiffIndex, refs core.DiffRefs, f output.Finding) (pos *core.Position, exact bool, snappedFrom int) {
	if idx == nil || f.Line <= 0 || !idx.HasFile(f.Path) {
		return nil, false, 0
	}
	if p, ok := idx.Anchor(refs, f.Path, f.Line); ok {
		return p, true, 0
	}
	line, ok := idx.Snap(f.Path, f.Line, SnapDistance)
	if !ok {
		return nil, false, 0
	}
	if p, ok := idx.Anchor(refs, f.Path, line); ok {
		return p, false, f.Line
	}
	return nil, false, 0
}

// Where a finding ends up when published (Place).
const (
	Inline  = "inline"         // a thread on its own line
	Snapped = "inline_snapped" // a thread on a nearby changed line
	General = "general_thread" // blocking, line not in the diff: a general thread
	Summary = "summary"        // not blocking, line not in the diff: listed in the summary
)

// Placement is the decision of Place.
type Placement struct {
	Kind        string
	Pos         *core.Position
	Exact       bool
	SnappedFrom int
	Blocking    bool
}

// Place decides how a finding is published: inline on its line,
// snapped to a nearby commentable line, as a general thread (blocking) or
// in the summary (not blocking). publishFindings and submit_finding share it.
func (p *Publisher) Place(idx env.DiffIndex, refs core.DiffRefs, f output.Finding) Placement {
	pl := Placement{Blocking: p.env.Blocking(f.Severity)}
	pl.Pos, pl.Exact, pl.SnappedFrom = p.Anchor(idx, refs, f)
	switch {
	case pl.Pos != nil && pl.Exact:
		pl.Kind = Inline
	case pl.Pos != nil:
		pl.Kind = Snapped
	case pl.Blocking:
		pl.Kind = General
	default:
		pl.Kind = Summary
	}
	return pl
}

// ScanMR is Scan with the summary note id the store
// remembers for the MR.
func (p *Publisher) ScanMR(ctx context.Context, job core.Job) (*mrthread.Threads, error) {
	state, err := p.env.Store.GetMRState(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return nil, fmt.Errorf("get mr state: %w", err)
	}
	var known int64
	if state != nil {
		known = state.SummaryNoteID
	}
	return p.Scan(ctx, job.ProjectID, job.MRIID, known)
}

// Scan lists the MR discussions and scans them (mrthread.Scan).
func (p *Publisher) Scan(ctx context.Context, projectID, mrIID, knownSummaryID int64) (*mrthread.Threads, error) {
	ctx, tsp := tracing.Start(ctx, "review.threads")
	defer tsp.End()
	ds, err := p.env.GitLab.ListDiscussions(ctx, projectID, mrIID)
	if err != nil {
		return nil, fmt.Errorf("list discussions: %w", err)
	}
	return mrthread.Scan(ds, knownSummaryID, p.env.IsBotNote), nil
}

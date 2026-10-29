package learning

// Classifying the open outcomes of an MR from its finding threads: binding
// an outcome to its thread and telling fixed from dismissed by the lines
// around the finding.

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
)

const (
	// outcomeWindow is the ±lines window compared to tell fixed from
	// dismissed.
	outcomeWindow = 3
	// maxOtherMRs caps the other MRs of the project checked per review.
	maxOtherMRs = 3
	// bindSlack: a thread created this long before an outcome may still
	// belong to it (clock skew between GitLab and the bot).
	bindSlack = 10 * time.Minute
)

// lineChecker is implemented by *repo.Checkout (LinesChanged).
type lineChecker interface {
	LinesChanged(ctx context.Context, from, to, path string, start, end int) (bool, error)
}

// classifyOtherMRs classifies the open outcomes of up to maxOtherMRs other
// MRs of the same project, least recently checked first: a thread is often
// resolved right before the merge, and a merged MR gets no further review.
// Their lines are compared up to the MR's current head when the checkout
// has that commit (the clone carries the heads of every MR the bot fetched).
func (l *Feature) classifyOtherMRs(ctx context.Context, job core.Job, co core.Checkout) {
	ost := l.OutcomeStore()
	open, err := ost.ListOutcomes(ctx, learn.OutcomeFilter{ProjectPath: job.ProjectPath, Status: learn.OutcomeOpen, Limit: 1000})
	if err != nil {
		l.env.Log.Warn("outcomes: list open", "mr", job.MRKey(), "err", err)
		return
	}
	var mrs []int64
	seen := map[int64]bool{job.MRIID: true}
	for i := len(open) - 1; i >= 0 && len(mrs) < maxOtherMRs; i-- { // oldest updated first
		if m := open[i].MRIID; !seen[m] {
			seen[m] = true
			mrs = append(mrs, m)
		}
	}
	for _, iid := range mrs {
		_ = ost.TouchOutcomes(ctx, job.ProjectPath, iid)
		other, err := l.env.GitLab.GetMR(ctx, job.ProjectID, iid)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil || other.DiffRefs.HeadSHA == "" {
			l.env.Log.Warn("outcomes: get other mr", "project", job.ProjectPath, "mr", iid, "err", err)
			continue
		}
		l.classifyOutcomes(ctx, job, iid, other.DiffRefs.HeadSHA, co)
	}
}

// classifyOutcomes looks at an MR's open outcomes and their threads. to is
// the MR head to compare lines with ("" = the checkout's HEAD, i.e. the MR
// being reviewed).
func (l *Feature) classifyOutcomes(ctx context.Context, job core.Job, mrIID int64, to string, co core.Checkout) {
	log := l.env.Log.With("project", job.ProjectPath, "mr", mrIID)
	ost := l.OutcomeStore()
	all, err := ost.ListOutcomes(ctx, learn.OutcomeFilter{ProjectPath: job.ProjectPath, MRIID: mrIID})
	if err != nil {
		log.Warn("outcomes: list", "err", err)
		return
	}
	var open []learn.Outcome
	bound := map[string]bool{}
	for _, o := range all {
		if o.DiscussionID != "" {
			bound[o.DiscussionID] = true
		}
		if o.Status == learn.OutcomeOpen {
			open = append(open, o)
		}
	}
	if len(open) == 0 {
		return
	}
	ds, err := l.env.GitLab.ListDiscussions(ctx, job.ProjectID, mrIID)
	if err != nil {
		log.Warn("outcomes: list discussions", "err", err)
		return
	}
	byID := map[string]core.Discussion{}
	// Candidate threads per finding id, newest first.
	cands := map[string][]core.Discussion{}
	for _, d := range ds {
		byID[d.ID] = d
		if len(d.Notes) == 0 || d.Notes[0].System || !l.env.IsBotNote(d.Notes[0]) || bound[d.ID] {
			continue
		}
		if id, ok := marker.ParseFinding(d.Notes[0].Body); ok {
			cands[id] = append(cands[id], d)
		}
	}
	for id := range cands {
		c := cands[id]
		sort.SliceStable(c, func(i, j int) bool { return c[i].Notes[0].ID > c[j].Notes[0].ID })
	}
	// Bind unbound outcomes, newest first, to the newest matching thread
	// (an older thread with the same id predates the outcome).
	sort.SliceStable(open, func(i, j int) bool { return open[i].ID > open[j].ID })
	for _, o := range open {
		var d core.Discussion
		found := false
		if o.DiscussionID != "" {
			d, found = byID[o.DiscussionID]
		} else {
			for i, c := range cands[o.FindingID] {
				t := c.Notes[0].CreatedAt
				if !t.IsZero() && !o.CreatedAt.IsZero() && t.Before(o.CreatedAt.Add(-bindSlack)) {
					continue
				}
				d, found = c, true
				cands[o.FindingID] = append(cands[o.FindingID][:i:i], cands[o.FindingID][i+1:]...)
				break
			}
			if found {
				if err := ost.SetOutcomeDiscussion(ctx, o.ID, d.ID); err != nil {
					log.Warn("outcomes: bind", "err", err)
				}
			}
		}
		if !found {
			if o.DiscussionID != "" {
				// The thread was deleted: nothing to learn from.
				l.setOutcome(ctx, o, learn.OutcomeClosed)
			}
			continue
		}
		if !d.Resolved {
			continue
		}
		st := l.classifyResolved(ctx, co, o, to, d)
		l.setOutcome(ctx, o, st)
		if st != o.Status {
			log.Info("outcomes: resolved thread classified", "finding", o.FindingID, "status", st, "resolved_by", resolvedByName(d))
		}
	}
}

// resolvedByName is GitLab's resolved_by username (for logs only: the
// service token may be a person's own token, so who resolved a thread does
// not say whether the bot did it; the bot's own records do).
func resolvedByName(d core.Discussion) string {
	for _, n := range d.Notes {
		if n.ResolvedByUsername != "" {
			return n.ResolvedByUsername
		}
	}
	return ""
}

// classifyResolved decides the status of a resolved finding thread whose
// outcome is still open in our records. Every resolve the bot performs
// (publish.MarkFixed, chat resolve, non-blocking auto-resolve) updates the outcome
// first, so an open outcome on a resolved thread means a human resolved it.
// GitLab's resolved_by is deliberately NOT used: the service token that
// resolves on the bot's behalf may be a team member's personal token.
func (l *Feature) classifyResolved(ctx context.Context, co core.Checkout, o learn.Outcome, to string, d core.Discussion) string {
	if mrthread.FixedByBot(d, l.env.IsBotNote) {
		return learn.OutcomeFixed // the bot said it is fixed; a human closed it
	}
	// Resolved by a human.
	lc, ok := core.CheckoutAs[lineChecker](co)
	if !ok || o.HeadSHAAtPost == "" || o.Path == "" {
		return learn.OutcomeOpen // cannot tell; keep it open
	}
	start, end := o.Line-outcomeWindow, o.Line+outcomeWindow
	if o.Line <= 0 {
		start, end = 0, 0 // whole file
	} else if start < 1 {
		start = 1
	}
	changed, err := lc.LinesChanged(ctx, o.HeadSHAAtPost, to, o.Path, start, end)
	switch {
	case errors.Is(err, core.ErrNotFound) && to == "":
		return learn.OutcomeClosed // the posted head is gone (force push)
	case errors.Is(err, core.ErrNotFound):
		return learn.OutcomeOpen // another MR's head is not in this clone yet
	case err != nil:
		l.env.Log.Warn("outcomes: lines changed", "project", o.ProjectPath, "mr", o.MRIID, "path", o.Path, "err", err)
		return learn.OutcomeOpen
	case changed:
		return learn.OutcomeFixed
	default:
		return learn.OutcomeDismissed
	}
}

func (l *Feature) setOutcome(ctx context.Context, o learn.Outcome, status string) {
	if status == o.Status {
		return
	}
	if err := l.OutcomeStore().SetOutcomeStatus(ctx, o.ID, status); err != nil {
		l.env.Log.Warn("outcomes: set status", "id", o.ID, "err", err)
		return
	}
	l.env.Log.Info("outcomes: classified", "project", o.ProjectPath, "mr", o.MRIID, "finding", o.FindingID, "status", status)
}

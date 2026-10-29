package review

// "@bot pause" / "@bot resume": stop and restart the automatic reviews of
// one MR (Deps.MRFlags), and the pause line of the summary note.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const (
	pauseLinePrefix = "⏸ Авторевью на паузе"

	msgPaused        = "⏸ Авторевью на паузе. Продолжить: `%s resume`"
	msgAlreadyPaused = "⏸ Авторевью уже на паузе (поставил %s). Продолжить: `%s resume`"
	msgResumed       = "▶️ Авторевью возобновлено."
	msgResumedQueued = " Есть непроверенные коммиты — ревью поставлено в очередь."
	msgNotPaused     = "▶️ Авторевью для этого MR и так включено. Поставить на паузу: `%s pause`"
	msgNoFlags       = "Пауза недоступна: хранилище бота не поддерживает флаги MR."
)

// handlePause sets the MR's pause flag and adds the pause line to the
// summary.
func (r *Reviewer) handlePause(ctx context.Context, job core.Job) error {
	f := r.flags()
	if f == nil {
		return r.env.Reply(ctx, job, msgNoFlags)
	}
	paused, by, _, err := f.GetMRPause(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return fmt.Errorf("get mr pause: %w", err)
	}
	if paused {
		return r.env.Reply(ctx, job, fmt.Sprintf(msgAlreadyPaused, quietUser(by), r.env.Mention()))
	}
	if err := f.SetMRPause(ctx, job.ProjectID, job.MRIID, job.Username, r.env.Now()); err != nil {
		return fmt.Errorf("set mr pause: %w", err)
	}
	r.env.Log.Info("review: auto review paused", "mr", job.MRKey(), "user", job.Username)
	r.updateSummaryPause(ctx, job, pauseLine(job.Username))
	return r.env.Reply(ctx, job, fmt.Sprintf(msgPaused, r.env.Mention()))
}

// handleResume clears the MR's pause flag, removes the pause line and
// enqueues a review when commits arrived while the MR was paused.
func (r *Reviewer) handleResume(ctx context.Context, job core.Job) error {
	f := r.flags()
	if f == nil {
		return r.env.Reply(ctx, job, msgNoFlags)
	}
	was, err := f.ClearMRPause(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return fmt.Errorf("clear mr pause: %w", err)
	}
	if !was {
		return r.env.Reply(ctx, job, fmt.Sprintf(msgNotPaused, r.env.Mention()))
	}
	r.env.Log.Info("review: auto review resumed", "mr", job.MRKey(), "user", job.Username)
	r.updateSummaryPause(ctx, job, "")
	msg := msgResumed
	if r.resumeNeedsReview(ctx, job) && r.enqueueJob(core.Job{
		Kind: core.JobAutoReview, ProjectID: job.ProjectID, ProjectPath: job.ProjectPath, MRIID: job.MRIID,
		EnqueuedAt: r.env.Now(),
	}) {
		r.env.Log.Info("review: review of unreviewed commits enqueued after resume", "mr", job.MRKey())
		msg += msgResumedQueued
	}
	return r.env.Reply(ctx, job, msg)
}

// resumeNeedsReview reports whether an automatic review would have run
// while the MR was paused: open, not a draft, auto review on, no ignore
// marker, and the head is not reviewed yet.
func (r *Reviewer) resumeNeedsReview(ctx context.Context, job core.Job) bool {
	log := r.env.Log.With("mr", job.MRKey())
	mr, err := r.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: resume: get mr", "err", err)
		return false
	}
	if mr.State != "opened" || mr.Draft || r.hasIgnoreMarker(mr.Description) {
		return false
	}
	eff, err := r.effectiveConfig(ctx, job.ProjectID, mr)
	if err != nil {
		log.Warn("review: resume: repo config", "err", err)
		return false
	}
	if !eff.Enabled || !eff.AutoReview {
		return false
	}
	state, err := r.env.Store.GetMRState(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: resume: mr state", "err", err)
		return false
	}
	return state == nil || state.ReviewedSHA != mr.DiffRefs.HeadSHA
}

// pauseLine is the summary's pause line (by: who paused, "" = unknown).
func pauseLine(by string) string {
	if by == "" {
		return pauseLinePrefix
	}
	return fmt.Sprintf("%s (поставил %s)", pauseLinePrefix, quietUser(by))
}

// quietUser renders "@username" so that it does not notify the user again
// on every summary update (a zero-width space after "@").
func quietUser(u string) string {
	u = strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return r
		}
		return -1
	}, u)
	return "@\u200b" + u
}

// withPauseLine adds the pause line to a freshly rendered summary when the
// MR is paused (manual reviews run while paused).
func (r *Reviewer) withPauseLine(ctx context.Context, job core.Job, body string) string {
	f := r.flags()
	if f == nil {
		return body
	}
	paused, by, _, err := f.GetMRPause(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		r.env.Log.Warn("review: get mr pause for summary", "mr", job.MRKey(), "err", err)
		return body
	}
	if !paused {
		return body
	}
	return setPauseLine(body, pauseLine(by))
}

// updateSummaryPause sets (line != "") or removes the pause line in the
// existing summary note. Errors are logged only.
func (r *Reviewer) updateSummaryPause(ctx context.Context, job core.Job, line string) {
	log := r.env.Log.With("mr", job.MRKey())
	t, err := r.scanSummary(ctx, job)
	if err != nil {
		log.Warn("review: scan discussions for pause line", "err", err)
		return
	}
	if t.SummaryID == 0 {
		return
	}
	body := setPauseLine(t.SummaryBody, line)
	if body == t.SummaryBody {
		return
	}
	if err := r.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, t.SummaryID, mdtext.NeutralizeQuickActions(body)); err != nil {
		log.Warn("review: update summary pause line", "err", err)
	}
}

// scanSummary scans the MR threads for the summary note, trying the id the
// stored state remembers first (a failed state read only loses that hint).
func (r *Reviewer) scanSummary(ctx context.Context, job core.Job) (*mrthread.Threads, error) {
	var known int64
	if st, err := r.env.Store.GetMRState(ctx, job.ProjectID, job.MRIID); err == nil && st != nil {
		known = st.SummaryNoteID
	}
	return r.pub.Scan(ctx, job.ProjectID, job.MRIID, known)
}

// setPauseLine removes any pause line (and the blank line after it) from a
// summary body and, when line is not empty, inserts it after the title.
func setPauseLine(body, line string) string {
	out := summary.RemoveLines(strings.Split(body, "\n"), func(l string) bool {
		return strings.HasPrefix(strings.TrimSpace(l), pauseLinePrefix)
	})
	if line == "" {
		return strings.Join(out, "\n")
	}
	at := max(slices.IndexFunc(out, summary.IsTitle), 0) // unknown layout: after the marker line
	return strings.Join(summary.InsertAfter(out, at, line), "\n")
}

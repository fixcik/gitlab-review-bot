package review

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// blockingLine is the line of a blocking finding note that the dismissal
// replaces (finding.NoteBodyOpts).
const blockingLine = "⛔ **Блокирует мерж.** Исправьте и закройте тред."

// msgDismissed / msgDismissedQuiet answer a person who said the remark is not
// a problem (a blocking remark / one that did not block).
const (
	msgDismissed      = "Принял, @%s: снимаю блокировку с этого замечания и закрываю тред. Если передумаете, откройте тред снова."
	msgDismissedQuiet = "Принял, @%s: закрываю тред. Если передумаете, откройте его снова."
)

// dismissThread closes a bot finding thread after a Developer or above said
// in it that the remark is not a problem ("не страшно"). A blocking finding
// is rewritten first (the note says who lifted the blocking instead of
// "Блокирует мерж"); the thread is resolved either way, so GitLab does not hold
// the merge for it, the outcome is recorded as dismissed, the merge-gate line
// of the summary is recounted and the author gets one short answer. Only the
// model's own findings, never secrets, compatibility threads or linters; the
// learning step has already checked the author's role. acted is false when
// there was nothing to do. Best effort: a failed edit or resolve leaves the
// thread as it was.
func (r *Reviewer) dismissThread(ctx context.Context, job core.Job, disc *core.Discussion) (bool, error) {
	log := r.env.Log.With("mr", job.MRKey(), "user", job.Username)
	if disc == nil || len(disc.Notes) == 0 || disc.Resolved {
		return false, nil
	}
	root := disc.Notes[0]
	findingID, ok := marker.ParseFinding(root.Body)
	if !ok || !r.env.IsBotNote(root) {
		return false, nil
	}
	blocking := marker.HasBlocking(root.Body)
	if blocking {
		if err := r.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, root.ID, mdtext.NeutralizeQuickActions(unblockBody(root.Body, job.Username))); err != nil {
			log.Warn("review: dismiss: edit the finding", "discussion", disc.ID, "err", err)
			return false, nil
		}
	}
	if err := r.env.Resolve(ctx, job.ProjectID, job.MRIID, disc.ID); err != nil {
		log.Warn("review: dismiss: resolve the thread", "discussion", disc.ID, "err", err)
		if !blocking {
			return false, nil
		}
	}
	r.learning.OutcomeDismissedByReply(ctx, job, disc.ID, findingID)
	if blocking {
		if t, err := r.scanSummary(ctx, job); err == nil && t.SummaryID != 0 {
			gate, fixedPending := linters.GateCount(t)
			r.updateSummaryGate(ctx, job, t, summary.GateLine(gate, fixedPending))
		}
	}
	log.Info("review: thread closed by a reply", "discussion", disc.ID, "finding", findingID, "was_blocking", blocking)
	if st := reactFrom(ctx); st != nil {
		st.mu.Lock()
		st.quiet = false // the usual ↩️ on the reply, it was answered
		st.mu.Unlock()
	}
	msg := msgDismissedQuiet
	if blocking {
		msg = msgDismissed
	}
	return true, r.env.Reply(ctx, job, fmt.Sprintf(msg, job.Username))
}

// unblockBody is the first note of a blocking finding thread after the
// blocking was lifted: no blocking marker, and the "blocks the merge" line
// says who lifted it.
func unblockBody(body, user string) string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		switch {
		case strings.TrimSpace(l) == marker.Blocking:
			continue
		case strings.TrimSpace(l) == blockingLine:
			out = append(out, fmt.Sprintf("_Блокировка снята: @%s ответил в треде, что это не страшно._", user))
		default:
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

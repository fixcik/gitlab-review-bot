package review

// The merge-blocking thread of an MR over the size limit
// (review.big_mr.block): opened by a too-big review, closed once the MR is
// back under the limit, overridden or the block is turned off.

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// sizeThread is the bot's too-big thread found in the MR.
type sizeThread struct {
	id        string
	resolved  bool
	fixedByMe bool // the bot posted its "fixed" reply
}

// findTooBigThread returns the newest too-big thread of the bot (nil when
// none; ok=false when the discussions could not be read).
func (r *Reviewer) findTooBigThread(ctx context.Context, job core.Job) (*sizeThread, bool) {
	ds, err := r.env.GitLab.ListDiscussions(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		r.env.Log.Warn("review: list discussions for the size thread", "mr", job.MRKey(), "err", err)
		return nil, false
	}
	var found *sizeThread
	for _, d := range ds {
		if len(d.Notes) == 0 {
			continue
		}
		first := d.Notes[0]
		if first.System || !r.env.IsBotNote(first) || !strings.Contains(marker.Header(first.Body), marker.TooBigThread) {
			continue
		}
		found = &sizeThread{id: d.ID, resolved: d.Resolved, fixedByMe: mrthread.FixedByBot(d, r.env.IsBotNote)}
	}
	return found, true
}

// sizeCount is the size thread's part of the merge gate.
type sizeCount struct {
	// open: the thread blocks the merge (pending and unpublished included).
	open int
	// pending: declared fixed but left unresolved in GitLab (a Reporter
	// bot): "fixed, waiting to be closed" in the gate line.
	pending int
	// unpublished: the thread should block but could not be posted: it
	// counts in the gate anyway (fail closed) and is posted by the next
	// review.
	unpublished int
}

// tooBigThread makes sure a too-big MR has its open blocking thread when
// review.big_mr.block is on and returns its count in the gate. A thread
// a person resolved is a deliberate override: it is not re-posted while the
// MR stays too big. With block off an open thread of an earlier review is
// resolved (it must not keep holding the merge).
func (r *Reviewer) tooBigThread(ctx context.Context, job core.Job, eff config.Effective, g sizeGate, threads *mrthread.Threads) sizeCount {
	if !eff.BigMR.Block {
		return r.resolveSizeThread(ctx, job, msgSizeBlockOff, threads.OpenSizeBlocking)
	}
	th, ok := r.findTooBigThread(ctx, job)
	if !ok {
		return sizeCount{open: threads.OpenSizeBlocking}
	}
	if th != nil && !th.fixedByMe {
		if th.resolved {
			return sizeCount{} // resolved by a person
		}
		return sizeCount{open: 1} // still open
	}
	body := fmt.Sprintf("%s\n%s\n📦 **MR слишком большой для ревью: %d %s (порог %d)** — мерж заблокирован (`review.big_mr.block`).\n\n"+
		"Разбейте MR на части поменьше (советы — в сводке) или снимите порог: `%s review force`",
		marker.TooBigThread, marker.Blocking, g.lines, mdtext.LinesWord(g.lines), g.limit, r.env.Mention())
	if g.label != "" {
		body += " или метка " + mdtext.InlineCode(g.label)
	}
	body += ". Тред закроется сам после ревью: когда MR станет меньше порога или порог будет снят."
	if created, err := r.pub.Drafts(ctx, job, []publish.Draft{{Body: mdtext.NeutralizeQuickActions(body), What: "size thread"}}); err != nil || !created[0] {
		r.env.Log.Warn("review: size thread not posted, the merge gate stays blocked", "mr", job.MRKey(), "err", err)
		return sizeCount{open: 1, unpublished: 1}
	}
	return sizeCount{open: 1}
}

// msgSizeBlockOff closes the size thread once review.big_mr.block is off.
const msgSizeBlockOff = "блокировка мержа по размеру MR выключена (`review.big_mr.block`)"

// closeTooBigThread resolves the open too-big thread after a review ran
// (the MR shrank below the limit, or the limit was overridden), whether or
// not review.big_mr.block is still on. It returns the thread's count in the
// merge gate (see resolveSizeThread).
func (r *Reviewer) closeTooBigThread(ctx context.Context, job core.Job, g sizeGate, threads *mrthread.Threads) sizeCount {
	comment := fmt.Sprintf("ревью выполнено: изменений %d %s, порог %d", g.lines, mdtext.LinesWord(g.lines), g.limit)
	switch {
	case !g.on():
		comment = "ограничение размера MR выключено, ревью выполнено"
	case g.override == overrideForce:
		comment = fmt.Sprintf("порог снят командой `%s review force`, ревью выполнено", r.env.Mention())
	case g.override == overrideLabel:
		comment = "порог снят меткой " + mdtext.InlineCode(g.label) + ", ревью выполнено"
	}
	return r.resolveSizeThread(ctx, job, comment, threads.OpenSizeBlocking)
}

// resolveSizeThread resolves the bot's open size thread with the fixed
// reply. It returns the thread's count in the merge gate: nothing when
// there is none or it is resolved now (one the bot already declared fixed
// counts as fixedPendingBlocking of the scan); open when it stays
// unresolved in GitLab: a Reporter bot without a service token (also
// pending: declared fixed) or any other resolve error (still open, not
// fixed). known is the scan's count, used (as open, not fixed) when the
// discussions cannot be read.
func (r *Reviewer) resolveSizeThread(ctx context.Context, job core.Job, comment string, known int) sizeCount {
	th, ok := r.findTooBigThread(ctx, job)
	if !ok {
		return sizeCount{open: known}
	}
	if th == nil || th.resolved || th.fixedByMe {
		return sizeCount{}
	}
	switch done, unresolved := r.pub.MarkFixed(ctx, job, th.id, comment); {
	case !done:
		return sizeCount{open: 1}
	case unresolved:
		return sizeCount{open: 1, pending: 1}
	}
	return sizeCount{}
}

// closeSizeThreadNoChanges: the reviewed range has no changes left (the
// big change was reverted), so the MR is no longer over the limit: close
// an open size thread and recount the summary's gate line (no review, no
// summary re-render follows).
func (r *Reviewer) closeSizeThreadNoChanges(ctx context.Context, job core.Job, knownSummary int64) {
	t, err := r.pub.Scan(ctx, job.ProjectID, job.MRIID, knownSummary)
	if err != nil {
		r.env.Log.Warn("review: scan discussions for the size thread", "mr", job.MRKey(), "err", err)
		return
	}
	if t.OpenSizeBlocking == 0 {
		return
	}
	left := r.resolveSizeThread(ctx, job, "в проверяемом диапазоне больше нет изменений", t.OpenSizeBlocking)
	blocking, pending := linters.GateCount(t)
	blocking += left.open - 1 // the newest size thread, decided now
	pending += left.pending   // declared fixed, not resolved in GitLab
	r.updateSummaryGate(ctx, job, t, summary.GateLine(blocking, pending))
}

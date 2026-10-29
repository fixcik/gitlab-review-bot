package review

// "@bot resolve": resolve every open remark thread of the bot through the
// Moderator (service token), then clear the summary's merge-gate line.

import (
	"context"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const (
	msgResolveNone   = "Открытых тредов бота в этом MR нет."
	msgResolved      = "✅ Закрыто тредов бота: %d."
	msgResolveFailed = " Не удалось закрыть: %d (подробности в логах бота)."
)

// openBotThreads lists unresolved resolvable discussions the bot started
// with a remark marker (findings, breaking changes, lint, ...). The summary
// and plain replies of the bot to commands are not remark threads.
func (r *Reviewer) openBotThreads(ctx context.Context, job core.Job) ([]botThread, error) {
	ds, err := r.env.GitLab.ListDiscussions(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return nil, fmt.Errorf("list discussions: %w", err)
	}
	var ids []botThread
	for _, d := range ds {
		if !d.Resolvable || d.Resolved || len(d.Notes) == 0 {
			continue
		}
		first := d.Notes[0]
		header := marker.Header(first.Body)
		if first.System || !r.env.IsBotNote(first) || marker.IsSummaryNote(first.Body) || strings.Contains(header, "reviewbot:summary") ||
			!marker.BotThreadRe.MatchString(header) {
			continue
		}
		fid, _ := marker.ParseFinding(first.Body)
		ids = append(ids, botThread{DiscussionID: d.ID, FindingID: fid})
	}
	return ids, nil
}

// botThread is an open remark thread of the bot; FindingID is set for
// review findings (their outcomes are tracked, see learning/outcomes.go).
type botThread struct {
	DiscussionID string
	FindingID    string
}

// handleResolveAll resolves every open remark thread of the bot through
// (the service token when there is one, else the bot's own) and records the findings' outcomes as resolved by command.
func (r *Reviewer) handleResolveAll(ctx context.Context, job core.Job) error {
	log := r.env.Log.With("mr", job.MRKey(), "user", job.Username)
	ids, err := r.openBotThreads(ctx, job)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return r.env.Reply(ctx, job, msgResolveNone)
	}
	done, failed := 0, 0
	for _, th := range ids {
		if err := r.env.Resolve(ctx, job.ProjectID, job.MRIID, th.DiscussionID); err != nil {
			log.Warn("review: resolve bot thread", "discussion", th.DiscussionID, "err", err)
			failed++
			continue
		}
		done++
		if th.FindingID != "" {
			// The bot resolved it (on a person's command): not a human
			// dismissal of the finding, so it must not feed rule synthesis.
			r.learning.OutcomeResolvedByCommand(ctx, job, th.DiscussionID, th.FindingID)
		}
	}
	log.Info("review: bot threads resolved by command", "resolved", done, "failed", failed)
	if failed == 0 {
		r.clearSummaryGate(ctx, job)
	}
	msg := fmt.Sprintf(msgResolved, done)
	if failed > 0 {
		msg += fmt.Sprintf(msgResolveFailed, failed)
	}
	return r.env.Reply(ctx, job, msg)
}

// clearSummaryGate rewrites the "Мерж заблокирован" line of the summary
// after every bot thread was resolved.
func (r *Reviewer) clearSummaryGate(ctx context.Context, job core.Job) {
	t, err := r.scanSummary(ctx, job)
	if err != nil || t.SummaryID == 0 {
		return
	}
	body, ok := summary.SetGateLine(t.SummaryBody, summary.GateClearLine)
	if !ok || body == t.SummaryBody {
		return
	}
	if err := r.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, t.SummaryID, mdtext.NeutralizeQuickActions(body)); err != nil {
		r.env.Log.Warn("review: update summary gate", "mr", job.MRKey(), "err", err)
	}
}

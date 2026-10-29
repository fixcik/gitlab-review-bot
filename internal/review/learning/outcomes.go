package learning

// Learning from actions. Every finding the bot posts as a thread gets an
// outcome row (learn.OutcomeStore, implemented by *store.SQLite):
//
//   - open: recorded when the thread is published (non-blocking findings the
//     service token resolves right away are recorded as resolved_by_bot);
//   - fixed: the bot resolved the thread because Claude reported it fixed
//     (or asked a human to close it as a Reporter), or a human resolved it
//     after the code around the finding changed;
//   - dismissed: a human resolved the thread and lines path:line±3 did not
//     change between the posted head and the current head. "A human" means
//     a resolve the bot did not perform according to its own records (every
//     bot resolve updates the outcome); GitLab's resolved_by is not used,
//     because the service token may be a person's own token;
//   - resolved_by_bot: the bot resolved it without a verdict (non-blocking
//     auto-resolve, a chat answer that closed the thread).
//
// Threads are classified on the next review of the same MR (that is when a
// checkout of the current head exists to compare the lines). When a project
// collects review.learn.dismiss_threshold dismissed findings not yet used,
// a light-model run (no tools) turns the last <= 20 of them into 0..3 team
// rules, stored through learn.Store and announced in the summary of the MR
// whose review triggered the run (that summary and the next one).
//
// Attribution of the synthesis run (token accounting): kind "learning" and
// no user (Username ""): the run is caused by dismissals collected across
// the project, not by the author of the MR being reviewed, so charging it
// to that author would be misleading. Its usage row (UsageLearn) has
// UserID 0 for the same reason.

import (
	"context"
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

const (
	// statsWindow is the period of the acceptance stats in "@bot правила".
	statsWindow = 30 * 24 * time.Hour
	// MaxOutcomeBody caps the stored body excerpt.
	MaxOutcomeBody = 600
)

// OutcomeStore returns the outcome store, or nil when there is none or
// learning has no rule store.
func (l *Feature) OutcomeStore() learn.OutcomeStore {
	if l.learnings == nil || l.outcomes == nil {
		return nil
	}
	return l.outcomes
}

// OutcomesOn reports whether outcomes are tracked for the repo:
// review.learn.from_actions, learning on for the repo and a store.
func (l *Feature) OutcomesOn(eff config.Effective) bool {
	return l.env.Cfg.Review.Learn.FromActions && l.RulesOn(eff) && l.OutcomeStore() != nil
}

// RecordPublished stores an outcome for every finding just posted as a
// thread. Non-blocking findings are resolved by the bot right after
// publishing; they are recorded as resolved_by_bot.
func (l *Feature) RecordPublished(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, published []output.Finding) {
	if !l.OutcomesOn(eff) || len(published) == 0 {
		return
	}
	ost := l.OutcomeStore()
	for _, f := range published {
		status := learn.OutcomeOpen
		if !l.env.Blocking(f.Severity) {
			status = learn.OutcomeResolvedByBot
		}
		if _, err := ost.RecordOutcome(ctx, learn.Outcome{
			ProjectPath: job.ProjectPath, MRIID: job.MRIID, FindingID: finding.ID(f.Path, f.Title),
			Title: mdtext.OneLine(f.Title), Severity: string(f.Severity), Path: f.Path, Line: f.Line,
			BodyExcerpt: mdtext.Shorten(f.Body, MaxOutcomeBody), HeadSHAAtPost: mr.DiffRefs.HeadSHA,
			Status: status, CreatedAt: l.env.Now(),
		}); err != nil {
			l.env.Log.Warn("outcomes: record", "mr", job.MRKey(), "err", err)
		}
	}
}

// outcomeBotClosed marks the open outcome of a finding thread the bot just
// closed (fixed: Claude said it is fixed; resolved_by_bot: no verdict).
func (l *Feature) outcomeBotClosed(ctx context.Context, job core.Job, eff config.Effective, discussionID, findingID, status string) {
	if !l.OutcomesOn(eff) {
		return
	}
	if _, err := l.OutcomeStore().CloseOpenOutcome(ctx, job.ProjectPath, job.MRIID, discussionID, findingID, status); err != nil {
		l.env.Log.Warn("outcomes: close", "mr", job.MRKey(), "status", status, "err", err)
	}
}

// outcomeDismissedByRule: a human reply in a finding thread became a rule;
// the finding counts as dismissed and is linked to that rule (so it is not
// used for synthesis again).
func (l *Feature) outcomeDismissedByRule(ctx context.Context, job core.Job, findingID string, ruleID int64) {
	ost := l.OutcomeStore()
	if !l.env.Cfg.Review.Learn.FromActions || ost == nil || findingID == "" {
		return
	}
	open, err := ost.ListOutcomes(ctx, learn.OutcomeFilter{ProjectPath: job.ProjectPath, MRIID: job.MRIID, Status: learn.OutcomeOpen})
	if err != nil {
		l.env.Log.Warn("outcomes: list", "mr", job.MRKey(), "err", err)
		return
	}
	for _, o := range open {
		if o.DiscussionID == job.DiscussionID || (o.DiscussionID == "" && o.FindingID == findingID) {
			if err := ost.SetOutcomeStatus(ctx, o.ID, learn.OutcomeDismissed); err == nil {
				_ = ost.MarkOutcomesUsed(ctx, []int64{o.ID}, ruleID)
			}
			return
		}
	}
}

// OutcomeFixed records that the bot closed a finding thread because Claude
// reported it fixed (resolved, or asked a human to close it as a Reporter).
func (l *Feature) OutcomeFixed(ctx context.Context, job core.Job, eff config.Effective, discussionID, findingID string) {
	l.outcomeBotClosed(ctx, job, eff, discussionID, findingID, learn.OutcomeFixed)
}

// OutcomeResolvedByCommand records that "@bot resolve" closed a finding
// thread. Every resolve the bot performs must update the outcome, else the
// next review takes the thread for one a human closed without a fix
// (dismissed). The
// repo config is not loaded for the command; an outcome only exists when
// tracking was on for the repo when the finding was posted.
func (l *Feature) OutcomeResolvedByCommand(ctx context.Context, job core.Job, discussionID, findingID string) {
	ost := l.OutcomeStore()
	if ost == nil || !l.env.Cfg.Review.Learn.FromActions {
		return
	}
	if _, err := ost.CloseOpenOutcome(ctx, job.ProjectPath, job.MRIID, discussionID, findingID, learn.OutcomeResolvedByBot); err != nil {
		l.env.Log.Warn("outcomes: close", "mr", job.MRKey(), "status", learn.OutcomeResolvedByBot, "err", err)
	}
}

// OutcomeDismissedByReply records that a person said in the thread that the
// remark is not a problem and the bot lifted its blocking (a human decision:
// dismissed, unlike a resolve the bot did on its own).
func (l *Feature) OutcomeDismissedByReply(ctx context.Context, job core.Job, discussionID, findingID string) {
	ost := l.OutcomeStore()
	if ost == nil || !l.env.Cfg.Review.Learn.FromActions {
		return
	}
	if _, err := ost.CloseOpenOutcome(ctx, job.ProjectPath, job.MRIID, discussionID, findingID, learn.OutcomeDismissed); err != nil {
		l.env.Log.Warn("outcomes: close", "mr", job.MRKey(), "status", learn.OutcomeDismissed, "err", err)
	}
}

// OutcomeChatResolved records that a chat answer closed a finding thread
// ("fixed or not an issue" — no verdict on the finding itself).
func (l *Feature) OutcomeChatResolved(ctx context.Context, job core.Job, eff config.Effective, findingID string) {
	l.outcomeBotClosed(ctx, job, eff, job.DiscussionID, findingID, learn.OutcomeResolvedByBot)
}

// AfterReviewOutcomes binds and classifies the MR's open outcomes, runs
// synthesis when the project reached the threshold, and returns the summary
// line announcing synthesized rules ("" for none). Never fails the review.
func (l *Feature) AfterReviewOutcomes(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, co core.Checkout) string {
	if !l.OutcomesOn(eff) {
		return ""
	}
	l.classifyOutcomes(ctx, job, job.MRIID, "", co)
	l.classifyOtherMRs(ctx, job, co)
	l.synthesizeRules(ctx, job, mr, eff)
	return l.synthAnnouncement(ctx, job)
}

// OutcomeStatsFooter is the per-project acceptance stats for the last 30
// days ("" when outcome tracking is off or there is nothing to show).
func (l *Feature) OutcomeStatsFooter(ctx context.Context, job core.Job) string {
	ost := l.OutcomeStore()
	if !l.env.Cfg.Review.Learn.FromActions || ost == nil {
		return ""
	}
	st, err := ost.OutcomeStats(ctx, job.ProjectPath, l.env.Now().Add(-statsWindow))
	if err != nil {
		l.env.Log.Warn("outcomes: stats", "mr", job.MRKey(), "err", err)
		return ""
	}
	if st.Open+st.Fixed+st.Dismissed+st.ResolvedByBot+st.RejectedByVerifier == 0 {
		return ""
	}
	prec := "—"
	if p, ok := st.Precision(); ok {
		prec = fmt.Sprintf("%.0f%%", p*100)
	}
	s := fmt.Sprintf("\n📊 **Замечания бота в `%s` за 30 дней:** исправлено %d · закрыто без правок %d · открыто %d · точность (исправлено / (исправлено + закрыто без правок)) %s",
		job.ProjectPath, st.Fixed, st.Dismissed, st.Open, prec)
	if st.ResolvedByBot > 0 {
		s += fmt.Sprintf(" · закрыто ботом без оценки %d", st.ResolvedByBot)
	}
	if st.RejectedByVerifier > 0 {
		s += fmt.Sprintf(" · отклонено перепроверкой до публикации %d", st.RejectedByVerifier)
	}
	return s + "\n"
}

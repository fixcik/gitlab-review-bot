package review

// "@bot help" (core.JobHelp): the bot's commands, whether the repository
// takes part, and the author's remaining limits. No Claude run, no limits.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// handleHelp answers "@bot help": the command list of every feature, the
// repository's state and the author's remaining limits for the next hour.
func (r *Reviewer) handleHelp(ctx context.Context, job core.Job) error {
	lim := r.env.Cfg.Limits
	userLimits := core.UserLimits{
		RerunsPerHour: lim.UserRerunsPerHour, ChatPerHour: lim.UserChatPerHour, MRRerunCooldown: lim.MRRerunCooldown.D(),
	}
	enabled, notOptedIn := true, ""
	if mr, err := r.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID); err != nil {
		r.env.Log.Warn("review: help: get mr", "mr", job.MRKey(), "err", err)
	} else {
		eff, err := r.effectiveConfig(ctx, job.ProjectID, mr)
		var rc *repoConfigError
		switch {
		case err == nil:
			userLimits, enabled = eff.UserLimits(), eff.Enabled
		case errors.As(err, &rc) && rc.notOptedIn:
			notOptedIn = fmt.Sprintf(msgHelpNotOptedIn, cfgCode(mr.TargetBranch))
		default:
			r.env.Log.Warn("review: help: repo config", "mr", job.MRKey(), "err", err)
		}
	}

	var b strings.Builder
	b.WriteString("### 🤖 Команды бота\n\n")
	b.WriteString(summary.CommandsList(r.env.Mention()))
	b.WriteString(r.learning.RulesCommandsList())
	b.WriteString(r.mrCommandsList())
	b.WriteString(r.learning.MemoryCommandsList())
	b.WriteString("\nАвтоматическое ревью запускается при открытии MR и при новых коммитах (кроме Draft). ")
	b.WriteString("Переход из Draft в Ready запускает полное ревью. Команды доступны с ролью Developer и выше.\n")
	if !enabled {
		b.WriteString("\n⚠️ " + msgDisabled + "\n")
	}
	b.WriteString(notOptedIn)

	rem, err := r.env.Limiter.Remaining(ctx, job.UserID, job.Username, userLimits)
	if err != nil {
		r.env.Log.Warn("review: help: remaining", "err", err)
	} else {
		b.WriteString("\n**Оставшиеся лимиты на ближайший час:**\n")
		fmt.Fprintf(&b, "- перезапуски ревью: %s\n", remainingText(rem.Reruns))
		fmt.Fprintf(&b, "- вопросы: %s\n", remainingText(rem.Chat))
		fmt.Fprintf(&b, "- общий лимит запусков: %s\n", remainingText(rem.Global))
		if d := userLimits.MRRerunCooldown; d > 0 {
			fmt.Fprintf(&b, "- перезапуск одного MR — не чаще раза в %s\n", fmtMinutes(d.Minutes()))
		}
	}
	return r.env.Reply(ctx, job, b.String())
}

// remainingText renders a remaining limit (negative = no limit).
func remainingText(n int) string {
	if n < 0 {
		return "без лимита"
	}
	return fmt.Sprintf("%d", n)
}

// fmtMinutes renders a duration in minutes as hours, minutes or seconds.
func fmtMinutes(m float64) string {
	if m >= 60 && int(m)%60 == 0 {
		return fmt.Sprintf("%d ч", int(m)/60)
	}
	if m < 1 {
		return fmt.Sprintf("%d с", int(m*60))
	}
	return fmt.Sprintf("%d мин", int(m))
}

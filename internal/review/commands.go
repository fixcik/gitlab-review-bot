package review

// MR commands that never run Claude and never consume limits:
//
//	@bot pause   / пауза          stop automatic reviews of this MR
//	@bot resume  / продолжить     restart them (+ enqueue a review of new commits)
//	@bot ignore  / игнорировать   in the MR DESCRIPTION: no auto reviews while present
//	@bot resolve / закрой треды   resolve every open bot thread (service token)
//	@bot config  / конфиг         effective configuration for this MR
//
// All of them require Developer+. The bot never changes the repository or
// the MR itself: it only writes notes, and resolving goes through the
// Moderator (service token).

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// MRFlags stores per-MR flags set by commands (Deps.MRFlags, *store.SQLite).
type MRFlags interface {
	GetMRPause(ctx context.Context, projectID, mrIID int64) (paused bool, by string, at time.Time, err error)
	SetMRPause(ctx context.Context, projectID, mrIID int64, by string, at time.Time) error
	ClearMRPause(ctx context.Context, projectID, mrIID int64) (bool, error)
}

// flags returns Deps.MRFlags, or nil when none is wired.
func (r *Reviewer) flags() MRFlags { return r.mrFlags }

// SetEnqueue gives the reviewer the queue's Enqueue, used by "@bot resume"
// to schedule a review of commits pushed while the MR was paused. Call it
// before the queue starts.
func (r *Reviewer) SetEnqueue(f func(core.Job)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enqueue = f
}

// enqueueJob schedules job through SetEnqueue's function; false when none
// is set yet.
func (r *Reviewer) enqueueJob(job core.Job) bool {
	r.mu.Lock()
	f := r.enqueue
	r.mu.Unlock()
	if f == nil {
		return false
	}
	f(job)
	return true
}

const (
	msgIgnoreHowTo  = "Чтобы бот не запускал автоматическое ревью этого MR, добавьте строку `%[1]s ignore` (или `%[1]s игнорировать`) в **описание** MR — в комментарии она не действует. Пока строка есть в описании, авторевью не выполняется; `%[1]s review`, вопросы и правила работают как обычно. Уберите строку, чтобы вернуть авторевью. Остановить авторевью без правки описания: `%[1]s pause`."
	msgIgnoreActive = "\n\nСейчас маркер в описании **есть** — авторевью не выполняется."
)

// handleMRCommand runs JobPause, JobResume, JobIgnore, JobResolveAll and
// JobConfig. No Claude run, no limits.
func (r *Reviewer) handleMRCommand(ctx context.Context, job core.Job) error {
	ok, err := r.env.CheckAccess(ctx, job)
	if err != nil || !ok {
		return err
	}
	// A repository that is not opted in (repoconfig.go) gets one reply on
	// how to connect it; an invalid config does not stop these commands
	// (config answers with the errors itself).
	mr, err := r.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return fmt.Errorf("get mr: %w", err)
	}
	if _, err := r.targetRepoConfig(ctx, job.ProjectID, mr); err != nil {
		var rc *repoConfigError
		if !errors.As(err, &rc) {
			return err
		}
		if rc.notOptedIn {
			return r.denyRepoConfig(ctx, job, mr, rc)
		}
	}
	switch job.Kind {
	case core.JobPause:
		return r.handlePause(ctx, job)
	case core.JobResume:
		return r.handleResume(ctx, job)
	case core.JobIgnore:
		return r.handleIgnore(ctx, job)
	case core.JobResolveAll:
		return r.handleResolveAll(ctx, job)
	case core.JobConfig:
		return r.handleConfig(ctx, job)
	}
	return fmt.Errorf("review: unknown mr command %q", job.Kind)
}

// ---------------------------------------------------------------------------
// Auto-review gate
// ---------------------------------------------------------------------------

// skipAuto reports whether an automatic review of mr must be skipped: the
// MR is paused, or its description carries the ignore marker.
func (r *Reviewer) skipAuto(ctx context.Context, job core.Job, mr *core.MergeRequest) (bool, error) {
	log := r.env.Log.With("mr", job.MRKey())
	if r.hasIgnoreMarker(mr.Description) {
		log.Info("review: ignore marker in mr description, skipping auto review")
		return true, nil
	}
	f := r.flags()
	if f == nil {
		return false, nil
	}
	paused, by, _, err := f.GetMRPause(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return false, fmt.Errorf("get mr pause: %w", err)
	}
	if paused {
		log.Info("review: auto review paused, skipping", "paused_by", by)
		return true, nil
	}
	return false, nil
}

// mentions are the spellings that address the bot.
func (r *Reviewer) mentions() []string {
	out := []string{r.env.Mention()}
	if r.env.BotUsername != "" && !strings.EqualFold(out[0], "@"+r.env.BotUsername) {
		out = append(out, "@"+r.env.BotUsername)
	}
	return out
}

// hasIgnoreMarker reports whether text contains "@bot ignore" or "@bot
// игнорировать" (case-insensitive, the mention as a whole word).
func (r *Reviewer) hasIgnoreMarker(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, m := range r.mentions() {
		if m == "" || m == "@" {
			continue
		}
		re := regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_@.\-])` + regexp.QuoteMeta(m) +
			`[\s,:;—–\-]+(?:ignore|игнорировать|игнорируй)(?:$|[^\p{L}\p{N}_])`)
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// handleIgnore explains the ignore marker (it only works in the MR
// description) and says whether the description carries it now.
func (r *Reviewer) handleIgnore(ctx context.Context, job core.Job) error {
	msg := fmt.Sprintf(msgIgnoreHowTo, r.env.Mention())
	if mr, err := r.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID); err == nil && r.hasIgnoreMarker(mr.Description) {
		msg += msgIgnoreActive
	}
	return r.env.Reply(ctx, job, msg)
}

// mrCommandsList extends "@bot help".
// The lines are core.KindInfo.Help.
func (r *Reviewer) mrCommandsList() string {
	var b strings.Builder
	for _, k := range []core.JobKind{core.JobPause, core.JobResume, core.JobCheck, core.JobResolveAll, core.JobConfig, core.JobIgnore} {
		b.WriteString(k.Help(r.env.Mention()))
	}
	return b.String()
}

package review

// "@bot check" (core.JobCheck; aliases recheck / проверь / перепроверь):
// re-check the bot's OPEN findings against the CURRENT head without a new
// commit and close the fixed ones.
//
//   - In the MR's general discussion (or any thread that is not a bot
//     remark): every open finding thread of the bot (Claude-originated).
//     Open lint and compat threads are only reported: re-checking them needs
//     the tools (linters, oasdiff), which this cheap command does not run;
//     they close after the next push/review.
//   - In a bot finding thread: only that finding.
//
// Nothing open -> reply, no booking and no Claude run. Otherwise one small
// Claude run (claude.model, no advisor or search subagent, read-only
// Read/Grep/Glob plus the context repositories) answers fixed / still_open
// per finding; fixed ones go through publish.MarkFixed (the same reply, resolve with
// the service token and Reporter fallback as in a review) and their outcome
// is recorded; then the summary's merge-gate line is rewritten.
//
// Limits: a rerun (UsageRerun: per-user hourly limit + the global limit,
// booked with Reserve) without the per-MR rerun cooldown: the usage is
// recorded without the MR key, so the cooldown neither applies to it nor
// starts from it (see jobUsage).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const (
	msgCheckNone         = "Открытых замечаний бота нет."
	msgCheckDone         = "Проверил %d %s на `%s`: закрыто %d, остаются %d."
	msgCheckPending      = " Ждут закрытия человеком: %d (у бота нет прав закрывать треды)."
	msgCheckPostponed    = "⏳ Перепроверка отложена до %s: %s"
	msgCheckFailed       = "❌ Перепроверка не удалась: %s"
	msgCheckThreadClosed = "Замечание этого треда уже закрыто или отмечено исправленным — перепроверять нечего."
	msgCheckToolThread   = "Этот тред — %s: команда `%s check` его не перепроверяет (нужен запуск %s). Он закроется после следующего пуша или ревью."
	msgCheckToolNote     = "Открытые треды %s команда не перепроверяет: для них нужен запуск %s. Они закроются после следующего пуша или ревью."
	msgCheckResolveFail  = "не удалось закрыть тред автоматически"
	// What re-checking lint / compat threads needs (msgCheckToolThread,
	// msgCheckToolNote).
	checkToolsLint   = "линтеров"
	checkToolsCompat = "проверок совместимости (oasdiff)"
	// maxCheckList is how many still-open findings the reply lists before
	// collapsing the rest.
	maxCheckList = 10
	// checkBodyRunes caps a finding body in the prompt.
	checkBodyRunes = 1000
)

// checkScope is what a "@bot check" command covers.
type checkScope struct {
	ids []string // open finding ids to re-check, in thread order
	// thread: the command was posted in an open bot finding thread (ids
	// holds just that finding).
	thread bool
	// tool is "lint" or "compat" when the command was posted in such a
	// thread of the bot (not re-checked here).
	tool string
	// closed: the command was posted in a bot finding thread that is no
	// longer open for the bot (resolved, or already marked fixed).
	closed bool
}

// checkScopeFor picks the findings: the one of the command's thread when it
// is a bot finding thread, else every open finding.
func (r *Reviewer) checkScopeFor(ctx context.Context, job core.Job, t *mrthread.Threads) checkScope {
	if job.DiscussionID == "" {
		return allOpenScope(t)
	}
	for _, id := range t.OpenOrder {
		if t.Open[id].DiscussionID == job.DiscussionID {
			return checkScope{ids: []string{id}, thread: true}
		}
	}
	for _, oc := range t.OpenCompat {
		if oc.DiscussionID == job.DiscussionID {
			return checkScope{tool: "compat"}
		}
	}
	// A general note has a discussion of its own too: look at its first
	// note to tell a (closed) bot remark thread from anything else.
	d, err := r.env.GitLab.GetDiscussion(ctx, job.ProjectID, job.MRIID, job.DiscussionID)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			r.env.Log.Warn("review: check: get discussion, checking every open finding", "mr", job.MRKey(), "discussion", job.DiscussionID, "err", err)
		}
		return allOpenScope(t)
	}
	if d == nil || len(d.Notes) == 0 {
		return allOpenScope(t)
	}
	first := d.Notes[0]
	if first.System || !r.env.IsBotNote(first) {
		return allOpenScope(t)
	}
	if _, ok := marker.ParseFinding(first.Body); ok {
		return checkScope{closed: true}
	}
	if _, _, ok := marker.ParseCompat(first.Body); ok {
		return checkScope{tool: "compat"}
	}
	if _, _, ok := marker.ParseLint(first.Body); ok {
		return checkScope{tool: "lint"}
	}
	return allOpenScope(t)
}

// allOpenScope covers every open finding of the bot.
func allOpenScope(t *mrthread.Threads) checkScope {
	return checkScope{ids: append([]string(nil), t.OpenOrder...)}
}

// toolThreadsNote says that open lint / compat threads are not re-checked
// ("" when there are none).
func toolThreadsNote(t *mrthread.Threads) string {
	var kinds, tools []string
	if t.OpenLint > 0 {
		kinds = append(kinds, fmt.Sprintf("линтеров и сканеров (%d)", t.OpenLint))
		tools = append(tools, checkToolsLint)
	}
	if n := len(t.OpenCompat); n > 0 {
		kinds = append(kinds, fmt.Sprintf("несовместимых изменений (%d)", n))
		tools = append(tools, checkToolsCompat)
	}
	if len(kinds) == 0 {
		return ""
	}
	return fmt.Sprintf(msgCheckToolNote, strings.Join(kinds, " и "), strings.Join(tools, " и "))
}

// withNote appends note as its own paragraph ("" = msg unchanged).
func withNote(msg, note string) string {
	if note == "" {
		return msg
	}
	return msg + "\n\n" + note
}

// checkLeft is a finding that stays open after the check.
type checkLeft struct {
	f   mrthread.OpenFinding
	why string
}

// handleCheck runs "@bot check" (core.JobCheck): see the file comment.
func (r *Reviewer) handleCheck(ctx context.Context, job core.Job) error {
	log := r.env.Log.With("mr", job.MRKey(), "kind", job.Kind, "user", job.Username)

	mr, err := r.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return fmt.Errorf("get mr: %w", err)
	}
	jobctx.AttributeMR(ctx, job, mr)
	if mr.State != "opened" {
		return r.env.Reply(ctx, job, msgNotOpened)
	}
	eff, ok, err := r.commandConfig(ctx, job, mr)
	if !ok {
		return err
	}
	if !eff.Enabled {
		return r.env.Reply(ctx, job, msgDisabled)
	}
	resumed := core.RunAdmitted(ctx) // charged before, then postponed (or replayed)
	if resumed {
		log.Info("review: check: resuming postponed command")
	} else {
		ok, err := r.env.CheckAccess(ctx, job)
		if err != nil || !ok {
			return err
		}
	}

	// Open bot threads (before any booking: nothing open costs nothing).
	var known int64
	state, err := r.env.Store.GetMRState(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return fmt.Errorf("get mr state: %w", err)
	}
	if state != nil {
		known = state.SummaryNoteID
	}
	t, err := r.pub.Scan(ctx, job.ProjectID, job.MRIID, known)
	if err != nil {
		return r.failCheck(ctx, job, err)
	}
	sc := r.checkScopeFor(ctx, job, t)
	switch {
	case sc.tool == "lint":
		return r.env.Reply(ctx, job, fmt.Sprintf(msgCheckToolThread, "результат линтера или сканера", r.env.Mention(), checkToolsLint))
	case sc.tool == "compat":
		return r.env.Reply(ctx, job, fmt.Sprintf(msgCheckToolThread, "несовместимое изменение", r.env.Mention(), checkToolsCompat))
	case sc.closed:
		return r.env.Reply(ctx, job, msgCheckThreadClosed)
	case len(sc.ids) == 0:
		log.Info("review: check: no open findings")
		return r.env.Reply(ctx, job, withNote(msgCheckNone, toolThreadsNote(t)))
	}

	// Limits: a rerun without the per-MR cooldown, booked with the global
	// limit; the Claude run commits the booking, any other exit releases it.
	rreq := core.ReserveRequest{Usage: env.JobUsage(job), Username: job.Username, Global: true}
	if !resumed {
		ul := eff.UserLimits()
		ul.MRRerunCooldown = 0
		rreq.User = &ul
	}
	dec, booked, err := r.env.Limiter.Reserve(ctx, rreq)
	if err != nil {
		return err
	}
	if booked != nil {
		defer booked.Release()
		ctx = core.WithReservation(ctx, booked)
	}
	if !dec.Allowed {
		jobctx.Denied(ctx) // 🚫 (reactions.go)
		return r.env.Reply(ctx, job, "⛔ "+dec.Reason)
	}

	co, err := r.env.Repos.Checkout(ctx, job.ProjectPath, mr)
	if err != nil {
		return r.failCheck(ctx, job, fmt.Errorf("checkout: %w", err))
	}
	defer pipeline.CloseCheckout(log, co)
	rc := r.crossrepo.AcquireContext(ctx, job, eff, mr, true)
	defer rc.Release()
	rc.NoteProblems(ctx)
	bs := r.env.PrepareBase(ctx, job, co, mr) // /base (env/base.go)
	defer bs.Release()

	head := mr.DiffRefs.HeadSHA
	lang := prompts.LanguageName(eff.Language)
	data := checkData{Base: diffcap.FullDiffBase(mr), Head: head, BaseDir: bs.Root, ContextBlock: rc.Block, Language: lang}
	for _, id := range sc.ids {
		f := t.Open[id]
		data.Findings = append(data.Findings, prompts.OpenFinding{ID: id, Path: f.Path, Line: f.Line, Title: f.Title, Body: mdtext.Shorten(f.Body, checkBodyRunes)})
	}
	sys, err := prompts.Render(checkSystemTmpl, prompts.System{Language: lang, BaseDir: bs.Root})
	if err != nil {
		return fmt.Errorf("render check system prompt: %w", err)
	}
	prompt, err := prompts.Render(checkUserTmpl, data)
	if err != nil {
		return fmt.Errorf("render check prompt: %w", err)
	}
	var verdicts map[string]checkVerdict
	err = r.env.RunStructured(ctx, job, core.RunRequest{
		Dir: co.Dir(), Prompt: prompt, SystemPrompt: sys, BaseDir: bs.Dir,
		JSONSchema: checkSchema,
		// claude.model only: no advisor and no search subagent (cheap).
		Model: r.env.Cfg.Claude.Model, Timeout: r.env.Cfg.Claude.Timeout.D(),
		ContextDirs: rc.Dirs,
	}, func(s string) error {
		v, err := parseCheckOutput(s, sc.ids)
		if err == nil {
			verdicts = v
		}
		return err
	})
	if err != nil {
		return r.checkRunFailed(ctx, job, log, err)
	}
	tl := r.closeFixedFindings(ctx, job, eff, t, sc.ids, verdicts, rc)

	// The summary's merge-gate line: every open blocking bot thread after
	// this check (findings, compat, lint, the size thread, fixed but not
	// yet closed), counted like every other gate recount (linters.GateCount). The
	// blocking findings closed now no longer count; the pending ones among
	// them are added back as fixed but not yet closed.
	blocking, fixedPending := linters.GateCount(t)
	blocking, fixedPending = blocking-tl.closedBlocking+tl.pendingBlocking, fixedPending+tl.pendingBlocking
	r.updateSummaryGate(ctx, job, t, summary.GateLine(blocking, fixedPending))

	log.Info("review: check done", "head", head, "checked", len(sc.ids), "closed", tl.closed, "left", len(tl.left))
	msg := fmt.Sprintf(msgCheckDone, len(sc.ids),
		mdtext.Plural(len(sc.ids), "открытое замечание", "открытых замечания", "открытых замечаний"), mdtext.ShortSHA(head), tl.closed, len(tl.left))
	if tl.pending > 0 {
		msg += fmt.Sprintf(msgCheckPending, tl.pending)
	}
	msg += renderCheckLeft(tl.left)
	if !sc.thread {
		// General check: mention the tool threads it did not touch.
		msg = withNote(msg, toolThreadsNote(t))
	}
	return r.env.Reply(ctx, job, msg)
}

// checkRunFailed handles a failed check run: a Claude limit postpones the
// command, an invalid answer and any other error are reported to its author.
func (r *Reviewer) checkRunFailed(ctx context.Context, job core.Job, log *slog.Logger, err error) error {
	var rl *core.ErrClaudeRateLimited
	if errors.As(err, &rl) {
		at := r.env.RetryAt(rl.ResetAt)
		log.Warn("review: check: claude rate limited", "retry_at", at, "msg", rl.Message)
		if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgCheckPostponed, r.env.Clock(at), reasonClaudeLimit)); rerr != nil {
			log.Warn("review: reply postponed", "err", rerr)
		}
		return &core.RetryLater{At: at, Reason: rl.Error()}
	}
	var inv *env.InvalidOutputError
	if errors.As(err, &inv) {
		jobctx.BotProblem(ctx, problems.InvalidOutput, "ответ перепроверки замечаний не по схеме", inv.Error())
		if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgCheckFailed, reasonInvalidJSON)); rerr != nil {
			log.Warn("review: reply failed", "err", rerr)
		}
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return r.failCheck(ctx, job, fmt.Errorf("claude run: %w", err))
}

// checkTally is what closing the fixed findings did.
type checkTally struct {
	closed int
	// pending: closed findings declared fixed but left unresolved in GitLab
	// (a Reporter bot without a service token).
	pending int
	// closedBlocking / pendingBlocking: the blocking ones among closed /
	// pending.
	closedBlocking, pendingBlocking int
	left                            []checkLeft
}

// closeFixedFindings closes the findings the check found fixed exactly like
// a review does (publish.MarkFixed, outcome); a missing verdict keeps one open.
func (r *Reviewer) closeFixedFindings(ctx context.Context, job core.Job, eff config.Effective, t *mrthread.Threads, ids []string,
	verdicts map[string]checkVerdict, rc *crossrepo.RunContext) checkTally {
	var tl checkTally
	for _, id := range ids {
		f := t.Open[id]
		v := verdicts[id] // missing -> still open
		if v.Status != checkFixed {
			tl.left = append(tl.left, checkLeft{f: f, why: v.Comment})
			continue
		}
		done, isPending := r.pub.MarkFixed(ctx, job, f.DiscussionID, mdtext.Shorten(rc.Link(v.Comment), 600))
		if !done {
			tl.left = append(tl.left, checkLeft{f: f, why: msgCheckResolveFail})
			continue
		}
		tl.closed++
		if f.Blocking {
			tl.closedBlocking++
		}
		r.learning.OutcomeFixed(ctx, job, eff, f.DiscussionID, id)
		if isPending {
			tl.pending++
			if f.Blocking {
				tl.pendingBlocking++
			}
		}
	}
	return tl
}

// renderCheckLeft lists the findings that stay open: up to maxCheckList,
// the rest collapsed.
func renderCheckLeft(left []checkLeft) string {
	if len(left) == 0 {
		return ""
	}
	item := func(c checkLeft) string {
		loc := strings.ReplaceAll(c.f.Path, "`", "")
		if c.f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", loc, c.f.Line)
		}
		s := "- **" + mdtext.OneLine(strings.ReplaceAll(c.f.Title, "*", "")) + "**"
		if loc != "" {
			s += " · `" + loc + "`"
		}
		if c.why != "" {
			s += " — " + mdtext.Shorten(c.why, 200)
		}
		return s + "\n"
	}
	var b strings.Builder
	b.WriteString("\n\nОстаются открытыми:\n")
	for i, c := range left {
		if i == maxCheckList {
			fmt.Fprintf(&b, "\n<details>\n<summary>Ещё %d</summary>\n\n", len(left)-maxCheckList)
		}
		b.WriteString(item(c))
	}
	if len(left) > maxCheckList {
		b.WriteString("\n</details>\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// updateSummaryGate rewrites the merge-gate line of the existing summary
// note (scanned in t). No summary or no gate line: nothing to do.
func (r *Reviewer) updateSummaryGate(ctx context.Context, job core.Job, t *mrthread.Threads, line string) {
	if t.SummaryID == 0 {
		return
	}
	body, ok := summary.SetGateLine(t.SummaryBody, line)
	if !ok || body == t.SummaryBody {
		return
	}
	if err := r.env.GitLab.UpdateNote(ctx, job.ProjectID, job.MRIID, t.SummaryID, mdtext.NeutralizeQuickActions(body)); err != nil {
		r.env.Log.Warn("review: check: update summary gate", "mr", job.MRKey(), "err", err)
	}
}

// failCheck tells the command author that the check failed, then returns
// err.
func (r *Reviewer) failCheck(ctx context.Context, job core.Job, err error) error {
	if ctx.Err() == nil {
		if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgCheckFailed, failReason(ctx, err))); rerr != nil {
			r.env.Log.Warn("review: reply failure", "err", rerr)
		}
	}
	return err
}

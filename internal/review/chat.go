package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
)

// maxThreadNotes bounds how much of a discussion goes into the chat prompt.
const maxThreadNotes = 30

// handleChat answers a question addressed to the bot (core.JobChat) with one
// Claude run over the MR, the thread and the code, booked against the
// per-user chat limit and the global limit.
func (r *Reviewer) handleChat(ctx context.Context, job core.Job) error {
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
	if !resumed {
		ok, err := r.env.CheckAccess(ctx, job)
		if err != nil || !ok {
			return err
		}
	}
	// Per-user chat limit and the global limit, checked and booked at once
	// (the first Claude run commits the booking, see env.RunStructured).
	rreq := core.ReserveRequest{Usage: env.JobUsage(job), Username: job.Username, Global: true}
	if !resumed {
		ul := eff.UserLimits()
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
		jobctx.Denied(ctx)
		return r.env.Reply(ctx, job, "⛔ "+dec.Reason)
	}

	// Thread context.
	var disc *core.Discussion
	if job.DiscussionID != "" {
		disc, err = r.env.GitLab.GetDiscussion(ctx, job.ProjectID, job.MRIID, job.DiscussionID)
		if err != nil {
			if !errors.Is(err, core.ErrNotFound) {
				return fmt.Errorf("get discussion: %w", err)
			}
			log.Warn("review: chat discussion not found", "discussion", job.DiscussionID)
			disc = nil
		}
	}

	co, err := r.env.Repos.Checkout(ctx, job.ProjectPath, mr)
	if err != nil {
		return r.failChat(ctx, job, fmt.Errorf("checkout: %w", err))
	}
	defer pipeline.CloseCheckout(log, co)

	base := diffcap.FullDiffBase(mr)
	files, err := co.ChangedFiles(ctx, base, eff.Ignore)
	if err != nil {
		log.Warn("review: chat changed files", "err", err)
		files = nil
	}
	if len(files) > 200 {
		files = append(files[:200:200], fmt.Sprintf("… and %d more", len(files)-200))
	}

	// Diff coverage from the CI artifact, when known (pipeline/coverage.go).
	covNote := pipeline.CoverageForClaude(r.pipeline.CollectCoverage(ctx, job, mr, eff))
	// The code before the merge request (/base, env/base.go).
	bs := r.env.PrepareBase(ctx, job, co, mr)
	defer bs.Release()
	lang := prompts.LanguageName(eff.Language)
	data := chatData{
		Title: mr.Title, Description: mr.Description,
		BaseSHA: base, HeadSHA: mr.DiffRefs.HeadSHA, Files: files,
		Question: job.Question, Asker: job.Username, BaseDir: bs.Root, Language: lang,
	}
	if disc != nil && len(disc.Notes) > 0 {
		r.addChatThread(&data, disc, job.NoteID)
	}

	// Context repositories (read-only, never fail the answer).
	rc := r.crossrepo.AcquireContext(ctx, job, eff, mr, true)
	defer rc.Release()
	data.ContextBlock = rc.Block

	sys, err := prompts.Render(chatSystemTmpl, prompts.System{Language: lang, BaseDir: bs.Root})
	if err != nil {
		return fmt.Errorf("render chat system prompt: %w", err)
	}
	// Team rules and learning from disagreement (learning/chat.go).
	// The system prompt and the schema depend on the repository only, not on
	// the thread (prompt caching): the learning rule is offered whenever
	// learning is on and kept only for a bot thread (learnOn).
	rules, learnOn, repoLearn := r.learning.PromptRules(ctx, job, eff), r.learning.ChatLearnOn(eff, disc), r.learning.RulesOn(eff)
	data.RulesBlock = learning.RenderRulesBlock(rules)
	sys += learning.RulesSystemNote(rules) + learning.ChatLearnNote(repoLearn, lang)
	paths := matchPathInstructions(eff.PathInstructions, pathFilesForChat(data.Path, files))
	data.RulesBlock, sys = data.RulesBlock+renderPathBlock(paths), sys+pathSystemNote(eff.PathInstructions)
	// Project memory (learning/memory.go).
	mem, ctx := r.learning.PrepareMemory(ctx, job, eff)
	data.ContextBlock, sys = data.ContextBlock+mem.Block, sys+mem.System
	// Team skills (skills.go).
	sk, ctx := r.prepareSkills(ctx, job, eff)
	defer sk.release()
	sys += sk.system
	data.CoverageBlock = covNote // per-MR facts: in the user prompt, not the system prompt
	prompt, err := prompts.Render(chatUserTmpl, data)
	if err != nil {
		return fmt.Errorf("render chat prompt: %w", err)
	}

	var out *output.ChatOutput
	var rawOut string
	err = r.env.RunStructured(ctx, job, r.env.WithModels(core.RunRequest{
		Dir: co.Dir(), Prompt: prompt, SystemPrompt: sys, BaseDir: bs.Dir,
		JSONSchema: mem.WithSchema(learning.ChatSchemaFor(repoLearn)), Timeout: r.env.Cfg.Claude.Timeout.D(),
		ContextDirs: rc.Dirs, SkillsDir: sk.dir,
	}), func(s string) error {
		o, err := parseChatOutput(s)
		if err == nil {
			o.Reply = rc.Link(o.Reply)
			out, rawOut = o, s
		}
		return err
	})
	if err != nil {
		return r.chatRunFailed(ctx, job, log, err)
	}

	reply := out.Reply + r.learning.LearnFromChat(ctx, job, mr, rawOut, learnOn)
	reply += r.learning.FinishChatMemory(ctx, job, eff, mem)
	if out.Resolve && data.IsFinding && !disc.Resolved {
		// Resolve first so the reply can say whether it worked. A Reporter
		// bot gets 403 here: then it asks a human to close the thread.
		err := r.env.Resolve(ctx, job.ProjectID, job.MRIID, job.DiscussionID)
		switch {
		case errors.Is(err, core.ErrForbidden):
			reply = marker.Fixed + "\n" + reply + "\n\n_Похоже, вопрос закрыт — тред можно закрыть._"
		case err != nil:
			log.Warn("review: chat resolve", "err", err)
		}
		if err == nil || errors.Is(err, core.ErrForbidden) {
			r.learning.OutcomeChatResolved(ctx, job, eff, data.FindingID)
		}
	}
	if err := r.env.Reply(ctx, job, reply); err != nil {
		return err
	}
	log.Info("review: chat answered", "resolve", out.Resolve && data.IsFinding)
	return nil
}

// addChatThread puts the discussion the question was asked in into the
// prompt data: what kind of bot thread it is, where it sits and its notes
// (the first one and the latest, without the question itself).
func (r *Reviewer) addChatThread(data *chatData, disc *core.Discussion, questionID int64) {
	first := disc.Notes[0]
	if id, ok := marker.ParseFinding(first.Body); ok && r.env.IsBotNote(first) {
		data.IsFinding, data.FindingID = true, id
	} else if _, _, ok := marker.ParseCompat(first.Body); ok && r.env.IsBotNote(first) {
		// A breaking-change thread blocks the merge; a chat answer never
		// closes it (a later review or a human does).
		data.IsCompat = true
	}
	data.Path, data.Line = mrthread.NoteLocation(first)
	notes := disc.Notes
	if len(notes) > maxThreadNotes {
		notes = append([]core.Note{notes[0]}, notes[len(notes)-maxThreadNotes+1:]...)
	}
	for _, n := range notes {
		if n.System || n.ID == questionID {
			continue
		}
		author := "@" + n.AuthorUsername
		if r.env.IsBotNote(n) {
			author += " (you, the review bot)"
		}
		data.Notes = append(data.Notes, prompts.ChatNote{Author: author, Text: mdtext.TruncateKeepLines(marker.StripNoteMarkers(n.Body), 4000)})
	}
}

// chatRunFailed ends a chat job whose Claude run failed: a rate limit
// postpones it, otherwise the author is told it failed.
func (r *Reviewer) chatRunFailed(ctx context.Context, job core.Job, log *slog.Logger, err error) error {
	var rl *core.ErrClaudeRateLimited
	var inv *env.InvalidOutputError
	switch {
	case errors.As(err, &rl):
		at := r.env.RetryAt(rl.ResetAt)
		if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgChatPostponed, r.env.Clock(at))); rerr != nil {
			log.Warn("review: reply postponed", "err", rerr)
		}
		return &core.RetryLater{At: at, Reason: rl.Error()}
	case errors.As(err, &inv):
		if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgChatFailed, reasonInvalidJSON)); rerr != nil {
			log.Warn("review: reply failed", "err", rerr)
		}
		return err
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		return r.failChat(ctx, job, fmt.Errorf("claude run: %w", err))
	}
}

// failChat tells the question's author that the answer failed, then returns
// err.
func (r *Reviewer) failChat(ctx context.Context, job core.Job, err error) error {
	if ctx.Err() == nil {
		if rerr := r.env.Reply(ctx, job, fmt.Sprintf(msgChatFailed, failReason(ctx, err))); rerr != nil {
			r.env.Log.Warn("review: reply failure", "err", rerr)
		}
	}
	return err
}

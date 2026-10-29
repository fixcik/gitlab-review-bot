package learning

// Learning from chat answers: in a thread the bot started, the chat answer
// may carry an optional "learning" rule.

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
)

// chatLearning is the optional "learning" field of the chat answer.
type chatLearning struct {
	Rule  string `json:"rule"`
	Scope string `json:"scope"`
}

// ChatLearnOn reports whether the chat run may propose a rule: learning on
// for the repo and the thread was started by the bot. (The asker already
// passed the Developer+ check of the chat flow.)
func (l *Feature) ChatLearnOn(eff config.Effective, disc *core.Discussion) bool {
	if !l.RulesOn(eff) || disc == nil || len(disc.Notes) == 0 {
		return false
	}
	first := disc.Notes[0]
	return !first.System && l.env.IsBotNote(first)
}

// ChatLearnNote is appended to the chat system prompt when learning is on
// for the repository (RulesOn), for every thread: the system prompt and
// the schema stay the same whoever started the thread (prompt caching). The
// rule itself is kept only for a thread the bot started (ChatLearnOn, see
// LearnFromChat).
func ChatLearnNote(on bool, lang string) string {
	if !on {
		return ""
	}
	return "\n- Only when this thread was started by you (the review bot: the first note of the discussion is yours): if the user disagrees with your remark and explains why it is wrong or irrelevant for this code base, or states a durable team convention or constraint, ALSO fill \"learning\" with a short imperative review rule in " + lang +
		" (e.g. \"Не требуй ретраев для вызовов через API-шлюз: ретраи делает шлюз.\") and its scope: \"project\" if it is specific to this repository, \"global\" if it applies to any repository of the team. Omit \"learning\" in threads you did not start, for questions, thanks, one-off decisions or when unsure. Never put secrets, tokens or URLs with credentials into a rule."
}

// ChatSchemaFor returns ChatSchema, extended with the optional "learning"
// object when on (learning on for the repository, see ChatLearnNote).
func ChatSchemaFor(on bool) string {
	if !on {
		return jsonschema.Chat
	}
	out, err := jsonschema.EditProps(jsonschema.Chat, func(props map[string]any) bool {
		props["learning"] = map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"rule", "scope"},
			"description":          "Optional, only in a thread started by you (the review bot): a team review rule learned from the user's explanation in this thread.",
			"properties": map[string]any{
				"rule":  map[string]any{"type": "string", "description": "One short imperative sentence usable as reviewer guidance."},
				"scope": map[string]any{"type": "string", "enum": []string{learn.ScopeProject, learn.ScopeGlobal}},
			},
		}
		return true
	})
	if err != nil {
		return jsonschema.Chat
	}
	return out
}

// LearnFromChat stores the chat answer's "learning" (if any) and returns the
// text to append to the reply ("" when nothing was learned).
func (l *Feature) LearnFromChat(ctx context.Context, job core.Job, mr *core.MergeRequest, raw string, on bool) string {
	if !on || raw == "" {
		return ""
	}
	var wrap struct {
		Learning *chatLearning `json:"learning"`
	}
	if err := jsonschema.UnmarshalStrict(raw, &wrap); err != nil || wrap.Learning == nil {
		return ""
	}
	rule := learn.CleanRule(wrap.Learning.Rule)
	if rule == "" {
		return ""
	}
	saved, created, err := l.addNoteRule(ctx, job, mr, wrap.Learning.Scope, rule)
	if err != nil {
		l.env.Log.Warn("learn: save chat rule", "mr", job.MRKey(), "err", err)
		return ""
	}
	l.env.Log.Info("learn: rule saved from chat", "id", saved.ID, "scope", saved.Scope, "created", created)
	jobctx.Learned(ctx) // 🧠 on the question (review/reactions.go)
	return "\n\n" + l.rememberedMsg(saved, created)
}

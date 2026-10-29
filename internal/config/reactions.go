package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Reactions configures the bot's award emoji (review.reactions, see
// internal/reactions and docs/features/reactions.md). Each event has a
// GitLab emoji name; an empty name turns that event off.
type Reactions struct {
	// Enabled turns every reaction on or off (default true).
	Enabled bool `yaml:"enabled"`
	// PickedUp: a command or question addressed to the bot was accepted
	// (on the note, right away; again when a postponed job resumes).
	PickedUp string `yaml:"picked_up"`
	// Done replaces PickedUp when the job finished; "" only removes it.
	Done string `yaml:"done"`
	// Failed replaces PickedUp when the job failed (error reply).
	Failed string `yaml:"failed"`
	// Postponed replaces PickedUp while the job waits for a limit.
	Postponed string `yaml:"postponed"`
	// Denied replaces PickedUp when access or a per-user limit refused the
	// command (next to the text reply).
	Denied string `yaml:"denied"`
	// Thanks answers a pure "thanks" to the bot or in a bot thread: no
	// Claude run, no text reply.
	Thanks string `yaml:"thanks"`
	// Ack answers a pure acknowledgement in a bot remark thread ("ок,
	// поправлю"): no Claude run.
	Ack string `yaml:"ack"`
	// Learned marks the note a team rule was learned from.
	Learned string `yaml:"learned"`
	// Noted marks a reply in a bot thread that was read and led to nothing
	// (no rule, no question): the reaction that stays after the 👀 goes.
	Noted string `yaml:"noted"`
	// CleanReview goes on the MR itself after a review without findings
	// and with nothing blocking (removed again by a later review that is
	// not clean).
	CleanReview string `yaml:"clean_review"`
	// IgnoreUsers are GitLab usernames (other bots) whose notes never get
	// reactions. GitLab's own bot users (project/group access token bots,
	// support-bot, alert-bot, ghost, …) are always ignored.
	IgnoreUsers []string `yaml:"ignore_users"`
}

func defaultReactions() Reactions {
	return Reactions{
		Enabled: true, PickedUp: "eyes", Done: "leftwards_arrow_with_hook", Failed: "x",
		Postponed: "hourglass_flowing_sand", Denied: "no_entry_sign", Thanks: "heart", Ack: "thumbsup",
		Learned: "brain", Noted: "thumbsup", CleanReview: "tada",
	}
}

// emojiNameRe: GitLab emoji names are lower-case letters, digits, "_",
// "+" and "-" ("thumbsup", "+1", "e-mail", "100").
var emojiNameRe = regexp.MustCompile(`^[a-z0-9_+\-]{1,64}$`)

// ValidEmojiName reports whether s can be a GitLab emoji name.
func ValidEmojiName(s string) bool { return emojiNameRe.MatchString(s) }

// Emoji returns the configured name for event ("picked_up", "done", …;
// the keys of review.reactions). "" when the event or every reaction is
// off, or the event is unknown.
func (r Reactions) Emoji(event string) string {
	if !r.Enabled {
		return ""
	}
	var s string
	switch event {
	case "picked_up":
		s = r.PickedUp
	case "done":
		s = r.Done
	case "failed":
		s = r.Failed
	case "postponed":
		s = r.Postponed
	case "denied":
		s = r.Denied
	case "thanks":
		s = r.Thanks
	case "ack":
		s = r.Ack
	case "learned":
		s = r.Learned
	case "noted":
		s = r.Noted
	case "clean_review":
		s = r.CleanReview
	}
	return strings.TrimSpace(s)
}

// gitlabBotUserRe: users GitLab creates itself (project and group access
// tokens: project_123_bot, project_123_bot_<hex>; group_7_bot…).
var gitlabBotUserRe = regexp.MustCompile(`^(project|group)_\d+_bot(_[0-9a-f]+)?\d*$`)

// gitlabInternalBots are GitLab's internal bot accounts.
var gitlabInternalBots = map[string]bool{
	"ghost": true, "support-bot": true, "alert-bot": true, "visual-review-bot": true, "security-bot": true,
	"automation-bot": true, "security-policy-bot": true, "admin-bot": true, "suggested-reviewers-bot": true,
	"gitlab-duo": true, "duo-developer": true, "gitlab-llm-bot": true,
}

// Ignored reports whether notes of username never get reactions.
func (r Reactions) Ignored(username string) bool {
	u := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
	if u == "" {
		return false
	}
	if gitlabInternalBots[u] || gitlabBotUserRe.MatchString(u) {
		return true
	}
	for _, x := range r.IgnoreUsers {
		if strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(x), "@"), u) {
			return true
		}
	}
	return false
}

func (r Reactions) validate() []string {
	var errs []string
	for _, e := range []struct{ key, v string }{
		{"picked_up", r.PickedUp}, {"done", r.Done}, {"failed", r.Failed}, {"postponed", r.Postponed},
		{"denied", r.Denied}, {"thanks", r.Thanks}, {"ack", r.Ack}, {"learned", r.Learned}, {"noted", r.Noted}, {"clean_review", r.CleanReview},
	} {
		if v := strings.TrimSpace(e.v); v != "" && !ValidEmojiName(v) {
			errs = append(errs, fmt.Sprintf("review.reactions.%s: %q is not a GitLab emoji name (lower-case letters, digits, _, + and -, e.g. thumbsup)", e.key, e.v))
		}
	}
	for _, u := range r.IgnoreUsers {
		u = strings.TrimPrefix(strings.TrimSpace(u), "@")
		if !validGitLabUsername(u) {
			errs = append(errs, fmt.Sprintf("review.reactions.ignore_users: %q is not a GitLab username", u))
		}
	}
	return errs
}

// RepoReactions is the reactions section of .reviewbot.yaml: a repository
// may only turn reactions off (enabled: false).
type RepoReactions struct {
	Enabled *bool `yaml:"enabled"`
}

// reactionsEnabled reports the repo's own switch (true when not set). Safe
// on a nil *Repo.
func (r *Repo) reactionsEnabled() bool {
	return r == nil || r.Reactions == nil || defaultTrue(r.Reactions.Enabled)
}

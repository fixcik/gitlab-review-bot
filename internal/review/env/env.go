// Package env is what a review feature gets from the orchestrator: the
// narrow set of collaborators and job helpers every feature shares (the
// GitLab client with the job's memo, store, repositories, limits, service
// config, logger and clock, the bot's identity, the structured Claude run,
// replies and access checks, repository config access, the merge base
// export). Feature-specific state lives in the feature, not here.
//
// It is built once by review.New. It must not depend on internal/review or
// any of its feature packages.
package env

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
)

// DiffIndex is what the reviewer needs from internal/diffmap.Map.
type DiffIndex interface {
	Anchor(refs core.DiffRefs, path string, newLine int) (*core.Position, bool)
	Snap(path string, newLine, maxDist int) (int, bool)
	HasFile(path string) bool
}

// EmptyIndex anchors nothing (no diff index).
type EmptyIndex struct{}

// Anchor finds nothing.
func (EmptyIndex) Anchor(core.DiffRefs, string, int) (*core.Position, bool) { return nil, false }

// Snap finds nothing.
func (EmptyIndex) Snap(string, int, int) (int, bool) { return 0, false }

// HasFile is always false.
func (EmptyIndex) HasFile(string) bool { return false }

// Moderator is the service-token client (Maintainer, writes restricted to
// reviewers and thread resolution). The bot's own token is a Reporter and
// cannot do either.
type Moderator interface {
	ResolveDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string, resolved bool) error
	AddReviewer(ctx context.Context, projectID, mrIID, userID int64) (bool, error)
}

// RepoConfig reads the effective configuration of an MR's repository
// (.reviewbot.yaml of the target branch merged with the service config;
// cached, see review/repoconfig.go).
type RepoConfig interface {
	// Effective loads and merges the config. A repository config that
	// refuses the run (invalid, not opted in) is an error the orchestrator
	// recognizes.
	Effective(ctx context.Context, projectID int64, mr *core.MergeRequest) (config.Effective, error)
	// Command is Effective for a command: a repository config that refuses
	// it is answered and ok is false; ok is false on other errors too (err
	// set).
	Command(ctx context.Context, job core.Job, mr *core.MergeRequest) (eff config.Effective, ok bool, err error)
	// Auto is Effective for an automatic run (nothing to answer): a
	// repository config that refuses it skips the run silently (ok false,
	// err nil); ok is false on other errors too (err set).
	Auto(ctx context.Context, projectID int64, mr *core.MergeRequest) (eff config.Effective, ok bool, err error)
	// NotOptedIn reports whether err (from Effective) means the repository
	// is not opted in (no .reviewbot.yaml while it is required).
	NotOptedIn(err error) bool
}

// DefaultBackoff is used when a rate limit gives no reset time.
const DefaultBackoff = 30 * time.Minute

// MaxRetryWait caps how far ahead a postponed job is scheduled (RetryAt):
// a reset time further away (a bogus one, e.g. a far-future time in text
// the model was tricked into printing) would otherwise park the job until
// then. The job tries again after a day instead; a real longer limit (a
// weekly one) just postpones it again, a day at a time.
const MaxRetryWait = 24 * time.Hour

// Env is the features' environment. The fields are set once (review.New;
// tests may replace them before the first job).
type Env struct {
	// GitLab is the bot's client, wrapped in the per-job memo of MR reads.
	GitLab core.GitLab
	// Mod is the service-token client; nil means no reviewer assignment
	// and thread resolution through GitLab (403 for a Reporter bot).
	Mod     Moderator
	Repos   core.Repos
	Runner  core.Runner
	Store   core.Store
	Limiter core.Limiter
	Cfg     *config.Service
	// Index builds the line index of an MR diff (diffmap.Build).
	Index func([]core.FileDiff) DiffIndex
	Log   *slog.Logger
	Now   func() time.Time
	// BotID / BotUsername: the account behind GITLAB_TOKEN (without "@").
	BotID       int64
	BotUsername string
	// RepoConfig: the repository config of an MR.
	RepoConfig RepoConfig
}

// Mention is how people address the bot: always "@" + the username of the
// account behind GITLAB_TOKEN (the account that writes the bot's notes).
func (e *Env) Mention() string {
	return "@" + e.BotUsername
}

// IsBotNote reports whether n was written by the bot's account.
func (e *Env) IsBotNote(n core.Note) bool {
	if e.BotID != 0 && n.AuthorID != 0 {
		return n.AuthorID == e.BotID
	}
	return e.BotUsername != "" && strings.EqualFold(n.AuthorUsername, e.BotUsername)
}

// Resolve resolves a thread with the service token when available, else
// with the bot's own token (which fails with core.ErrForbidden for a
// Reporter).
func (e *Env) Resolve(ctx context.Context, projectID, mrIID int64, discussionID string) error {
	if e.Mod != nil {
		return e.Mod.ResolveDiscussion(ctx, projectID, mrIID, discussionID, true)
	}
	return e.GitLab.ResolveDiscussion(ctx, projectID, mrIID, discussionID, true)
}

// Blocking reports whether a finding of severity s must block the merge.
func (e *Env) Blocking(s core.Severity) bool {
	return finding.Rank(s) <= finding.Rank(core.Severity(strings.ToLower(strings.TrimSpace(e.Cfg.Review.BlockMinSeverity))))
}

// Clock formats a time for users: "15:04", or "02.01 15:04" if not today.
func (e *Env) Clock(t time.Time) string {
	now := e.Now()
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	if y1 != y2 || m1 != m2 || d1 != d2 {
		return t.Format("02.01 15:04")
	}
	return t.Format("15:04")
}

// DockerSandbox: Claude runs in a container and sees the mount paths
// (/repos, /base) rather than the host paths.
func (e *Env) DockerSandbox() bool { return e.Cfg.Sandbox.Mode != "local" }

// RetryAt is when a postponed job runs again: at, or DefaultBackoff from
// now when at is unknown or past; never later than MaxRetryWait from now.
func (e *Env) RetryAt(at time.Time) time.Time {
	now := e.Now()
	if at.IsZero() || !at.After(now) {
		return now.Add(DefaultBackoff)
	}
	if limit := now.Add(MaxRetryWait); at.After(limit) {
		return limit
	}
	return at
}

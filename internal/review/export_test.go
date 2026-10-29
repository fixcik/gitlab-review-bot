package review

import (
	"context"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Hooks for the orchestrator's external tests (package review_test, which
// build the reviewer through review/reviewtest): unexported methods and
// state they check.

func (r *Reviewer) HasIgnoreMarker(text string) bool { return r.hasIgnoreMarker(text) }

func (r *Reviewer) EnsureReviewer(ctx context.Context, job core.Job) { r.ensureReviewer(ctx, job) }

// HandleReview is handleReview without Handle's per-job context (no GitLab
// memo, no reactions).
func (r *Reviewer) HandleReview(ctx context.Context, job core.Job) error {
	return r.handleReview(ctx, job)
}

// RepoConfigRead is a read of .reviewbot.yaml (cfgFile) or a cache entry
// (cfgCacheEntry) as the tests see it.
type RepoConfigRead struct {
	Found, TooBig, Valid bool
	Raw                  []byte
	Issues               config.RepoErrors
}

// ReadRepoConfig is readRepoConfig.
func (r *Reviewer) ReadRepoConfig(ctx context.Context, projectID int64, ref string) (RepoConfigRead, error) {
	f, err := r.readRepoConfig(ctx, projectID, ref)
	return RepoConfigRead{Found: f.found, TooBig: f.tooBig, Valid: f.valid(), Raw: f.raw, Issues: f.issues}, err
}

// CfgCacheEntry is the config cache's entry of (projectID, ref) and the
// cache's byte count.
func (r *Reviewer) CfgCacheEntry(projectID int64, ref string) (RepoConfigRead, int) {
	r.cfgCache.mu.Lock()
	defer r.cfgCache.mu.Unlock()
	e := r.cfgCache.m[cfgCacheKey{projectID, ref}]
	return RepoConfigRead{Found: e.found, TooBig: e.tooBig, Raw: e.raw}, r.cfgCache.bytes
}

// Unexported names used by the orchestrator's external tests.

const (
	CheckSchema          = checkSchema
	MaxThreadNotes       = maxThreadNotes
	MemoDiscussions      = memoDiscussions
	MemoGetMR            = memoGetMR
	MemoGetMRDiffs       = memoGetMRDiffs
	MsgCheckFailed       = msgCheckFailed
	MsgCheckNone         = msgCheckNone
	MsgCheckResolveFail  = msgCheckResolveFail
	MsgCheckThreadClosed = msgCheckThreadClosed
	MsgDisabled          = msgDisabled
	MsgNotOpened         = msgNotOpened
	MsgResolveNone       = msgResolveNone
	MsgResumed           = msgResumed
	ProblemsMaxLines     = problemsMaxLines
	ReasonClaudeLimit    = reasonClaudeLimit
	ReasonInternal       = reasonInternal
	ReasonInvalidJSON    = reasonInvalidJSON
	RouteLight           = routeLight
	RouteHeavy           = routeHeavy
	RouteNormal          = routeNormal
	SkipTooBig           = skipTooBig
	SkipTrivial          = skipTrivial
)

type (
	CheckOutput  = checkOutput
	CheckVerdict = checkVerdict
)

var (
	UnpublishedLine    = unpublishedLine
	DiffSize           = diffSize
	NewMemoGitLab      = newMemoGitLab
	NewMemoModerator   = newMemoModerator
	ParseReviewOutput  = parseReviewOutput
	WithGitLabMemo     = withGitLabMemo
	WithProblemsSchema = withProblemsSchema
)

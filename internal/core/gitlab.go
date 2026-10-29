package core

import (
	"context"
	"errors"
	"time"
)

// GitLab access levels.
const (
	AccessGuest      = 10
	AccessReporter   = 20
	AccessDeveloper  = 30
	AccessMaintainer = 40
	AccessOwner      = 50
)

// ErrNotFound is returned by GitLab methods for 404s.
var ErrNotFound = errors.New("not found")

// ErrForbidden is returned by GitLab methods for 403s (e.g. a Reporter bot
// trying to resolve a thread) and for writes the client refuses to send.
var ErrForbidden = errors.New("forbidden")

// ErrTooLarge is returned by capped reads (raw files with a cap, job
// artifacts and reports, traces) when the body exceeds the cap.
var ErrTooLarge = errors.New("gitlab: response too large")

// ErrReportNeedsArchive: the GitLab server returned the artifacts archive
// instead of the requested report artifact (it does not support file_type on
// GET /jobs/:id/artifacts, GitLab < 19.4) and the report file is not in the
// archive either.
var ErrReportNeedsArchive = errors.New("gitlab: this GitLab cannot download a report artifact by type, and the report is not in the artifacts archive")

type DiffRefs struct {
	BaseSHA  string
	StartSHA string
	HeadSHA  string
}

type MergeRequest struct {
	ProjectID      int64
	IID            int64
	Title          string
	Description    string
	State          string // opened, closed, merged, locked
	Draft          bool
	SourceBranch   string
	TargetBranch   string
	AuthorUsername string
	WebURL         string
	DiffRefs       DiffRefs
	CreatedAt      time.Time
	// DivergedCommits is how many commits the target branch has that the
	// source branch lacks (GitLab diverged_commits_count); -1 = unknown.
	DivergedCommits int
	// Pipeline is the MR's head pipeline; nil when there is none.
	Pipeline *Pipeline
}

// CIJob is one CI job of a pipeline.
type CIJob struct {
	ID     int64
	Name   string
	Status string // success, failed, running, …
	WebURL string
	// Stage, FailureReason (script_failure, runner_system_failure, …) and
	// AllowFailure as GitLab reports them (CI failure digest).
	Stage         string
	FailureReason string
	AllowFailure  bool
}

// Pipeline is the head pipeline of an MR.
type Pipeline struct {
	ID     int64
	Status string // created, pending, running, success, failed, canceled, skipped, manual, scheduled, …
	SHA    string
	WebURL string
}

// FileDiff is one entry of the MR diff (GitLab /diffs endpoint).
type FileDiff struct {
	OldPath     string
	NewPath     string
	NewFile     bool
	RenamedFile bool
	DeletedFile bool
	Diff        string // unified diff hunks, without "diff --git" header
}

// Position anchors a note to a diff line (GitLab "text" position).
// Set NewLine for added/context lines, OldLine for removed lines.
type Position struct {
	BaseSHA  string
	StartSHA string
	HeadSHA  string
	OldPath  string
	NewPath  string
	OldLine  int
	NewLine  int
}

type Note struct {
	ID             int64
	AuthorID       int64
	AuthorUsername string
	Body           string
	System         bool
	Position       *Position
	CreatedAt      time.Time
	// ResolvedByID / ResolvedByUsername: who resolved the discussion
	// (GitLab `resolved_by` of a resolvable note); zero when not resolved or
	// not reported.
	ResolvedByID       int64
	ResolvedByUsername string
}

// Commit is one commit of an MR (GitLab /merge_requests/:iid/commits).
type Commit struct {
	SHA        string
	Title      string
	Message    string
	AuthoredAt time.Time
}

// Award is an award emoji (a reaction) on an MR or on one of its notes.
type Award struct {
	ID       int64
	Name     string // GitLab emoji name, e.g. "thumbsup"
	UserID   int64
	Username string
}

type Discussion struct {
	ID         string
	Resolvable bool
	Resolved   bool
	// Individual: a plain, non-threaded comment (GitLab individual_note).
	Individual bool
	Notes      []Note
}

// GitLab is the subset of the GitLab REST API (v4) the bot uses.
type GitLab interface {
	// CurrentUser returns the bot's own user.
	CurrentUser(ctx context.Context) (id int64, username string, err error)
	GetMR(ctx context.Context, projectID, mrIID int64) (*MergeRequest, error)
	// GetMRDiffs returns all file diffs of the MR (all pages).
	GetMRDiffs(ctx context.Context, projectID, mrIID int64) ([]FileDiff, error)
	// ListDiscussions returns all discussions of the MR (all pages).
	ListDiscussions(ctx context.Context, projectID, mrIID int64) ([]Discussion, error)
	GetDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string) (*Discussion, error)

	// CreateDraftNote adds an unpublished review note; pos may be nil.
	CreateDraftNote(ctx context.Context, projectID, mrIID int64, body string, pos *Position) (int64, error)
	// PublishDraftNotes publishes all of the bot's draft notes on the MR.
	PublishDraftNotes(ctx context.Context, projectID, mrIID int64) error
	// DeleteDraftNotes removes the bot's leftover drafts (from a crashed run).
	DeleteDraftNotes(ctx context.Context, projectID, mrIID int64) error

	CreateNote(ctx context.Context, projectID, mrIID int64, body string) (int64, error)
	UpdateNote(ctx context.Context, projectID, mrIID, noteID int64, body string) error
	ReplyToDiscussion(ctx context.Context, projectID, mrIID int64, discussionID, body string) error
	ResolveDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string, resolved bool) error

	// AccessLevel returns the user's effective access level in the project,
	// including inherited group membership. 0 if not a member.
	AccessLevel(ctx context.Context, projectID, userID int64) (int, error)
	// GetRawFile returns a file at ref; ErrNotFound if it doesn't exist.
	GetRawFile(ctx context.Context, projectID int64, path, ref string) ([]byte, error)
	// ListMRCommits returns all commits of the MR (all pages), newest first.
	ListMRCommits(ctx context.Context, projectID, mrIID int64) ([]Commit, error)
}

// GitLabAs finds an optional capability T (an interface the real client
// implements beyond core.GitLab) on gl or on any client it wraps (via an
// `Unwrap() GitLab` method, as decorators provide). A plain type assertion
// on a decorated client would silently report the capability as missing.
func GitLabAs[T any](gl GitLab) (T, bool) {
	for i := 0; gl != nil && i < 8; i++ {
		if t, ok := gl.(T); ok {
			return t, true
		}
		u, ok := gl.(interface{ Unwrap() GitLab })
		if !ok {
			break
		}
		gl = u.Unwrap()
	}
	var zero T
	return zero, false
}

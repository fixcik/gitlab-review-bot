package events

import (
	"context"
	"time"
)

// PollMR is the last state of an MR the poller (internal/poller) has seen.
type PollMR struct {
	ProjectID int64
	IID       int64
	State     string
	Draft     bool
	SHA       string
	Title     string
	// UpdatedAt is the MR's updated_at when it was last processed; an MR
	// returned again with the same value has nothing new.
	UpdatedAt time.Time
	// LastNoteID is the highest note ID seen on the MR.
	LastNoteID int64
	// NotesSince: notes created before this time are history (they existed
	// before the poller started tracking the MR) and never trigger jobs.
	NotesSince time.Time
}

// PollState persists the poller's cursors and MR records (*store.SQLite).
type PollState interface {
	GetPollCursor(ctx context.Context, scope string) (time.Time, bool, error)
	SetPollCursor(ctx context.Context, scope string, t time.Time) error
	GetPollMR(ctx context.Context, projectID, mrIID int64) (*PollMR, error) // nil when unknown
	SavePollMR(ctx context.Context, r PollMR) error
}

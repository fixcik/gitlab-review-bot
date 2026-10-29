package core

import (
	"context"
	"time"
)

type MRState struct {
	ProjectID     int64
	MRIID         int64
	ReviewedSHA   string // head SHA of the last successful review
	SummaryNoteID int64
	UpdatedAt     time.Time
}

// CIMain is the CI state of one main branch (the CI failure digest,
// internal/review/pipeline; stored by *store.SQLite).
type CIMain struct {
	ProjectID int64
	Branch    string
	// GreenPipeline / GreenSHA: the last pipeline that passed.
	GreenPipeline int64
	GreenSHA      string
	// RedPipeline / RedSHA: the first failed pipeline of the open incident
	// (0 = main is green).
	RedPipeline int64
	RedSHA      string
	// CulpritMR / NoteID: the merged MR that got the note and the note (0 =
	// none: a direct push, or the note could not be written).
	CulpritMR int64
	NoteID    int64
	// LastPipeline: the newest pipeline handled (older events are stale).
	LastPipeline int64
	UpdatedAt    time.Time
}

// UsageKind classifies Claude runs for rate limiting.
type UsageKind string

const (
	UsageAuto  UsageKind = "auto"  // automatic reviews
	UsageRerun UsageKind = "rerun" // @bot review / full review / check
	UsageChat  UsageKind = "chat"  // @bot <question>
	UsageLearn UsageKind = "learn" // rule extraction from thread replies
)

type Usage struct {
	UserID int64 // 0 for auto
	Kind   UsageKind
	MRKey  string
	At     time.Time
}

type UsageFilter struct {
	UserID *int64
	Kind   *UsageKind
	MRKey  *string
	Since  time.Time
}

type Store interface {
	GetMRState(ctx context.Context, projectID, mrIID int64) (*MRState, error) // nil, nil if none
	SaveMRState(ctx context.Context, s MRState) error
	RecordUsage(ctx context.Context, u Usage) error
	// ListUsage returns matching records, oldest first.
	ListUsage(ctx context.Context, f UsageFilter) ([]Usage, error)
	PruneUsage(ctx context.Context, before time.Time) error
	Close() error
}

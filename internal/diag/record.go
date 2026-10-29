package diag

import (
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// RecordVersion is the run.json format version.
const RecordVersion = 1

// Record is the content of run.json.
type Record struct {
	Version    int            `json:"version"`
	ID         string         `json:"id"`
	Job        JobInfo        `json:"job"`
	StartedAt  time.Time      `json:"started_at"`
	FinishedAt time.Time      `json:"finished_at,omitzero"`
	Status     string         `json:"status"`
	Reason     string         `json:"reason,omitempty"`
	Error      string         `json:"error,omitempty"`
	ErrorClass string         `json:"error_class,omitempty"`
	RetryAt    time.Time      `json:"retry_at,omitzero"`
	Attempt    int            `json:"attempt"`
	DurationMS int64          `json:"duration_ms"`
	Steps      []TimelineStep `json:"steps"`
	Claude     []ClaudeRun    `json:"claude"`
	// Truncated: long strings were shortened to fit diag.max_run_mb.
	Truncated bool `json:"truncated,omitempty"`
	// TraceID is the job's trace (tracing on and the trace sampled).
	TraceID string `json:"trace_id,omitempty"`
	// Route is the model route of a review (review.routing; SetRoute).
	Route *RouteInfo `json:"route,omitempty"`
}

// RouteInfo is the model route of a review: light (low risk, the light
// model without the advisor) or normal, the model and why.
type RouteInfo struct {
	Route  string `json:"route"`
	Model  string `json:"model,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// JobInfo describes the queue job.
type JobInfo struct {
	Kind         string    `json:"kind"`
	ProjectID    int64     `json:"project_id"`
	ProjectPath  string    `json:"project_path"`
	MRIID        int64     `json:"mr_iid"`
	MRTitle      string    `json:"mr_title,omitempty"`
	MRURL        string    `json:"mr_url,omitempty"`
	MRAuthor     string    `json:"mr_author,omitempty"`
	HeadSHA      string    `json:"head_sha,omitempty"`
	Username     string    `json:"username,omitempty"`
	UserID       int64     `json:"user_id,omitempty"`
	DiscussionID string    `json:"discussion_id,omitempty"`
	NoteID       int64     `json:"note_id,omitempty"`
	Question     string    `json:"question,omitempty"`
	ForceFull    bool      `json:"force_full,omitempty"`
	EnqueuedAt   time.Time `json:"enqueued_at,omitzero"`
}

// TimelineStep is one entry of the timeline.
type TimelineStep struct {
	N          int       `json:"n"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Start      time.Time `json:"start"`
	DurationMS int64     `json:"duration_ms"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail,omitempty"`
	Fields     Fields    `json:"fields,omitempty"`
	Stdout     string    `json:"stdout,omitempty"`
	Stderr     string    `json:"stderr,omitempty"`
	Error      string    `json:"error,omitempty"`
	// Parent is the N of the enclosing step, 0 = top level.
	Parent int `json:"parent,omitempty"`
	// Claude is the 1-based index of the Claude run of a claude step.
	Claude int `json:"claude,omitempty"`
}

// Tokens is a token count with JSON names.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Total is the sum of all kinds.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheRead + t.CacheWrite }

func tokensOf(u core.TokenUsage) Tokens {
	return Tokens{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite}
}

// ModelTokens is one model's share.
type ModelTokens struct {
	Tokens
	CostUSD float64 `json:"cost_usd"`
}

// ContextDirInfo is a context repository given to a run.
type ContextDirInfo struct {
	Name   string `json:"name"`
	SHA    string `json:"sha,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// ClaudeRun is one Claude session.
type ClaudeRun struct {
	N            int              `json:"n"`
	Step         int              `json:"step"`
	Purpose      string           `json:"purpose,omitempty"` // meter kind: review, chat, verification, learning
	Start        time.Time        `json:"start"`
	DurationMS   int64            `json:"duration_ms"`
	Model        string           `json:"model,omitempty"`
	Dir          string           `json:"dir,omitempty"`
	Timeout      string           `json:"timeout,omitempty"`
	NoTools      bool             `json:"no_tools,omitempty"`
	Argv         []string         `json:"argv,omitempty"`
	SystemPrompt string           `json:"system_prompt,omitempty"`
	Prompt       string           `json:"prompt"`
	JSONSchema   string           `json:"json_schema,omitempty"`
	AllowedTools []string         `json:"allowed_tools,omitempty"`
	Disallowed   []string         `json:"disallowed_tools,omitempty"`
	ContextDirs  []ContextDirInfo `json:"context_dirs,omitempty"`
	// MCPURL / MCPTools: the bot's MCP server of the run (never the token).
	MCPURL   string   `json:"mcp_url,omitempty"`
	MCPTools []string `json:"mcp_tools,omitempty"`

	Status     string                 `json:"status"` // running | ok | error
	Output     string                 `json:"output,omitempty"`
	Usage      Tokens                 `json:"usage"`
	Models     map[string]ModelTokens `json:"models,omitempty"`
	CostUSD    float64                `json:"cost_usd"`
	NumTurns   int                    `json:"num_turns"`
	CLIMS      int64                  `json:"cli_duration_ms,omitempty"` // duration_ms reported by the CLI
	ExitCode   *int                   `json:"exit_code,omitempty"`
	Stderr     string                 `json:"stderr,omitempty"`
	Error      string                 `json:"error,omitempty"`
	ErrorClass string                 `json:"error_class,omitempty"`

	Transcript          string `json:"transcript,omitempty"` // file name in the run dir
	Events              int    `json:"events"`
	TranscriptBytes     int64  `json:"transcript_bytes"`
	TranscriptTruncated bool   `json:"transcript_truncated,omitempty"`
}

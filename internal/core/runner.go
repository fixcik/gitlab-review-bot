package core

import (
	"context"
	"fmt"
	"time"
)

// ErrClaudeRateLimited means the Claude subscription/API usage limit was hit.
type ErrClaudeRateLimited struct {
	ResetAt time.Time // zero if unknown
	Message string
}

func (e *ErrClaudeRateLimited) Error() string { return "claude usage limit: " + e.Message }

// ErrClaudeAuth means Claude rejected the bot's credentials (an invalid,
// expired or revoked token): retrying cannot help until the token is replaced.
type ErrClaudeAuth struct{ Message string }

func (e *ErrClaudeAuth) Error() string { return "claude authentication failed: " + e.Message }

// RunRequest runs one headless Claude session in Dir.
//
// The model never runs commands: a run with tools gets exactly the
// read-only built-in tools Read, Grep and Glob (plus the code-search
// subagent and the bot's MCP tools when asked for), a NoTools run gets
// none. There is no Bash and no repository-defined command (see
// docs/security.md).
type RunRequest struct {
	Dir          string
	Prompt       string
	SystemPrompt string // appended to Claude Code's system prompt
	// JSONSchema, when set, is passed via --json-schema and the structured
	// output is returned in RunResult.Output.
	JSONSchema string
	Model      string
	Timeout    time.Duration

	// ContextDirs are read-only snapshots of other repositories Claude may
	// read (Read/Grep/Glob) during the run. Docker mode mounts each one
	// read-only at ContextDir.MountPath(true); local mode uses the host
	// paths. Both modes pass them via --add-dir.
	ContextDirs []ContextDir

	// SkillsDir is a staged, read-only copy of the team skills (see
	// internal/skills). Docker mode mounts it read-only at SkillsRoot; local
	// mode uses the host path. Both modes pass it via --add-dir. Empty = no
	// skills.
	SkillsDir string

	// BaseDir is a read-only export (no .git) of the merge base of the
	// merge request: the code before it, for comparisons Claude used to make
	// with `git show <merge base>:<path>` (see TreeExporter). Docker mode
	// mounts it read-only at BaseRoot; local mode uses the host path. Both
	// modes pass it via --add-dir. Empty = not available.
	BaseDir string

	// Advisor enables the server-side advisor tool with this model (claude
	// --advisor). Ignored with NoTools.
	Advisor string
	// SearchModel enables the read-only "code-search" subagent (Read, Grep,
	// Glob) on this model. Ignored with NoTools.
	SearchModel string
	// DeepReviewModel enables the read-only "deep-review" subagent (Read,
	// Grep, Glob) on this model: a second opinion the review run asks once
	// on a non-trivial MR. Ignored with NoTools.
	DeepReviewModel string

	// NoTools: a text-only run (e.g. learning extraction). The runner
	// exposes the smallest built-in tool set it can (Read only, no Bash).
	NoTools bool

	// MCP connects the run to the bot's MCP server (review tools); nil =
	// no MCP server. Ignored with NoTools.
	MCP *MCPServer
}

// MCPServer is the bot's MCP endpoint for one run. Token is a per-run
// secret: runners hand it to the CLI only inside the MCP config and never
// log, record or put it on a command line.
type MCPServer struct {
	// Name is the server name in the MCP config; Claude Code names the
	// tools mcp__<Name>__<tool>.
	Name string
	// URL is the endpoint as the CLI sees it (http://egress:8091/mcp).
	URL   string
	Token string
	// Tools are the tool names the run may call (allowed explicitly).
	Tools []string
}

// AllowedTools are the --allowedTools entries for the MCP tools.
func (m *MCPServer) AllowedTools() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.Tools))
	for _, t := range m.Tools {
		out = append(out, "mcp__"+m.Name+"__"+t)
	}
	return out
}

// String never prints the token.
func (m *MCPServer) String() string {
	if m == nil {
		return "<nil>"
	}
	return fmt.Sprintf("mcp %s at %s (%d tools)", m.Name, m.URL, len(m.Tools))
}

// GoString keeps %#v from printing the token.
func (m *MCPServer) GoString() string { return m.String() }

// SkillsRoot is where RunRequest.SkillsDir is mounted inside the sandbox
// container.
const SkillsRoot = "/skills"

// SkillsMountPath is the path Claude sees for a staged skills dir.
func SkillsMountPath(hostDir string, docker bool) string {
	if docker {
		return SkillsRoot
	}
	return hostDir
}

// BaseRoot is where RunRequest.BaseDir is mounted inside the sandbox
// container: /base/<path> is <path> at the merge base.
const BaseRoot = "/base"

// BaseMountPath is the path Claude sees for an exported merge base.
func BaseMountPath(hostDir string, docker bool) string {
	if docker {
		return BaseRoot
	}
	return hostDir
}

// ContextRequest asks for the context repositories of one run.
type ContextRequest struct {
	// Exclude filters project paths of gitlab.groups (the reviewed project,
	// the repo's context.exclude). Related entries are not filtered.
	Exclude func(projectPath string) bool
	// Related are project paths that must come first, beyond max_repos,
	// and may lie outside gitlab.groups (context.related).
	Related []string
}

// ContextMissing is a related repository that could not be provided.
type ContextMissing struct {
	Path   string
	Reason string // human-readable, Russian
}

// ContextReposRoot is where context repositories are mounted inside the
// sandbox container: /repos/<group>/<project>.
const ContextReposRoot = "/repos"

// ContextDir is a read-only working tree (no .git) of another repository at
// the tip of its default branch.
type ContextDir struct {
	HostPath string // directory on the bot's host
	Name     string // "group/sub/project" (GitLab path_with_namespace)
	SHA      string // commit the tree was exported from
	Branch   string // default branch it was taken from
}

// MountPath is the path at which Claude sees the snapshot: under
// ContextReposRoot in docker mode, the host path in local mode.
func (d ContextDir) MountPath(docker bool) string {
	if docker {
		return ContextReposRoot + "/" + d.Name
	}
	return d.HostPath
}

type RunResult struct {
	// Output is the structured JSON output (when JSONSchema was set) or the
	// final text answer.
	Output   string
	CostUSD  float64
	Duration time.Duration
	NumTurns int
	// Usage is the run's token usage: the sum of Models when the CLI
	// reported a per-model breakdown (subagents included), else its
	// top-level `usage`. Zero when the CLI reported neither.
	Usage TokenUsage
	// Models is the per-model breakdown (`modelUsage`); nil if absent.
	Models map[string]ModelUsage
	// ReadFiles are the file paths Claude opened with Read during the run
	// (as Claude saw them, deduplicated, in order; capped). Used to report
	// which team skills were consulted.
	ReadFiles []string
	// AdvisorDropped: the request asked for the advisor (RunRequest.Advisor)
	// but the run went without it (the account or model does not support
	// it). Reported as a verification problem.
	AdvisorDropped bool
	// DeepReviewError: the run asked the deep-review subagent and that call
	// failed (the run finished without the second opinion); the error text
	// of the first failed call, capped. "" = no failure.
	DeepReviewError string
	// MCPDropped: the request carried an MCP server (RunRequest.MCP) but the
	// run went without the tools (their config could not be written).
	MCPDropped bool
	// MCPStatus is the connection status the CLI reported for the
	// request's MCP server in its stream-json init event ("connected",
	// "failed", "needs-auth", "pending", ...; "" = not reported or no MCP).
	MCPStatus string
}

// ExecRequest runs one program (no shell) in the sandbox image WITHOUT
// network and WITHOUT any credentials. Used for the bot's deterministic
// checkers (gitleaks, oasdiff, oas2to3, buf, graphql-inspector); never for
// repository-defined commands.
type ExecRequest struct {
	// Dir is mounted as the working directory (/work in docker mode).
	Dir string
	// Command is the argv; Command[0] is looked up in PATH.
	Command []string
	// Env are extra NAME=value pairs. Credential names are refused.
	Env     []string
	Timeout time.Duration
}

// ExecResult is what a finished Exec left behind. A program that is not
// installed yields ExitCode 127 (not an error).
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// TokenUsage counts the tokens of one or more Claude runs.
type TokenUsage struct {
	Input      int64 // input_tokens (not cached)
	Output     int64 // output_tokens
	CacheRead  int64 // cache_read_input_tokens
	CacheWrite int64 // cache_creation_input_tokens
}

// Total is the sum of all token kinds.
func (u TokenUsage) Total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// Add returns the field-wise sum of u and o.
func (u TokenUsage) Add(o TokenUsage) TokenUsage {
	return TokenUsage{Input: u.Input + o.Input, Output: u.Output + o.Output,
		CacheRead: u.CacheRead + o.CacheRead, CacheWrite: u.CacheWrite + o.CacheWrite}
}

// ModelUsage is one model's share of a run.
type ModelUsage struct {
	TokenUsage
	CostUSD float64 // client-side estimate at API prices
}

// FailedRun wraps a Runner.Run error when the CLI still reported what the
// failed run consumed (e.g. max turns reached). errors.As/Is see Err.
type FailedRun struct {
	Err     error
	Partial RunResult // Output is empty
}

func (e *FailedRun) Error() string { return e.Err.Error() }
func (e *FailedRun) Unwrap() error { return e.Err }

// Runner runs Claude (Run) and the bot's own deterministic checkers (Exec)
// in the sandbox.
type Runner interface {
	Run(ctx context.Context, req RunRequest) (*RunResult, error)
	// Exec runs one command without network and credentials. The error is
	// non-nil only when the command could not be run at all (bad Dir, docker
	// failure, timeout/cancel); a non-zero exit is reported in ExecResult.
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
}

// Package claude implements core.Runner: it runs headless Claude Code
// (`claude -p`) against a checkout, either inside a hardened docker
// container whose only network route is the egress proxy ("docker" mode),
// or directly on the host with no isolation ("local" mode, prototyping only).
package claude

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Options configures the runner.
type Options struct {
	Mode         string // "docker" | "local"
	Image        string // sandbox image (docker mode)
	Network      string // internal docker network for claude runs
	EgressProxy  string // e.g. http://egress:3128, as seen from Network
	CPUs, Memory string // docker --cpus / --memory; empty = no limit
	ClaudeBin    string // local mode, default "claude"; docker mode always runs "claude" in the image
	DockerBin    string // default "docker"
	// User is the docker --user value ("uid:gid"). Default: the uid:gid of
	// this process, so files written into the clone stay removable. If the
	// service runs as root, set a non-root user and make the work dir
	// writable for it.
	User   string
	Logger *slog.Logger
	// Telemetry, when set, turns on Claude Code's own OpenTelemetry traces
	// for every Claude run (tracing.claude_cli, experimental; see
	// telemetry.go). Exec commands never get it.
	Telemetry *CLITelemetry
	// HideEnv are variables of the service environment that no child
	// process gets (the secrets referenced by tracing.headers).
	HideEnv []string
}

const (
	defaultRunTimeout  = 15 * time.Minute
	defaultExecTimeout = 3 * time.Minute
	// execNotFound is the conventional "command not found" exit code; docker
	// run returns it too when the image has no such executable.
	execNotFound = 127
	maxCapture   = 32 << 20 // cap on captured stdout
	killTimeout  = 30 * time.Second
)

type runner struct {
	o       Options
	authEnv []string // names of credential env vars present in the service env
	log     *slog.Logger
	// environ is the service environment (overridable in tests).
	environ func() []string
	user    string // docker --user
	// advisorOff is set once a run was refused because of the advisor
	// tool; later runs go without it.
	advisorOff atomic.Bool
	// deepWarned: a failed deep-review call was logged (once per process).
	deepWarned atomic.Bool
}

// NewRunner validates options and returns a core.Runner.
func NewRunner(o Options) (core.Runner, error) {
	return newRunner(o, os.Environ)
}

func newRunner(o Options, environ func() []string) (*runner, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Mode == "" {
		o.Mode = "docker"
	}
	if o.ClaudeBin == "" {
		o.ClaudeBin = "claude"
	}
	if o.DockerBin == "" {
		o.DockerBin = "docker"
	}
	if len(o.HideEnv) > 0 {
		base, hide := environ, append([]string(nil), o.HideEnv...)
		environ = func() []string { return filterEnv(base(), hide) }
	}
	r := &runner{o: o, log: o.Logger, environ: environ, user: o.User}
	if r.user == "" {
		r.user = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	env := envMap(environ())
	for _, name := range authEnvNames {
		if env[name] != "" {
			r.authEnv = append(r.authEnv, name)
		}
	}
	if len(r.authEnv) == 0 {
		return nil, errors.New("claude: neither CLAUDE_CODE_OAUTH_TOKEN nor ANTHROPIC_API_KEY is set")
	}
	switch o.Mode {
	case "docker":
		var missing []string
		if o.Image == "" {
			missing = append(missing, "Image")
		}
		if o.Network == "" {
			missing = append(missing, "Network")
		}
		if o.EgressProxy == "" {
			missing = append(missing, "EgressProxy")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("claude: docker mode requires %s", strings.Join(missing, ", "))
		}
		if _, err := exec.LookPath(o.DockerBin); err != nil {
			return nil, fmt.Errorf("claude: docker binary %q: %w", o.DockerBin, err)
		}
		if strings.HasPrefix(r.user, "0:") || r.user == "0" || r.user == "root" {
			r.log.Warn("sandbox containers will run as root (uid 0); set claude.Options.User to a non-root uid:gid")
		}
	case "local":
		if _, err := exec.LookPath(o.ClaudeBin); err != nil {
			return nil, fmt.Errorf("claude: claude binary %q: %w", o.ClaudeBin, err)
		}
		r.log.Warn("!!! sandbox.mode=local: Claude and the bot's checkers run directly on this host " +
			"with NO isolation (filesystem, network, processes). Use only for prototyping on trusted repositories !!!")
	default:
		return nil, fmt.Errorf("claude: unknown mode %q", o.Mode)
	}
	return r, nil
}

// Run implements core.Runner.
func (r *runner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	asked := req.Advisor != ""
	if r.advisorOff.Load() {
		req.Advisor = ""
	}
	res, err := r.run(ctx, req)
	if err != nil && req.Advisor != "" && isAdvisorError(err) {
		// The account or model does not support the advisor: remember it
		// for the life of the process and run without it.
		r.advisorOff.Store(true)
		r.log.Warn("claude: advisor not available, running without it from now on", "advisor", req.Advisor, "err", err)
		req.Advisor = ""
		res, err = r.run(ctx, req)
	}
	if res != nil && asked && req.Advisor == "" {
		res.AdvisorDropped = true // a verification problem for the caller
	}
	return res, err
}

func (r *runner) run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	dir, err := checkDir(req.Dir)
	if err != nil {
		return nil, err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	mcpAsked := mcpOn(req)
	var mcp *mcpSetup
	if mcpAsked {
		m, err := r.prepareMCP(req.MCP, dir)
		if err != nil {
			// The review still works without the tools: the prompt tells
			// the model to put findings into the final answer then.
			r.log.Warn("claude: MCP config not written, running without the MCP tools", "err", err)
			req.MCP = nil
		} else {
			mcp = m
			defer mcp.cleanup()
		}
	}
	ctxDirs := usableContextDirs(req.ContextDirs, r.log)
	skillsDir := usableSkillsDir(req.SkillsDir, r.log)
	baseDir := usableBaseDir(req.BaseDir, dir, r.log)
	ro := readOnlyDirs{base: baseDir, ctx: ctxDirs, skills: skillsDir}
	claudeArgs := buildClaudeArgsScoped(req, r.readScope(dir, ro, mcp))
	if mcp != nil {
		claudeArgs = withMCPConfigArg(claudeArgs, mcp.cliPath)
	}
	var cmd *exec.Cmd
	var name string
	if r.o.Mode == "docker" {
		name = containerName("run")
		cmd = r.dockerClaudeCmd(ctx, req, name, dir, ro, mcp, claudeArgs)
	} else {
		var closeFD func()
		cmd, closeFD = r.localClaudeCmd(ctx, req, dir, ro, mcp, claudeArgs)
		if closeFD != nil {
			defer closeFD()
		}
	}
	cmd.Stdin = strings.NewReader(req.Prompt)

	start := time.Now()
	r.log.Info("claude run started", "mode", r.o.Mode, "dir", dir, "container", name,
		"tools", !req.NoTools, "schema", req.JSONSchema != "", "model", req.Model, "base", baseDir != "", "context_repos", len(ctxDirs), "skills", skillsDir != "",
		"mcp", mcp != nil)
	sc := newStreamCapture(ctx) // stream-json: transcript to the observer, result to parseOutput
	p := r.execWith(ctx, cmd, name, sc)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("claude run aborted after %s: %w", time.Since(start).Round(time.Second), ctxErr)
	}
	if r.o.Mode == "docker" && p.ExitCode == 125 {
		return nil, fmt.Errorf("claude: docker run failed: %s", describeFailure(p))
	}
	res, err := parseOutput(p, req.JSONSchema != "")
	err = sc.refineError(err)
	if res != nil {
		res.ReadFiles = sc.ReadFiles()
	}
	if err != nil {
		r.log.Warn("claude run failed", "err", err, "exit", p.ExitCode, "took", time.Since(start).Round(time.Millisecond))
		return nil, err
	}
	if res.Duration == 0 {
		res.Duration = time.Since(start)
	}
	if res.DeepReviewError = sc.DeepReviewError(); res.DeepReviewError != "" && !r.deepWarned.Swap(true) {
		r.log.Warn("claude: the deep-review subagent failed, the run finished without it (logged once)",
			"model", req.DeepReviewModel, "err", res.DeepReviewError)
	}
	res.MCPDropped = mcpAsked && mcp == nil
	if mcp != nil {
		res.MCPStatus = sc.MCPStatus(req.MCP.Name)
	}
	r.log.Info("claude run finished", "cost_usd", res.CostUSD, "turns", res.NumTurns,
		"duration", res.Duration.Round(time.Millisecond), "output_bytes", len(res.Output), "mcp_status", res.MCPStatus)
	return res, nil
}

// readScope is what the model may read in this run (readscope.go).
func (r *runner) readScope(dir string, ro readOnlyDirs, mcp *mcpSetup) readScope {
	if r.o.Mode == "docker" {
		return dockerReadScope(ro)
	}
	mcpDir := ""
	if mcp != nil {
		mcpDir = filepath.Dir(mcp.cliPath)
	}
	return localReadScope(dir, ro, envHome(r.environ()), mcpDir)
}

func checkDir(dir string) (string, error) {
	if dir == "" {
		return "", errors.New("claude: empty Dir")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("claude: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("claude: %s is not a directory", abs)
	}
	return abs, nil
}

func containerName(kind string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "reviewbot-" + kind + "-" + hex.EncodeToString(b)
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

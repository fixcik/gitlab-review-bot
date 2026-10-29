package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Local mode hands the credential to claude on fd 3: it is not in the
// child's environment, the fd variable is, and the pipe carries the token.
func TestLocalClaudeCmdTokenOnFD(t *testing.T) {
	r, err := newRunner(Options{Mode: "local", ClaudeBin: "/bin/sh"}, apiKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	cmd, closer := r.localClaudeCmd(context.Background(), core.RunRequest{}, t.TempDir(), readOnlyDirs{}, nil, []string{"-p"})
	if closer == nil || len(cmd.ExtraFiles) != 1 {
		t.Fatalf("closer=%v extra=%d", closer != nil, len(cmd.ExtraFiles))
	}
	defer closer()
	if slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.Contains(kv, "sk-test-key") }) ||
		!slices.Contains(cmd.Env, "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=3") {
		t.Fatalf("env = %v", cmd.Env)
	}
	b, _ := io.ReadAll(cmd.ExtraFiles[0])
	if string(b) != "sk-test-key\n" {
		t.Fatalf("fd = %q", b)
	}
}

// With both credentials set, the first one (the OAuth token) goes to the
// fd.
func TestTokenFDEnvFirstCredentialWins(t *testing.T) {
	r := &runner{}
	env, files, closer, err := r.tokenFDEnv([]string{"ANTHROPIC_API_KEY=k", "CLAUDE_CODE_OAUTH_TOKEN=oauth-tok"})
	if err != nil || closer == nil {
		t.Fatal(err)
	}
	defer closer()
	b, _ := io.ReadAll(files[0])
	if string(b) != "oauth-tok\n" || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3") ||
		slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=oauth-tok") {
		t.Fatalf("env=%v fd=%q", env, b)
	}
}

// describeFailure: stderr and stdout tails, stdout capped at half the
// stderr budget.
func TestDescribeFailureParts(t *testing.T) {
	if got := describeFailure(processOutput{Stdout: []byte(" out \n")}); got != "stdout: out" {
		t.Errorf("stdout only: %q", got)
	}
	long := strings.Repeat("o", tailBytes)
	got := describeFailure(processOutput{Stderr: []byte("err"), Stdout: []byte(long), Err: fmt.Errorf("x")})
	if got != "stderr: err; stdout: ..."+long[:tailBytes/2] {
		t.Errorf("both: %d bytes %q", len(got), got[:40])
	}
}

// A local command's whole process tree dies on timeout (process group):
// a background child holding stdout does not keep Exec waiting.
func TestExecLocalTimeoutKillsProcessGroup(t *testing.T) {
	r, err := newRunner(Options{Mode: "local", ClaudeBin: "/bin/sh"}, apiKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = r.Exec(context.Background(), core.ExecRequest{Dir: t.TempDir(), Command: []string{"sh", "-c", "sleep 30 & wait"}, Timeout: 300 * time.Millisecond})
	if err == nil {
		t.Fatal("no timeout error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Exec took %s: background child survived", d)
	}
}

// Stderr keeps its last 1 MiB (the error is at the end).
func TestExecStderrKeepsTail(t *testing.T) {
	r, err := newRunner(Options{Mode: "local", ClaudeBin: "/bin/sh"}, apiKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Exec(context.Background(), core.ExecRequest{Dir: t.TempDir(), Command: []string{"sh", "-c",
		`i=0; while [ $i -lt 1200 ]; do printf '%01000d\n' 0 >&2; i=$((i+1)); done; echo THE-ERROR >&2; exit 2`}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 2 || len(res.Stderr) != 1<<20 || !strings.HasSuffix(res.Stderr, "THE-ERROR\n") {
		t.Fatalf("exit=%d stderr=%d bytes, tail %q", res.ExitCode, len(res.Stderr), res.Stderr[max(0, len(res.Stderr)-20):])
	}
}

// The MCP config gets its intended mode whatever the umask: world-readable
// for another sandbox uid even under umask 077.
func TestPrepareMCPModeIgnoresUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	m := &core.MCPServer{Name: "reviewbot", URL: "http://reviewbot:8091/mcp", Token: "tok-secret"}
	r := &runner{o: Options{Mode: "docker"}, user: "54321:54321"}
	clone := filepath.Join(t.TempDir(), "clone")
	os.Mkdir(clone, 0o755)
	ms, err := r.prepareMCP(m, clone)
	if err != nil {
		t.Fatal(err)
	}
	defer ms.cleanup()
	fi, _ := os.Stat(filepath.Join(ms.mount.Host, mcpConfigName))
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// newRunner: defaults, the docker settings it requires, and a warning for
// sandbox containers running as root.
func TestNewRunnerDockerRequirements(t *testing.T) {
	bin := t.TempDir()
	docker := writeScript(t, bin, "docker", "exit 0\n")
	for _, tc := range []struct {
		edit func(*Options)
		want string
	}{
		{func(o *Options) { o.Image = "" }, "requires Image"},
		{func(o *Options) { o.Network = "" }, "requires Network"},
		{func(o *Options) { o.EgressProxy = "" }, "requires EgressProxy"},
		{func(o *Options) { o.Image, o.Network = "", "" }, "requires Image, Network"},
	} {
		o := dockerOpts()
		o.DockerBin = docker
		tc.edit(&o)
		if _, err := newRunner(o, apiKeyEnv); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
	for user, warn := range map[string]bool{"0:0": true, "0": true, "root": true, "1000:1000": false, "10:0": false} {
		var logs bytes.Buffer
		o := dockerOpts()
		o.DockerBin, o.User, o.Logger = docker, user, slog.New(slog.NewTextHandler(&logs, nil))
		r, err := newRunner(o, apiKeyEnv)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(logs.String(), "will run as root"); got != warn || r.o.ClaudeBin != "claude" {
			t.Errorf("user %s: warned=%v claude=%q", user, got, r.o.ClaudeBin)
		}
	}
	t.Setenv("PATH", bin) // LookPath uses the process PATH: no claude there
	if _, err := newRunner(Options{Mode: "local"}, apiKeyEnv); err == nil || !strings.Contains(err.Error(), `"claude"`) {
		t.Errorf("default claude binary: %v", err)
	}
}

// With both credentials set, local mode hands the first one to Claude on fd
// 3 and the second one does not stay in Claude's environment either (nor a
// stale fd variable from the service environment). If the pipe cannot be
// set up, only the first credential goes into the environment.
func TestLocalClaudeCmdBothCredentials(t *testing.T) {
	both := func() []string {
		return []string{"PATH=/bin", "ANTHROPIC_API_KEY=sk-second", "CLAUDE_CODE_OAUTH_TOKEN=oauth-first",
			"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=7"}
	}
	r, err := newRunner(Options{Mode: "local", ClaudeBin: "/bin/sh"}, both)
	if err != nil {
		t.Fatal(err)
	}
	cmd, closer := r.localClaudeCmd(context.Background(), core.RunRequest{}, t.TempDir(), readOnlyDirs{}, nil, []string{"-p"})
	if closer == nil || len(cmd.ExtraFiles) != 1 {
		t.Fatalf("closer=%v extra=%d", closer != nil, len(cmd.ExtraFiles))
	}
	defer closer()
	b, _ := io.ReadAll(cmd.ExtraFiles[0])
	leaks := func(env []string) bool {
		return slices.ContainsFunc(env, func(kv string) bool {
			return strings.Contains(kv, "sk-second") || strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") ||
				strings.HasPrefix(kv, "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=")
		})
	}
	if string(b) != "oauth-first\n" || leaks(cmd.Env) || slices.ContainsFunc(cmd.Env, func(kv string) bool { return strings.Contains(kv, "oauth-first") }) ||
		!slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3") {
		t.Fatalf("env=%v fd=%q", cmd.Env, b)
	}

	old := osPipe
	t.Cleanup(func() { osPipe = old })
	osPipe = func() (*os.File, *os.File, error) { return nil, nil, errors.New("too many open files") }
	cmd, closer = r.localClaudeCmd(context.Background(), core.RunRequest{}, t.TempDir(), readOnlyDirs{}, nil, []string{"-p"})
	if closer != nil || leaks(cmd.Env) || !slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=oauth-first") {
		t.Fatalf("fallback env = %v", cmd.Env)
	}
}

func TestSingleCredentialEnv(t *testing.T) {
	if got := singleCredentialEnv([]string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN="}); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Errorf("no credential: %v", got)
	}
	if got := singleCredentialEnv([]string{"ANTHROPIC_API_KEY=k", "PATH=/bin"}); !slices.Equal(got, []string{"PATH=/bin", "ANTHROPIC_API_KEY=k"}) {
		t.Errorf("api key: %v", got)
	}
}

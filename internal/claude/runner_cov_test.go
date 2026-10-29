package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func apiKeyEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=sk-test-key"}
}

// checkDir accepts only an existing directory and returns its absolute path.
func TestCheckDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	os.WriteFile(file, []byte("x"), 0o644)
	for _, tc := range []struct {
		in, want string
	}{
		{"", "empty Dir"},
		{filepath.Join(dir, "missing"), "no such file"},
		{file, "is not a directory"},
	} {
		if _, err := checkDir(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("checkDir(%q) = %v, want %q", tc.in, err, tc.want)
		}
	}
	t.Chdir(dir)
	os.Mkdir("sub", 0o755)
	if got, err := checkDir("sub"); err != nil || got != filepath.Join(dir, "sub") {
		t.Errorf("relative dir = %q, %v", got, err)
	}
	// The working directory is gone: a relative path cannot be resolved.
	gone := filepath.Join(dir, "gone")
	os.Mkdir(gone, 0o755)
	t.Chdir(gone)
	os.Remove(gone)
	if _, err := checkDir("sub"); err == nil {
		t.Error("relative path resolved without a working directory")
	}
}

// newRunner defaults to docker mode and checks that the binaries exist.
func TestNewRunnerModesAndBinaries(t *testing.T) {
	bin := t.TempDir()
	docker := writeScript(t, bin, "docker", "exit 0\n")
	o := dockerOpts()
	o.Mode, o.DockerBin = "", docker
	r, err := newRunner(o, apiKeyEnv)
	if err != nil || r.o.Mode != "docker" {
		t.Fatalf("default mode: %v %+v", err, r)
	}
	o.DockerBin = filepath.Join(bin, "no-docker")
	if _, err := newRunner(o, apiKeyEnv); err == nil || !strings.Contains(err.Error(), "docker binary") {
		t.Errorf("missing docker: %v", err)
	}
	if _, err := newRunner(Options{Mode: "local", ClaudeBin: filepath.Join(bin, "no-claude")}, apiKeyEnv); err == nil || !strings.Contains(err.Error(), "claude binary") {
		t.Errorf("missing claude: %v", err)
	}
}

// Run: a missing Dir fails before anything runs; docker's own failure (exit
// 125) is an error with its stderr; a result without duration_ms gets the
// measured wall time.
func TestRunDirDockerFailureAndDuration(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	docker := writeScript(t, bin, "docker", `
touch "`+marker+`"
cat >/dev/null
case "$MODE" in
  fail) echo 'docker: invalid reference format' >&2; exit 125;;
  *) echo '{"type":"result","subtype":"success","result":"plain","num_turns":1}';;
esac
`)
	o := dockerOpts()
	o.DockerBin = docker
	mode := "fail"
	r, err := newRunner(o, func() []string { return append(apiKeyEnv(), "MODE="+mode) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), core.RunRequest{Dir: filepath.Join(bin, "missing"), Prompt: "x"}); err == nil {
		t.Fatal("missing dir accepted")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("docker ran for a missing dir")
	}
	_, err = r.Run(context.Background(), core.RunRequest{Dir: t.TempDir(), Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "docker run failed") || !strings.Contains(err.Error(), "invalid reference format") {
		t.Fatalf("exit 125: %v", err)
	}
	mode = "ok"
	res, err := r.Run(context.Background(), core.RunRequest{Dir: t.TempDir(), Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "plain" || res.Duration <= 0 {
		t.Fatalf("res = %+v", res)
	}
}

// Exec: a missing Dir and docker's own failure are errors; a program that
// cannot be started is an error, not an exit code.
func TestExecErrors(t *testing.T) {
	bin := t.TempDir()
	docker := writeScript(t, bin, "docker", "echo 'docker: Error response from daemon: no such image' >&2; exit 125\n")
	o := dockerOpts()
	o.DockerBin = docker
	r, err := newRunner(o, apiKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Exec(context.Background(), core.ExecRequest{Dir: filepath.Join(bin, "missing"), Command: []string{"true"}}); err == nil {
		t.Error("missing dir accepted")
	}
	if _, err := r.Exec(context.Background(), core.ExecRequest{Dir: t.TempDir(), Command: []string{"true"}}); err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Errorf("exit 125: %v", err)
	}

	// Local mode: an executable file that is not a program.
	bad := filepath.Join(bin, "not-a-program")
	os.WriteFile(bad, []byte("\x00\x01garbage"), 0o755)
	lr, err := newRunner(Options{Mode: "local", ClaudeBin: "/bin/sh"}, apiKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	res, err := lr.Exec(context.Background(), core.ExecRequest{Dir: t.TempDir(), Command: []string{bad}})
	if err == nil || res != nil || !strings.Contains(err.Error(), "not-a-program") {
		t.Fatalf("start failure: %+v %v", res, err)
	}
}

// A failed `docker rm -f` of a cancelled container is logged with its
// output (and the run still reports the cancellation).
func TestRemoveContainerFailureLogged(t *testing.T) {
	bin := t.TempDir()
	docker := writeScript(t, bin, "docker", `
if [ "$1" = "rm" ]; then echo "Error: daemon unavailable" >&2; exit 1; fi
cat >/dev/null
exec sleep 30
`)
	var logs bytes.Buffer
	o := dockerOpts()
	o.DockerBin = docker
	o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	r, err := newRunner(o, apiKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Exec(context.Background(), core.ExecRequest{Dir: t.TempDir(), Command: []string{"x"}, Timeout: 200 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	s := logs.String()
	if !strings.Contains(s, "docker rm -f failed") || !strings.Contains(s, "container=reviewbot-exec-") || !strings.Contains(s, "daemon unavailable") {
		t.Fatalf("log:\n%s", s)
	}
}

// capBuffer keeps the head (stdout) or the tail (stderr) of what was
// written and always reports the full length as written.
func TestCapBuffer(t *testing.T) {
	head := &capBuffer{max: 5}
	tailB := &capBuffer{max: 5, keepTail: true}
	for _, s := range []string{"abc", "defg", "hij"} {
		for _, b := range []*capBuffer{head, tailB} {
			if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
				t.Fatalf("Write = %d, %v", n, err)
			}
		}
	}
	if string(head.Bytes()) != "abcde" || string(tailB.Bytes()) != "fghij" {
		t.Fatalf("head=%q tail=%q", head.Bytes(), tailB.Bytes())
	}
	exact := &capBuffer{max: 3}
	exact.Write([]byte("xyz"))
	if string(exact.Bytes()) != "xyz" {
		t.Fatalf("exact = %q", exact.Bytes())
	}
}

// The process-group cancel of a command that never started is a no-op.
func TestProcessGroupCancelBeforeStart(t *testing.T) {
	cmd := exec.Command("true")
	setProcessGroup(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("no process group")
	}
	if err := cmd.Cancel(); err != nil {
		t.Fatalf("Cancel = %v", err)
	}
}

// When the token pipe cannot be created, local mode falls back to the
// credential in the environment (and has no extra fd or closer).
func TestLocalClaudeCmdPipeFailureFallsBackToEnv(t *testing.T) {
	old := osPipe
	t.Cleanup(func() { osPipe = old })
	osPipe = func() (*os.File, *os.File, error) { return nil, nil, errors.New("too many open files") }

	var logs bytes.Buffer
	r, err := newRunner(Options{Mode: "local", ClaudeBin: "/bin/sh", Logger: slog.New(slog.NewTextHandler(&logs, nil))}, apiKeyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.tokenFDEnv(apiKeyEnv()); err == nil {
		t.Fatal("tokenFDEnv hid the pipe error")
	}
	cmd, closer := r.localClaudeCmd(context.Background(), core.RunRequest{}, t.TempDir(), readOnlyDirs{}, nil, []string{"-p"})
	if closer != nil || cmd.ExtraFiles != nil {
		t.Fatalf("closer=%v extra=%v", closer != nil, cmd.ExtraFiles)
	}
	if !slices.Contains(cmd.Env, "ANTHROPIC_API_KEY=sk-test-key") || slices.ContainsFunc(cmd.Env, func(kv string) bool {
		return strings.Contains(kv, "FILE_DESCRIPTOR")
	}) {
		t.Fatalf("env = %v", cmd.Env)
	}
	if !strings.Contains(logs.String(), "token-on-fd setup failed") || strings.Contains(logs.String(), "sk-test-key") {
		t.Fatalf("log: %s", logs.String())
	}
}

// prepareMCP refuses a config directory it cannot create, and one whose path
// would break the docker -v syntax (then nothing is left behind).
func TestPrepareMCPErrors(t *testing.T) {
	m := &core.MCPServer{Name: "reviewbot", URL: "http://reviewbot:8091/mcp", Token: "tok-secret"}
	r := &runner{o: Options{Mode: "docker"}, user: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())}
	if _, err := r.prepareMCP(m, filepath.Join(t.TempDir(), "missing", "clone")); err == nil || !strings.Contains(err.Error(), "mcp config dir") {
		t.Errorf("missing parent: %v", err)
	}
	parent := filepath.Join(t.TempDir(), "a:b")
	clone := filepath.Join(parent, "clone")
	os.MkdirAll(clone, 0o755)
	if _, err := r.prepareMCP(m, clone); err == nil || !strings.Contains(err.Error(), "cannot be mounted") {
		t.Errorf("colon path: %v", err)
	}
	ents, _ := os.ReadDir(parent)
	if len(ents) != 1 || ents[0].Name() != "clone" {
		t.Errorf("left behind: %v", ents)
	}
	for _, sep := range []string{",", "\n"} {
		parent := filepath.Join(t.TempDir(), "a"+sep+"b")
		os.MkdirAll(filepath.Join(parent, "clone"), 0o755)
		if _, err := r.prepareMCP(m, filepath.Join(parent, "clone")); err == nil {
			t.Errorf("path with %q accepted", sep)
		}
	}
}

// --mcp-config goes after --strict-mcp-config, or first when that flag is
// absent (never after a variadic flag's values).
func TestWithMCPConfigArgAndHost(t *testing.T) {
	got := withMCPConfigArg([]string{"-p", "--model", "x"}, "/etc/m.json")
	if strings.Join(got, " ") != "--mcp-config /etc/m.json -p --model x" {
		t.Errorf("no strict flag: %v", got)
	}
	got = withMCPConfigArg([]string{"-p", "--strict-mcp-config", "--strict-mcp-config"}, "/m")
	if strings.Join(got, " ") != "-p --strict-mcp-config --mcp-config /m --strict-mcp-config" {
		t.Errorf("strict flag: %v", got)
	}
	for _, tc := range []struct {
		m    *core.MCPServer
		want string
	}{
		{nil, ""},
		{&core.MCPServer{URL: "http://[::1"}, ""},
		{&core.MCPServer{URL: "http://Reviewbot:8091/mcp"}, "Reviewbot"},
	} {
		if h := mcpHost(tc.m); h != tc.want {
			t.Errorf("mcpHost(%v) = %q, want %q", tc.m, h, tc.want)
		}
	}
}

// Unusable skills and context paths are dropped with a warning naming the
// reason (or silently without a logger).
func TestUnusableDirsLogged(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	for _, p := range []string{"relative/skills", "/a:b", filepath.Join(t.TempDir(), "missing")} {
		logs.Reset()
		if got := usableSkillsDir(p, log); got != "" {
			t.Errorf("%q kept", p)
		}
		if !strings.Contains(logs.String(), "skills not mounted") {
			t.Errorf("%q: no warning: %s", p, logs.String())
		}
		if got := usableSkillsDir(p, nil); got != "" {
			t.Errorf("%q kept without logger", p)
		}
	}
	d := t.TempDir()
	if got := usableSkillsDir(d, log); got != d {
		t.Errorf("dir dropped: %q", got)
	}
	logs.Reset()
	got := usableContextDirs([]core.ContextDir{{Name: "../x", HostPath: d}, {Name: "ok", HostPath: d}}, log)
	if len(got) != 1 || got[0].Name != "ok" || !strings.Contains(logs.String(), "reason=\"invalid name\"") {
		t.Errorf("context dirs = %+v, log %s", got, logs.String())
	}
}

// NewRunner reads the credentials from the process environment.
func TestNewRunnerUsesProcessEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	if _, err := NewRunner(Options{Mode: "local", ClaudeBin: "/bin/sh"}); err == nil {
		t.Fatal("runner without credentials")
	}
	t.Setenv("ANTHROPIC_API_KEY", "k")
	if r, err := NewRunner(Options{Mode: "local", ClaudeBin: "/bin/sh"}); err != nil || r == nil {
		t.Fatalf("NewRunner = %v", err)
	}
}

// The MCP config is owner-only when the sandbox runs as this uid, and
// world-readable (in a random, run-scoped directory) for another uid.
func TestPrepareMCPModes(t *testing.T) {
	m := &core.MCPServer{Name: "reviewbot", URL: "http://reviewbot:8091/mcp", Token: "tok-secret"}
	for _, tc := range []struct {
		user          string
		dirMode, mode os.FileMode
	}{
		{fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), 0o700, 0o600},
		{"54321:54321", 0o755, 0o644},
	} {
		r := &runner{o: Options{Mode: "docker"}, user: tc.user}
		clone := filepath.Join(t.TempDir(), "clone")
		os.Mkdir(clone, 0o755)
		ms, err := r.prepareMCP(m, clone)
		if err != nil {
			t.Fatal(err)
		}
		dir := ms.mount.Host
		di, _ := os.Stat(dir)
		fi, _ := os.Stat(filepath.Join(dir, mcpConfigName))
		if di.Mode().Perm() != tc.dirMode || fi.Mode().Perm() != tc.mode || filepath.Dir(dir) != filepath.Dir(clone) {
			t.Errorf("user %s: dir %v file %v in %s", tc.user, di.Mode().Perm(), fi.Mode().Perm(), dir)
		}
		ms.cleanup()
		if _, err := os.Stat(dir); err == nil {
			t.Error("config dir not removed")
		}
	}
}

// The MCP config directory is denied both as given and resolved (a /tmp
// symlink), so neither spelling of the path reaches the token file.
func TestLocalReadScopeDeniesResolvedMCPDir(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var is itself a symlink
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	os.Mkdir(real, 0o700)
	link := filepath.Join(base, "link")
	os.Symlink(real, link)
	s := localReadScope(t.TempDir(), readOnlyDirs{}, "/nonexistent-home", link)
	if !slices.Equal(s.deny, []string{link, real}) {
		t.Fatalf("deny = %v", s.deny)
	}
	s = localReadScope(t.TempDir(), readOnlyDirs{}, "/nonexistent-home", real)
	if !slices.Equal(s.deny, []string{real}) {
		t.Fatalf("deny = %v", s.deny)
	}
}

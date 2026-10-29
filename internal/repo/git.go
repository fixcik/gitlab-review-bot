package repo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const (
	// networkTimeout bounds a single fetch from GitLab.
	networkTimeout = 15 * time.Minute
	// localTimeout bounds local git operations (clone --local, diff, ...).
	localTimeout = 5 * time.Minute
	// maxErrOutput is how much of stderr goes into an error message.
	maxErrOutput = 2048
)

// gitRunner runs git with a controlled environment.
type gitRunner struct {
	bin   string
	token string
	// caFile is gitlab.ca_file: a PEM bundle git trusts for HTTPS to GitLab
	// (http.sslCAInfo), passed per process like the auth header.
	caFile string
	// hide are more variable names (case-insensitive) never passed to git
	// (Options.HideEnv: the tracing header secrets).
	hide []string
}

// hostSafeGitConfig is forced (via GIT_CONFIG_COUNT) on every host-side git
// invocation. A clone fetched from GitLab is attacker-controlled data: its
// .gitattributes, .git/config and submodule config must not be able to make
// the bot's git run a program while it diffs or checks out the MR. These keys
// disable external diff/textconv drivers, fsmonitor and hooks, the pager, SSH
// and transport helpers. Combined with GIT_CONFIG_NOSYSTEM and
// GIT_CONFIG_GLOBAL=/dev/null in env(), no config source the repo controls can
// define a driver command. safe.directory is scoped by callers (the clone dir)
// rather than "*".
// It intentionally does NOT force protocol.allow: the bot controls every
// remote it fetches (a GitLab https URL or a local path) and never recurses
// submodules, and protocol.allow=never would break clone --local / fetch.
var hostSafeGitConfig = [][2]string{
	{"core.fsmonitor", "false"},
	{"core.hooksPath", "/dev/null"},
	{"core.pager", "cat"},
	{"core.editor", "false"},
	{"core.sshCommand", "false"},
	{"diff.external", ""},
	{"uploadpack.packObjectsHook", ""},
}

// gitCmd describes one git invocation.
type gitCmd struct {
	dir     string
	args    []string
	auth    bool // network command: add the GitLab auth header and CA file
	timeout time.Duration
	// config is extra "-c key=value" style configuration passed through the
	// environment (GIT_CONFIG_COUNT), so it never shows up in argv.
	config [][2]string
	// env are extra NAME=value entries (GIT_INDEX_FILE of a tree checkout).
	env []string
}

// gitError carries the exit code of a failed git command.
type gitError struct {
	args     []string
	exitCode int
	stderr   string
	err      error
}

func (e *gitError) Error() string {
	msg := fmt.Sprintf("git %s: %v", strings.Join(e.args, " "), e.err)
	if e.stderr != "" {
		msg += ": " + e.stderr
	}
	return msg
}

func (e *gitError) Unwrap() error { return e.err }

// exitCode returns the git exit code for err, or -1.
func exitCode(err error) int {
	var ge *gitError
	if errors.As(err, &ge) {
		return ge.exitCode
	}
	return -1
}

func (g *gitRunner) authHeader() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+g.token))
}

// minRedactLen is the shortest token that is redacted from error text.
// Replacing a very short "token" would corrupt unrelated text (URLs, paths).
const minRedactLen = 8

// redact removes the token (raw and base64-encoded) from s.
func (g *gitRunner) redact(s string) string {
	if len(g.token) < minRedactLen {
		return s
	}
	s = strings.ReplaceAll(s, g.token, "[REDACTED]")
	s = strings.ReplaceAll(s, base64.StdEncoding.EncodeToString([]byte("oauth2:"+g.token)), "[REDACTED]")
	return s
}

// env builds the environment for git: the service env minus anything that
// could redirect git (GIT_DIR, ...) or leak secrets, plus our config.
func (g *gitRunner) env(c gitCmd) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(name, "GIT_"):
			continue
		case core.IsSecretEnv(name):
			// Every service secret and Claude credential (one shared list).
			continue
		case slices.ContainsFunc(g.hide, func(h string) bool { return strings.EqualFold(h, name) }):
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_CONFIG_NOSYSTEM=1",
		// Drop the global config too: a hostile repo cannot define an LFS
		// smudge filter or a diff/textconv driver in it, but a filter the
		// host operator happens to have configured globally (e.g. git-lfs)
		// must not fire on the MR's files during our diff/checkout either.
		"GIT_CONFIG_GLOBAL=/dev/null",
		"LC_ALL=C",
	)
	// Forced config neutralizes every program a repo could make git run.
	// Prepended so an explicit c.config entry could only ever override it
	// deliberately from our own code, never from the repo.
	cfg := append([][2]string{}, hostSafeGitConfig...)
	cfg = append(cfg, c.config...)
	if c.auth && g.caFile != "" {
		// gitlab.ca_file for fetches from an internal GitLab. Per process,
		// never written to a git config.
		cfg = append(cfg, [2]string{"http.sslCAInfo", g.caFile})
	}
	if c.auth && g.token != "" {
		// Passed through the environment rather than "-c" so that the token
		// is not visible in the process list. Never written to any config.
		cfg = append(cfg, [2]string{"http.extraHeader", g.authHeader()})
	}
	env = append(env, c.env...)
	if len(cfg) > 0 {
		env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(cfg)))
		for i, kv := range cfg {
			env = append(env,
				fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]),
				fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
		}
	}
	return env
}

// cappedBuffer keeps the first max bytes written to it and silently drops
// the rest (the command still runs to completion, bounded by its timeout).
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.over = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// runCapped is run with stdout capped at max bytes; truncated reports that
// output was dropped.
func (g *gitRunner) runCapped(ctx context.Context, c gitCmd, max int) (out []byte, truncated bool, err error) {
	cb := &cappedBuffer{max: max}
	err = g.runTo(ctx, c, cb)
	return cb.buf.Bytes(), cb.over, err
}

// run executes git and returns stdout.
func (g *gitRunner) run(ctx context.Context, c gitCmd) ([]byte, error) {
	var stdout bytes.Buffer
	err := g.runTo(ctx, c, &stdout)
	return stdout.Bytes(), err
}

// runTo executes git writing stdout to w.
func (g *gitRunner) runTo(ctx context.Context, c gitCmd, w io.Writer) error {
	ctx, cancel := withGitTimeout(ctx, c)
	defer cancel()
	cmd := g.command(ctx, c)
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return g.failure(ctx, c, err, stderr.String())
	}
	return nil
}

// stream runs git and hands its stdout to fn while it runs (for outputs too
// large to buffer, such as git archive). The same controlled environment as
// run is used.
func (g *gitRunner) stream(ctx context.Context, c gitCmd, fn func(io.Reader) error) error {
	ctx, cancel := withGitTimeout(ctx, c)
	defer cancel()
	cmd := g.command(ctx, c)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	fnErr := fn(out)
	if fnErr != nil {
		cancel() // stop git; its error is secondary
		_, _ = io.Copy(io.Discard, out)
	}
	werr := cmd.Wait()
	if fnErr != nil {
		return fnErr
	}
	if werr != nil {
		return g.failure(ctx, c, werr, stderr.String())
	}
	return nil
}

// withGitTimeout bounds one git invocation (localTimeout by default).
func withGitTimeout(ctx context.Context, c gitCmd) (context.Context, context.CancelFunc) {
	timeout := c.timeout
	if timeout <= 0 {
		timeout = localTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// command is the git process of c with the controlled environment.
func (g *gitRunner) command(ctx context.Context, c gitCmd) *exec.Cmd {
	cmd := exec.CommandContext(ctx, g.bin, c.args...)
	cmd.Dir = c.dir
	cmd.Env = g.env(c)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// failure is the *gitError of a failed git process: exit code, the timeout
// or cancellation when that killed it, and the tail of stderr with the token
// redacted (args too).
func (g *gitRunner) failure(ctx context.Context, c gitCmd, err error, stderr string) error {
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	if ctx.Err() != nil {
		err = fmt.Errorf("%w (%v)", ctx.Err(), err)
	}
	errOut := strings.TrimSpace(g.redact(stderr))
	if len(errOut) > maxErrOutput {
		errOut = "..." + errOut[len(errOut)-maxErrOutput:]
	}
	return &gitError{args: redactArgs(g, c.args), exitCode: code, stderr: errOut, err: err}
}

func redactArgs(g *gitRunner, args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = g.redact(a)
	}
	return out
}

// hasCommit reports whether rev names a commit in the repository at dir.
func (g *gitRunner) hasCommit(ctx context.Context, dir, rev string) bool {
	_, err := g.run(ctx, gitCmd{dir: dir, args: []string{"cat-file", "-e", rev + "^{commit}"}})
	return err == nil
}

// fetchArgs is a quiet fetch of refspecs from remote without tags or
// FETCH_HEAD.
func fetchArgs(remote string, refspecs ...string) []string {
	return append([]string{"fetch", "--no-tags", "--quiet", "--no-write-fetch-head", "--", remote}, refspecs...)
}

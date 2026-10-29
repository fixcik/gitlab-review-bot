package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Exec implements core.Runner: one program, no shell, no network, no
// credentials. Docker mode uses a container with --network none and the same
// hardening as the review container; local mode runs the program in Dir with
// a scrubbed environment (no isolation beyond that, like the rest of local
// mode).
func (r *runner) Exec(ctx context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return nil, errors.New("claude: exec: empty command")
	}
	dir, err := checkDir(req.Dir)
	if err != nil {
		return nil, err
	}
	for _, kv := range req.Env {
		if !validExecEnv(kv) {
			return nil, fmt.Errorf("claude: exec: refused env entry %q", strings.SplitN(kv, "=", 2)[0])
		}
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	var name string
	if r.o.Mode == "docker" {
		name = containerName("exec")
		args := buildDockerArgs(r.o, execSpec(name, dir, r.user, req.Env, req.Command))
		cmd = exec.CommandContext(ctx, r.o.DockerBin, args...)
		cmd.Env = scrubEnv(r.environ())
	} else {
		bin, lerr := exec.LookPath(req.Command[0])
		if lerr != nil {
			return &core.ExecResult{ExitCode: execNotFound, Stderr: lerr.Error()}, nil
		}
		cmd = exec.CommandContext(ctx, bin, req.Command[1:]...)
		cmd.Dir = dir
		cmd.Env = append(append(scrubEnv(r.environ()), quietToolEnv...), req.Env...)
	}
	cmd.Stdin = nil
	start := time.Now()
	r.log.Info("exec started", "mode", r.o.Mode, "cmd", req.Command[0], "container", name)
	p := r.exec(ctx, cmd, name)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("exec %q aborted after %s: %w", req.Command[0], time.Since(start).Round(time.Second), ctxErr)
	}
	if r.o.Mode == "docker" && p.ExitCode == 125 {
		return nil, fmt.Errorf("claude: docker run failed: %s", describeFailure(p))
	}
	if p.Err != nil && p.ExitCode < 0 {
		return nil, fmt.Errorf("exec %q: %w", req.Command[0], p.Err)
	}
	r.log.Info("exec finished", "cmd", req.Command[0], "exit", p.ExitCode, "took", time.Since(start).Round(time.Millisecond))
	return &core.ExecResult{ExitCode: p.ExitCode, Stdout: string(p.Stdout), Stderr: string(p.Stderr)}, nil
}

// exec runs cmd, capturing output. In docker mode a cancelled context
// removes the container (killing the docker CLI alone would leave it running).
func (r *runner) exec(ctx context.Context, cmd *exec.Cmd, container string) processOutput {
	return r.execWith(ctx, cmd, container, &capBuffer{max: maxCapture})
}

// execWith is exec with a custom stdout sink; the finished process is
// reported to the Observer in ctx (stream.go).
func (r *runner) execWith(ctx context.Context, cmd *exec.Cmd, container string, stdout outputSink) (p processOutput) {
	defer func(start time.Time) { observeProcess(ctx, cmd, p, time.Since(start)) }(time.Now())
	stderr := &capBuffer{max: 1 << 20, keepTail: true}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 10 * time.Second
	if container != "" {
		cmd.Cancel = func() error {
			r.removeContainer(container)
			return cmd.Process.Kill()
		}
	} else {
		setProcessGroup(cmd)
	}
	err := cmd.Run()
	p = processOutput{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), Err: err}
	if err != nil {
		p.ExitCode = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			p.ExitCode = ee.ExitCode()
		}
	}
	if ctx.Err() != nil && container != "" {
		// Make sure it is gone even if the CLI exited on its own.
		r.removeContainer(container)
	}
	return p
}

func (r *runner) removeContainer(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.o.DockerBin, "rm", "-f", name)
	cmd.Env = scrubEnv(r.environ())
	if out, err := cmd.CombinedOutput(); err != nil {
		r.log.Warn("docker rm -f failed", "container", name, "err", err, "out", tail(strings.TrimSpace(string(out)), 300))
	} else {
		r.log.Info("container removed", "container", name)
	}
}

// capBuffer keeps at most max bytes (head or, with keepTail, tail).
type capBuffer struct {
	buf      bytes.Buffer
	max      int
	keepTail bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	n := len(p)
	c.buf.Write(p)
	if c.buf.Len() > c.max {
		if c.keepTail {
			b := c.buf.Bytes()
			keep := append([]byte(nil), b[len(b)-c.max:]...)
			c.buf.Reset()
			c.buf.Write(keep)
		} else {
			c.buf.Truncate(c.max)
		}
	}
	return n, nil
}

func (c *capBuffer) Bytes() []byte { return c.buf.Bytes() }

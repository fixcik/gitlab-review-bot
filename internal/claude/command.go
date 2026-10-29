package claude

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// dockerClaudeCmd is `docker run` of a review container named name.
func (r *runner) dockerClaudeCmd(ctx context.Context, req core.RunRequest, name, dir string, ro readOnlyDirs, mcp *mcpSetup, claudeArgs []string) *exec.Cmd {
	claudeArgs = append(claudeArgs, addDirArgs(ro, true)...) // variadic flag: keep last
	spec := reviewSpec(r.o, name, dir, r.user, r.authEnv, claudeArgs)
	spec.ROMounts = ro.mounts()
	if mcp != nil {
		spec.ROMounts = append(spec.ROMounts, *mcp.mount)
		spec.SetEnv = withNoProxy(spec.SetEnv, mcpHost(req.MCP))
	}
	telemetryEnv := withTelemetryDocker(ctx, r.o.Telemetry, &spec)
	cmd := exec.CommandContext(ctx, r.o.DockerBin, buildDockerArgs(r.o, spec)...)
	// The docker CLI resolves "-e NAME" from its own env.
	cmd.Env = append(localClaudeEnv(r.environ()), telemetryEnv...)
	return cmd
}

// localClaudeCmd is the claude CLI run directly in dir. The returned closer
// (nil when unused) releases the token pipe after the run.
func (r *runner) localClaudeCmd(ctx context.Context, req core.RunRequest, dir string, ro readOnlyDirs, mcp *mcpSetup, claudeArgs []string) (*exec.Cmd, func()) {
	claudeArgs = append(claudeArgs, addDirArgs(ro, false)...) // variadic flag: keep last
	cmd := exec.CommandContext(ctx, r.o.ClaudeBin, claudeArgs...)
	cmd.Dir = dir
	// Hand the Claude token to the CLI on an inherited file descriptor
	// instead of in the environment. Falls back to the plain env var if the
	// pipe cannot be set up. Either way only one credential (the first set)
	// reaches Claude: a second one is dropped.
	var closeFD func()
	env, fdEnv, closer, ferr := r.tokenFDEnv(localClaudeEnv(r.environ()))
	if ferr != nil {
		r.log.Warn("token-on-fd setup failed, passing token in env", "err", ferr)
		cmd.Env = singleCredentialEnv(localClaudeEnv(r.environ()))
	} else {
		cmd.Env = env
		cmd.ExtraFiles = fdEnv
		closeFD = closer
	}
	if mcp != nil {
		cmd.Env = withNoProxy(cmd.Env, mcpHost(req.MCP))
	}
	cmd.Env = withTelemetryLocal(ctx, r.o.Telemetry, cmd.Env)
	return cmd, closeFD
}

// osPipe is os.Pipe (a variable for fault-injection tests).
var osPipe = os.Pipe

// tokenFDEnv rewrites env so the Claude credential travels on an inherited
// file descriptor instead of an environment variable. It returns the rewritten
// environment (credential var removed, matching CLAUDE_CODE_*_FILE_DESCRIPTOR
// pointing at fd 3; every other credential and fd variable removed too, so
// a second credential never stays in Claude's environment), the ExtraFiles slice to attach (index 0 == fd 3), and a
// closer to release the pipe. If no known credential var is present it returns
// env unchanged with a nil ExtraFiles slice.
func (r *runner) tokenFDEnv(env []string) ([]string, []*os.File, func(), error) {
	name, value := firstCredential(env)
	fdVar := authFDEnvName[name]
	if name == "" || fdVar == "" {
		return env, nil, nil, nil
	}
	pr, pw, err := osPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	// Write the token (newline-terminated so the reader stops promptly) then
	// close the writer. A credential is far smaller than the pipe buffer, but
	// write from a goroutine so we never block if that ever changes.
	go func() {
		_, _ = pw.WriteString(value + "\n")
		_ = pw.Close()
	}()
	// ExtraFiles[0] becomes fd 3 in the child.
	out := append(withoutCredentials(env), fdVar+"=3")
	closer := func() { _ = pr.Close() }
	return out, []*os.File{pr}, closer, nil
}

// firstCredential is the first Claude credential (authEnvNames order) set in
// env, or "" when there is none.
func firstCredential(env []string) (name, value string) {
	m := envMap(env)
	for _, cred := range authEnvNames {
		if v := m[cred]; v != "" {
			return cred, v
		}
	}
	return "", ""
}

// singleCredentialEnv is env with only the first Claude credential left.
func singleCredentialEnv(env []string) []string {
	out := withoutCredentials(env)
	if name, value := firstCredential(env); name != "" {
		out = append(out, name+"="+value)
	}
	return out
}

// withoutCredentials drops every Claude credential and its
// CLAUDE_CODE_*_FILE_DESCRIPTOR variable from env.
func withoutCredentials(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(authEnvNames, k) || isAuthFDVar(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isAuthFDVar(name string) bool {
	for _, v := range authFDEnvName {
		if v == name {
			return true
		}
	}
	return false
}

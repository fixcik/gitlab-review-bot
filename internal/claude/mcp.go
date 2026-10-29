package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// The bot's MCP server (core.RunRequest.MCP, internal/mcpserver).
//
// The CLI gets the server through --mcp-config <file>: an "http" server with
// the run's bearer token in an Authorization header. The token is written
// only into that file (0600, in a fresh temp directory, removed after the
// run); it never appears on a command line (docker CLI or claude argv, which
// are visible in the process list and recorded in /runs) or in an
// environment variable. Docker mode mounts the directory read-only at
// containerMCPDir; local mode passes the host path. Nothing in the sandbox
// runs repository code (Claude has no Bash), and the token dies with the
// run.
//
// --strict-mcp-config stays: no other MCP server (user config, the
// repository's .mcp.json) is loaded.

// containerMCPDir is where the MCP config directory is mounted.
const containerMCPDir = "/etc/reviewbot-mcp"

// mcpConfigName is the config file name inside the directory.
const mcpConfigName = "mcp.json"

func mcpOn(req core.RunRequest) bool {
	return req.MCP != nil && !req.NoTools && req.MCP.URL != "" && req.MCP.Name != ""
}

// mcpConfigJSON is the --mcp-config payload for one run.
func mcpConfigJSON(m *core.MCPServer) ([]byte, error) {
	if m == nil || m.Name == "" || m.URL == "" || m.Token == "" {
		return nil, errors.New("claude: incomplete MCP server")
	}
	if strings.ContainsAny(m.Token, "\r\n") {
		return nil, errors.New("claude: invalid MCP token")
	}
	return json.Marshal(map[string]any{"mcpServers": map[string]any{
		m.Name: map[string]any{
			"type":    "http",
			"url":     m.URL,
			"headers": map[string]string{"Authorization": "Bearer " + m.Token},
			// Claude Code defers MCP tool definitions (loaded on demand via
			// tool search); the review run restricts its built-in tools, so
			// the definitions are loaded from the first turn instead.
			"alwaysLoad": true,
		},
	}})
}

// mcpSetup is the prepared MCP config of one run.
type mcpSetup struct {
	cliPath string   // path the CLI reads
	mount   *roMount // docker mode: read-only mount of the directory
	cleanup func()
}

// prepareMCP writes the MCP config for a run in dir (the clone). Docker
// mode puts the directory next to the clone (same host path inside and
// outside the bot's container, like the clone) so the host daemon can
// mount it; local mode uses the system temp directory.
func (r *runner) prepareMCP(m *core.MCPServer, dir string) (*mcpSetup, error) {
	payload, err := mcpConfigJSON(m)
	if err != nil {
		return nil, err
	}
	docker := r.o.Mode == "docker"
	parent := ""
	if docker {
		parent = filepath.Dir(dir)
	}
	tmp, err := os.MkdirTemp(parent, ".reviewbot-mcp-")
	if err != nil {
		return nil, fmt.Errorf("claude: mcp config dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	// The sandbox user reads the file: owner-only when it is this process's
	// uid (the default); otherwise it must be world-readable (the directory
	// name is random and lives only for the run).
	dirMode, fileMode := os.FileMode(0o700), os.FileMode(0o600)
	if docker && !r.sameUser() {
		dirMode, fileMode = 0o755, 0o644
	}
	if err := os.Chmod(tmp, dirMode); err != nil {
		cleanup()
		return nil, fmt.Errorf("claude: mcp config dir: %w", err)
	}
	path := filepath.Join(tmp, mcpConfigName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fileMode)
	if err == nil {
		_, err = f.Write(payload)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = os.Chmod(path, fileMode) // umask
	}
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("claude: write mcp config: %w", err)
	}
	if !docker {
		return &mcpSetup{cliPath: path, cleanup: cleanup}, nil
	}
	if strings.ContainsAny(tmp, ":,\n") {
		cleanup()
		return nil, errors.New("claude: mcp config dir path cannot be mounted")
	}
	return &mcpSetup{cliPath: containerMCPDir + "/" + mcpConfigName, mount: &roMount{Host: tmp, Container: containerMCPDir}, cleanup: cleanup}, nil
}

// sameUser reports whether the sandbox user is this process's uid.
func (r *runner) sameUser() bool {
	uid, _, _ := strings.Cut(r.user, ":")
	return uid == fmt.Sprint(os.Getuid())
}

// withMCPConfigArg inserts "--mcp-config <path>" right after
// --strict-mcp-config. The flag is variadic in the CLI, so it must be
// followed by another flag, never by a positional value.
func withMCPConfigArg(args []string, path string) []string {
	out := make([]string, 0, len(args)+2)
	inserted := false
	for _, a := range args {
		out = append(out, a)
		if a == "--strict-mcp-config" && !inserted {
			out = append(out, "--mcp-config", path)
			inserted = true
		}
	}
	if !inserted {
		out = append([]string{"--mcp-config", path}, out...)
	}
	return out
}

// mcpHost is the host of the MCP URL ("" when unparsable).
func mcpHost(m *core.MCPServer) string {
	if m == nil {
		return ""
	}
	u, err := url.Parse(m.URL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// withNoProxy adds host to NO_PROXY / no_proxy in env (creating them when
// absent), so the CLI talks to the MCP endpoint directly instead of through
// the egress proxy (which only tunnels CONNECT to allowlisted hosts).
func withNoProxy(env []string, host string) []string {
	if host == "" {
		return env
	}
	out := make([]string, 0, len(env)+2)
	seen := map[string]bool{}
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		if name == "NO_PROXY" || name == "no_proxy" {
			seen[name] = true
			if !containsHost(val, host) {
				if val != "" {
					val += ","
				}
				val += host
			}
			kv = name + "=" + val
		}
		out = append(out, kv)
	}
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		if !seen[name] {
			out = append(out, name+"=localhost,127.0.0.1,"+host)
		}
	}
	return out
}

func containsHost(list, host string) bool {
	for _, h := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(h), host) {
			return true
		}
	}
	return false
}

package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const mcpToken = "f00dfeedf00dfeedf00dfeedf00dfeedf00dfeedf00dfeedf00dfeedf00dfeed"

func mcpReq(dir string) core.RunRequest {
	return core.RunRequest{Dir: dir, Prompt: "review", JSONSchema: `{"type":"object"}`,
		MCP: &core.MCPServer{Name: "reviewbot", URL: "http://egress:8091/mcp", Token: mcpToken, Tools: []string{"diff_lines", "submit_finding"}}}
}

func TestMCPAllowedToolsAndFlags(t *testing.T) {
	args := buildClaudeArgs(mcpReq("/w"))
	i := indexOf(args, "--allowedTools")
	j := indexOf(args, "--disallowedTools")
	allowed := args[i+1 : j]
	if !slices.Contains(allowed, "mcp__reviewbot__diff_lines") || !slices.Contains(allowed, "mcp__reviewbot__submit_finding") ||
		!slices.Contains(allowed, "Read(//work/**)") || slices.Contains(allowed, "Read") {
		t.Errorf("allowed %v", allowed)
	}
	if !slices.Contains(args, "--strict-mcp-config") || !hasPair(args, "--permission-mode", "dontAsk") {
		t.Errorf("hardening flags lost: %v", args)
	}
	if strings.Contains(strings.Join(args, " "), mcpToken) {
		t.Error("token in claude args")
	}
	// Text-only runs never get MCP tools.
	req := mcpReq("/w")
	req.NoTools = true
	if strings.Contains(strings.Join(buildClaudeArgs(req), " "), "mcp__") {
		t.Error("NoTools run got MCP tools")
	}
	// Without MCP the args are unchanged.
	plain := mcpReq("/w")
	plain.MCP = nil
	if pa := strings.Join(buildClaudeArgs(plain), " "); strings.Contains(pa, "mcp__") || strings.Contains(pa, "--mcp-config") {
		t.Errorf("unexpected mcp args: %s", pa)
	}

	withCfg := withMCPConfigArg(args, "/etc/reviewbot-mcp/mcp.json")
	k := indexOf(withCfg, "--strict-mcp-config")
	if withCfg[k+1] != "--mcp-config" || withCfg[k+2] != "/etc/reviewbot-mcp/mcp.json" || !strings.HasPrefix(withCfg[k+3], "--") {
		t.Errorf("--mcp-config placement: %v", withCfg[k:k+4])
	}
	if len(withCfg) != len(args)+2 {
		t.Errorf("args changed beyond the insert")
	}
}

func TestMCPConfigJSON(t *testing.T) {
	b, err := mcpConfigJSON(mcpReq("/w").MCP)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	s := cfg.MCPServers["reviewbot"]
	if s.Type != "http" || s.URL != "http://egress:8091/mcp" || s.Headers["Authorization"] != "Bearer "+mcpToken || len(cfg.MCPServers) != 1 {
		t.Errorf("config %s", b)
	}
	for _, m := range []*core.MCPServer{nil, {Name: "x", URL: "http://a"}, {Name: "x", URL: "http://a", Token: "a\nb"}} {
		if _, err := mcpConfigJSON(m); err == nil {
			t.Errorf("accepted %v", m)
		}
	}
}

func TestMCPServerStringHidesToken(t *testing.T) {
	m := mcpReq("/w").MCP
	for _, s := range []string{m.String(), m.GoString(), strings.Join(m.AllowedTools(), ",")} {
		if strings.Contains(s, mcpToken) {
			t.Errorf("token printed: %s", s)
		}
	}
}

func TestWithNoProxy(t *testing.T) {
	env := withNoProxy([]string{"A=1", "NO_PROXY=localhost,127.0.0.1", "no_proxy=localhost"}, "egress")
	if !slices.Contains(env, "NO_PROXY=localhost,127.0.0.1,egress") || !slices.Contains(env, "no_proxy=localhost,egress") || !slices.Contains(env, "A=1") {
		t.Errorf("env %v", env)
	}
	if again := withNoProxy(env, "EGRESS"); !slices.Equal(again, env) {
		t.Errorf("host added twice: %v", again)
	}
	env = withNoProxy([]string{"PATH=/bin"}, "127.0.0.1")
	if !slices.Contains(env, "NO_PROXY=localhost,127.0.0.1,127.0.0.1") || !slices.Contains(env, "no_proxy=localhost,127.0.0.1,127.0.0.1") {
		t.Errorf("created: %v", env)
	}
	if got := withNoProxy([]string{"X=1"}, ""); !slices.Equal(got, []string{"X=1"}) {
		t.Errorf("empty host: %v", got)
	}
}

type argvObserver struct {
	mu    sync.Mutex
	procs []ProcessInfo
}

func (o *argvObserver) Event([]byte) {}
func (o *argvObserver) Process(p ProcessInfo) {
	o.mu.Lock()
	o.procs = append(o.procs, p)
	o.mu.Unlock()
}

func TestRunDockerMCP(t *testing.T) {
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	// The fake docker prints its args and the content of the host side of
	// the MCP mount, and checks the file is readable only by its owner.
	docker := writeScript(t, bin, "docker", `
echo "ARGS: $*" >> "`+logf+`"
prev=""
for a in "$@"; do
  if [ "$prev" = "-v" ]; then
    case "$a" in *:/etc/reviewbot-mcp:ro) host="${a%%:/etc/reviewbot-mcp:ro}"; echo "CFG: $(cat "$host/mcp.json")" >> "`+logf+`"; echo "MODE: $(stat -c %a "$host/mcp.json" 2>/dev/null || stat -f %Lp "$host/mcp.json") $(stat -c %a "$host" 2>/dev/null || stat -f %Lp "$host")" >> "`+logf+`"; echo "HOSTDIR: $host" >> "`+logf+`";; esac
  fi
  prev="$a"
done
cat >/dev/null
echo '`+fakeResult+`'
`)
	parent := t.TempDir()
	work := filepath.Join(parent, "clone")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	o := dockerOpts()
	o.DockerBin = docker
	r, err := newRunner(o, func() []string { return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"} })
	if err != nil {
		t.Fatal(err)
	}
	obs := &argvObserver{}
	if _, err := r.Run(WithObserver(context.Background(), obs), mcpReq(work)); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(logf)
	s := string(log)
	argsLine := strings.SplitN(s, "\n", 2)[0]
	if strings.Contains(argsLine, mcpToken) {
		t.Fatalf("token on the docker command line:\n%s", argsLine)
	}
	for _, want := range []string{
		"--mcp-config /etc/reviewbot-mcp/mcp.json --permission-mode",
		"-e NO_PROXY=localhost,127.0.0.1,egress", "-e no_proxy=localhost,127.0.0.1,egress",
		"mcp__reviewbot__submit_finding", "--strict-mcp-config",
	} {
		if !strings.Contains(argsLine, want) {
			t.Errorf("args lack %q:\n%s", want, argsLine)
		}
	}
	if !strings.Contains(s, `CFG: {"mcpServers":{"reviewbot":{"alwaysLoad":true,"headers":{"Authorization":"Bearer `+mcpToken+`"},"type":"http","url":"http://egress:8091/mcp"}}}`) {
		t.Errorf("config file:\n%s", s)
	}
	if !strings.Contains(s, "MODE: 600 700") {
		t.Errorf("permissions:\n%s", s)
	}
	var hostDir string
	for _, l := range strings.Split(s, "\n") {
		if h, ok := strings.CutPrefix(l, "HOSTDIR: "); ok {
			hostDir = h
		}
	}
	if filepath.Dir(hostDir) != parent || strings.HasPrefix(hostDir, work) {
		t.Errorf("config dir %q must be next to the clone, not inside it", hostDir)
	}
	if _, err := os.Stat(hostDir); !os.IsNotExist(err) {
		t.Errorf("config dir left behind: %v", err)
	}
	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.procs) != 1 {
		t.Fatalf("procs %d", len(obs.procs))
	}
	argv := strings.Join(obs.procs[0].Argv, " ")
	if strings.Contains(argv, mcpToken) || !strings.Contains(argv, "--mcp-config <mcp config:") {
		t.Errorf("recorded argv: %s", argv)
	}
}

func TestRunLocalMCP(t *testing.T) {
	bin := t.TempDir()
	claudeBin := writeScript(t, bin, "claude", `
prev=""
for a in "$@"; do
  if [ "$prev" = "--mcp-config" ]; then cat "$a" > cfg.txt; echo "$a" > cfgpath.txt; fi
  prev="$a"
done
echo "$*" > args.txt
echo "NP=$NO_PROXY np=$no_proxy" > env.txt
cat >/dev/null
echo '`+fakeResult+`'
`)
	work := t.TempDir()
	r, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k", "NO_PROXY=corp.example"}
	})
	if err != nil {
		t.Fatal(err)
	}
	req := mcpReq(work)
	req.MCP.URL = "http://127.0.0.1:8091/mcp"
	if res, err := r.Run(context.Background(), req); err != nil || res.MCPDropped {
		t.Fatal(err, res)
	}
	// A config that cannot be written: the run goes on without the tools
	// and says so (a verification problem for the reviewer).
	bad := mcpReq(work)
	bad.MCP.URL, bad.MCP.Token = "http://127.0.0.1:8091/mcp", "line\nbreak"
	if res, err := r.Run(context.Background(), bad); err != nil || !res.MCPDropped {
		t.Fatalf("dropped: %v %+v", err, res)
	}
	req = mcpReq(work)
	req.MCP.URL = "http://127.0.0.1:8091/mcp"
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	read := func(n string) string {
		b, _ := os.ReadFile(filepath.Join(work, n))
		return strings.TrimSpace(string(b))
	}
	if !strings.Contains(read("cfg.txt"), "Bearer "+mcpToken) || strings.Contains(read("args.txt"), mcpToken) {
		t.Errorf("cfg=%q args=%q", read("cfg.txt"), read("args.txt"))
	}
	if read("env.txt") != "NP=corp.example,127.0.0.1 np=localhost,127.0.0.1,127.0.0.1" {
		t.Errorf("env %q", read("env.txt"))
	}
	if _, err := os.Stat(read("cfgpath.txt")); !os.IsNotExist(err) {
		t.Errorf("config file left behind: %v", err)
	}
}

// The init event's status of the request's MCP server reaches the result
// (the reviewer falls back to a run without MCP when it is not connected).
func TestRunMCPStatus(t *testing.T) {
	for _, tc := range []struct{ init, want string }{
		{`{"type":"system","subtype":"init","mcp_servers":[{"name":"reviewbot","status":"failed"}]}`, "failed"},
		{`{"type":"system","subtype":"init","mcp_servers":[{"name":"reviewbot","status":"connected"}]}`, "connected"},
		{`{"type":"system","subtype":"init"}`, ""},
	} {
		bin := t.TempDir()
		claudeBin := writeScript(t, bin, "claude", `
cat >/dev/null
echo '`+tc.init+`'
echo '`+fakeResult+`'
`)
		r, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"}
		})
		if err != nil {
			t.Fatal(err)
		}
		req := mcpReq(t.TempDir())
		req.MCP.URL = "http://127.0.0.1:8091/mcp"
		res, err := r.Run(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.MCPStatus != tc.want {
			t.Errorf("%s: status %q", tc.init, res.MCPStatus)
		}
	}
}

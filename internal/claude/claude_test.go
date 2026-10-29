package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func dockerOpts() Options {
	return Options{
		Mode: "docker", Image: "reviewbot-sandbox:latest", Network: "reviewbot-sandbox",
		EgressProxy: "http://egress:3128", CPUs: "2", Memory: "4g",
	}
}

func indexOf(args []string, v string) int { return slices.Index(args, v) }

// hasPair reports whether flag is immediately followed by value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func TestBuildClaudeArgs(t *testing.T) {
	args := buildClaudeArgs(core.RunRequest{
		SystemPrompt: "be strict",
		JSONSchema:   `{"type":"object"}`,
		Model:        "opus",
	})
	for _, want := range [][2]string{
		{"--output-format", "stream-json"},
		{"--append-system-prompt", "be strict"},
		{"--json-schema", `{"type":"object"}`},
		{"--model", "opus"},
		{"--allowedTools", "Read(//work/**)"},
		{"--disallowedTools", "Bash"},
		{"--setting-sources", "user"},
		// Headless hardening: auto-deny outside the allowlist, never prompt,
		// restrict the built-in toolset, no skills/commands.
		{"--permission-mode", "dontAsk"},
		{"--permission-prompts", "none"},
		{"--tools", "Read,Grep,Glob"},
	} {
		if !hasPair(args, want[0], want[1]) {
			t.Errorf("missing %s %s in %q", want[0], want[1], args)
		}
	}
	if indexOf(args, "--disable-slash-commands") < 0 {
		t.Error("--disable-slash-commands missing")
	}
	if args[0] != "-p" || indexOf(args, "--no-session-persistence") < 0 {
		t.Errorf("args = %q", args)
	}
	if indexOf(args, "--bare") >= 0 {
		t.Error("--bare must never be used")
	}
	// No shell and nothing that writes in the built-in toolset.
	if ti := indexOf(args, "--tools"); strings.Contains(args[ti+1], "Bash") || strings.Contains(args[ti+1], "Edit") || strings.Contains(args[ti+1], "Write") {
		t.Errorf("--tools must not include Bash/Edit/Write: %q", args[ti+1])
	}
	// bypassPermissions must never be used.
	if pm := indexOf(args, "--permission-mode"); args[pm+1] == "bypassPermissions" {
		t.Error("must not bypass permissions")
	}
	// Allowed: reading the clone only (no other directory in this run);
	// never a bare Read/Grep/Glob, which would allow any path.
	i, j := indexOf(args, "--allowedTools"), indexOf(args, "--disallowedTools")
	if strings.Join(args[i+1:j], "|") != "Read(//work/**)" {
		t.Errorf("allowed tools = %q", args[i+1:j])
	}
	// No Bash permission rule anywhere, no Claude Code sandbox settings.
	for _, a := range args {
		if strings.HasPrefix(a, "Bash(") || strings.Contains(a, `"sandbox"`) || a == "--settings" {
			t.Errorf("unexpected argument %q", a)
		}
	}
	if strings.Join(args[j+1:j+4], "|") != "Bash|WebFetch|WebSearch" {
		t.Errorf("disallowed = %q", args[j:])
	}

	minimal := buildClaudeArgs(core.RunRequest{NoTools: true})
	for _, f := range []string{"--allowedTools", "--append-system-prompt", "--json-schema", "--model"} {
		if indexOf(minimal, f) >= 0 {
			t.Errorf("unexpected %s in %q", f, minimal)
		}
	}
	// The permission hardening is unconditional.
	for _, f := range []string{"--permission-mode", "--permission-prompts", "--tools", "--disable-slash-commands", "--disallowedTools"} {
		if indexOf(minimal, f) < 0 {
			t.Errorf("minimal args missing %s", f)
		}
	}
}

func TestBaseEnvForcesSafeGit(t *testing.T) {
	env := strings.Join(baseEnv(), "\n")
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0=/work",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("baseEnv missing %q:\n%s", want, env)
		}
	}
	// safe.directory must be scoped, never "*".
	if strings.Contains(env, "GIT_CONFIG_VALUE_0=*") {
		t.Error("safe.directory must not be *")
	}
	// Every forced driver-defanging key must be present.
	for _, k := range []string{"core.fsmonitor", "core.hooksPath", "core.pager", "diff.external"} {
		if !strings.Contains(env, k) {
			t.Errorf("baseEnv missing git key %q", k)
		}
	}
	// GIT_CONFIG_COUNT must match the number of key/value pairs.
	var count int
	for _, kv := range baseEnv() {
		if n, _, ok := strings.Cut(kv, "="); ok && n == "GIT_CONFIG_COUNT" {
			_, _ = fmt.Sscanf(kv, "GIT_CONFIG_COUNT=%d", &count)
		}
	}
	keys := 0
	for _, kv := range baseEnv() {
		if strings.HasPrefix(kv, "GIT_CONFIG_KEY_") {
			keys++
		}
	}
	if count != keys || count == 0 {
		t.Errorf("GIT_CONFIG_COUNT=%d but %d keys", count, keys)
	}
}

func TestBuildDockerArgsReview(t *testing.T) {
	o := dockerOpts()
	spec := reviewSpec(o, "reviewbot-run-x", "/data/work/abc", "1000:1001",
		[]string{"CLAUDE_CODE_OAUTH_TOKEN"}, []string{"-p", "--output-format", "json"})
	args := buildDockerArgs(o, spec)

	if args[0] != "run" || indexOf(args, "--rm") < 0 || indexOf(args, "-i") < 0 || indexOf(args, "--read-only") < 0 {
		t.Fatalf("args = %q", args)
	}
	for _, want := range [][2]string{
		{"--network", "reviewbot-sandbox"},
		{"--user", "1000:1001"},
		{"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"},
		{"--pids-limit", "512"},
		{"--cpus", "2"},
		{"--memory", "4g"},
		{"--name", "reviewbot-run-x"},
		{"-v", "/data/work/abc:/work:ro"}, // a Claude run only reads the checkout
		{"-w", "/work"},
		{"-e", "HOME=/home/claude"},
		{"-e", "HTTPS_PROXY=http://egress:3128"},
		{"-e", "https_proxy=http://egress:3128"},
		{"-e", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"},
		{"-e", "DISABLE_AUTOUPDATER=1"},
		{"-e", "CLAUDE_CODE_OAUTH_TOKEN"}, // name only: value comes from docker CLI env
	} {
		if !hasPair(args, want[0], want[1]) {
			t.Errorf("missing %s %s", want[0], want[1])
		}
	}
	if !slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, "/tmp:rw,exec") }) ||
		!slices.ContainsFunc(args, func(s string) bool { return strings.HasPrefix(s, "/home/claude:rw") }) {
		t.Error("tmpfs mounts missing")
	}
	img := indexOf(args, "reviewbot-sandbox:latest")
	if img < 0 || args[img+1] != "claude" || args[img+2] != "-p" {
		t.Errorf("image/cmd wrong: %q", args[img:])
	}
	for _, a := range args {
		if strings.HasPrefix(a, "CLAUDE_CODE_OAUTH_TOKEN=") || strings.HasPrefix(a, "ANTHROPIC_API_KEY=") {
			t.Errorf("secret value on command line: %s", a)
		}
	}
}

func TestScrubEnv(t *testing.T) {
	in := []string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=x", "ANTHROPIC_API_KEY=y", "GITLAB_TOKEN=z", "GITLAB_WEBHOOK_SECRET=s", "HOME=/h"}
	got := scrubEnv(in)
	if strings.Join(got, ",") != "PATH=/bin,HOME=/h" {
		t.Errorf("scrubEnv = %q", got)
	}
	lc := strings.Join(localClaudeEnv(in), ",")
	if strings.Contains(lc, "GITLAB") || !strings.Contains(lc, "CLAUDE_CODE_OAUTH_TOKEN=x") {
		t.Errorf("localClaudeEnv = %s", lc)
	}
}

func TestTokenFDEnv(t *testing.T) {
	r := &runner{}
	// OAuth token present: moved to an fd, removed from env.
	env, files, closer, err := r.tokenFDEnv([]string{"PATH=/bin", "CLAUDE_CODE_OAUTH_TOKEN=secret-xyz", "HOME=/h"})
	if err != nil {
		t.Fatal(err)
	}
	if closer != nil {
		defer closer()
	}
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "secret-xyz") || strings.Contains(joined, "CLAUDE_CODE_OAUTH_TOKEN=") {
		t.Errorf("token still in env: %v", env)
	}
	if !strings.Contains(joined, "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3") {
		t.Errorf("fd var missing: %v", env)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 extra file, got %d", len(files))
	}
	got, _ := io.ReadAll(files[0])
	if strings.TrimSpace(string(got)) != "secret-xyz" {
		t.Errorf("fd content = %q", got)
	}

	// API key variant.
	env2, files2, closer2, err := r.tokenFDEnv([]string{"ANTHROPIC_API_KEY=k", "PATH=/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if closer2 != nil {
		defer closer2()
	}
	if len(files2) != 1 || !slices.ContainsFunc(env2, func(s string) bool { return s == "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR=3" }) {
		t.Errorf("api key fd env = %v", env2)
	}
	for _, kv := range env2 {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Error("api key still in env")
		}
	}

	// No credential: unchanged, no files.
	env3, files3, closer3, err := r.tokenFDEnv([]string{"PATH=/bin"})
	if err != nil || files3 != nil || closer3 != nil || len(env3) != 1 {
		t.Errorf("no-cred case: env=%v files=%v err=%v", env3, files3, err)
	}
}

func TestNewRunnerRequiresAuth(t *testing.T) {
	o := Options{Mode: "local", ClaudeBin: "/bin/sh"}
	if _, err := newRunner(o, func() []string { return []string{"PATH=/bin"} }); err == nil {
		t.Fatal("expected error without credentials")
	}
	r, err := newRunner(o, func() []string { return []string{"ANTHROPIC_API_KEY=k"} })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.authEnv, ",") != "ANTHROPIC_API_KEY" {
		t.Errorf("authEnv = %v", r.authEnv)
	}
	if _, err := newRunner(Options{Mode: "docker", DockerBin: "/bin/sh"}, func() []string { return []string{"ANTHROPIC_API_KEY=k"} }); err == nil {
		t.Fatal("docker mode without image/network must fail")
	}
	if _, err := newRunner(Options{Mode: "weird"}, func() []string { return []string{"ANTHROPIC_API_KEY=k"} }); err == nil {
		t.Fatal("unknown mode must fail")
	}
}

// ---------------------------------------------------------------------------
// Output parsing
// ---------------------------------------------------------------------------

func TestParseOutput(t *testing.T) {
	cases := []struct {
		name     string
		p        processOutput
		wantJSON bool
		want     string
		wantErr  string
		rate     bool
		resetAt  int64
	}{
		{
			name:     "structured output",
			p:        processOutput{Stdout: []byte(`{"type":"result","subtype":"success","is_error":false,"result":"done","structured_output":{"summary":"ok", "findings":[]},"total_cost_usd":0.12,"num_turns":5,"duration_ms":1234}`)},
			wantJSON: true,
			want:     `{"findings":[],"summary":"ok"}`,
		},
		{
			name: "plain text",
			p:    processOutput{Stdout: []byte(`{"type":"result","subtype":"success","is_error":false,"result":"Hello there","total_cost_usd":0.01,"num_turns":1,"duration_ms":10}`)},
			want: "Hello there",
		},
		{
			name:     "fenced json in result",
			p:        processOutput{Stdout: []byte(`{"type":"result","subtype":"success","result":"Here you go:\n` + "```json" + `\n{\"reply\": \"hi\", \"resolve\": true}\n` + "```" + `\n"}`)},
			wantJSON: true,
			want:     `{"reply":"hi","resolve":true}`,
		},
		{
			name:     "bare json in result",
			p:        processOutput{Stdout: []byte(`{"type":"result","subtype":"success","result":"Result: {\"reply\": \"x {y}\"} trailing"}`)},
			wantJSON: true,
			want:     `{"reply":"x {y}"}`,
		},
		{
			name:     "json requested but missing",
			p:        processOutput{Stdout: []byte(`{"type":"result","subtype":"success","result":"no json here"}`)},
			wantJSON: true,
			wantErr:  "no JSON object",
		},
		{
			name:    "is_error",
			p:       processOutput{Stdout: []byte(`{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key"}`), ExitCode: 1, Err: errors.New("exit 1")},
			wantErr: "Invalid API key",
		},
		{
			name:    "max turns",
			p:       processOutput{Stdout: []byte(`{"type":"result","subtype":"error_max_turns","is_error":false,"num_turns":50}`)},
			wantErr: "error_max_turns",
		},
		{
			name:    "usage limit with reset",
			p:       processOutput{Stdout: []byte(`{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached|1735689600"}`), ExitCode: 1},
			rate:    true,
			resetAt: 1735689600,
		},
		{
			name: "rate limit plain stderr",
			p:    processOutput{Stderr: []byte("API Error: 429 Too Many Requests"), ExitCode: 1, Err: errors.New("exit 1")},
			rate: true,
		},
		{
			name: "overloaded",
			p:    processOutput{Stdout: []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}"}`), ExitCode: 1},
			rate: true,
		},
		{
			name:    "non-json stdout, non-zero exit",
			p:       processOutput{Stdout: []byte("boom"), Stderr: []byte("line1\nfatal: something broke"), ExitCode: 2, Err: errors.New("exit status 2")},
			wantErr: "something broke",
		},
		{
			name:    "non-json stdout, zero exit",
			p:       processOutput{Stdout: []byte("hello")},
			wantErr: "unexpected output",
		},
		{
			name:    "empty output",
			p:       processOutput{ExitCode: 1, Err: errors.New("exit status 1")},
			wantErr: "exit status 1",
		},
		{
			name: "stream-json lines",
			p: processOutput{Stdout: []byte(`{"type":"system","subtype":"init"}
{"type":"assistant","message":{}}
{"type":"result","subtype":"success","result":"final"}`)},
			want: "final",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := parseOutput(tc.p, tc.wantJSON)
			if tc.rate {
				var rl *core.ErrClaudeRateLimited
				if !errors.As(err, &rl) {
					t.Fatalf("want rate limit, got %v", err)
				}
				if tc.resetAt != 0 && rl.ResetAt.Unix() != tc.resetAt {
					t.Fatalf("ResetAt = %v", rl.ResetAt)
				}
				return
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				var rl *core.ErrClaudeRateLimited
				if errors.As(err, &rl) {
					t.Fatalf("unexpected rate limit: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Output != tc.want {
				t.Fatalf("Output = %q, want %q", res.Output, tc.want)
			}
		})
	}
}

func TestParseOutputMetrics(t *testing.T) {
	res, err := parseOutput(processOutput{Stdout: []byte(`{"type":"result","subtype":"success","result":"x","total_cost_usd":0.12,"num_turns":5,"duration_ms":1234}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.CostUSD != 0.12 || res.NumTurns != 5 || res.Duration != 1234*time.Millisecond {
		t.Fatalf("res = %+v", res)
	}
}

func TestDetectRateLimit(t *testing.T) {
	for _, s := range []string{
		"Claude AI usage limit reached|1735689600",
		"You've hit your usage limit",
		"rate_limit_error",
		"Rate limit exceeded",
		"HTTP 429",
		"Overloaded",
	} {
		if detectRateLimit(s) == nil {
			t.Errorf("not detected: %q", s)
		}
	}
	for _, s := range []string{"all good", "found 4290 issues", "Invalid API key"} {
		if detectRateLimit(s) != nil {
			t.Errorf("false positive: %q", s)
		}
	}
	if rl := detectRateLimit("Claude AI usage limit reached|1735689600000"); rl == nil || rl.ResetAt.Unix() != 1735689600 {
		t.Errorf("ms timestamp: %+v", rl)
	}
	if rl := detectRateLimit("usage limit reached"); rl == nil || !rl.ResetAt.IsZero() {
		t.Errorf("no timestamp: %+v", rl)
	}
}

// ---------------------------------------------------------------------------
// End-to-end with fake binaries
// ---------------------------------------------------------------------------

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

const fakeResult = `{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"summary":"ok"},"total_cost_usd":0.5,"num_turns":3,"duration_ms":100}`

func TestRunDockerFake(t *testing.T) {
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	docker := writeScript(t, bin, "docker", `
echo "ARGS: $*" >> "`+logf+`"
echo "TOKEN_IN_ENV=$CLAUDE_CODE_OAUTH_TOKEN GL=$GITLAB_TOKEN" >> "`+logf+`"
echo "STDIN: $(cat)" >> "`+logf+`"
echo '`+fakeResult+`'
`)
	work := t.TempDir()
	o := dockerOpts()
	o.DockerBin = docker
	r, err := newRunner(o, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "CLAUDE_CODE_OAUTH_TOKEN=sk-oauth-secret", "GITLAB_TOKEN=glpat-x"}
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "review please", JSONSchema: `{"type":"object"}`})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != `{"summary":"ok"}` || res.CostUSD != 0.5 {
		t.Fatalf("res = %+v", res)
	}
	log, _ := os.ReadFile(logf)
	s := string(log)
	argsLine := strings.SplitN(s, "\n", 2)[0]
	if !strings.Contains(argsLine, "-e CLAUDE_CODE_OAUTH_TOKEN ") || strings.Contains(argsLine, "sk-oauth-secret") {
		t.Errorf("token handling wrong:\n%s", s)
	}
	if !strings.Contains(s, "TOKEN_IN_ENV=sk-oauth-secret GL=\n") {
		t.Errorf("docker CLI env wrong:\n%s", s)
	}
	if !strings.Contains(s, "STDIN: review please") {
		t.Errorf("prompt not on stdin:\n%s", s)
	}
	if !strings.Contains(s, "-v "+work+":/work:ro") {
		t.Errorf("mount missing:\n%s", s)
	}
	// No Bash anywhere and no Claude Code sandbox settings (no bubblewrap
	// needed in the image).
	if strings.Contains(argsLine, "Bash(") || strings.Contains(argsLine, "--settings") || strings.Contains(argsLine, "--tools Bash") {
		t.Errorf("Bash or sandbox settings in a review run:\n%s", argsLine)
	}
}

func TestRunDockerCancelRemovesContainer(t *testing.T) {
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	docker := writeScript(t, bin, "docker", `
if [ "$1" = "rm" ]; then echo "RM: $*" >> "`+logf+`"; exit 0; fi
cat >/dev/null
exec sleep 30
`)
	o := dockerOpts()
	o.DockerBin = docker
	r, err := newRunner(o, func() []string { return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"} })
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = r.Run(context.Background(), core.RunRequest{Dir: t.TempDir(), Prompt: "x", Timeout: 300 * time.Millisecond})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatal("cancel took too long")
	}
	log, _ := os.ReadFile(logf)
	if !strings.Contains(string(log), "RM: rm -f reviewbot-run-") {
		t.Fatalf("container not removed: %q", log)
	}
}

func TestLocalMode(t *testing.T) {
	bin := t.TempDir()
	claudeBin := writeScript(t, bin, "claude", `
pwd > "$PWD/pwd.txt"
echo "$*" > args.txt
echo "GL=$GITLAB_TOKEN" > env.txt
cat > stdin.txt
echo '`+fakeResult+`'
`)
	work := t.TempDir()
	r, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k", "GITLAB_TOKEN=gl"}
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "hi", Model: "sonnet", JSONSchema: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != `{"summary":"ok"}` {
		t.Fatalf("Output = %q", res.Output)
	}
	read := func(n string) string {
		b, _ := os.ReadFile(filepath.Join(work, n))
		return strings.TrimSpace(string(b))
	}
	if !strings.Contains(read("args.txt"), "--model sonnet") || read("stdin.txt") != "hi" || read("env.txt") != "GL=" {
		t.Errorf("args=%q stdin=%q env=%q", read("args.txt"), read("stdin.txt"), read("env.txt"))
	}

}

func TestDockerIntegration(t *testing.T) {
	if os.Getenv("REVIEWBOT_DOCKER_TESTS") == "" {
		t.Skip("set REVIEWBOT_DOCKER_TESTS=1 to run against a real docker daemon")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker unavailable")
	}
	o := dockerOpts()
	o.Image = "alpine:3.20"
	r, err := newRunner(o, func() []string { return append(os.Environ(), "ANTHROPIC_API_KEY=dummy") })
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Exec(context.Background(), core.ExecRequest{Dir: t.TempDir(), Command: []string{"id"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("exec: %+v %v", res, err)
	}
}

// ---------------------------------------------------------------------------
// Exec (deterministic checkers: no network, no credentials)
// ---------------------------------------------------------------------------

func TestBuildDockerArgsExec(t *testing.T) {
	o := dockerOpts()
	args := buildDockerArgs(o, execSpec("reviewbot-exec-x", "/w", "1000:1000", []string{"REVIEWBOT_BASE_SHA=abc"},
		[]string{"oasdiff", "breaking", "--fail-on", "ERR", "old/a.yaml", "new/a.yaml"}))
	if !hasPair(args, "--network", "none") {
		t.Errorf("exec must run with --network none: %q", args)
	}
	if hasPair(args, "--network", o.Network) {
		t.Error("exec must not join the sandbox network")
	}
	for _, want := range [][2]string{
		{"--user", "1000:1000"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"},
		{"--pids-limit", "512"}, {"--cpus", "2"}, {"--memory", "4g"}, {"-v", "/w:/work"}, {"-w", "/work"},
		{"-e", "REVIEWBOT_BASE_SHA=abc"}, {"-e", "NO_COLOR=1"}, {"-e", "GIT_CONFIG_NOSYSTEM=1"},
	} {
		if !hasPair(args, want[0], want[1]) {
			t.Errorf("missing %s %s", want[0], want[1])
		}
	}
	if indexOf(args, "--read-only") < 0 || indexOf(args, "--rm") < 0 {
		t.Error("exec must be hardened like setup")
	}
	for _, a := range args {
		if strings.Contains(a, "CLAUDE_CODE_OAUTH_TOKEN") || strings.Contains(a, "ANTHROPIC_API_KEY") ||
			strings.Contains(a, "HTTPS_PROXY") || strings.Contains(a, "GITLAB") || strings.Contains(a, "JIRA") {
			t.Errorf("exec container gets %q", a)
		}
	}
	// argv is passed as is, without a shell.
	img := indexOf(args, o.Image)
	if got := strings.Join(args[img+1:], " "); got != "oasdiff breaking --fail-on ERR old/a.yaml new/a.yaml" {
		t.Errorf("cmd = %q", got)
	}
	if indexOf(args[img:], "sh") >= 0 {
		t.Error("exec must not use a shell")
	}
}

func TestValidExecEnv(t *testing.T) {
	for _, ok := range []string{"REVIEWBOT_BASE_SHA=abc", "A=", "X1=y"} {
		if !validExecEnv(ok) {
			t.Errorf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"ANTHROPIC_API_KEY=x", "claude_code_oauth_token=x", "GITLAB_TOKEN=x", "JIRA_TOKEN=x", "GITLAB_SERVICE_TOKEN=x", "lower=x", "1A=x", "NOEQ", "=x", "A-B=x"} {
		if validExecEnv(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestScrubEnvServiceTokens(t *testing.T) {
	in := []string{"PATH=/bin", "GITLAB_SERVICE_TOKEN=s", "GITLAB_HOOKS_TOKEN=h", "JIRA_TOKEN=j", "ANTHROPIC_API_KEY=k",
		"USAGE_TOKEN=u", "DIAG_TOKEN=d", "GITLAB_WEBHOOK_SECRET=w"}
	if got := strings.Join(scrubEnv(in), ","); got != "PATH=/bin" {
		t.Errorf("scrubEnv = %q", got)
	}
	for _, kv := range []string{"USAGE_TOKEN=u", "DIAG_TOKEN=d"} {
		if validExecEnv(kv) {
			t.Errorf("validExecEnv accepted %q", kv)
		}
	}
	lc := strings.Join(localClaudeEnv(in), ",")
	if strings.Contains(lc, "GITLAB") || strings.Contains(lc, "JIRA") || strings.Contains(lc, "USAGE_TOKEN") || strings.Contains(lc, "DIAG_TOKEN") ||
		!strings.Contains(lc, "ANTHROPIC_API_KEY=k") {
		t.Errorf("localClaudeEnv = %s", lc)
	}
}

// Every secret of the shared list (internal/core) is kept from setup/exec
// and every service secret from Claude: the lists cannot drift.
func TestScrubEnvUsesSharedSecretList(t *testing.T) {
	in := []string{"PATH=/bin"}
	for _, name := range core.SecretEnv() {
		in = append(in, name+"=v")
	}
	if got := strings.Join(scrubEnv(in), ","); got != "PATH=/bin" {
		t.Errorf("scrubEnv = %q", got)
	}
	for _, name := range core.SecretEnv() {
		if validExecEnv(name + "=v") {
			t.Errorf("validExecEnv accepted %s", name)
		}
	}
	lc := localClaudeEnv(in)
	for _, name := range core.ServiceSecretEnv() {
		if strings.Contains(strings.Join(lc, ","), name+"=") {
			t.Errorf("localClaudeEnv kept %s", name)
		}
	}
	for _, name := range []string{"GITLAB_SERVICE_TOKEN", "GITLAB_HOOKS_TOKEN", "JIRA_TOKEN", "USAGE_TOKEN", "DIAG_TOKEN", "METRICS_TOKEN", "MAP_TOKEN"} {
		if !core.IsServiceSecretEnv(name) {
			t.Errorf("%s missing from the shared secret list", name)
		}
	}
}

func TestExecDockerFake(t *testing.T) {
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	docker := writeScript(t, bin, "docker", `
echo "ARGS: $*" >> "`+logf+`"
echo "ENV: $CLAUDE_CODE_OAUTH_TOKEN$ANTHROPIC_API_KEY$GITLAB_TOKEN$JIRA_TOKEN" >> "`+logf+`"
case "$*" in
  *missing-tool*) echo 'docker: Error response from daemon: exec: "missing-tool": executable file not found in $PATH' >&2; exit 127;;
  *oasdiff*) echo '[{"id":"x","level":3}]'; echo "warn" >&2; exit 1;;
esac
`)
	o := dockerOpts()
	o.DockerBin = docker
	r, err := newRunner(o, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "CLAUDE_CODE_OAUTH_TOKEN=secret", "GITLAB_TOKEN=gl", "JIRA_TOKEN=jt"}
	})
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	res, err := r.Exec(context.Background(), core.ExecRequest{Dir: work, Command: []string{"oasdiff", "breaking", "old/a", "new/a"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 1 || !strings.Contains(res.Stdout, `"id":"x"`) || strings.TrimSpace(res.Stderr) != "warn" {
		t.Fatalf("res = %+v", res)
	}
	log, _ := os.ReadFile(logf)
	s := string(log)
	if !strings.Contains(s, "--network none") || strings.Contains(s, "secret") || !strings.Contains(s, "ENV: \n") {
		t.Errorf("exec invocation wrong:\n%s", s)
	}
	if !strings.Contains(s, "reviewbot-exec-") {
		t.Errorf("container name: %s", s)
	}
	// Missing tool: exit 127 is a result, not an error.
	res, err = r.Exec(context.Background(), core.ExecRequest{Dir: work, Command: []string{"missing-tool"}})
	if err != nil || res.ExitCode != 127 {
		t.Fatalf("missing tool: %+v %v", res, err)
	}
	// Credentials can never be passed through Env.
	if _, err := r.Exec(context.Background(), core.ExecRequest{Dir: work, Command: []string{"x"}, Env: []string{"ANTHROPIC_API_KEY=x"}}); err == nil {
		t.Error("credential env accepted")
	}
	if _, err := r.Exec(context.Background(), core.ExecRequest{Dir: work}); err == nil {
		t.Error("empty command accepted")
	}
}

func TestExecLocalScrubbedEnv(t *testing.T) {
	bin := t.TempDir()
	writeScript(t, bin, "envtool", `
echo "K=$ANTHROPIC_API_KEY G=$GITLAB_TOKEN S=$GITLAB_SERVICE_TOKEN J=$JIRA_TOKEN B=$REVIEWBOT_BASE_SHA C=$NO_COLOR"
pwd
exit 3
`)
	work := t.TempDir()
	r, err := newRunner(Options{Mode: "local", ClaudeBin: "/bin/sh"}, func() []string {
		return []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k", "GITLAB_TOKEN=gl", "GITLAB_SERVICE_TOKEN=svc", "JIRA_TOKEN=j"}
	})
	if err != nil {
		t.Fatal(err)
	}
	// LookPath uses the service PATH; put the tool there for the test.
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	res, err := r.Exec(context.Background(), core.ExecRequest{Dir: work, Command: []string{"envtool"}, Env: []string{"REVIEWBOT_BASE_SHA=abc"}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if res.ExitCode != 3 || lines[0] != "K= G= S= J= B=abc C=1" {
		t.Fatalf("res = %+v", res)
	}
	if real, _ := filepath.EvalSymlinks(work); lines[1] != work && lines[1] != real {
		t.Errorf("cwd = %s, want %s", lines[1], work)
	}
	res, err = r.Exec(context.Background(), core.ExecRequest{Dir: work, Command: []string{"definitely-not-installed-xyz"}})
	if err != nil || res.ExitCode != 127 {
		t.Fatalf("missing tool: %+v %v", res, err)
	}
	_, err = r.Exec(context.Background(), core.ExecRequest{Dir: work, Command: []string{"sleep", "5"}, Timeout: 200 * time.Millisecond})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout err = %v", err)
	}
}

func TestNoToolsRunHasNoBash(t *testing.T) {
	args := buildClaudeArgs(core.RunRequest{NoTools: true})
	for i, a := range args {
		if a == "--tools" {
			if args[i+1] != "Read" {
				t.Fatalf("--tools %q, want Read", args[i+1])
			}
			return
		}
	}
	t.Fatal("--tools missing")
}

func TestDefaultRunToolSet(t *testing.T) {
	args := buildClaudeArgs(core.RunRequest{})
	if v, ok := argValue(args, "--tools"); !ok || v != "Read,Grep,Glob" {
		t.Fatalf("--tools %q", v)
	}
}

func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestAdvisorAndSearchArgs(t *testing.T) {
	args := buildClaudeArgs(core.RunRequest{Model: "sonnet", Advisor: "opus", SearchModel: "haiku"})
	joined := strings.Join(args, " ")
	if v, _ := argValue(args, "--tools"); v != "Read,Grep,Glob,Task" {
		t.Errorf("--tools %q", v)
	}
	if v, _ := argValue(args, "--advisor"); v != "opus" {
		t.Errorf("--advisor %q", v)
	}
	if !strings.Contains(joined, "--allowedTools Read(//work/**) Task(code-search) --disallowedTools") {
		t.Errorf("allowlist: %s", joined)
	}
	for _, d := range builtinAgentDeny {
		if !strings.Contains(joined, d) {
			t.Errorf("missing deny %s", d)
		}
	}
	v, ok := argValue(args, "--agents")
	if !ok {
		t.Fatal("--agents missing")
	}
	var agents map[string]struct {
		Description, Prompt, Model string
		Tools                      []string
	}
	if err := json.Unmarshal([]byte(v), &agents); err != nil {
		t.Fatal(err)
	}
	a := agents["code-search"]
	if len(agents) != 1 || a.Model != "haiku" || strings.Join(a.Tools, ",") != "Read,Grep,Glob" || !strings.Contains(a.Prompt, "UNTRUSTED DATA") {
		t.Errorf("agents %+v", agents)
	}

	// Off when empty, and never for tool-less runs.
	for _, req := range []core.RunRequest{{}, {NoTools: true, Advisor: "opus", SearchModel: "haiku"}} {
		args := strings.Join(buildClaudeArgs(req), " ")
		if strings.Contains(args, "--advisor") || strings.Contains(args, "--agents") || strings.Contains(args, "Task") {
			t.Errorf("unexpected helpers for %+v: %s", req, args)
		}
	}
}

// The deep-review subagent: read-only, next to code-search in one --agents
// value; alone it still needs Task; tool-less runs never get it.
func TestDeepReviewAgentArgs(t *testing.T) {
	decode := func(t *testing.T, args []string) map[string]struct {
		Description, Prompt, Model string
		Tools                      []string
	} {
		t.Helper()
		v, ok := argValue(args, "--agents")
		if !ok {
			t.Fatal("--agents missing")
		}
		var agents map[string]struct {
			Description, Prompt, Model string
			Tools                      []string
		}
		if err := json.Unmarshal([]byte(v), &agents); err != nil {
			t.Fatal(err)
		}
		return agents
	}
	args := buildClaudeArgs(core.RunRequest{Model: "sonnet", SearchModel: "haiku", DeepReviewModel: "opus"})
	joined := strings.Join(args, " ")
	if v, _ := argValue(args, "--tools"); v != "Read,Grep,Glob,Task" {
		t.Errorf("--tools %q", v)
	}
	if !strings.Contains(joined, "--allowedTools Read(//work/**) Task(code-search) Task(deep-review) --disallowedTools") {
		t.Errorf("allowlist: %s", joined)
	}
	agents := decode(t, args)
	d := agents[DeepReviewAgentName]
	if len(agents) != 2 || agents[SearchAgentName].Model != "haiku" || d.Model != "opus" || strings.Join(d.Tools, ",") != "Read,Grep,Glob" ||
		!strings.Contains(d.Prompt, "UNTRUSTED DATA") || !strings.Contains(d.Prompt, "/base") || d.Description == "" {
		t.Errorf("agents %+v", agents)
	}

	// Deep review alone: Task, the deny list of the built-in agents, one agent.
	args = buildClaudeArgs(core.RunRequest{DeepReviewModel: "opus"})
	joined = strings.Join(args, " ")
	if v, _ := argValue(args, "--tools"); v != "Read,Grep,Glob,Task" || !strings.Contains(joined, builtinAgentDeny[0]) || strings.Contains(joined, "Task(code-search)") {
		t.Errorf("deep only: %s", joined)
	}
	if agents := decode(t, args); len(agents) != 1 || agents[DeepReviewAgentName].Model != "opus" {
		t.Errorf("deep only agents %+v", agents)
	}
	if args := strings.Join(buildClaudeArgs(core.RunRequest{NoTools: true, DeepReviewModel: "opus"}), " "); strings.Contains(args, "deep-review") || strings.Contains(args, "Task") {
		t.Errorf("tool-less run: %s", args)
	}
}

func TestAdvisorFallback(t *testing.T) {
	bin := t.TempDir()
	// Fails while --advisor is present, succeeds without it; counts calls.
	claudeBin := writeScript(t, bin, "claude", `
echo x >> calls.txt
cat > /dev/null
case "$*" in
  *--advisor*) echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API Error: 400 Error: The model \"claude-sonnet\" does not support the advisor tool."}'; exit 1 ;;
esac
echo '`+fakeResult+`'
`)
	work := t.TempDir()
	r, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"}
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := func() int {
		b, _ := os.ReadFile(filepath.Join(work, "calls.txt"))
		return strings.Count(string(b), "x")
	}
	req := core.RunRequest{Dir: work, Prompt: "hi", JSONSchema: "{}", Advisor: "opus"}
	if res, err := r.Run(context.Background(), req); err != nil || res.Output != `{"summary":"ok"}` || !res.AdvisorDropped {
		t.Fatalf("run: %v %+v", err, res)
	}
	if calls() != 2 || !r.advisorOff.Load() {
		t.Fatalf("calls %d, advisorOff %v", calls(), r.advisorOff.Load())
	}
	// Remembered: the next run goes straight without the advisor (and says
	// so: a verification problem for the reviewer).
	if res, err := r.Run(context.Background(), req); err != nil || calls() != 3 || !res.AdvisorDropped {
		t.Fatalf("second run: %v, calls %d, %+v", err, calls(), res)
	}
	// A run that did not ask for the advisor never reports it dropped.
	if res, err := r.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "hi", JSONSchema: "{}"}); err != nil || res.AdvisorDropped || res.MCPDropped {
		t.Fatalf("plain run: %v %+v", err, res)
	}
	// Other failures are not retried.
	if isAdvisorError(errors.New("claude run failed: overloaded")) || isAdvisorError(&core.ErrClaudeRateLimited{Message: "advisor"}) {
		t.Error("only advisor errors trigger the fallback")
	}
}

// A failed deep-review call does not fail the run: the result says so
// (a problem for the caller) and the runner warns once per process.
func TestRunReportsFailedDeepReview(t *testing.T) {
	bin := t.TempDir()
	claudeBin := writeScript(t, bin, "claude", `
cat > /dev/null
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t9","name":"Task","input":{"subagent_type":"deep-review"}}]}}'
echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t9","is_error":true,"content":"model not available"}]}}'
echo '`+fakeResult+`'
`)
	work := t.TempDir()
	r, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"}
	})
	if err != nil {
		t.Fatal(err)
	}
	req := core.RunRequest{Dir: work, Prompt: "hi", JSONSchema: "{}", DeepReviewModel: "opus"}
	for i := 0; i < 2; i++ {
		res, err := r.Run(context.Background(), req)
		if err != nil || res.Output != `{"summary":"ok"}` || res.DeepReviewError != "model not available" {
			t.Fatalf("run %d: %v %+v", i, err, res)
		}
	}
	if !r.deepWarned.Load() {
		t.Error("the runner must warn once")
	}
}

package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// flagValues returns the values of a variadic flag (up to the next flag).
func flagValues(args []string, flag string) []string {
	i := indexOf(args, flag)
	if i < 0 {
		return nil
	}
	var out []string
	for _, a := range args[i+1:] {
		if strings.HasPrefix(a, "--") {
			break
		}
		out = append(out, a)
	}
	return out
}

// ruleDir returns the absolute directory of a "Read(//dir/**)" rule.
func ruleDir(r string) (string, bool) {
	if !strings.HasPrefix(r, "Read(//") || !strings.HasSuffix(r, "/**)") {
		return "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(r, "Read(/"), "/**)"), true
}

// The exact rules of a docker-mode review with every read-only directory,
// the search subagent and MCP: Read is scoped to the mounted inputs, Grep and
// Glob are not allowed by bare name, and the deny list covers the rest of the
// container, the MCP config mount and $HOME (Claude Code's credentials).
func TestDockerReadRulesExact(t *testing.T) {
	req := core.RunRequest{
		BaseDir: "/data/repos/work/c-tree-1", SkillsDir: "/data/skills/run-1", SearchModel: "haiku",
		ContextDirs: []core.ContextDir{{Name: "g/p", HostPath: "/data/repos/snap/g-p"}},
		MCP:         &core.MCPServer{Name: "reviewbot", URL: "http://egress:8091/mcp", Token: "t", Tools: []string{"coverage"}},
	}
	args := buildClaudeArgs(req)
	wantAllow := []string{
		"Read(//work/**)", "Read(//base/**)", "Read(//repos/**)", "Read(//skills/**)",
		"Task(code-search)", "mcp__reviewbot__coverage",
	}
	if got := flagValues(args, "--allowedTools"); !slices.Equal(got, wantAllow) {
		t.Errorf("allowed:\n got %q\nwant %q", got, wantAllow)
	}
	if got := AllowedTools(req); !slices.Equal(got, wantAllow) {
		t.Errorf("AllowedTools = %q", got)
	}
	wantDeny := []string{
		"Bash", "WebFetch", "WebSearch", "Edit", "Write", "NotebookEdit",
		"Read(//proc/**)", "Read(//sys/**)", "Read(//dev/**)", "Read(//etc/**)", "Read(//root/**)",
		"Read(//home/**)", "Read(//tmp/**)", "Read(//var/**)", "Read(//run/**)", "Read(//usr/**)",
		"Read(//opt/**)", "Read(//bin/**)", "Read(//sbin/**)", "Read(//lib/**)", "Read(//lib64/**)",
		"Read(//boot/**)", "Read(//srv/**)", "Read(//mnt/**)", "Read(//media/**)", "Read(//private/**)",
		"Read(//Users/**)", "Read(//Volumes/**)", "Read(//etc/reviewbot-mcp/**)", "Read(//.dockerenv)",
		"Read(~/**)", "Read(~/.claude/**)", "Read(~/.ssh/**)", "Read(~/.config/**)", "Read(~/.docker/**)",
		"Read(~/.aws/**)", "Read(~/.kube/**)", "Read(~/.gnupg/**)", "Read(~/.claude.json)",
		"Read(~/.claude.json.backup)", "Read(~/.netrc)", "Read(~/.git-credentials)",
	}
	wantDeny = append(wantDeny, builtinAgentDeny...)
	if got := flagValues(args, "--disallowedTools"); !slices.Equal(got, wantDeny) {
		t.Errorf("denied:\n got %q\nwant %q", got, wantDeny)
	}
	// Diagnostics show the same deny list (without the agent rules).
	if got := DisallowedTools(); !slices.Equal(got, wantDeny[:len(wantDeny)-len(builtinAgentDeny)]) {
		t.Errorf("DisallowedTools = %q", got)
	}
	// The search subagent has the same three read tools (the permission
	// rules are the session's).
	v, _ := argValue(args, "--agents")
	if !strings.Contains(v, `"tools":["Read","Grep","Glob"]`) {
		t.Errorf("--agents %s", v)
	}
}

// Only the directories of the run are allowed; a bare read tool never is.
func TestDockerReadRulesPerRun(t *testing.T) {
	for _, tc := range []struct {
		req  core.RunRequest
		want string
	}{
		{core.RunRequest{}, "Read(//work/**)"},
		{core.RunRequest{BaseDir: "/b"}, "Read(//work/**) Read(//base/**)"},
		{core.RunRequest{ContextDirs: []core.ContextDir{{Name: "g/p", HostPath: "/s"}}}, "Read(//work/**) Read(//repos/**)"},
		{core.RunRequest{SkillsDir: "/k"}, "Read(//work/**) Read(//skills/**)"},
	} {
		got := AllowedTools(tc.req)
		if strings.Join(got, " ") != tc.want {
			t.Errorf("%+v: %q", tc.req, got)
		}
		for _, bare := range []string{"Read", "Grep", "Glob"} {
			if slices.Contains(got, bare) {
				t.Errorf("bare %s allowed: %q", bare, got)
			}
		}
	}
}

// Text-only runs get no allowlist but the same Read deny rules.
func TestNoToolsRunKeepsReadDenyRules(t *testing.T) {
	args := buildClaudeArgs(core.RunRequest{NoTools: true})
	if indexOf(args, "--allowedTools") >= 0 {
		t.Fatalf("allowlist on a text-only run: %q", args)
	}
	deny := flagValues(args, "--disallowedTools")
	for _, r := range []string{"Read(//proc/**)", "Read(//etc/reviewbot-mcp/**)", "Read(~/**)", "Read(~/.claude/**)"} {
		if !slices.Contains(deny, r) {
			t.Errorf("missing %s in %q", r, deny)
		}
	}
}

// No deny rule may cover an allowed directory (deny wins): in docker mode
// none does; in local mode the roots that contain the data dir or $HOME are
// left out, the specific credential paths stay.
func TestLocalReadScope(t *testing.T) {
	home := "/home/u"
	work := "/home/u/data/repos/work/c1"
	ro := readOnlyDirs{
		base:   "/home/u/data/repos/work/c1-tree-2",
		ctx:    []core.ContextDir{{Name: "g/p", HostPath: "/var/lib/rb/snap/g-p"}},
		skills: "/home/u/data/skills/r1",
	}
	s := localReadScope(work, ro, home, "/tmp/.reviewbot-mcp-123")
	allow := s.allowRules()
	want := []string{
		"Read(//home/u/data/repos/work/c1/**)", "Read(//home/u/data/repos/work/c1-tree-2/**)",
		"Read(//var/lib/rb/snap/g-p/**)", "Read(//home/u/data/skills/r1/**)",
	}
	if !slices.Equal(allow, want) {
		t.Errorf("allow %q", allow)
	}
	deny := s.denyRules()
	for _, r := range []string{"Read(//home/**)", "Read(//var/**)", "Read(~/**)"} {
		if slices.Contains(deny, r) {
			t.Errorf("%s would deny an allowed directory: %q", r, deny)
		}
	}
	for _, r := range []string{"Read(//proc/**)", "Read(//etc/**)", "Read(//tmp/**)", "Read(//tmp/.reviewbot-mcp-123/**)",
		"Read(~/.claude/**)", "Read(~/.claude.json)", "Read(~/.ssh/**)"} {
		if !slices.Contains(deny, r) {
			t.Errorf("missing %s in %q", r, deny)
		}
	}
	for _, r := range deny {
		if d, ok := ruleDir(r); ok && s.coversAllowed(d) {
			t.Errorf("%s covers an allowed directory", r)
		}
	}
	for _, r := range dockerReadScope(readOnlyDirs{base: "/b", ctx: ro.ctx, skills: "/k"}).denyRules() {
		if d, ok := ruleDir(r); ok && (within(containerWorkDir, d) || within(core.BaseRoot, d)) {
			t.Errorf("docker: %s covers an allowed directory", r)
		}
	}
}

// Paths that cannot be written as a literal rule (glob characters, commas
// and spaces the CLI would split on, relative or unclean) produce no rule.
func TestReadRulesSkipUnsafePaths(t *testing.T) {
	for _, p := range []string{"/data/a b", "/data/a,b", "/data/a*", "/data/[x]", "/data/a(1)", "data/rel", "/data/../etc", "/data/x/", "/"} {
		s := readScope{allow: []string{p}, deny: []string{p}}
		if r := s.allowRules(); len(r) != 0 {
			t.Errorf("%q: allow %q", p, r)
		}
		for _, r := range s.denyRules() {
			if strings.Contains(r, "data") {
				t.Errorf("%q: deny %q", p, r)
			}
		}
	}
}

// Local mode lists symlinked directories under both names.
func TestLocalReadScopeSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "clone")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	allow := localReadScope(link, readOnlyDirs{}, "/nonexistent-home", "").allowRules()
	if !slices.Contains(allow, "Read(/"+link+"/**)") || !slices.Contains(allow, "Read(/"+resolved+"/**)") {
		t.Errorf("allow %q", allow)
	}
}

// The runner passes the host paths in local mode: the clone and the merge
// base are allowed, and nothing in the deny list covers them.
func TestLocalRunReadRules(t *testing.T) {
	parent := t.TempDir()
	work, base := filepath.Join(parent, "clone"), filepath.Join(parent, "clone-tree-1")
	for _, d := range []string{work, base} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	claudeBin := writeScript(t, bin, "claude", `
for a in "$@"; do echo "$a"; done > args.txt
cat >/dev/null
echo '`+fakeResult+`'
`)
	r, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k", "HOME=" + parent}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "x", JSONSchema: "{}", BaseDir: base}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(work, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	allow := flagValues(args, "--allowedTools")
	if !ruleablePath(work) {
		t.Skipf("temp path %q cannot be a rule", work)
	}
	if !slices.Contains(allow, "Read(/"+work+"/**)") || !slices.Contains(allow, "Read(/"+base+"/**)") {
		t.Errorf("allow %q", allow)
	}
	deny := flagValues(args, "--disallowedTools")
	if slices.Contains(deny, "Read(~/**)") {
		t.Errorf("$HOME contains the clone, ~/** must not be denied: %q", deny)
	}
	if !slices.Contains(deny, "Read(~/.claude/**)") || !slices.Contains(deny, "Read(//proc/**)") {
		t.Errorf("deny %q", deny)
	}
	for _, rule := range deny {
		if d, ok := ruleDir(rule); ok && (within(work, d) || within(base, d)) {
			t.Errorf("%s denies the clone or the base", rule)
		}
	}
}

package claude

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Read scope of a run.
//
// Without Bash, Claude Code's file-reading tools (Read, Grep, Glob) are the
// only way the model touches the filesystem, and a bare `Read` in
// --allowedTools would allow ANY path: /proc/self/environ (the Claude token
// in docker mode), the MCP config with the run's bearer token, ~/.claude*.
// So the allowlist names only the directories the run needs, and a deny list
// covers everything else the container (or host) has. Deny rules win over
// allow rules and over the working directories (cwd, --add-dir).
//
// Claude Code 2.1.x permission rule syntax (code.claude.com/docs/en/
// permissions, "Read & Edit"): gitignore-style specifiers; `//path` is an
// absolute path, `~/path` is relative to $HOME, `/path` would be relative to
// the settings file and `path` / `./path` to the cwd, so every rule here is
// written in the absolute or home form. Read rules are applied, best effort,
// to the other built-in tools that read files (Grep, Glob): that is why
// Grep and Glob are no longer allowed by bare name (a bare `Grep` would allow
// searching any path); inside the working directories they need no rule,
// elsewhere they fall back to these Read rules. The search subagent shares
// the session's permission context (same allow and deny rules).
//
// Rules are passed as separate argv values; the CLI also splits a value on
// commas and spaces, so a path that contains them, a glob metacharacter or
// anything else outside safeRulePath is never turned into a rule (a host
// directory like that is still readable as a working directory via
// --add-dir; a deny rule for it is simply not emitted).

// readScope is what the model may read in one run.
type readScope struct {
	// allow are the absolute directories the run reads: the clone (cwd),
	// the merge base, the context repositories and the skills.
	allow []string
	// home is $HOME as the CLI sees it (the `~/` of the rules).
	home string
	// deny are extra absolute directories to refuse (the MCP config).
	deny []string
}

// dockerReadScope is the scope of a sandbox container: the clone at /work
// and the mount roots of the read-only directories this run has (/base,
// /repos, /skills), $HOME on tmpfs and the MCP config mount.
func dockerReadScope(ro readOnlyDirs) readScope {
	allow := []string{containerWorkDir}
	if ro.base != "" {
		allow = append(allow, core.BaseRoot)
	}
	if len(ro.ctx) > 0 {
		allow = append(allow, core.ContextReposRoot)
	}
	if ro.skills != "" {
		allow = append(allow, core.SkillsRoot)
	}
	return readScope{allow: allow, home: containerHome, deny: []string{containerMCPDir}}
}

// requestDirs are the read-only directories a request asks for (before the
// runner drops unusable ones).
func requestDirs(req core.RunRequest) readOnlyDirs {
	return readOnlyDirs{base: req.BaseDir, ctx: req.ContextDirs, skills: req.SkillsDir}
}

// localReadScope is the scope of a local-mode run: the host paths of the
// clone and the read-only directories, the bot's $HOME, and the MCP config
// directory (mcpDir, "" = none). Paths are also listed with symlinks
// resolved (macOS /tmp -> /private/tmp), whichever form the CLI compares.
func localReadScope(dir string, ro readOnlyDirs, home, mcpDir string) readScope {
	var allow []string
	add := func(p string) {
		if p == "" {
			return
		}
		allow = append(allow, p)
		if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
			allow = append(allow, r)
		}
	}
	add(dir)
	add(ro.base)
	for _, c := range ro.ctx {
		add(c.HostPath)
	}
	add(ro.skills)
	s := readScope{allow: allow, home: home}
	if mcpDir != "" {
		s.deny = append(s.deny, mcpDir)
		if r, err := filepath.EvalSymlinks(mcpDir); err == nil && r != mcpDir {
			s.deny = append(s.deny, r)
		}
	}
	return s
}

// envHome is $HOME of env, or the process's home directory.
func envHome(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "HOME="); ok && v != "" {
			return v
		}
	}
	h, _ := os.UserHomeDir()
	return h
}

// sensitiveRoots are refused whole: every top-level directory of the
// sandbox image (node:22-bookworm-slim) that is not a mounted input, plus
// the macOS host roots for local mode. A root that contains an allowed
// directory (local mode: a data dir under /home or /var) is left out, deny
// would win over the allow rule.
var sensitiveRoots = []string{
	"/proc", "/sys", "/dev", "/etc", "/root", "/home", "/tmp", "/var", "/run",
	"/usr", "/opt", "/bin", "/sbin", "/lib", "/lib64", "/boot", "/srv", "/mnt",
	"/media", "/private", "/Users", "/Volumes",
}

// sensitiveFiles are single files at the root.
var sensitiveFiles = []string{"/.dockerenv"}

// homeDirs and homeFiles under $HOME are refused even when the whole home
// cannot be (local mode with the data dir inside $HOME): Claude Code's own
// credentials and settings, and the usual credential stores.
var (
	homeDirs  = []string{".claude", ".ssh", ".config", ".docker", ".aws", ".kube", ".gnupg"}
	homeFiles = []string{".claude.json", ".claude.json.backup", ".netrc", ".git-credentials"}
)

// safeRulePath: absolute, clean, and only characters that are literal in a
// gitignore pattern and survive the CLI's comma/space splitting.
var safeRulePath = regexp.MustCompile(`^/[A-Za-z0-9._@+=/-]*$`)

func ruleablePath(p string) bool {
	return safeRulePath.MatchString(p) && filepath.Clean(p) == p
}

// within reports whether p is root or inside it.
func within(p, root string) bool {
	return p == root || root == "/" || strings.HasPrefix(p, root+"/")
}

// coversAllowed reports whether denying p would also deny an allowed
// directory (p is one of them or contains one).
func (s readScope) coversAllowed(p string) bool {
	for _, a := range s.allow {
		if within(a, p) {
			return true
		}
	}
	return false
}

// dirRule is the rule for everything under an absolute directory.
func dirRule(dir string) string { return "Read(/" + dir + "/**)" }

// allowRules are the Read rules of the directories the run reads.
func (s readScope) allowRules() []string {
	var out []string
	seen := map[string]bool{}
	for _, d := range s.allow {
		if !ruleablePath(d) || d == "/" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, dirRule(d))
	}
	return out
}

// denyRules are the Read rules for everything sensitive outside the allowed
// directories. They go into --disallowedTools for every run, text-only ones
// included.
func (s readScope) denyRules() []string {
	var out []string
	seen := map[string]bool{}
	emit := func(r string) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, d := range append(append([]string(nil), sensitiveRoots...), s.deny...) {
		if ruleablePath(d) && d != "/" && !s.coversAllowed(d) {
			emit(dirRule(d))
		}
	}
	for _, f := range sensitiveFiles {
		if !s.coversAllowed(f) {
			emit("Read(/" + f + ")")
		}
	}
	home := s.home
	if !ruleablePath(home) || home == "/" {
		return out
	}
	if !s.coversAllowed(home) {
		emit("Read(~/**)")
	}
	for _, d := range homeDirs {
		if !s.coversAllowed(home + "/" + d) {
			emit("Read(~/" + d + "/**)")
		}
	}
	for _, f := range homeFiles {
		if !s.coversAllowed(home + "/" + f) {
			emit("Read(~/" + f + ")")
		}
	}
	return out
}

package lint

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/safefs"
)

// ScratchDir is where the target-branch versions of tool configs are written
// inside the checkout (git-excluded, removed after the run).
const ScratchDir = ".reviewbot-lint"

// containerWorkDir is the checkout as tools see it in docker mode.
const containerWorkDir = "/work"

type runEnv struct {
	c       *Checker
	req     Request
	changed *Changed
	files   []string // every file of the head tree
	dir     string
	timeout time.Duration
	log     interface {
		Info(msg string, args ...any)
	}
	scratch string // created on demand
}

// run executes argv in the checkout. ok=false means st was filled in and the
// output must not be parsed.
func (e *runEnv) run(ctx context.Context, argv []string, st *ToolStatus) (*core.ExecResult, bool) {
	out, err := e.c.Runner.Exec(ctx, core.ExecRequest{Dir: e.dir, Command: argv, Timeout: e.timeout})
	if err != nil {
		st.State = StateFailed
		st.Reason = "не удалось запустить: " + shortText(err.Error(), 200)
		if errors.Is(err, context.DeadlineExceeded) {
			st.Reason = "превышено время выполнения"
		}
		return nil, false
	}
	if out.ExitCode == 127 || notFoundText(out.Stderr) {
		st.State = StateUnavailable
		st.Reason = argv[0] + " не установлен в sandbox-образе"
		return nil, false
	}
	return out, true
}

func notFoundText(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "executable file not found") && strings.Contains(s, "$path")
}

func failed(st *ToolStatus, why string, out *core.ExecResult) {
	st.State = StateFailed
	st.Reason = why
	if out != nil {
		d := strings.TrimSpace(stripANSI(out.Stderr))
		if d == "" {
			d = strings.TrimSpace(stripANSI(out.Stdout))
		}
		if d != "" {
			st.Reason += ": " + shortText(lastLines(d, 3), 200)
		}
	}
}

func lastLines(s string, n int) string {
	ls := strings.Split(strings.TrimSpace(s), "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
	}
	return strings.Join(ls, " ")
}

// scratchDir creates the scratch dir (once).
func (e *runEnv) scratchDir() (string, error) {
	if e.scratch != "" {
		return e.scratch, nil
	}
	// The clone is the MR's: a symlink committed at ScratchDir must not
	// redirect our writes outside it (safefs replaces it with a real dir).
	d, err := safefs.PrepareDir(e.dir, ScratchDir, true)
	if err != nil {
		return "", err
	}
	safefs.ExcludeFromGit(e.dir, "/"+ScratchDir+"/")
	e.scratch = d
	return d, nil
}

func (e *runEnv) cleanup() {
	if e.scratch != "" {
		_ = os.RemoveAll(e.scratch)
		e.scratch = ""
	}
}

// relPath maps a path a tool printed back to the repository path.
func (e *runEnv) relPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	prefixes := []string{filepath.ToSlash(e.dir) + "/", containerWorkDir + "/"}
	// Tools that print real paths (PHPStan) resolve symlinks in the clone
	// directory (e.g. /tmp -> /private/tmp).
	if real, err := filepath.EvalSymlinks(e.dir); err == nil && real != e.dir {
		prefixes = append(prefixes, filepath.ToSlash(real)+"/")
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(p, prefix) {
			p = p[len(prefix):]
			break
		}
	}
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	return path.Clean(p)
}

// keep filters findings to the lines the MR adds.
func keep(fs []Finding, ch *Changed) []Finding {
	var out []Finding
	for _, f := range fs {
		if f.Path == "" || f.Line <= 0 {
			continue
		}
		if !ch.Line(f.Path, f.Line) {
			// Multi-line result: anchor at the first added line inside it.
			moved := false
			for l := f.Line + 1; l <= f.EndLine && l <= f.Line+200; l++ {
				if ch.Line(f.Path, l) {
					f.Line, moved = l, true
					break
				}
			}
			if !moved {
				continue
			}
		}
		f.InDiff = true
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------------------
// gitleaks
// ---------------------------------------------------------------------------

// gitleaks scans the commits of the MR (`git log -p -U0 base..head`), so a
// secret added and later removed inside the MR is still found: it stays in
// the history that would be merged. The scanned source is the .git directory,
// and the config / ignore file come from the TARGET branch (written to the
// scratch dir), so the MR cannot allowlist its own secrets by adding a
// .gitleaks.toml or .gitleaksignore. --redact replaces the secret in the
// report; the parser keeps no secret-bearing field anyway.
func (e *runEnv) gitleaks(ctx context.Context) ([]Finding, ToolStatus) {
	st := ToolStatus{Tool: ToolGitleaks}
	if !validRev(e.req.Base) || !validRev(e.req.Head) {
		failed(&st, "неизвестный диапазон коммитов", nil)
		return nil, st
	}
	// A cached checkout's tree is shared and read-only and has no .git: the
	// history comes from a per-job clone of the mirror (repo.HistoryDir),
	// where the scratch dir is written too.
	if hd, ok := core.CheckoutAs[historyDirer](e.req.Checkout); ok {
		d, err := hd.HistoryDir(ctx)
		if err != nil {
			failed(&st, "не удалось подготовить историю коммитов: "+shortText(err.Error(), 200), nil)
			return nil, st
		}
		e.dir = d
	}
	if fi, err := os.Stat(filepath.Join(e.dir, ".git")); err != nil || !fi.IsDir() {
		failed(&st, "в клоне нет каталога .git", nil)
		return nil, st
	}
	sd, err := e.scratchDir()
	if err != nil {
		failed(&st, "не удалось подготовить конфиг: "+err.Error(), nil)
		return nil, st
	}
	defer e.cleanup()
	cfg := []byte(gitleaksDefaultConfig)
	if b, err := e.req.Checkout.ShowFile(ctx, e.req.Base, ".gitleaks.toml"); err == nil && len(b) > 0 && len(b) < 1<<20 {
		cfg = b
	}
	if err := safefs.WriteFile(sd, "gitleaks.toml", cfg); err != nil {
		failed(&st, "не удалось подготовить конфиг: "+err.Error(), nil)
		return nil, st
	}
	_ = safefs.MkdirAll(sd, "ignore")
	if b, err := e.req.Checkout.ShowFile(ctx, e.req.Base, ".gitleaksignore"); err == nil && len(b) < 1<<20 {
		_ = safefs.WriteFile(sd, "ignore/.gitleaksignore", b)
	}
	argv := GitleaksCommand(e.req.Base, e.req.Head)
	out, ok := e.run(ctx, argv, &st)
	if !ok {
		return nil, st
	}
	switch out.ExitCode {
	case 0, gitleaksLeakExit:
	default:
		failed(&st, fmt.Sprintf("gitleaks завершился с кодом %d", out.ExitCode), out)
		return nil, st
	}
	all, err := ParseGitleaks([]byte(out.Stdout))
	if err != nil {
		failed(&st, "непонятный вывод gitleaks", nil)
		return nil, st
	}
	st.State, st.Total = StateRan, len(all)
	// Every secret in the MR's commits counts, in the final diff or not;
	// InDiff says whether the line is an added line of the MR diff (inline
	// thread) or only in the history (general thread).
	seen := map[string]bool{}
	var fs []Finding
	for _, f := range all {
		f.Path = e.relPath(f.Path)
		f.InDiff = e.changed.Line(f.Path, f.Line)
		k := f.ID()
		if seen[k] {
			continue
		}
		seen[k] = true
		fs = append(fs, f)
	}
	return fs, st
}

// historyDirer is optionally implemented by a checkout (repo.Checkout):
// a directory with a real .git directory of the MR's history that the bot
// may write next to (never the shared tree of a cached checkout).
type historyDirer interface {
	HistoryDir(ctx context.Context) (string, error)
}

// gitleaksLeakExit is passed as --exit-code: gitleaks exits 1 on any error
// (including a partial scan) and with --exit-code when leaks were found, so
// a distinct value separates "leaks" from "error".
const gitleaksLeakExit = 2

// gitleaksDefaultConfig is used when the target branch has no .gitleaks.toml:
// the built-in rule set.
const gitleaksDefaultConfig = "[extend]\nuseDefault = true\n"

// GitleaksCommand is the gitleaks argv (cwd = checkout). Flags verified
// against gitleaks v8.30.0 (cmd/root.go, cmd/git.go, sources/git.go):
// --log-opts is appended to `git -C <source> log -p -U0`; --report-path -
// writes the JSON report to stdout (logs go to stderr); --redact without a
// value means 100%; --platform none skips reading the remote;
// --ignore-gitleaks-allow: a "gitleaks:allow" comment added by the MR itself
// must not hide its own secret. The source is the .git directory, not the
// working tree: gitleaks always also loads <source>/.gitleaksignore, and the
// MR's own copy must not apply.
func GitleaksCommand(base, head string) []string {
	return []string{
		"gitleaks", "git",
		"--log-opts=" + base + ".." + head,
		"--config", ScratchDir + "/gitleaks.toml",
		"--gitleaks-ignore-path", ScratchDir + "/ignore",
		"--report-format", "json",
		"--report-path", "-",
		"--redact",
		"--ignore-gitleaks-allow",
		"--no-banner",
		"--no-color",
		"--log-level", "error",
		"--platform", "none",
		"--exit-code", fmt.Sprint(gitleaksLeakExit),
		".git",
	}
}

// validRev accepts commit SHAs and plain ref names (no spaces, no options).
func validRev(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || len(s) > 200 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./-~^", r)) {
			return false
		}
	}
	return true
}

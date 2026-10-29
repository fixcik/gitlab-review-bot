package claude

import (
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Read-only directories of a run: the merge base export
// (core.RunRequest.BaseDir), context repositories (snapshots of other
// projects, core.RunRequest.ContextDirs) and the staged team skills
// (core.RunRequest.SkillsDir).
//
// Docker mode: each one is bind-mounted READ-ONLY (the kernel-level `:ro` is
// the boundary) at /base, /repos/<group>/<project> and /skills, and Claude
// gets one `--add-dir` with the mount roots, so its read-only tools (Read,
// Grep, Glob) may read there without a prompt under dontAsk. Claude has no
// tool that writes (no Bash, Edit, Write), so --add-dir grants nothing more.
//
// Local mode: the host paths are passed with --add-dir. Files are exported
// read-only (0444); there is no other isolation in local mode.
//
// Verified against Claude Code 2.1.310 (`claude --help`: "--add-dir
// <directories...> Additional directories to allow tool access to";
// code.claude.com/docs/en/permissions#working-directories).

// readOnlyDirs are the usable read-only directories of one run.
type readOnlyDirs struct {
	base   string // merge base export (host path), "" = none
	ctx    []core.ContextDir
	skills string // staged skills (host path), "" = none
}

// mounts returns the read-only docker mounts.
func (d readOnlyDirs) mounts() []roMount {
	var out []roMount
	if d.base != "" {
		out = append(out, roMount{Host: d.base, Container: core.BaseRoot})
	}
	out = append(out, contextMounts(d.ctx)...)
	return append(out, skillsMounts(d.skills)...)
}

// contextNameRe: GitLab full paths; no ':' or ',' (docker -v syntax), no
// "." / ".." segments.
var contextNameRe = regexp.MustCompile(`^[A-Za-z0-9_\-][A-Za-z0-9_.\-]*(/[A-Za-z0-9_\-][A-Za-z0-9_.\-]*)*$`)

// roMount is one read-only bind mount.
type roMount struct{ Host, Container string }

// usableContextDirs drops entries that cannot be mounted safely (bad name,
// host path with ':' or ',', missing dir) and duplicates.
func usableContextDirs(dirs []core.ContextDir, log *slog.Logger) []core.ContextDir {
	var out []core.ContextDir
	seen := map[string]bool{}
	for _, d := range dirs {
		reason := ""
		switch {
		case !contextNameRe.MatchString(d.Name) || strings.Contains(d.Name, ".."):
			reason = "invalid name"
		case !filepath.IsAbs(d.HostPath) || strings.ContainsAny(d.HostPath, ":,\n"):
			reason = "unusable host path"
		case seen[strings.ToLower(d.Name)]:
			reason = "duplicate"
		case !isDir(d.HostPath):
			reason = "missing directory"
		}
		if reason != "" {
			if log != nil {
				log.Warn("context repository not mounted", "name", d.Name, "path", d.HostPath, "reason", reason)
			}
			continue
		}
		seen[strings.ToLower(d.Name)] = true
		out = append(out, d)
	}
	return out
}

// contextMounts returns the read-only docker mounts for dirs.
func contextMounts(dirs []core.ContextDir) []roMount {
	out := make([]roMount, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, roMount{Host: d.HostPath, Container: d.MountPath(true)})
	}
	return out
}

// addDirArgs returns one --add-dir flag for the merge base, the context
// repositories and the skills dir: the mount roots in docker mode (only
// mounted directories exist there), the host paths in local mode (so other
// snapshots on the host stay outside the working dirs). The flag is
// variadic, so it must come last in the argument list (the prompt goes on
// stdin).
func addDirArgs(d readOnlyDirs, docker bool) []string {
	var paths []string
	if d.base != "" {
		paths = append(paths, core.BaseMountPath(d.base, docker))
	}
	if len(d.ctx) > 0 {
		if docker {
			paths = append(paths, core.ContextReposRoot)
		} else {
			for _, c := range d.ctx {
				paths = append(paths, c.HostPath)
			}
		}
	}
	if d.skills != "" {
		paths = append(paths, core.SkillsMountPath(d.skills, docker))
	}
	if len(paths) == 0 {
		return nil
	}
	return append([]string{"--add-dir"}, paths...)
}

// usableBaseDir returns dir when it can be mounted safely as /base: an
// absolute host path without docker -v separators, an existing directory,
// not the clone (workDir) or inside it. Else "" (the run goes without it).
func usableBaseDir(dir, workDir string, log *slog.Logger) string {
	if dir == "" {
		return ""
	}
	reason := ""
	switch {
	case !filepath.IsAbs(dir) || strings.ContainsAny(dir, ":,\n"):
		reason = "unusable host path"
	case dir == workDir || strings.HasPrefix(dir, workDir+string(filepath.Separator)):
		reason = "inside the clone"
	case !isDir(dir):
		reason = "missing directory"
	}
	if reason != "" {
		if log != nil {
			log.Warn("merge base not mounted", "path", dir, "reason", reason)
		}
		return ""
	}
	return dir
}

// usableSkillsDir returns dir when it can be mounted safely, else "".
func usableSkillsDir(dir string, log *slog.Logger) string {
	if dir == "" {
		return ""
	}
	reason := ""
	switch {
	case !filepath.IsAbs(dir) || strings.ContainsAny(dir, ":,\n"):
		reason = "unusable host path"
	case !isDir(dir):
		reason = "missing directory"
	}
	if reason != "" {
		if log != nil {
			log.Warn("skills not mounted", "path", dir, "reason", reason)
		}
		return ""
	}
	return dir
}

// isDir reports whether p is an existing directory.
func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// skillsMounts is the read-only docker mount of the staged skills.
func skillsMounts(dir string) []roMount {
	if dir == "" {
		return nil
	}
	return []roMount{{Host: dir, Container: core.SkillsRoot}}
}

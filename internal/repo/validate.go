package repo

// Checks of the untrusted names that end up in git arguments, refspecs and
// paths: revisions, branch names and project paths.

import (
	"regexp"
	"strings"
)

func uniqueNonEmpty(vals ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range vals {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// validRev accepts hex object names (abbreviated or full).
func validRev(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

// validRefName is a conservative check for branch names used in refspecs.
func validRefName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") ||
		strings.Contains(s, "..") || strings.HasSuffix(s, ".lock") || strings.Contains(s, "@{") {
		return false
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(":?*[\\^~", r) {
			return false
		}
	}
	return true
}

// projectPathRe accepts GitLab full paths: segments of [A-Za-z0-9_.-].
var projectPathRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]+(/[A-Za-z0-9_.\-]+)*$`)

// ValidProjectPath reports whether p is a safe GitLab project path (usable
// in a mount path and a remote URL).
func ValidProjectPath(p string) bool {
	if !projectPathRe.MatchString(p) {
		return false
	}
	for _, s := range strings.Split(p, "/") {
		if s == "." || s == ".." || strings.HasSuffix(s, ".git") || strings.HasPrefix(s, "-") {
			return false
		}
	}
	return true
}

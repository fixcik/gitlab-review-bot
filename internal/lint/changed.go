package lint

import (
	"regexp"
	"strings"
)

// Changed is the set of lines added by the MR, per NEW file path.
type Changed struct {
	lines map[string]map[int]bool
	order []string
}

// Has reports whether the MR adds lines to path (or creates it).
func (c *Changed) Has(path string) bool {
	if c == nil {
		return false
	}
	_, ok := c.lines[path]
	return ok
}

// Line reports whether line of path is added by the MR.
func (c *Changed) Line(path string, line int) bool {
	if c == nil {
		return false
	}
	return c.lines[path][line]
}

// Any reports whether any line in [from, to] of path is added.
func (c *Changed) Any(path string, from, to int) bool {
	if to < from {
		to = from
	}
	if to-from > 200 {
		to = from + 200
	}
	for l := from; l <= to; l++ {
		if c.Line(path, l) {
			return true
		}
	}
	return false
}

// Files lists the paths with added lines, in diff order.
func (c *Changed) Files() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.order...)
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ParseChangedLines extracts the added new-file lines from `git diff` output.
// Deleted files and removed-only hunks contribute nothing.
func ParseChangedLines(diff string) *Changed {
	c := &Changed{lines: map[string]map[int]bool{}}
	path := ""
	newNo, newLeft := 0, 0
	inHunk := false
	for _, raw := range strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(raw, "diff --git "):
			path, inHunk = "", false
			continue
		case !inHunk && strings.HasPrefix(raw, "+++ "):
			path = diffPath(raw[4:])
			continue
		case !inHunk && strings.HasPrefix(raw, "--- "):
			continue
		}
		if m := hunkRe.FindStringSubmatch(raw); m != nil {
			newNo = atoi(m[1])
			newLeft = 1
			if m[2] != "" {
				newLeft = atoi(m[2])
			}
			inHunk = true
			continue
		}
		if !inHunk || path == "" {
			continue
		}
		switch {
		case strings.HasPrefix(raw, "+"):
			if newLeft <= 0 {
				inHunk = false
				continue
			}
			set := c.lines[path]
			if set == nil {
				set = map[int]bool{}
				c.lines[path] = set
				c.order = append(c.order, path)
			}
			set[newNo] = true
			newNo++
			newLeft--
		case strings.HasPrefix(raw, "-"), strings.HasPrefix(raw, `\`):
		case strings.HasPrefix(raw, " "), raw == "":
			newNo++
			newLeft--
		default:
			inHunk = false
		}
	}
	return c
}

// diffPath parses the path of a "+++ b/path" line ("" for /dev/null).
func diffPath(s string) string {
	s = strings.TrimRight(s, "\r")
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, `"`) {
		s = unquoteC(s)
	}
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, "b/")
}

// unquoteC undoes git's C-style path quoting ("a\tb", octal escapes).
func unquoteC(s string) string {
	s = strings.TrimSuffix(strings.TrimPrefix(s, `"`), `"`)
	var b []byte
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch != '\\' || i+1 >= len(s) {
			b = append(b, ch)
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b = append(b, '\n')
		case 't':
			b = append(b, '\t')
		case '"', '\\':
			b = append(b, s[i])
		default:
			if i+2 < len(s) && isOctal(s[i]) && isOctal(s[i+1]) && isOctal(s[i+2]) {
				b = append(b, (s[i]-'0')<<6|(s[i+1]-'0')<<3|(s[i+2]-'0'))
				i += 2
			} else {
				b = append(b, '\\', s[i])
			}
		}
	}
	return string(b)
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

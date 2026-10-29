// Package citelink renders Claude's `group/project:path:line` citations as
// links to the cited file at a known commit. It is a leaf: it must not
// depend on internal/review or any of its feature packages.
package citelink

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Linker renders `group/project:path:line` references to known
// projects as GitLab blob links pinned to a commit.
type Linker struct {
	base  string            // GitLab URL without trailing slash
	repos map[string]citRef // lower(path) -> project
}

type citRef struct{ path, sha string }

// New is a linker for the projects of the GitLab instance at gitlabURL.
func New(gitlabURL string) *Linker {
	return &Linker{base: strings.TrimRight(strings.TrimSpace(gitlabURL), "/"), repos: map[string]citRef{}}
}

// Add makes citations of projectPath link to the commit sha.
func (l *Linker) Add(projectPath, sha string) {
	p := strings.Trim(strings.TrimSpace(projectPath), "/")
	if p == "" || !validRefSHA(sha) {
		return
	}
	l.repos[strings.ToLower(p)] = citRef{path: p, sha: sha}
}

func validRefSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// citationRe matches group/project:path[:line[-end]]. The project needs at
// least one '/', the file path no spaces or ':'.
var citationRe = regexp.MustCompile(`([A-Za-z0-9_][A-Za-z0-9_.\-]*(?:/[A-Za-z0-9_][A-Za-z0-9_.\-]*)+):([A-Za-z0-9_.\-@+]+(?:/[A-Za-z0-9_.\-@+]+)*)(?::(\d+)(?:-(\d+))?)?`)

// protectedRe: existing Markdown links/images, autolinks and bare URLs are
// left untouched.
var protectedRe = regexp.MustCompile(`!?\[[^\]\n]*\]\([^)\n]*\)|<[a-zA-Z][a-zA-Z0-9+.\-]*:[^>\s]*>|[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s)\]>]+`)

// Link rewrites citations of known projects in s. Fenced code blocks are
// left alone; an inline code span that is exactly one citation becomes a
// link around the code span; other inline code is left alone.
func (l *Linker) Link(s string) string {
	if l == nil || l.base == "" || len(l.repos) == 0 || !strings.Contains(s, ":") {
		return s
	}
	lines := strings.Split(s, "\n")
	fence := ""
	for i, ln := range lines {
		t := strings.TrimLeft(ln, " \t")
		if fence != "" {
			if strings.HasPrefix(t, fence) && strings.TrimSpace(strings.TrimLeft(t, fence[:1])) == "" {
				fence = ""
			}
			continue
		}
		if f := fenceOpen(t); f != "" {
			fence = f
			continue
		}
		lines[i] = l.linkLine(ln)
	}
	return strings.Join(lines, "\n")
}

// fenceOpen returns the fence ("```", "~~~~", ...) a line opens, or "".
func fenceOpen(t string) string {
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(t) && t[n] == c {
			n++
		}
		if n >= 3 {
			return t[:n]
		}
	}
	return ""
}

// linkLine handles one line outside fenced blocks: inline code spans are
// split off first.
func (l *Linker) linkLine(ln string) string {
	var b strings.Builder
	for len(ln) > 0 {
		i := strings.IndexByte(ln, '`')
		if i < 0 {
			b.WriteString(l.linkText(ln))
			break
		}
		b.WriteString(l.linkText(ln[:i]))
		ln = ln[i:]
		n := 0
		for n < len(ln) && ln[n] == '`' {
			n++
		}
		delim := ln[:n]
		end := strings.Index(ln[n:], delim)
		if end < 0 {
			// Unclosed backticks: literal text.
			b.WriteString(delim)
			ln = ln[n:]
			continue
		}
		code := ln[n : n+end]
		span := ln[:n+end+n]
		ln = ln[n+end+n:]
		if u, ok := l.whole(strings.TrimSpace(code)); ok {
			b.WriteString("[" + span + "](" + u + ")")
		} else {
			b.WriteString(span)
		}
	}
	return b.String()
}

// whole reports whether s is exactly one citation of a known project.
func (l *Linker) whole(s string) (string, bool) {
	m := citationRe.FindStringSubmatchIndex(s)
	if m == nil || m[0] != 0 || m[1] != len(s) {
		return "", false
	}
	return l.url(s, m)
}

// linkText rewrites citations in plain text, skipping protected regions.
func (l *Linker) linkText(s string) string {
	if !strings.Contains(s, ":") {
		return s
	}
	var b strings.Builder
	last := 0
	prot := protectedRe.FindAllStringIndex(s, -1)
	for _, m := range citationRe.FindAllStringSubmatchIndex(s, -1) {
		if m[6] < 0 {
			// No line number: a trailing '.' ends the sentence, not the path.
			for m[1] > m[4]+1 && s[m[1]-1] == '.' {
				m[1]--
			}
			m[5] = m[1]
		}
		start, end := m[0], m[1]
		if inRegions(prot, start, end) {
			continue
		}
		if start > 0 && isCitationChar(s[start-1]) {
			continue // part of a longer word or path
		}
		if end < len(s) && (isCitationChar(s[end]) && s[end] != '.' && s[end] != '-' || s[end] == ':') {
			continue
		}
		u, ok := l.url(s, m)
		if !ok {
			continue
		}
		b.WriteString(s[last:start])
		b.WriteString("[" + s[start:end] + "](" + u + ")")
		last = end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

func isCitationChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == '-' || c == '/' || c == '@' || c == '+' || c == '[' || c == '\\'
}

func inRegions(regions [][]int, start, end int) bool {
	for _, r := range regions {
		if start < r[1] && end > r[0] {
			return true
		}
	}
	return false
}

// url builds the blob link for submatch indexes m of s.
func (l *Linker) url(s string, m []int) (string, bool) {
	ref, ok := l.repos[strings.ToLower(s[m[2]:m[3]])]
	if !ok {
		return "", false
	}
	file := s[m[4]:m[5]]
	for _, seg := range strings.Split(file, "/") {
		if seg == "." || seg == ".." {
			return "", false
		}
	}
	var parts []string
	for _, seg := range strings.Split(file, "/") {
		parts = append(parts, url.PathEscape(seg))
	}
	u := l.base + "/" + ref.path + "/-/blob/" + ref.sha + "/" + strings.Join(parts, "/")
	if m[6] >= 0 {
		line, err := strconv.Atoi(s[m[6]:m[7]])
		if err != nil || line <= 0 {
			return "", false
		}
		u += "#L" + strconv.Itoa(line)
		if m[8] >= 0 {
			if e, err := strconv.Atoi(s[m[8]:m[9]]); err == nil && e > line {
				u += "-" + strconv.Itoa(e)
			}
		}
	}
	return u, true
}

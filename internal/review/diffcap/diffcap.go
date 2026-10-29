// Package diffcap splits a unified diff into per-file chunks and caps it to
// what Claude gets (files and bytes, the most changed files first). It is a
// leaf: it must not depend on internal/review or any of its feature packages.
package diffcap

import (
	"sort"
	"strings"
)

// Chunk is the part of a unified diff that belongs to one file.
type Chunk struct {
	Path    string
	Text    string
	Changed int // number of added + removed lines
}

// Split cuts `git diff` output into per-file chunks.
func Split(diff string) []Chunk {
	if diff == "" {
		return nil
	}
	var chunks []Chunk
	lines := strings.SplitAfter(diff, "\n")
	cur := -1
	inHunk := false // past the first "@@": "+++ "/"--- " are changed lines
	for _, l := range lines {
		if strings.HasPrefix(l, "diff --git ") || cur < 0 {
			chunks = append(chunks, Chunk{Path: headerPath(l)})
			cur = len(chunks) - 1
			inHunk = false
		}
		c := &chunks[cur]
		c.Text += l
		switch {
		case strings.HasPrefix(l, "@@"):
			inHunk = true
		case !inHunk && strings.HasPrefix(l, "+++ "):
			if p := diffLinePath(l[4:], "b/"); p != "" {
				c.Path = p
			}
		case !inHunk && strings.HasPrefix(l, "--- "):
			if c.Path == "" {
				c.Path = diffLinePath(l[4:], "a/")
			}
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, "-"):
			c.Changed++
		}
	}
	return chunks
}

func headerPath(l string) string {
	if !strings.HasPrefix(l, "diff --git ") {
		return ""
	}
	l = strings.TrimRight(l, "\r\n")
	if i := strings.LastIndex(l, " b/"); i >= 0 {
		return l[i+3:]
	}
	return ""
}

func diffLinePath(s, prefix string) string {
	s = strings.TrimRight(s, "\r\n")
	if i := strings.IndexByte(s, '\t'); i >= 0 {
		s = s[:i]
	}
	s = strings.Trim(s, `"`)
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(s, prefix)
}

// Result is the diff actually sent to Claude.
type Result struct {
	Diff      string
	Omitted   []string // files left out entirely
	Truncated bool     // the diff text was cut mid-file
	Reviewed  int      // files whose diff is included
	Total     int
}

// Partial reports whether Claude gets less than the whole diff.
func (c Result) Partial() bool { return c.Reviewed < c.Total || c.Truncated }

// Cap limits the diff to maxFiles files and maxBytes bytes, preferring
// the files with the most changed lines (largest impact). Included files keep
// their original order.
func Cap(diff string, maxFiles, maxBytes int) Result {
	chunks := Split(diff)
	total := len(chunks)
	fitsFiles := maxFiles <= 0 || len(chunks) <= maxFiles
	fitsBytes := maxBytes <= 0 || len(diff) <= maxBytes
	if fitsFiles && fitsBytes {
		return Result{Diff: diff, Reviewed: len(chunks), Total: total}
	}

	order := make([]int, len(chunks))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return chunks[order[a]].Changed > chunks[order[b]].Changed })

	keep := make([]bool, len(chunks))
	used, n := 0, 0
	for _, i := range order {
		if maxFiles > 0 && n >= maxFiles {
			break
		}
		sz := len(chunks[i].Text)
		if maxBytes > 0 && used+sz > maxBytes {
			continue
		}
		keep[i] = true
		used += sz
		n++
	}

	res := Result{Total: total}
	var b strings.Builder
	if n == 0 && len(chunks) > 0 {
		// Even the smallest relevant file does not fit: send a cut version of
		// the most impactful one.
		i := order[0]
		keep[i] = true
		t := chunks[i].Text
		if maxBytes > 0 && len(t) > maxBytes {
			t = cutAtLine(t, maxBytes)
			res.Truncated = true
		}
		b.WriteString(t)
		n = 1
	} else {
		for i, c := range chunks {
			if keep[i] {
				b.WriteString(c.Text)
			}
		}
	}
	res.Diff = b.String()
	res.Reviewed = n
	res.Omitted = omittedPaths(chunks, keep)
	return res
}

// omittedPaths lists chunk paths that are not kept.
func omittedPaths(chunks []Chunk, keep []bool) []string {
	var out []string
	for i, c := range chunks {
		if !keep[i] && c.Path != "" {
			out = append(out, c.Path)
		}
	}
	return out
}

func cutAtLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[:max]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		s = s[:i+1]
	}
	return s
}

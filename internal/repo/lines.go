package repo

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// LinesChanged reports whether lines start..end (1-based, inclusive, in the
// file as it was at commit from) of path were modified, deleted, or had
// lines inserted between them in from..to (to "" = HEAD). A start <= 0 means the whole
// file. Used to tell "a human fixed the code and closed the thread" from
// "closed the thread without changes". It runs `git diff -U0` with the same
// forced safe configuration as every other host-side git call (no external
// diff, no textconv). An unknown commit yields core.ErrNotFound.
func (c *Checkout) LinesChanged(ctx context.Context, from, to, path string, start, end int) (bool, error) {
	if !validRev(from) {
		return false, fmt.Errorf("repo: invalid revision %q", from)
	}
	if to == "" {
		to = c.head()
	} else if !validRev(to) {
		return false, fmt.Errorf("repo: invalid revision %q", to)
	}
	if path == "" || strings.ContainsAny(path, "\x00\n") {
		return false, fmt.Errorf("repo: invalid path %q", path)
	}
	if err := c.neutralAttrs(); err != nil {
		return false, err
	}
	for _, rev := range []string{from, to} {
		if !c.git.hasCommit(ctx, c.repoDir(), rev) {
			return false, fmt.Errorf("repo: commit %s: %w", shortRev(rev), core.ErrNotFound)
		}
	}
	out, err := c.git.run(ctx, gitCmd{dir: c.repoDir(), args: []string{
		"diff", "--no-color", "--no-ext-diff", "--no-textconv", "--no-renames", "-U0", from, to,
		"--", ":(literal)" + path,
	}})
	if err != nil {
		return false, fmt.Errorf("repo: diff lines: %w", err)
	}
	return hunksTouch(out, start, end), nil
}

var hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

// hunksTouch reports whether any hunk of a -U0 diff touches old-side lines
// start..end (start <= 0: any hunk).
func hunksTouch(diff []byte, start, end int) bool {
	if end < start {
		end = start
	}
	sc := bufio.NewScanner(bytes.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "Binary files ") {
			return true
		}
		m := hunkHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if start <= 0 {
			return true
		}
		a, _ := strconv.Atoi(m[1])
		n := 1
		if m[2] != "" {
			n, _ = strconv.Atoi(m[2])
		}
		if n == 0 {
			// Pure insertion after old line a: it lands inside the window
			// when it goes between two of its lines (or right at its edges).
			if a >= start-1 && a <= end {
				return true
			}
			continue
		}
		if a <= end && a+n-1 >= start {
			return true
		}
	}
	return false
}

package review

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
)

// The note lists at most maxNoteIssues issues and counts the rest.
func TestConfigNoteCapsIssues(t *testing.T) {
	var issues config.RepoErrors
	for i := 1; i <= maxNoteIssues+5; i++ {
		issues = append(issues, config.RepoIssue{Line: i, Key: fmt.Sprintf("k%d", i), Message: "bad"})
	}
	var b strings.Builder
	writeIssues(&b, issues)
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != maxNoteIssues+1 {
		t.Fatalf("%d lines:\n%s", len(lines), b.String())
	}
	if lines[maxNoteIssues-1] != fmt.Sprintf("- строка %d: `k%d` — bad", maxNoteIssues, maxNoteIssues) {
		t.Errorf("last listed issue %q", lines[maxNoteIssues-1])
	}
	if lines[maxNoteIssues] != "- … и ещё 5" {
		t.Errorf("tail %q", lines[maxNoteIssues])
	}
	// Exactly maxNoteIssues: all listed, no tail.
	b.Reset()
	writeIssues(&b, issues[:maxNoteIssues])
	if strings.Contains(b.String(), "и ещё") || strings.Count(b.String(), "\n") != maxNoteIssues {
		t.Errorf("exactly %d issues:\n%s", maxNoteIssues, b.String())
	}
	// No line, no key: the message alone.
	b.Reset()
	writeIssues(&b, config.RepoErrors{{Message: "file too large"}})
	if b.String() != "- file too large\n" {
		t.Errorf("bare issue %q", b.String())
	}
}

// cfgCode always renders a closed, non-empty code span.
func TestCfgCode(t *testing.T) {
	for in, want := range map[string]string{
		"":          "` `",
		"main":      "`main`",
		"a`b":       "`a'b`",
		"x\n`y`\nz": "`x 'y' z`",
	} {
		if got := cfgCode(in); got != want {
			t.Errorf("cfgCode(%q) = %q, want %q", in, got, want)
		}
	}
	body, ok := configNoteBody("", cfgFile{found: true, issues: config.RepoErrors{{Message: "bad"}}}, nil, true, false)
	if !ok || !strings.Contains(body, "в целевой ветке ` ` содержит ошибки") {
		t.Fatalf("empty branch:\n%s", body)
	}
}

// An entry exactly ttl old is expired (a read would not use it either) and
// is pruned; a reset leaves only the new entry's bytes.
func TestRepoConfigCacheExpiryBoundaryAndReset(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	ttl := 5 * time.Minute
	var c cfgCache
	for i := 0; i < pruneCfgCacheAt; i++ {
		c.put(cfgCacheKey{1, fmt.Sprint(i)}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: []byte("x")}, at: now.Add(-ttl)}, ttl)
	}
	c.put(cfgCacheKey{1, "young"}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: []byte("y")}, at: now.Add(-ttl + time.Second)}, ttl)
	c.put(cfgCacheKey{1, "new"}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: []byte("zz")}, at: now}, ttl)
	if len(c.m) != 2 || c.bytes != 3 {
		t.Fatalf("after prune: %d entries %d bytes", len(c.m), c.bytes)
	}

	// Fresh entries over the byte budget: everything is dropped, the new
	// entry alone is counted.
	c = cfgCache{}
	chunk := make([]byte, config.MaxRepoConfigBytes)
	n := maxCfgCacheBytes / len(chunk)
	for i := 0; i < n; i++ {
		c.put(cfgCacheKey{2, fmt.Sprint(i)}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: chunk}, at: now}, ttl)
	}
	if len(c.m) != n || c.bytes != maxCfgCacheBytes {
		t.Fatalf("at the budget: %d entries %d bytes", len(c.m), c.bytes)
	}
	c.put(cfgCacheKey{2, "last"}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: []byte("abc")}, at: now}, ttl)
	if len(c.m) != 1 || c.bytes != 3 {
		t.Fatalf("after reset: %d entries %d bytes", len(c.m), c.bytes)
	}
}

// Fresh entries are never pruned; at maxCfgCache entries the cache starts
// over.
func TestRepoConfigCacheEntryBound(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	var c cfgCache
	for i := 0; i < maxCfgCache; i++ {
		c.put(cfgCacheKey{1, fmt.Sprint(i)}, cfgCacheEntry{cfgRaw: cfgRaw{found: true}, at: now}, time.Minute)
	}
	if len(c.m) != maxCfgCache {
		t.Fatalf("fresh entries dropped: %d left", len(c.m))
	}
	c.put(cfgCacheKey{1, "over"}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: []byte("ab")}, at: now}, time.Minute)
	if _, ok := c.m[cfgCacheKey{1, "over"}]; len(c.m) != 1 || !ok || c.bytes != 2 {
		t.Fatalf("over the entry bound: %d entries %d bytes", len(c.m), c.bytes)
	}
}

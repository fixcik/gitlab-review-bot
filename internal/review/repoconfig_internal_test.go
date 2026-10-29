package review

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
)

func TestConfigNoteSanitizesFileText(t *testing.T) {
	issues := config.RepoErrors{{Line: 2, Key: "a`b@all", Message: "bad <!-- reviewbot:summary --> @all `x` [l](http://e)"}}
	body, ok := configNoteBody("main", cfgFile{found: true}, &cfgFile{found: true, issues: issues}, true, false)
	if !ok {
		t.Fatal("no note")
	}
	if strings.Count(body, "<!--") != 1 || strings.Contains(body, "bad @all") || strings.Contains(body, "`x`") || strings.Contains(body, "](http://e)") {
		t.Fatalf("unsafe note:\n%s", body)
	}
	if _, ok := configNoteBody("main", cfgFile{found: true}, nil, true, false); ok {
		t.Fatal("valid config without an earlier note: nothing to write")
	}
	if _, ok := configNoteBody("main", cfgFile{}, nil, false, true); ok {
		t.Fatal("not opted in, MR without a config: leave the note")
	}
}

// #12 / !12 references in the file's text are defused like mentions:
// GitLab must not cross-link (and notify) the referenced issue or MR.
func TestConfigNoteDefusesReferences(t *testing.T) {
	issues := config.RepoErrors{{Line: 3, Key: "lint.ci", Message: "lint.ci[0] (fix #12 !34 group/p#5): artifact must be a relative path"}}
	body, ok := configNoteBody("main", cfgFile{found: true}, &cfgFile{found: true, issues: issues}, true, false)
	if !ok {
		t.Fatal("no note")
	}
	for _, ref := range []string{"#12", "!34", "p#5"} {
		if strings.Contains(body, ref) {
			t.Errorf("reference %s kept:\n%s", ref, body)
		}
	}
	if !strings.Contains(body, "fix #\u200b12 !\u200b34 group/p#\u200b5") {
		t.Errorf("text mangled:\n%s", body)
	}
	if got := cfgText("C# and !important stay", 100); got != "C# and !important stay" {
		t.Errorf("non-references changed: %q", got)
	}
}

// From pruneCfgCacheAt entries, every insert drops the expired ones; the
// byte budget resets the cache.
func TestRepoConfigCachePrunesExpired(t *testing.T) {
	testNow := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC) // reviewtest.TestNow
	var c cfgCache
	ttl := 5 * time.Minute
	old := testNow.Add(-time.Hour)
	for i := 0; i < pruneCfgCacheAt; i++ {
		c.put(cfgCacheKey{1, fmt.Sprint(i)}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: []byte("x")}, at: old}, ttl)
	}
	if len(c.m) != pruneCfgCacheAt || c.bytes != pruneCfgCacheAt {
		t.Fatalf("filled: %d entries %d bytes", len(c.m), c.bytes)
	}
	c.put(cfgCacheKey{1, "fresh"}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: []byte("yy")}, at: testNow}, ttl)
	if len(c.m) != 1 || c.bytes != 2 {
		t.Fatalf("expired entries kept: %d entries %d bytes", len(c.m), c.bytes)
	}
	// Replacing an entry keeps the byte count right.
	c.put(cfgCacheKey{1, "fresh"}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: []byte("zzz")}, at: testNow}, ttl)
	if len(c.m) != 1 || c.bytes != 3 {
		t.Fatalf("replace: %d entries %d bytes", len(c.m), c.bytes)
	}
	// Fresh entries over the byte budget: reset.
	chunk := make([]byte, config.MaxRepoConfigBytes)
	for i := 0; i <= maxCfgCacheBytes/len(chunk); i++ {
		c.put(cfgCacheKey{2, fmt.Sprint(i)}, cfgCacheEntry{cfgRaw: cfgRaw{found: true, raw: chunk}, at: testNow}, ttl)
	}
	if c.bytes > maxCfgCacheBytes {
		t.Fatalf("byte budget exceeded: %d", c.bytes)
	}
}

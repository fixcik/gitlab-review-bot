package review_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

const (
	trivOld  = "old0000000000000"
	trivHead = "head111111111111"
)

// trivialHarness: an MR reviewed at trivOld, new commits up to trivHead
// changing the files of diff; before/after are the file contents.
func trivialHarness(t *testing.T, diff string, files []string, before, after map[string]string) *reviewtest.Harness {
	t.Helper()
	h := reviewtest.New(t)
	h.St.States["42!7"] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: trivOld, SummaryNoteID: 500}
	h.GL.Discussions = []core.Discussion{{ID: "s", Notes: []core.Note{reviewtest.BotNote(500,
		marker.Summary(trivOld)+"\n"+summary.Title+"\n\n"+summary.GateClearLine+"\n\nСтарое саммари.\n\n---\n<sub>Проверено: `base000..old0000` (полное)</sub>\n", nil)}}}
	h.Repos.Ancestors[trivOld] = true
	h.Repos.Diff, h.Repos.Files = diff, files
	h.Repos.ShowAt = map[string]string{}
	h.Repos.FilesAt = map[string][]string{trivOld: files, trivHead: files}
	for p, s := range before {
		h.Repos.ShowAt[trivOld+":"+p] = s
	}
	for p, s := range after {
		h.Repos.ShowAt[trivHead+":"+p] = s
	}
	return h
}

func fileDiff(path, hunk string) string {
	return "diff --git a/" + path + " b/" + path + "\nindex 1111111..2222222 100644\n--- a/" + path + "\n+++ b/" + path + "\n" + hunk
}

func TestTrivialIncrementSkipped(t *testing.T) {
	for _, tc := range []struct {
		name          string
		diff          string
		files         []string
		before, after map[string]string
		only          string
	}{
		{"whitespace only", fileDiff("cache.go", "@@ -1,2 +1,2 @@\n func f() {\n-return 1\n+\treturn 1\n"),
			[]string{"cache.go"}, map[string]string{"cache.go": "func f() {\nreturn 1\n}\n"},
			map[string]string{"cache.go": "func f() {\n\treturn 1\n}\n"}, "форматирование"},
		{"comment only", fileDiff("cache.go", "@@ -1,2 +1,3 @@\n+// f returns one.\n func f() {\n"),
			[]string{"cache.go"}, map[string]string{"cache.go": "func f() {\n\treturn 1\n}\n"},
			map[string]string{"cache.go": "// f returns one.\nfunc f() {\n\treturn 1 // one\n}\n"}, "комментарии"},
		{"docs only", fileDiff("README.md", "@@ -1 +1,2 @@\n # API\n+More words.\n"),
			[]string{"README.md"}, nil, nil, "документация"},
		{"docs and formatting", fileDiff("README.md", "@@ -1 +1,2 @@\n # API\n+x\n") + fileDiff("cache.go", "@@ -1 +1 @@\n-x := 1\n+x  :=  1\n"),
			[]string{"README.md", "cache.go"}, map[string]string{"cache.go": "x := 1\n"}, map[string]string{"cache.go": "x  :=  1\n"},
			"форматирование/документация"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := trivialHarness(t, tc.diff, tc.files, tc.before, tc.after)
			var skips []string
			h.Deps.OnSkip = func(r string) { skips = append(skips, r) }
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if len(h.Runner.Reqs) != 0 {
				t.Fatalf("claude ran %d times", len(h.Runner.Reqs))
			}
			if recs := h.Lim.Records(); len(recs) != 0 || h.Lim.GlobalCalls != 0 {
				t.Fatalf("limits touched: records %v, global checks %d", recs, h.Lim.GlobalCalls)
			}
			if len(skips) != 1 || skips[0] != review.SkipTrivial {
				t.Fatalf("skips %v", skips)
			}
			st, _ := h.St.Get()
			if st.ReviewedSHA != trivHead || st.SummaryNoteID != 500 {
				t.Fatalf("state %+v", st)
			}
			body := h.GL.Notes[500]
			want := "ℹ️ Инкремент `" + mdtext.ShortSHA(trivOld) + ".." + mdtext.ShortSHA(trivHead) + "` без значимых изменений (только " + tc.only + ") — ревью не требуется."
			if !strings.Contains(body, want) {
				t.Fatalf("status line %q missing in:\n%s", want, body)
			}
			if !strings.Contains(body, "Старое саммари.") || !strings.Contains(body, marker.Summary(trivHead)) {
				t.Fatalf("summary not kept / marker not moved:\n%s", body)
			}
			if len(h.GL.Replies) != 0 || len(h.GL.Resolved) != 0 {
				t.Fatalf("threads touched: replies %v resolved %v", h.GL.Replies, h.GL.Resolved)
			}
			// A later status (postponed) replaces the trivial line.
			again := summary.SetStatus(body, "⏳ Ревью отложено до 13:00: лимит.", trivHead, "@reviewbot")
			if strings.Contains(again, summary.TrivialStatusPrefix) {
				t.Fatalf("trivial status line not replaced:\n%s", again)
			}
		})
	}
}

func TestTrivialIncrementNotSkipped(t *testing.T) {
	code := map[string]string{"cache.go": "x := 1\ny := 2\n"}
	for _, tc := range []struct {
		name          string
		diff          string
		files         []string
		before, after map[string]string
	}{
		{"mixed", fileDiff("README.md", "@@ -1 +1,2 @@\n # API\n+x\n") + fileDiff("cache.go", "@@ -1,2 +1,2 @@\n x := 1\n-y := 2\n+y := 3\n"),
			[]string{"README.md", "cache.go"}, code, map[string]string{"cache.go": "x := 1\ny := 3\n"}},
		{"deleted code", fileDiff("cache.go", "@@ -1,2 +1 @@\n x := 1\n-y := 2\n"),
			[]string{"cache.go"}, code, map[string]string{"cache.go": "x := 1\n"}},
		{"renamed file", "diff --git a/cache.go b/lru.go\nsimilarity index 100%\nrename from cache.go\nrename to lru.go\n",
			[]string{"lru.go"}, code, map[string]string{"lru.go": code["cache.go"]}},
		{"new file", "diff --git a/doc.go b/doc.go\nnew file mode 100644\n--- /dev/null\n+++ b/doc.go\n@@ -0,0 +1 @@\n+// Package a.\n",
			[]string{"doc.go"}, nil, map[string]string{"doc.go": "// Package a.\n"}},
		{"unknown file type", fileDiff("data.bin2", "@@ -1 +1 @@\n-a\n+a \n"),
			[]string{"data.bin2"}, map[string]string{"data.bin2": "a\n"}, map[string]string{"data.bin2": "a \n"}},
		// The lexer must not take code or directives for comments.
		{"python sql string argument", fileDiff("repo.py", "@@ -3 +3 @@\n-        \"\"\"SELECT * FROM users WHERE tenant_id = %s\"\"\",\n+        \"\"\"SELECT * FROM users\"\"\",\n"),
			[]string{"repo.py"},
			map[string]string{"repo.py": "def users(db, tid):\n    return db.execute(\n        \"\"\"SELECT * FROM users WHERE tenant_id = %s\"\"\",\n        (tid,),\n    )\n"},
			map[string]string{"repo.py": "def users(db, tid):\n    return db.execute(\n        \"\"\"SELECT * FROM users\"\"\",\n        (tid,),\n    )\n"}},
		{"go build constraint", fileDiff("auth.go", "@@ -1 +1,3 @@\n+//go:build ignore\n+\n package auth\n"),
			[]string{"auth.go"}, map[string]string{"auth.go": "package auth\n"}, map[string]string{"auth.go": "//go:build ignore\n\npackage auth\n"}},
		{"php annotation", fileDiff("Ctl.php", "@@ -3,3 +3,2 @@\n  /**\n-  * @IsGranted(\"ROLE_ADMIN\")\n   */\n"),
			[]string{"Ctl.php"},
			map[string]string{"Ctl.php": "<?php\nclass A {\n /**\n  * @IsGranted(\"ROLE_ADMIN\")\n  */\n public function x() {}\n}\n"},
			map[string]string{"Ctl.php": "<?php\nclass A {\n /**\n  */\n public function x() {}\n}\n"}},
		{"shell heredoc", fileDiff("a.sh", "@@ -2 +2 @@\n-# keep\n+# drop\n"),
			[]string{"a.sh"}, map[string]string{"a.sh": "psql <<SQL\n# keep\nSQL\n"}, map[string]string{"a.sh": "psql <<SQL\n# drop\nSQL\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := trivialHarness(t, tc.diff, tc.files, tc.before, tc.after)
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
				t.Fatal(err)
			}
			if len(h.Runner.Reqs) != 1 {
				t.Fatalf("claude runs %d, want 1", len(h.Runner.Reqs))
			}
			if strings.Contains(h.GL.Notes[500], summary.TrivialStatusPrefix) {
				t.Fatalf("trivial status in a reviewed summary:\n%s", h.GL.Notes[500])
			}
		})
	}
}

func TestTrivialIncrementNeverForManualOrFull(t *testing.T) {
	diff := fileDiff("cache.go", "@@ -1 +1 @@\n-x := 1\n+x  :=  1\n")
	before, after := map[string]string{"cache.go": "x := 1\n"}, map[string]string{"cache.go": "x  :=  1\n"}
	for _, tc := range []struct {
		name string
		job  core.Job
		off  bool
	}{
		{"manual review", reviewtest.ManualJob(core.JobReview), false},
		{"full review", reviewtest.ManualJob(core.JobFullReview), false},
		{"switched off", reviewtest.AutoJob(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := trivialHarness(t, diff, []string{"cache.go"}, before, after)
			h.Cfg.Review.SkipTrivialIncrements = !tc.off
			h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
			if err := h.R().Handle(context.Background(), tc.job); err != nil {
				t.Fatal(err)
			}
			if len(h.Runner.Reqs) != 1 {
				t.Fatalf("claude runs %d, want 1", len(h.Runner.Reqs))
			}
		})
	}
}

// The deterministic checks still run on a trivial increment: a new TODO
// comment without a task gets its thread.
func TestTrivialIncrementRunsTodo(t *testing.T) {
	diff := fileDiff("cache.go", "@@ -1,2 +1,3 @@\n x := 1\n+// TODO: drop this\n y := 2\n")
	h := trivialHarness(t, diff, []string{"cache.go"}, map[string]string{"cache.go": "x := 1\ny := 2\n"},
		map[string]string{"cache.go": "x := 1\n// TODO: drop this\ny := 2\n"})
	h.Idx.Lines["cache.go"] = map[int]int{1: 0, 2: 1, 3: 0}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 0 {
		t.Fatalf("claude ran")
	}
	found := false
	for _, d := range h.GL.Published {
		if strings.Contains(d.Body, "tool=todo") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no todo thread published: %+v", h.GL.Published)
	}
}

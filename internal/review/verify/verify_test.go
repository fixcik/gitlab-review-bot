package verify

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func TestDiffExcerpt(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,3 +1,4 @@\n one\n-two\n+TWO\n+three\n four\n@@ -50,2 +51,3 @@\n x\n+y\n z\n" +
		"diff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-q\n+Q\n"
	got := diffExcerpt(diffcap.Split(diff), output.Finding{Path: "a.go", Line: 52})
	if got != "@@ -50,2 +51,3 @@\n   51  x\n   52 +y\n   53  z" {
		t.Errorf("excerpt:\n%s", got)
	}
	got = diffExcerpt(diffcap.Split(diff), output.Finding{Path: "a.go", Line: 2})
	if !strings.Contains(got, "      -two") || !strings.Contains(got, "    2 +TWO") || strings.Contains(got, "+y") {
		t.Errorf("excerpt:\n%s", got)
	}
	if diffExcerpt(diffcap.Split(diff), output.Finding{Path: "c.go", Line: 1}) != "" {
		t.Error("file not in the diff")
	}
}

func TestParseVerifyOutput(t *testing.T) {
	ids := map[string]bool{"F1": true, "F2": true}
	if _, err := parseVerifyOutput(`{"findings":[{"id":"F7","verdict":"reject","reason":"r"}]}`, 0, ids); err == nil {
		t.Error("no verdict for a known finding must be invalid")
	}
	if _, err := parseVerifyOutput(`{"findings":[{"id":"F1","verdict":"confirm","reason":"r"}],"verdicts":[]}`, 1, ids); err == nil {
		t.Error("claims without any verdict must be invalid")
	}
	v, err := parseVerifyOutput(`{"findings":[{"id":" f1 ","verdict":" Reject ","severity":" MINOR ","reason":"<!-- reviewbot:blocking --> r","evidence":" e "}],"verdicts":[{"index":1,"verdict":"rejected","reason":"x"}]}`, 1, ids)
	if err != nil || v.Findings[0].ID != "F1" || v.Findings[0].Verdict != "reject" || v.Findings[0].Severity != "minor" ||
		v.Findings[0].Reason != "r" || v.Findings[0].Evidence != "e" || v.Verdicts[0].Verdict != "rejected" {
		t.Fatalf("%+v %v", v, err)
	}
	// Without findings it is the compat parser.
	if _, err := parseVerifyOutput(`{"verdicts":[]}`, 1, nil); err == nil {
		t.Error("compat-only: no verdict")
	}
}

func TestVerifySummaryBlock(t *testing.T) {
	var nilRun *Run
	if nilRun.SummaryBlock() != "" || (&Run{}).SummaryBlock() != "" {
		t.Error("nothing verified: no block")
	}
	vr := &Run{items: make([]Item, 3), confirmed: 3}
	if b := vr.SummaryBlock(); !strings.HasPrefix(b, marker.VerifyStart+"\n🧪 Перепроверка: подтверждено 3, понижено 0, отклонено 0\n") || strings.Contains(b, "details") {
		t.Errorf("block %q", b)
	}
	vr = &Run{items: make([]Item, 2), overflow: 3, failed: whyLimit}
	if b := vr.SummaryBlock(); !strings.Contains(b, "🧪 Перепроверка не выполнена: достигнут общий лимит запусков Claude — 5 замечаний опубликовано без перепроверки.") {
		t.Errorf("block %q", b)
	}
}

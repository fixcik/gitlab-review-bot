package verify

import (
	"strings"
	"testing"
	"text/template"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// An answer that is not the schema's JSON is an invalid answer (the run is
// retried, then the claims are published unverified), not "no claims".
func TestParseCompatVerificationNotJSON(t *testing.T) {
	for _, s := range []string{"", "not json", `{"verdicts":[{"index":"1","verdict":"confirmed","reason":"r"}]}`} {
		if v, err := parseCompatVerification(s, 1); err == nil || v != nil {
			t.Errorf("%q: %+v %v", s, v, err)
		}
	}
}

// Claims are matched to verdicts by number: the first verdict of a number
// wins; a claim without a verdict is published unverified (it still
// blocks) with the reason why; a rejected claim is not published.
func TestCompatVerdictsSplit(t *testing.T) {
	items := []compatcheck.Item{{ID: "a", Title: "A"}, {ID: "b", Title: "B"}, {ID: "c", Title: "C"}, {ID: "d", Title: "D"}}
	publish, rejected := compatVerdicts(items, []output.CompatVerdict{
		{Index: 1, Verdict: output.VerdictConfirmed, Reason: "reads it in auth.go:3"},
		{Index: 1, Verdict: output.VerdictRejected, Reason: "second verdict for 1 is ignored"},
		{Index: 2, Verdict: output.VerdictRejected, Reason: "alias kept"},
		{Index: 4, Verdict: "maybe", Reason: "unknown verdict"},
		{Index: 9, Verdict: output.VerdictRejected, Reason: "no such claim"},
	})
	if len(rejected) != 1 || rejected[0].Item.ID != "b" || rejected[0].Reason != "alias kept" {
		t.Fatalf("rejected %+v", rejected)
	}
	want := []struct {
		id         string
		unverified bool
		note       string
	}{{"a", false, "reads it in auth.go:3"}, {"c", true, unverifiedNoAns}, {"d", true, unverifiedNoAns}}
	if len(publish) != len(want) {
		t.Fatalf("publish %+v", publish)
	}
	for i, w := range want {
		if p := publish[i]; p.ID != w.id || p.Unverified != w.unverified || p.VerifyNote != w.note {
			t.Errorf("publish[%d] = %+v, want %+v", i, p, w)
		}
	}
	if items[2].Unverified || items[0].VerifyNote != "" {
		t.Error("the input items must not change")
	}
}

// The reasons of unverified claims after a failed run.
func TestCompatUnverifiedReason(t *testing.T) {
	for result, want := range map[string]string{resultLimit: unverifiedLimit, resultClaude: unverifiedClaude,
		resultInvalid: unverifiedError, resultError: unverifiedError} {
		if got := compatUnverifiedReason(result); got != want {
			t.Errorf("%s: %q, want %q", result, got, want)
		}
	}
	got := unverifiedItems([]compatcheck.Item{{ID: "a"}, {ID: "b", VerifyNote: "old"}}, "why")
	if len(got) != 2 || !got[0].Unverified || got[0].VerifyNote != "why" || !got[1].Unverified || got[1].VerifyNote != "why" {
		t.Errorf("%+v", got)
	}
}

// Lines of a hunk that are not +, - or context are skipped without moving
// the line numbers; a whole-file finding (line 0) gets at most
// excerptMaxLines lines, then an ellipsis.
func TestDiffExcerptOddLinesAndCap(t *testing.T) {
	diff := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1,2 +1,3 @@\n one\n?garbage\n+two\n\\ No newline at end of file\n three\n"
	got := diffExcerpt(diffcap.Split(diff), output.Finding{Path: "a.go", Line: 2})
	if got != "@@ -1,2 +1,3 @@\n    1  one\n    2 +two\n    3  three" {
		t.Errorf("excerpt:\n%s", got)
	}

	var b strings.Builder
	b.WriteString("diff --git a/big.go b/big.go\n--- a/big.go\n+++ b/big.go\n@@ -0,0 +1,100 @@\n")
	for i := 0; i < 100; i++ {
		b.WriteString("+x\n")
	}
	got = diffExcerpt(diffcap.Split(b.String()), output.Finding{Path: "big.go"})
	lines := strings.Split(got, "\n")
	// The hunk header counts as one of the excerptMaxLines lines.
	if len(lines) != excerptMaxLines+1 || lines[0] != "@@ -0,0 +1,100 @@" || lines[excerptMaxLines-1] != "   79 +x" || lines[excerptMaxLines] != "      …" {
		t.Errorf("capped excerpt: %d lines, last %q", len(lines), lines[len(lines)-1])
	}
}

// The summary names every kind of proof once and counts every proven
// finding.
func TestProvenLineKinds(t *testing.T) {
	vr := &Run{proven: []provenFinding{{By: provenCompat}, {By: provenGitleaks}, {By: provenCompat}, {By: provenMigration}, {By: provenGitleaks}}}
	want := "🧪 Без перепроверки: 5 замечаний доказано проверками бота (проверка совместимости API, gitleaks, использование в задеплоенном коде).\n"
	if got := vr.provenLine(); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	vr = &Run{proven: []provenFinding{{By: provenGitleaks}, {By: provenGitleaks}}}
	if got := vr.provenLine(); got != "🧪 Без перепроверки: 2 замечания доказаны проверками бота (gitleaks).\n" {
		t.Errorf("got %q", got)
	}
}

// Without a merge base SHA the findings prompt names the source of the
// diff as the version before the merge request.
func TestRenderVerificationBaseDefaultsToFrom(t *testing.T) {
	in := Input{From: "from0000", Head: "head1111", BaseRoot: "/base", Lang: "English",
		Findings: []Item{{ID: "F1", F: output.Finding{Path: "a.go", Line: 1, Severity: "critical", Title: "t"}}}}
	_, prompt, _, err := renderVerification(in)
	if err != nil || !strings.Contains(prompt, "/base/<path> is the version before the merge request, at from0000)") {
		t.Errorf("%v\n%s", err, prompt)
	}
	in.Base = "base2222"
	if _, prompt, _, _ = renderVerification(in); !strings.Contains(prompt, "at base2222)") {
		t.Errorf("explicit base:\n%s", prompt)
	}
}

// A template that fails to render fails the whole rendering: no half
// prompt reaches the run.
func TestRenderVerificationTemplateErrors(t *testing.T) {
	bad := template.Must(template.New("bad").Parse("{{.NoSuchField}}"))
	findings := Input{From: "f", Head: "h", Findings: []Item{{ID: "F1", F: output.Finding{Path: "a.go", Severity: "critical", Title: "t"}}}}
	claims := Input{From: "f", Head: "h", Compat: []compatcheck.Item{{Path: "a.go", Title: "t"}}}
	for _, tc := range []struct {
		name string
		tmpl **template.Template
		in   Input
	}{
		{"findings system", &verifySystemTmpl, findings},
		{"findings user", &verifyUserTmpl, findings},
		{"claims system", &compatVerifySystemTmpl, claims},
		{"claims user", &compatVerifyUserTmpl, claims},
	} {
		t.Run(tc.name, func(t *testing.T) {
			BreakTemplate(t, tc.tmpl, bad)
			sys, prompt, schema, err := renderVerification(tc.in)
			if err == nil {
				t.Fatalf("no error: %q %q %q", sys, prompt, schema)
			}
			if tc.in.Findings != nil && (sys != "" || prompt != "" || schema != "") {
				t.Errorf("partial output: %q %q %q", sys, prompt, schema)
			}
		})
	}
}

// The proof windows are exact: a gitleaks result within one line of the
// finding, a deterministic breaking change within two; a change without a
// line proves nothing.
func TestProvenByWindows(t *testing.T) {
	lr := &linters.Run{Res: &lint.Result{Findings: []lint.Finding{
		{Tool: lint.ToolGitleaks, Path: "s.go", Line: 9}, {Tool: lint.ToolGitleaks, Path: "s.go", Line: 23},
	}}}
	cr := &compatcheck.Run{Enabled: true, Res: &compat.Result{Changes: []compat.Change{
		{Source: compat.SourceOasdiff, Path: "api.yaml", Line: 48, Message: "a"},
		{Source: compat.SourceOasdiff, Path: "api.yaml", Line: 63, Message: "b"},
		{Source: compat.SourceBuf, Path: "a.proto", Message: "no line"},
		{Source: compat.SourceOasdiff, Path: "b.yaml", Line: 1, Message: "first line"},
	}}}
	p := provenBy(nil, lr, cr)
	for _, tc := range []struct {
		f    output.Finding
		want string
	}{
		{output.Finding{Path: "s.go", Line: 10, EndLine: 22, Title: "token leak"}, provenGitleaks},                // 9 = line-1, 23 = end+1
		{output.Finding{Path: "s.go", Line: 11, EndLine: 21, Title: "token leak"}, ""},                            // 9 = line-2, 23 = end+2
		{output.Finding{Path: "s.go", Line: 24, Title: "token leak"}, provenGitleaks},                             // 23 = line-1
		{output.Finding{Path: "s.go", Line: 7, Title: "token leak"}, ""},                                          // 9 = line+2
		{output.Finding{Path: "api.yaml", Line: 50, EndLine: 61, Title: "Несовместимое изменение"}, provenCompat}, // 48 = line-2
		{output.Finding{Path: "api.yaml", Line: 51, EndLine: 60, Title: "Несовместимое изменение"}, ""},           // 48 = line-3, 63 = end+3
		{output.Finding{Path: "api.yaml", Line: 52, EndLine: 61, Title: "Несовместимое изменение"}, provenCompat}, // 63 = end+2
		{output.Finding{Path: "api.yaml", Line: 45, Title: "Несовместимое изменение"}, ""},                        // 48 = line+3
		{output.Finding{Path: "s.go", Line: 2, EndLine: 8, Title: "token leak"}, provenGitleaks},                  // 9 = end+1
		{output.Finding{Path: "b.yaml", Line: 2, Title: "Несовместимое изменение"}, provenCompat},
		{output.Finding{Path: "a.proto", Line: 1, Title: "Несовместимое изменение"}, ""}, // change without a line
		{output.Finding{Path: "a.proto", Title: "Несовместимое изменение"}, ""},
	} {
		if got := p(tc.f); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.f, got, tc.want)
		}
	}
}

// The excerpt keeps excerptContext lines on both sides of the finding.
func TestDiffExcerptWindow(t *testing.T) {
	var b strings.Builder
	b.WriteString("diff --git a/w.go b/w.go\n--- a/w.go\n+++ b/w.go\n@@ -1,40 +1,40 @@\n")
	for i := 1; i <= 40; i++ {
		b.WriteString(" l\n")
	}
	got := strings.Split(diffExcerpt(diffcap.Split(b.String()), output.Finding{Path: "w.go", Line: 20, EndLine: 22}), "\n")
	first, last := got[1], got[len(got)-1]
	if len(got) != 1+(22+excerptContext)-(20-excerptContext)+1 || first != "    8  l" || last != "   34  l" {
		t.Errorf("window %q .. %q (%d lines)", first, last, len(got))
	}
}

// One rejected finding gets the collapsed list (a change without a reason
// has no reason part); no "more" line without overflow.
func TestSummaryBlockSingleChange(t *testing.T) {
	vr := &Run{items: make([]Item, 2), confirmed: 1,
		rejected: []verifyChange{{F: output.Finding{Path: "a.go", Line: 3, Severity: "critical", Title: "Гонка"}}}}
	b := vr.SummaryBlock()
	for _, s := range []string{
		"🧪 Перепроверка: подтверждено 1, понижено 0, отклонено 1\n<details>\n<summary>Отклонено и понижено перепроверкой (1)</summary>\n\n",
		" · `a.go:3` — Гонка\n\n</details>\n",
	} {
		if !strings.Contains(b, s) {
			t.Errorf("block lacks %q:\n%s", s, b)
		}
	}
	if strings.Contains(b, "Ещё") || strings.Contains(b, "Гонка —") {
		t.Errorf("block:\n%s", b)
	}
}

// One claim with findings is enough for the claim rules and a required
// "verdicts", even when the caller did not set CompatOn.
func TestRenderVerificationOneClaim(t *testing.T) {
	in := Input{From: "f", Head: "h", Lang: "English",
		Findings: []Item{{ID: "F1", F: output.Finding{Path: "a.go", Line: 1, Severity: "critical", Title: "t"}}},
		Compat:   []compatcheck.Item{{Path: "a.go", Line: 1, Title: "dropped field"}}}
	sys, prompt, schema, err := renderVerification(in)
	if err != nil || !strings.Contains(sys, "Breaking-change claims") || !strings.Contains(prompt, "--- item 1") || schema != verifySchema(true) {
		t.Errorf("%v\n%s\n%s", err, sys, schema)
	}
}

// Without claims in the run, verdicts in the answer are dropped: no claim
// can be confirmed or rejected by accident.
func TestParseVerifyOutputDropsStrayVerdicts(t *testing.T) {
	v, err := parseVerifyOutput(`{"findings":[{"id":"F1","verdict":"confirm","reason":"r"}],"verdicts":[{"index":1,"verdict":"rejected","reason":"x"}]}`, 0, map[string]bool{"F1": true})
	if err != nil || v.Verdicts != nil || len(v.Findings) != 1 {
		t.Errorf("%+v %v", v, err)
	}
}

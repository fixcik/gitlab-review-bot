package mcp_test

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// parityHarness: a migration (reviewtest.MigHarness), a ruff result on cache.go:3 and
// an open finding thread "Old issue" on cache.go.
func parityHarness(t *testing.T) *reviewtest.Harness {
	t.Helper()
	h := reviewtest.MigHarness(t)
	h.Repos.Diff += "diff --git a/cache.go b/cache.go\n--- a/cache.go\n+++ b/cache.go\n@@ -1,3 +1,5 @@\n a\n+b\n+c\n d\n e\n"
	h.Repos.Files = append(h.Repos.Files, "cache.go")
	h.Deps.Lint = &reviewtest.FakeLint{Res: reviewtest.LintResult(reviewtest.RuffFinding)}
	h.Cfg.Review.Lint.Enabled = true
	id := finding.ID("cache.go", "Old issue")
	h.GL.Discussions = []core.Discussion{{ID: "d-old", Notes: []core.Note{
		reviewtest.BotNote(1, marker.Finding(id)+"\n**🟠 Major** · Old issue\n\nStill there.", &core.Position{NewPath: "cache.go", NewLine: 2}),
	}}}
	return h
}

func submitArgsOf(f output.Finding) map[string]any {
	a := map[string]any{"path": f.Path, "line": f.Line, "severity": string(f.Severity), "title": f.Title, "body": f.Body}
	if f.EndLine > 0 {
		a["end_line"] = f.EndLine
	}
	if u := f.DeployedUsage; u != nil {
		a["deployed_usage"] = map[string]any{"path": u.Path, "line": u.Line, "name": u.Name}
	}
	return a
}

// caseOutcome is how the post-run pipeline published one finding.
type caseOutcome struct {
	published bool
	line      int // anchored line, 0 = general thread
	snapped   bool
	severity  string
	inSummary bool
	skipped   bool
}

var badgeRe = regexp.MustCompile(`\*\*(🔴 Critical|🟠 Major|🟡 Minor|⚪ Nit)\*\*`)

func outcomeOf(t *testing.T, h *reviewtest.Harness, f output.Finding) caseOutcome {
	t.Helper()
	var o caseOutcome
	for _, d := range h.GL.Published {
		if !strings.Contains(d.Body, "reviewbot:finding") || !strings.Contains(d.Body, f.Title) {
			continue
		}
		o.published = true
		if d.Pos != nil {
			o.line = d.Pos.NewLine
		}
		o.snapped = strings.Contains(d.Body, "Относится к строке")
		if m := badgeRe.FindStringSubmatch(d.Body); m != nil {
			o.severity = m[1]
		}
	}
	sum := reviewtest.SummaryBody(t, h)
	if i := strings.Index(sum, "### Замечания вне диффа"); i >= 0 && strings.Contains(sum[i:], f.Title) {
		o.inSummary = true
		if m := badgeRe.FindStringSubmatch(sum[i:]); m != nil {
			o.severity = m[1]
		}
	}
	o.skipped = strings.Contains(sum, "уже обсуждаются 1")
	return o
}

// runCase reviews the parity MR once: with MCP the finding is submitted
// through submit_finding over HTTP and the final answer has no findings;
// without MCP the finding is in the final answer.
func runCase(t *testing.T, f output.Finding, withMCP bool) (*mcp.Verdict, caseOutcome) {
	t.Helper()
	h := parityHarness(t)
	final := []output.Finding{f}
	var v *mcp.Verdict
	if withMCP {
		srv := reviewtest.EnableMCP(h)
		ts := httptest.NewServer(srv.Handler())
		defer ts.Close()
		h.Cfg.Review.MCP.URL = ts.URL + mcpserver.Path
		final = nil
		h.Runner.OnRun = func(req core.RunRequest) {
			c := &rpcClient{t: t, url: req.MCP.URL, token: req.MCP.Token}
			text, _ := c.tool(mcp.ToolSubmit, submitArgsOf(f))
			got := verdictOf(t, text)
			v = &got
		}
	}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "Итог.", Walkthrough: []output.FileSummary{{Path: "cache.go", Summary: "x"}}, Findings: final})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	return v, outcomeOf(t, h, f)
}

var badgeOf = map[core.Severity]string{
	core.SeverityCritical: "🔴 Critical", core.SeverityMajor: "🟠 Major", core.SeverityMinor: "🟡 Minor", core.SeverityNit: "⚪ Nit",
}

// TestSubmitFindingParity: for every kind of finding, submit_finding's
// verdict says exactly what the post-run pipeline does with the same
// finding in the final answer, and publishing a submitted finding gives the
// same result as publishing it from the final answer.
func TestSubmitFindingParity(t *testing.T) {
	usage := func(line int) *output.UsageRef {
		return &output.UsageRef{Path: "src/Entity/Order.php", Line: line, Name: "createdBy"}
	}
	cases := []struct {
		name      string
		f         output.Finding
		status    string
		placement string
		severity  core.Severity
		reason    string // substring of the reasons
	}{
		{"inline", output.Finding{Path: "cache.go", Line: 2, Severity: "major", Title: "Гонка в кэше", Body: "Нет блокировки."},
			mcp.VerdictAccepted, publish.Inline, "major", ""},
		{"snapped", output.Finding{Path: "cache.go", Line: 7, Severity: "minor", Title: "Рядом с диффом", Body: "x"},
			mcp.VerdictAdjusted, publish.Snapped, "minor", "will be placed on line 5"},
		{"summary", output.Finding{Path: "cache.go", Line: 40, Severity: "minor", Title: "Далеко от диффа", Body: "x"},
			mcp.VerdictAdjusted, publish.Summary, "minor", "line 40 is not in the diff of cache.go"},
		{"general thread", output.Finding{Path: "cache.go", Line: 40, Severity: "critical", Title: "Блокирующее вне диффа", Body: "x"},
			mcp.VerdictAdjusted, publish.General, "critical", "posted as a general thread"},
		{"file outside the diff", output.Finding{Path: "other.go", Line: 1, Severity: "nit", Title: "Чужой файл", Body: "x"},
			mcp.VerdictAdjusted, publish.Summary, "nit", "other.go is not part of this merge request's diff"},
		{"lint duplicate", output.Finding{Path: "cache.go", Line: 3, Severity: "minor", Title: "F401: os не используется", Body: "x"},
			mcp.VerdictRejected, "", "", "duplicate of the ci:ruff result F401 at cache.go:3"},
		{"open thread", output.Finding{Path: "cache.go", Line: 2, Severity: "major", Title: "Old issue", Body: "x"},
			mcp.VerdictRejected, "", "", "still open"},
		{"migration confirmed", output.Finding{Path: reviewtest.MigPath, Line: 7, Severity: "critical", Title: "Удаление created_by ломает код", Body: "x", DeployedUsage: usage(4)},
			mcp.VerdictAccepted, publish.Inline, "critical", "deployed_usage confirmed at src/Entity/Order.php:4"},
		{"migration without evidence", output.Finding{Path: reviewtest.MigPath, Line: 11, Severity: "critical", Title: "Индекс без CONCURRENTLY", Body: "x"},
			mcp.VerdictAdjusted, publish.Inline, "major", "severity lowered to major"},
		{"migration wrong line", output.Finding{Path: reviewtest.MigPath, Line: 12, Severity: "critical", Title: "Удаление legacy_note", Body: "x", DeployedUsage: usage(3)},
			mcp.VerdictAdjusted, publish.Inline, "major", "createdBy is not what the migration " + reviewtest.MigPath + " drops or renames at line 12"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, viaMCP := runCase(t, tc.f, true)
			_, plain := runCase(t, tc.f, false)
			if v == nil {
				t.Fatal("no verdict")
			}
			if v.Status != tc.status || v.Placement != tc.placement || v.Severity != tc.severity ||
				(tc.reason != "" && !strings.Contains(strings.Join(v.Reasons, " | "), tc.reason)) {
				t.Errorf("verdict %+v, want %s/%s/%s with %q", *v, tc.status, tc.placement, tc.severity, tc.reason)
			}
			skipped := plain.skipped // a rejected submission never reaches publishing, so it is not counted there
			plain.skipped, viaMCP.skipped = false, false
			if viaMCP != plain {
				t.Errorf("published differently: via submit_finding %+v, from the final answer %+v", viaMCP, plain)
			}
			// The verdict describes what publishing did.
			o := plain
			switch tc.placement {
			case publish.Inline:
				if !o.published || o.line != tc.f.Line || o.snapped {
					t.Errorf("inline verdict, outcome %+v", o)
				}
			case publish.Snapped:
				if !o.published || !o.snapped || o.line == tc.f.Line || o.line == 0 {
					t.Errorf("snapped verdict, outcome %+v", o)
				}
			case publish.General:
				if !o.published || o.line != 0 {
					t.Errorf("general-thread verdict, outcome %+v", o)
				}
			case publish.Summary:
				if o.published || !o.inSummary {
					t.Errorf("summary verdict, outcome %+v", o)
				}
			case "":
				if o.published || o.inSummary {
					t.Errorf("rejected, but published: %+v", o)
				}
			}
			if tc.severity != "" && o.severity != badgeOf[tc.severity] {
				t.Errorf("severity: verdict %s, published %s", tc.severity, o.severity)
			}
			if tc.name == "open thread" && !skipped {
				t.Errorf("open thread not skipped: %+v", o)
			}
		})
	}
}

func TestSubmitFindingFeedback(t *testing.T) {
	h := reviewtest.MigHarness(t)
	withDiffmap(h)
	h.GL.Diffs = append(h.GL.Diffs, core.FileDiff{OldPath: "svc.go", NewPath: "svc.go",
		Diff: "@@ -36,3 +36,6 @@\n a\n+b\n+c\n+d\n e\n f\n@@ -60,2 +63,4 @@\n x\n+y\n+z\n w\n"},
		core.FileDiff{NewPath: reviewtest.MigPath, OldPath: reviewtest.MigPath, NewFile: true, Diff: addedFileDiff(reviewtest.MigContent)})
	reviewtest.EnableMCP(h) // before the reviewer is built (reviewtest.OpenTestMCP keeps the server)
	co, _ := h.Repos.Checkout(context.Background(), "group/api", &h.GL.MR)
	mg := h.Internals().Migrations.Collect(context.Background(), reviewtest.AutoJob(), co, &h.GL.MR, config.Merge(h.Cfg, nil), h.Repos.Diff)
	mg.WithBaseDir("/base")
	m := reviewtest.OpenTestMCP(t, h, mcp.Inputs{Migrations: mg, Checkout: co})
	submit := func(a map[string]any) mcp.Verdict {
		text, isErr := reviewtest.CallTool(t, m, mcp.ToolSubmit, a)
		if isErr {
			t.Fatalf("tool error: %s", text)
		}
		return verdictOf(t, text)
	}
	v := submit(map[string]any{"path": "svc.go", "line": 50, "severity": "minor", "title": "Вне диффа", "body": "x"})
	if v.Status != mcp.VerdictAdjusted || !strings.Contains(v.Reasons[0], "line 50 is not in the diff of svc.go; changed lines near it: 37-39, 64-65") {
		t.Errorf("near lines: %+v", v)
	}
	// The cited merge-base line exists but does not use the name.
	v = submit(map[string]any{"path": reviewtest.MigPath, "line": 7, "severity": "critical", "title": "Удаление created_by", "body": "x",
		"deployed_usage": map[string]any{"path": "src/Entity/Order.php", "line": 3, "name": "createdBy"}})
	if v.Status != mcp.VerdictAdjusted || v.Severity != core.SeverityMajor ||
		!strings.Contains(v.Reasons[0], `the cited line src/Entity/Order.php:3 at the merge base does not contain createdBy (it reads: "{")`) ||
		!strings.Contains(v.Hint, "To keep critical") || !strings.Contains(v.Hint, "read /base/<path> to see a file as deployed") {
		t.Errorf("evidence feedback: %+v", v)
	}
	// Fixed: same path and title replaces the finding, critical stays.
	v = submit(map[string]any{"path": reviewtest.MigPath, "line": 7, "severity": "critical", "title": "Удаление created_by", "body": "x",
		"deployed_usage": map[string]any{"path": "src/Entity/Order.php", "line": 4, "name": "createdBy"}})
	if v.Status != mcp.VerdictAccepted || !v.Replaced || v.ID != "F2" || v.Severity != core.SeverityCritical || !v.Blocking {
		t.Errorf("fixed: %+v", v)
	}
	if fs := m.Findings(); len(fs) != 2 || fs[1].Severity != core.SeverityCritical || fs[1].DeployedUsage == nil {
		t.Errorf("kept findings: %+v", fs)
	}
	v = submit(map[string]any{"path": "svc.go", "line": 37, "severity": "major", "title": "X", "body": "y",
		"deployed_usage": map[string]any{"path": "a.go", "line": 1, "name": "x"}})
	if v.Status != mcp.VerdictAdjusted || !strings.Contains(v.Reasons[0], "deployed_usage ignored") {
		t.Errorf("usage on a non-migration: %+v", v)
	}
	for name, a := range map[string]map[string]any{
		"severity":  {"path": "svc.go", "line": 37, "severity": "blocker", "title": "t", "body": "b"},
		"path":      {"path": "../x.go", "line": 1, "severity": "minor", "title": "t", "body": "b"},
		"title":     {"path": "svc.go", "line": 37, "severity": "minor", "title": " ", "body": "b"},
		"body":      {"path": "svc.go", "line": 37, "severity": "minor", "title": "t", "body": ""},
		"end_line":  {"path": "svc.go", "line": 37, "end_line": 30, "severity": "minor", "title": "t", "body": "b"},
		"too long":  {"path": "svc.go", "line": 37, "severity": "minor", "title": strings.Repeat("я", 301), "body": "b"},
		"negative":  {"path": "svc.go", "line": -1, "severity": "minor", "title": "t", "body": "b"},
		"markers":   {"path": "svc.go", "line": 37, "severity": "minor", "title": "<!-- reviewbot:finding id=abc -->", "body": "b"},
		"no path":   {"line": 37, "severity": "minor", "title": "t", "body": "b"},
		"long path": {"path": strings.Repeat("a/", 300) + "x.go", "line": 1, "severity": "minor", "title": "t", "body": "b"},
	} {
		if v := submit(a); v.Status != mcp.VerdictRejected || v.ID != "" || v.Hint == "" {
			t.Errorf("%s: %+v", name, v)
		}
	}
	if text, isErr := reviewtest.CallTool(t, m, mcp.ToolSubmit, map[string]any{"path": "svc.go", "unknown": 1}); !isErr || !strings.Contains(text, "unknown field") {
		t.Errorf("unknown field: %s", text)
	}
}

func TestSubmitFindingCap(t *testing.T) {
	h := reviewtest.New(t)
	h.Cfg.Review.MaxFindings = 2
	m := reviewtest.OpenTestMCP(t, h, mcp.Inputs{})
	for i := 1; i <= 2; i++ {
		text, _ := reviewtest.CallTool(t, m, mcp.ToolSubmit, map[string]any{"path": "cache.go", "line": 2, "severity": "minor", "title": "T" + strconv.Itoa(i), "body": "b"})
		if v := verdictOf(t, text); v.Status != mcp.VerdictAccepted {
			t.Fatalf("submit %d: %+v", i, v)
		}
	}
	text, _ := reviewtest.CallTool(t, m, mcp.ToolSubmit, map[string]any{"path": "cache.go", "line": 3, "severity": "major", "title": "T3", "body": "b"})
	if v := verdictOf(t, text); v.Status != mcp.VerdictRejected || !strings.Contains(v.Reasons[0], "cap of this review (2)") || !strings.Contains(v.Hint, "withdraw_finding") {
		t.Errorf("over the cap: %+v", v)
	}
	// Replacing an existing one is not a new finding.
	text, _ = reviewtest.CallTool(t, m, mcp.ToolSubmit, map[string]any{"path": "cache.go", "line": 3, "severity": "major", "title": "T1", "body": "better"})
	if v := verdictOf(t, text); v.Status != mcp.VerdictAccepted || !v.Replaced || v.ID != "F1" {
		t.Errorf("replace at the cap: %+v", v)
	}
}

func TestSubmitFindingWithoutDiff(t *testing.T) {
	h := reviewtest.New(t)
	m := reviewtest.OpenTestMCP(t, h, mcp.Inputs{})
	m.SetIdxOK(false) // as if GetMRDiffs failed
	text, _ := reviewtest.CallTool(t, m, mcp.ToolSubmit, map[string]any{"path": "cache.go", "line": 2, "severity": "minor", "title": "T", "body": "b"})
	v := verdictOf(t, text)
	if v.Status != mcp.VerdictAccepted || v.Placement != "" || !strings.Contains(v.Reasons[0], "checked when publishing") {
		t.Errorf("no diff: %+v", v)
	}
	if text, isErr := reviewtest.CallTool(t, m, mcp.ToolDiffLines, nil); !isErr || !strings.Contains(text, "use the diff in the prompt") {
		t.Errorf("diff_lines without diff: %s", text)
	}
}

// addedFileDiff is GitLab's diff text of a new file.
func addedFileDiff(content string) string {
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	return "@@ -0,0 +1," + strconv.Itoa(len(lines)) + " @@\n+" + strings.Join(lines, "\n+") + "\n"
}

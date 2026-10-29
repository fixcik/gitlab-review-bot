package migcheck_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/migrations"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func migEff(h *reviewtest.Harness) config.Effective {
	eff := config.Merge(h.Cfg, nil)
	eff.Migrations.Enabled = true
	return eff
}

func migCheckout(t *testing.T, h *reviewtest.Harness) core.Checkout {
	t.Helper()
	co, err := h.Repos.Checkout(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return co
}

// A deployed_usage with a line below 1 is no evidence: the critical stops
// blocking and loses the usage.
func TestMigrationsEvidenceLineZero(t *testing.T) {
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	out := &output.ReviewOutput{Findings: []output.Finding{
		{Path: evMig, Line: 1, Severity: "critical", Title: "line zero", DeployedUsage: &output.UsageRef{Path: "src/models/order.py", Line: 0, Name: "created_by"}},
	}}
	if n := h.Internals().Migrations.Enforce(context.Background(), discardLog(), co, out, m, "English"); n != 1 {
		t.Errorf("changed %d", n)
	}
	if f := out.Findings[0]; f.Severity != "major" || f.DeployedUsage != nil {
		t.Errorf("finding %+v", f)
	}
	if len(m.ProvenIDs()) != 0 {
		t.Errorf("proven %v", m.ProvenIDs())
	}
}

// diffErrCheckout cannot build a diff.
type diffErrCheckout struct{ core.Checkout }

func (diffErrCheckout) Diff(context.Context, string, []string) (string, error) {
	return "", errors.New("git diff: exit 128")
}

// The diff the migrations are looked for in: nothing when off, the
// reviewed diff of a full review, the system map's full diff of an
// incremental one, else the full MR diff — and nothing when that fails
// (no analysis rather than one on a partial diff).
func TestMigrationsDiffSource(t *testing.T) {
	h := reviewtest.MigHarness(t)
	mf, job, mr := h.Internals().Migrations, reviewtest.AutoJob(), &h.GL.MR
	co := migCheckout(t, h)
	eff := migEff(h)
	off := eff
	off.Migrations.Enabled = false
	if got := mf.Diff(context.Background(), job, co, mr, off, "reviewed", "sm", true); got != "" {
		t.Errorf("off: %q", got)
	}
	if got := mf.Diff(context.Background(), job, co, mr, eff, "reviewed", "sm", false); got != "reviewed" {
		t.Errorf("full review: %q", got)
	}
	if got := mf.Diff(context.Background(), job, co, mr, eff, "reviewed", "sm", true); got != "sm" {
		t.Errorf("incremental with the system map's diff: %q", got)
	}
	if got := mf.Diff(context.Background(), job, co, mr, eff, "reviewed", "", true); got != h.Repos.Diff {
		t.Errorf("incremental: %q", got)
	}
	if from := h.Repos.DiffFrom; len(from) != 1 || from[0] != mr.DiffRefs.BaseSHA {
		t.Errorf("full diff from %v", from)
	}
	if got := mf.Diff(context.Background(), job, diffErrCheckout{co}, mr, eff, "reviewed", "", true); got != "" {
		t.Errorf("failed full diff: %q", got)
	}
}

// A panic in the analysis drops the migrations for this review and records
// an evidence_failed problem.
func TestMigrationsCollectPanics(t *testing.T) {
	h := reviewtest.MigHarness(t)
	ctx, run := problems.New(problems.Options{}).Start(context.Background(), reviewtest.AutoJob())
	m := h.Internals().Migrations.Collect(ctx, reviewtest.AutoJob(), panickingCheckout{migCheckout(t, h)}, &h.GL.MR, migEff(h), h.Repos.Diff)
	if m != nil {
		t.Fatalf("run %+v", m)
	}
	ps := run.List()
	if len(ps) != 1 || ps[0].Kind != problems.EvidenceFailed || ps[0].Subject != "анализ миграций: внутренняя ошибка" || ps[0].Detail != "boom" {
		t.Errorf("problems %+v", ps)
	}
}

// A migration file that cannot be read at head is left out; nothing
// readable means no migrations.
func TestMigrationsCollectUnreadable(t *testing.T) {
	h := reviewtest.MigHarness(t)
	const other = "migrations/Version20261020130000.php"
	diff := h.Repos.Diff + reviewtest.NewFileDiff(other, reviewtest.MigContent)
	m := h.Internals().Migrations.Collect(context.Background(), reviewtest.AutoJob(), migCheckout(t, h), &h.GL.MR, migEff(h), diff)
	if m == nil || len(m.Res.Files) != 1 || m.Res.Files[0].Path != reviewtest.MigPath || m.Heads[reviewtest.MigPath] != reviewtest.MigContent {
		t.Fatalf("run %+v", m)
	}
	if _, ok := m.Heads[other]; ok {
		t.Error("unreadable file kept")
	}
	m = h.Internals().Migrations.Collect(context.Background(), reviewtest.AutoJob(), migCheckout(t, h), &h.GL.MR, migEff(h), reviewtest.NewFileDiff(other, reviewtest.MigContent))
	if m != nil {
		t.Errorf("nothing readable: %+v", m)
	}
}

// grepCheckout searches with git grep (core.WordGrepper).
type grepCheckout struct {
	core.Checkout
	base  string
	calls []string
}

func (g *grepCheckout) GrepWords(_ context.Context, rev string, words, pathspecs []string, maxBytes int) ([]core.GrepMatch, bool, error) {
	g.calls = append(g.calls, rev)
	if maxBytes <= 0 || strings.Join(pathspecs, ",") != strings.Join(migrations.SourcePathspecs, ",") {
		return nil, false, errors.New("bad grep arguments")
	}
	if rev != g.base {
		return nil, false, nil
	}
	return []core.GrepMatch{{Path: "src/Entity/Order.php", Line: 4, Text: "    private ?int $createdBy = null;"}}, false, nil
}

// A checkout that can grep is searched with git grep (base and head), not
// file by file.
func TestMigrationsCollectUsesGrep(t *testing.T) {
	h := reviewtest.MigHarness(t)
	g := &grepCheckout{Checkout: migCheckout(t, h), base: h.GL.MR.DiffRefs.BaseSHA}
	m := h.Internals().Migrations.Collect(context.Background(), reviewtest.AutoJob(), g, &h.GL.MR, migEff(h), h.Repos.Diff)
	if m == nil {
		t.Fatal("no run")
	}
	if strings.Join(g.calls, ",") != h.GL.MR.DiffRefs.BaseSHA+","+h.GL.MR.DiffRefs.HeadSHA {
		t.Errorf("grep calls %v", g.calls)
	}
	var u *migrations.UsageCheck
	for _, f := range m.Res.Findings {
		if f.Column == "created_by" {
			u = f.Usage
		}
	}
	if u == nil || !u.Checked || len(u.Base) != 1 || u.Base[0].Path != "src/Entity/Order.php" || u.Base[0].Line != 4 || u.HeadTotal != 0 {
		t.Fatalf("usage %+v", u)
	}
	if !strings.Contains(m.Prompt, "src/Entity/Order.php:4") {
		t.Errorf("prompt:\n%s", m.Prompt)
	}
}

// WithBaseDir is nil-safe and points the prompt at the export.
func TestMigrationsWithBaseDir(t *testing.T) {
	var nilRun *migcheck.Run
	nilRun.WithBaseDir("/base") // must not panic
	m := &migcheck.Run{Res: &migrations.Result{Files: []migrations.File{{Path: "db/1.up.sql", Framework: migrations.FwSQL, New: true}}}, Base: "ba5e000000000000"}
	m.WithBaseDir("/base")
	if m.BaseRoot != "/base" || !strings.Contains(m.Prompt, "read /base/<path> to see a file as deployed") || m.System == "" {
		t.Errorf("run %+v", m)
	}
}

// The deployed_usage property needs a findings array in the schema: a
// schema without one is a programming error.
func TestMigrationsWithSchemaNeedsFindings(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	migcheck.WithSchema(`{"type":"object","properties":{"findings":{"type":"array"}}}`)
}

// countingCheckout serves every file and counts the head reads of
// migration candidates.
type countingCheckout struct {
	core.Checkout
	head  string
	reads int
}

func (c *countingCheckout) ShowFile(_ context.Context, rev, path string) ([]byte, error) {
	if rev == c.head && strings.HasPrefix(path, "db/migrations/") {
		c.reads++
	}
	return []byte("ALTER TABLE t DROP COLUMN c;\n"), nil
}

// At most MigMaxReads candidate files are read; the rest are not
// analysed (and do not stop the review).
func TestMigrationsCollectReadCap(t *testing.T) {
	h := reviewtest.MigHarness(t)
	var diff strings.Builder
	for i := 0; i <= migcheck.MigMaxReads; i++ {
		diff.WriteString(reviewtest.NewFileDiff(fmt.Sprintf("db/migrations/%04d.up.sql", i), "ALTER TABLE t DROP COLUMN c;"))
	}
	co := &countingCheckout{Checkout: migCheckout(t, h), head: h.GL.MR.DiffRefs.HeadSHA}
	m := h.Internals().Migrations.Collect(context.Background(), reviewtest.AutoJob(), co, &h.GL.MR, migEff(h), diff.String())
	if co.reads != migcheck.MigMaxReads {
		t.Errorf("reads %d, want %d", co.reads, migcheck.MigMaxReads)
	}
	if m == nil || len(m.Res.Files)+m.Res.SkippedFiles != migcheck.MigMaxReads {
		t.Fatalf("run %+v", m)
	}
}

// Files the review ignores, deleted files and paths that are no migration
// candidates are not read; the check off reads nothing.
func TestMigrationsCollectSkips(t *testing.T) {
	h := reviewtest.MigHarness(t)
	co := &countingCheckout{Checkout: migCheckout(t, h), head: h.GL.MR.DiffRefs.HeadSHA}
	eff := migEff(h)
	eff.Ignore = append(eff.Ignore, "db/migrations/ignored/**")
	diff := reviewtest.NewFileDiff("db/migrations/ignored/1.up.sql", "DROP TABLE t;") +
		"diff --git a/db/migrations/2.up.sql b/db/migrations/2.up.sql\ndeleted file mode 100644\n--- a/db/migrations/2.up.sql\n+++ /dev/null\n@@ -1 +0,0 @@\n-DROP TABLE t;\n" +
		reviewtest.NewFileDiff("db/notes.txt", "DROP TABLE t;")
	if m := h.Internals().Migrations.Collect(context.Background(), reviewtest.AutoJob(), co, &h.GL.MR, eff, diff); m != nil || co.reads != 0 {
		t.Errorf("run %+v reads %d", m, co.reads)
	}
	off := migEff(h)
	off.Migrations.Enabled = false
	if m := h.Internals().Migrations.Collect(context.Background(), reviewtest.AutoJob(), migCheckout(t, h), &h.GL.MR, off, h.Repos.Diff); m != nil {
		t.Errorf("off: run %+v", m)
	}
}

// An unreadable file does not stop the reading of the next ones.
func TestMigrationsCollectUnreadableFirst(t *testing.T) {
	h := reviewtest.MigHarness(t)
	diff := reviewtest.NewFileDiff("migrations/Version20261020100000.php", reviewtest.MigContent) + h.Repos.Diff
	m := h.Internals().Migrations.Collect(context.Background(), reviewtest.AutoJob(), migCheckout(t, h), &h.GL.MR, migEff(h), diff)
	if m == nil || len(m.Res.Files) != 1 || m.Res.Files[0].Path != reviewtest.MigPath {
		t.Fatalf("run %+v", m)
	}
}

// ProvenIDs: the criticals whose evidence passed; nil-safe.
func TestMigrationsProvenIDs(t *testing.T) {
	var nilRun *migcheck.Run
	if nilRun.ProvenIDs() != nil {
		t.Error("nil run")
	}
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	out := &output.ReviewOutput{Findings: []output.Finding{
		{Path: evMig, Line: 1, Severity: "critical", Title: "valid", DeployedUsage: &output.UsageRef{Path: "src/models/order.py", Line: 3, Name: "created_by"}},
		{Path: evMig, Line: 1, Severity: "critical", Title: "missing"},
		{Path: evMig, Line: 1, Severity: "critical", Title: "valid too", DeployedUsage: &output.UsageRef{Path: "src/models/order.py", Line: 3, Name: "created_by"}},
	}}
	h.Internals().Migrations.Enforce(context.Background(), discardLog(), co, out, m, "English")
	if ids := m.ProvenIDs(); len(ids) != 2 || !ids[finding.ID(evMig, "valid")] || !ids[finding.ID(evMig, "valid too")] {
		t.Errorf("proven %v", ids)
	}
}

// Evidence on a line the analyzer parsed counts only for a contract
// operation (drop/rename): a CREATE INDEX naming the table proves nothing.
// On an unparsed first line the text check applies. A merge-base file that
// cannot be read is named as such.
func TestMigrationsEvidenceObjectsAndText(t *testing.T) {
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	mf := h.Internals().Migrations
	e := migcheck.NewEvidenceChecker(co, m)
	ev := func(line int, p string, uline int, name string) output.Finding {
		return output.Finding{Path: evMig, Line: line, Severity: "critical", DeployedUsage: &output.UsageRef{Path: p, Line: uline, Name: name}}
	}
	if ok, why := mf.Verdict(context.Background(), e, ev(5, "src/models/order.py", 2, "orders")); !ok || !strings.Contains(why, "is not what the migration drops") {
		t.Errorf("index line: %v %q", ok, why)
	}
	if ok, why := mf.Verdict(context.Background(), e, ev(1, "src/models/gone.py", 3, "created_by")); !ok || !strings.HasPrefix(why, "merge-base file: ") {
		t.Errorf("missing file: %v %q", ok, why)
	}

	const body = "DO $$ BEGIN EXECUTE 'ALTER TABLE orders DROP COLUMN legacy_note'; END $$;\n"
	res := migrations.Analyze([]migrations.Input{{Path: evMig, New: true, Content: []byte(body)}}, migrations.Config{})
	if len(res.Findings) != 0 {
		t.Fatalf("precondition: unparsed %+v", res.Findings)
	}
	m2 := &migcheck.Run{Res: res, Base: m.Base, Heads: map[string]string{evMig: body}}
	if ok, why := mf.Verdict(context.Background(), migcheck.NewEvidenceChecker(co, m2), ev(1, "src/models/order.py", 4, "legacy_note")); !ok || why != "" {
		t.Errorf("unparsed first line: %v %q", ok, why)
	}
	// The camelCase name of the dropped column counts in its snake_case form.
	if ok, why := mf.Verdict(context.Background(), migcheck.NewEvidenceChecker(co, m2), ev(1, "src/models/order.py", 4, "legacyNote")); !ok || strings.Contains(why, "drops or renames") {
		t.Errorf("camelCase name: %v %q", ok, why)
	}
}

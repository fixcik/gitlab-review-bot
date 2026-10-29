package migcheck_test

import (
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/migrations"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// Mutation-driven tests for the feature files of the package (migrations,
// compat, lint, tbd, coverage, sysmap deps).

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The evidence check must reach the bot's own path validation: a finding
// whose deployed_usage path is not clean stays downgraded even when the
// checkout would happily serve that path.
func TestMigrationsEvidenceUncleanPathNeverRead(t *testing.T) {
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	for _, p := range []string{"/src/models/order.py", "./src/models/order.py", "src//models/order.py"} {
		h.Repos.ShowAt[m.Base+":"+p] = evApp
	}
	out := &output.ReviewOutput{}
	for _, p := range []string{"/src/models/order.py", "./src/models/order.py", "src//models/order.py"} {
		out.Findings = append(out.Findings, output.Finding{Path: evMig, Line: 1, Severity: "critical", Title: p,
			DeployedUsage: &output.UsageRef{Path: p, Line: 3, Name: "created_by"}})
	}
	if n := h.Internals().Migrations.Enforce(context.Background(), discardLog(), co, out, m, "English"); n != len(out.Findings) {
		t.Errorf("changed %d of %d", n, len(out.Findings))
	}
	for _, f := range out.Findings {
		if f.Severity != "major" || f.DeployedUsage != nil {
			t.Errorf("%q: %s %+v", f.Title, f.Severity, f.DeployedUsage)
		}
	}
}

// The last line of the merge-base file is a valid usage line (boundary of
// the "past the end" check), with or without a trailing newline.
func TestMigrationsEvidenceLastLine(t *testing.T) {
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	h.Repos.ShowAt[m.Base+":src/last.py"] = "x = 1\nrow.created_by = 2\n"
	h.Repos.ShowAt[m.Base+":src/last2.py"] = "x = 1\nrow.created_by = 2"
	out := &output.ReviewOutput{Findings: []output.Finding{
		{Path: evMig, Line: 1, Severity: "critical", Title: "nl", DeployedUsage: &output.UsageRef{Path: "src/last.py", Line: 2, Name: "created_by"}},
		{Path: evMig, Line: 1, Severity: "critical", Title: "no nl", DeployedUsage: &output.UsageRef{Path: "src/last2.py", Line: 2, Name: "created_by"}},
		{Path: evMig, Line: 1, Severity: "critical", Title: "past", DeployedUsage: &output.UsageRef{Path: "src/last.py", Line: 3, Name: "created_by"}},
	}}
	if n := h.Internals().Migrations.Enforce(context.Background(), discardLog(), co, out, m, "English"); n != 1 {
		t.Errorf("changed %d", n)
	}
	for _, f := range out.Findings {
		want := core.Severity("critical")
		if f.Title == "past" {
			want = "major"
		}
		if f.Severity != want {
			t.Errorf("%q: %s, want %s", f.Title, f.Severity, want)
		}
	}
}

// Exactly migMaxEvidenceReads merge-base files are read per review: the
// file read last within the budget is checked, the next one is not.
func TestMigrationsEvidenceBudgetExact(t *testing.T) {
	for _, tc := range []struct {
		misses int
		want   core.Severity
	}{{migcheck.MaxEvidenceReads - 1, "critical"}, {migcheck.MaxEvidenceReads, "major"}} {
		h := reviewtest.New(t)
		m, co := evidenceRun(t, h)
		var fs []output.Finding
		for i := 0; i < tc.misses; i++ {
			fs = append(fs, output.Finding{Path: evMig, Line: 1, Severity: "critical", Title: "f" + strconv.Itoa(i),
				DeployedUsage: &output.UsageRef{Path: "src/m" + strconv.Itoa(i) + ".py", Line: 1, Name: "created_by"}})
		}
		// A repeated path is served from the cache and costs no read.
		fs = append(fs, output.Finding{Path: evMig, Line: 1, Severity: "critical", Title: "again",
			DeployedUsage: &output.UsageRef{Path: "src/m0.py", Line: 1, Name: "created_by"}})
		fs = append(fs, output.Finding{Path: evMig, Line: 1, Severity: "critical", Title: "last",
			DeployedUsage: &output.UsageRef{Path: "src/models/order.py", Line: 3, Name: "created_by"}})
		out := &output.ReviewOutput{Findings: fs}
		h.Internals().Migrations.Enforce(context.Background(), discardLog(), co, out, m, "English")
		if last := out.Findings[len(out.Findings)-1]; last.Severity != tc.want {
			t.Errorf("%d misses: last finding %s, want %s", tc.misses, last.Severity, tc.want)
		}
	}
}

func TestMigrationsEnforceNilOutput(t *testing.T) {
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	for _, mr := range []*migcheck.Run{nil, m} {
		if n := h.Internals().Migrations.Enforce(context.Background(), discardLog(), co, nil, mr, "English"); n != 0 {
			t.Errorf("nil output: %d", n)
		}
	}
}

// Every breaking-change item on a migration file becomes a non-blocking
// finding, wherever it sits in the list; the others stay compat items in
// their order.
func TestMigrationsCompatItemsInterleaved(t *testing.T) {
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	out := &output.ReviewOutput{Compatibility: &output.Compatibility{Breaking: []output.BreakingChange{
		{Path: "api.go", Line: 1, Title: "a"},
		{Path: evMig, Line: 1, Title: "m1", Explanation: "e", PreviousBehavior: "p", SafeAlternative: "s"},
		{Path: "api2.go", Line: 2, Title: "b"},
		{Path: evMig, Line: 5, Title: "m2"},
	}}}
	if n := h.Internals().Migrations.Enforce(context.Background(), discardLog(), co, out, m, "English"); n != 2 {
		t.Errorf("changed %d", n)
	}
	if b := out.Compatibility.Breaking; len(b) != 2 || b[0].Title != "a" || b[1].Title != "b" {
		t.Errorf("compat items %+v", b)
	}
	if len(out.Findings) != 2 || out.Findings[0].Title != "m1" || out.Findings[1].Title != "m2" {
		t.Fatalf("findings %+v", out.Findings)
	}
	f := out.Findings[0]
	if f.Severity != "major" || f.Path != evMig || f.Line != 1 || f.EndLine != 1 || f.Body != "e\n\nБыло: p\n\nКак безопаснее: s" {
		t.Errorf("finding from compat item %+v", f)
	}
}

// The cited name must be the object the migration drops or renames at the
// finding's lines: for a column drop the column, not the table or its
// entity; anywhere else in the file does not count.
func TestMigrationsEvidenceNameIsDroppedObject(t *testing.T) {
	h := reviewtest.New(t)
	m, co := evidenceRun(t, h)
	h.Repos.ShowAt[m.Base+":src/repo.py"] = "def load(id):\n    return Order.get(id)\nq = 'SELECT * FROM orders'\nrows = db.orders.find()\nst = row.status\nn = row.legacy_note\n"
	ev := func(title string, line, endLine, useLine int, name string) output.Finding {
		return output.Finding{Path: evMig, Line: line, EndLine: endLine, Severity: "critical", Title: title,
			DeployedUsage: &output.UsageRef{Path: "src/repo.py", Line: useLine, Name: name}}
	}
	out := &output.ReviewOutput{Findings: []output.Finding{
		// Line 1 drops orders.created_by (detected): the table does not count.
		ev("entity of the table", 1, 1, 2, "Order"),
		ev("table name", 1, 1, 4, "orders"),
		// Line 3 (DO block, not detected): only the identifier on the
		// finding's own lines, spelled as in code; the table's entity does not count.
		ev("DO block: entity", 3, 3, 2, "Order"),
		ev("DO block: other line's name", 3, 3, 5, "status"),
		// Line 5 is an index, not a drop: a name mentioned on line 5 is not a drop.
		ev("index line", 5, 5, 5, "status"),
		// Anchored on line 2 (DO start), range 2..4 covers the dropped column.
		ev("range covers it", 2, 4, 2, "Order"),
		// Confirmed: the dropped identifier itself.
		ev("ok: range covers the column", 2, 4, 6, "legacy_note"),
		ev("ok: DO block column", 3, 3, 6, "legacy_note"),
	}}
	h.Internals().Migrations.Enforce(context.Background(), discardLog(), co, out, m, "English")
	for _, f := range out.Findings {
		want := core.Severity("major")
		if strings.HasPrefix(f.Title, "ok: ") {
			want = "critical"
		}
		if f.Severity != want {
			t.Errorf("%q: %s, want %s", f.Title, f.Severity, want)
		}
	}

	// Detected drops and renames: the column for a column operation, the
	// table (or its entity) for a table operation; camelCase spellings of the
	// column count; the old name of a rename, not the new one.
	const body = "ALTER TABLE orders DROP COLUMN created_by;\n" +
		"DROP TABLE order_items;\n" +
		"ALTER TABLE orders RENAME COLUMN note TO comment;\n"
	const p = "db/migrations/3_mix.up.sql"
	res := migrations.Analyze([]migrations.Input{{Path: p, New: true, Content: []byte(body)}}, migrations.Config{})
	m2 := &migcheck.Run{Res: res, Base: m.Base, Heads: map[string]string{p: body}}
	h.Repos.ShowAt[m.Base+":src/m2.py"] = "a = row.createdBy\nb = OrderItem.all()\nc = Order.get(1)\nd = row.note\ne = row.comment\nf = db.order_items\n"
	e := migcheck.NewEvidenceChecker(co, m2)
	for _, tc := range []struct {
		line, use int
		name      string
		ok        bool
	}{
		{1, 1, "createdBy", true}, {1, 3, "Order", false},
		{2, 2, "OrderItem", true}, {2, 6, "order_items", true}, {2, 1, "createdBy", false},
		{3, 4, "note", true}, {3, 5, "comment", false}, {3, 3, "Order", false},
	} {
		f := output.Finding{Path: p, Line: tc.line, EndLine: tc.line, Severity: "critical",
			DeployedUsage: &output.UsageRef{Path: "src/m2.py", Line: tc.use, Name: tc.name}}
		if why := e.Check(context.Background(), f); (why == "") != tc.ok {
			t.Errorf("line %d name %s: %q, want ok=%v", tc.line, tc.name, why, tc.ok)
		}
	}
}

// Edge cases of the evidence name check: operations of other files or
// other lines listed first, findings outside the migration's lines, the
// last line of a migration without a trailing newline.
func TestMigrationsEvidenceDropsNameEdgesMut(t *testing.T) {
	h := reviewtest.New(t)
	_, co := evidenceRun(t, h)
	const base = "ba5e000000000000"
	const other = "db/migrations/0_other.up.sql"
	const p = "db/migrations/4_t.up.sql"
	const body = "SELECT 1;\nDROP TABLE order_items;\nDO $$ BEGIN EXECUTE 'ALTER TABLE orders DROP COLUMN legacy_note'; END $$"
	res := migrations.Analyze([]migrations.Input{
		{Path: other, New: true, Content: []byte("ALTER TABLE carts DROP COLUMN total;\n")},
		{Path: p, New: true, Content: []byte(body)},
	}, migrations.Config{})
	for _, op := range res.Findings {
		if op.Path == p && op.Line == 3 {
			t.Fatalf("precondition: the DO block is not parsed: %+v", op)
		}
	}
	m := &migcheck.Run{Res: res, Base: base, Heads: map[string]string{p: body}}
	h.Repos.ShowAt[base+":src/e.py"] = "a = OrderItem.all()\nb = row.legacy_note\n"
	e := migcheck.NewEvidenceChecker(co, m)
	for _, tc := range []struct {
		line, use int
		name      string
		ok        bool
	}{
		{2, 1, "OrderItem", true},   // the table op on line 2, after another file's op
		{3, 2, "legacy_note", true}, // last line (not parsed: DO block), no trailing newline
		{0, 2, "legacy_note", false},
		{4, 2, "legacy_note", false},
	} {
		f := output.Finding{Path: p, Line: tc.line, EndLine: tc.line, Severity: "critical",
			DeployedUsage: &output.UsageRef{Path: "src/e.py", Line: tc.use, Name: tc.name}}
		if why := e.Check(context.Background(), f); (why == "") != tc.ok {
			t.Errorf("line %d name %s: %q, want ok=%v", tc.line, tc.name, why, tc.ok)
		}
	}
	// A range with a non-contract operation (an index) before the drop.
	const p2 = "db/migrations/5_r.up.sql"
	const body2 = "CREATE INDEX idx_c ON carts (total);\nDROP TABLE order_items;\n"
	m.Res = migrations.Analyze([]migrations.Input{{Path: p2, New: true, Content: []byte(body2)}}, migrations.Config{})
	m.Heads = map[string]string{p2: body2}
	f := output.Finding{Path: p2, Line: 1, EndLine: 2, Severity: "critical", DeployedUsage: &output.UsageRef{Path: "src/e.py", Line: 1, Name: "OrderItem"}}
	if why := e.Check(context.Background(), f); why != "" {
		t.Errorf("range with an index before the drop: %q", why)
	}
}

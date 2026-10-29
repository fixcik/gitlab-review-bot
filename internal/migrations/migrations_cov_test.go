package migrations

import (
	"strings"
	"testing"
)

func TestFrameworkTitle(t *testing.T) {
	for fw, want := range map[Framework]string{
		FwSQL: "SQL", FwFlyway: "Flyway", FwGoose: "goose", FwGolangMigrate: "golang-migrate", FwSqitch: "Sqitch",
		FwDbmate: "dbmate", FwDoctrine: "Doctrine", FwLaravel: "Laravel", FwAlembic: "Alembic", FwDjango: "Django",
		Framework("knex"): "knex",
	} {
		if got := fw.Title(); got != want {
			t.Errorf("%s: %q, want %q", fw, got, want)
		}
	}
}

func TestOpObject(t *testing.T) {
	for _, tc := range []struct {
		op   Op
		want string
	}{
		{Op{Table: "orders", Column: "note"}, "orders.note"},
		{Op{Column: "note"}, "note"},
		{Op{Table: "orders"}, "orders"},
		{Op{}, ""},
	} {
		if got := tc.op.Object(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.op, got, tc.want)
		}
	}
}

func TestIsMigrationNil(t *testing.T) {
	var r *Result
	if r.IsMigration("db/1.up.sql") {
		t.Error("nil result")
	}
	r = &Result{Files: []File{{Path: "db/1.up.sql"}}}
	if !r.IsMigration("db/1.up.sql") || r.IsMigration("db/2.up.sql") {
		t.Error("lookup")
	}
}

// The level of a finding: no rule -> info; an uncertain extraction of a
// non-contract operation is at most minor; a contract operation is
// critical only when the deployed code uses a specific name.
func TestLevelOf(t *testing.T) {
	checked := func(base, head int, ambiguous bool) *UsageCheck {
		return &UsageCheck{Checked: true, BaseTotal: base, HeadTotal: head, Ambiguous: ambiguous}
	}
	for _, tc := range []struct {
		name string
		f    Finding
		want Level
	}{
		{"internal kind", Finding{Op: Op{Kind: createTable}}, LevelInfo},
		{"major", Finding{Op: Op{Kind: AlterType}}, LevelMajor},
		{"weak major", Finding{Op: Op{Kind: AlterType, Weak: true}}, LevelMinor},
		{"contract unchecked", Finding{Op: Op{Kind: DropColumn}}, LevelMajor},
		{"contract search failed", Finding{Op: Op{Kind: DropColumn}, Usage: &UsageCheck{Err: "x"}}, LevelMajor},
		{"contract used", Finding{Op: Op{Kind: DropColumn}, Usage: checked(2, 0, false)}, LevelCritical},
		{"contract used, generic", Finding{Op: Op{Kind: DropColumn}, Usage: checked(2, 0, true)}, LevelMajor},
		{"contract used by the MR", Finding{Op: Op{Kind: RenameColumn}, Usage: checked(0, 1, false)}, LevelMajor},
		{"contract unused", Finding{Op: Op{Kind: DropTable}, Usage: checked(0, 0, false)}, LevelInfo},
	} {
		if got := levelOf(tc.f); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	if (Finding{Op: Op{Kind: AlterType}, Usage: checked(1, 0, false)}).BreaksOldCode() {
		t.Error("a non-contract operation never breaks the old code")
	}
}

func TestLowerName(t *testing.T) {
	for in, want := range map[string]string{
		`public."Orders"`: "orders", ".Lead": "lead", "`db`.`Users`": "users", "[dbo].[T]": "t", " x ": "x", "plain": "plain",
	} {
		if got := lowerName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// Findings: most severe first, then by file, then by line.
func TestSetLevelsOrder(t *testing.T) {
	r := &Result{Findings: []Finding{
		{Op: Op{Kind: AlterType, Path: "b.sql", Line: 1}},
		{Op: Op{Kind: AlterType, Path: "a.sql", Line: 9}},
		{Op: Op{Kind: AlterType, Path: "a.sql", Line: 3}},
		{Op: Op{Kind: DropColumn, Path: "c.sql", Line: 1, Table: "t", Column: "c"}, Usage: &UsageCheck{Checked: true, BaseTotal: 1}},
	}}
	r.setLevels()
	var got []string
	for _, f := range r.Findings {
		got = append(got, f.Path+":"+string(rune('0'+f.Line))+":"+string(f.Level))
	}
	if want := "c.sql:1:critical a.sql:3:major a.sql:9:major b.sql:1:major"; strings.Join(got, " ") != want {
		t.Errorf("got  %s\nwant %s", strings.Join(got, " "), want)
	}
}

package migcheck

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/migrations"
)

// The prompt names a changed (not new) migration, the files over the scan
// limit, orders the places by file then line and says when a generic name
// was searched only next to its table.
func TestRenderMigrationsPromptStatesAndOrder(t *testing.T) {
	res := &migrations.Result{
		Files: []migrations.File{
			{Path: "db/migrations/b.up.sql", Framework: migrations.FwSQL},
			{Path: "db/migrations/a.up.sql", Framework: migrations.FwSQL, New: true},
		},
		SkippedFiles: 3,
		Findings: []migrations.Finding{
			{Op: migrations.Op{Kind: migrations.DropColumn, Path: "db/migrations/b.up.sql", Line: 2, Table: "orders", Column: "status"},
				Usage: &migrations.UsageCheck{Checked: true, Ambiguous: true}},
			{Op: migrations.Op{Kind: migrations.DropColumn, Path: "db/migrations/a.up.sql", Line: 9, Table: "orders", Column: "note"},
				Usage: &migrations.UsageCheck{Checked: true}},
		},
	}
	p := renderMigrationsPrompt(res, "ba5e000000000000", "")
	for _, w := range []string{
		"- db/migrations/b.up.sql (SQL, changed)\n",
		"- db/migrations/a.up.sql (SQL, new)\n",
		"- ... and 3 more migration file(s) not scanned (limit); read them yourself\n",
		"db/migrations/b.up.sql:2 drop_column orders.status: why to check:",
		"\n  (generic name: only files that also mention the table or its entity were searched)",
		"The deployed code is the merge base ba5e000000000000:",
	} {
		if !strings.Contains(p, w) {
			t.Errorf("prompt lacks %q:\n%s", w, p)
		}
	}
	if strings.Index(p, "a.up.sql:9 drop_column") > strings.Index(p, "b.up.sql:2 drop_column") {
		t.Errorf("places must be ordered by file:\n%s", p)
	}
	if strings.Count(p, "generic name") != 1 {
		t.Errorf("only the generic name is marked:\n%s", p)
	}
}

// The list of places: its header, one line per place (object omitted when
// the operation has none), nothing about skipped files when none were.
func TestRenderMigrationsPromptPlaces(t *testing.T) {
	res := &migrations.Result{
		Files: []migrations.File{{Path: "db/a.up.sql", Framework: migrations.FwSQL, New: true}},
		Findings: []migrations.Finding{
			{Op: migrations.Op{Kind: migrations.CreateIndex, Path: "db/a.up.sql", Line: 3}},
			{Op: migrations.Op{Kind: migrations.CreateIndex, Path: "db/a.up.sql", Line: 2, Table: "t"}},
		},
	}
	p := renderMigrationsPrompt(res, "b", "")
	want := "Places to check:\n- db/a.up.sql:2 create_index_not_concurrently t: why to check: "
	if !strings.Contains(p, want) || !strings.Contains(p, ".\n- db/a.up.sql:3 create_index_not_concurrently: why to check: ") ||
		strings.Contains(p, "more migration file(s) not scanned") || strings.Contains(p, "found none of the operation kinds") ||
		!strings.HasSuffix(p, ".\n</untrusted_migration_facts>\n") {
		t.Errorf("prompt:\n%s", p)
	}
}

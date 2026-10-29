package migrations

import (
	"reflect"
	"testing"
)

// TestFilesFromDiffHunks: added ranges are new-side line numbers across
// several hunks; removed lines and "\ No newline" markers do not advance
// the new side, header-like lines inside a hunk are content.
func TestFilesFromDiffHunks(t *testing.T) {
	diff := `diff --git a/db/migrations/V1__a.sql b/db/migrations/V1__a.sql
index 1..2 100644
--- a/db/migrations/V1__a.sql
+++ b/db/migrations/V1__a.sql
@@ -1,4 +1,5 @@
 one
-two
--- removed sql comment
+TWO
+THREE
 four
@@ -10,3 +11,4 @@ ctx
 ten
+++ added line that looks like a header
-eleven
\ No newline at end of file
+ELEVEN
\ No newline at end of file
diff --git a/x/old.sql b/x/new.sql
similarity index 80%
rename from x/old.sql
rename to x/new.sql
--- a/x/old.sql
+++ b/x/new.sql
@@ -3 +3 @@
-a
+b
`
	got := FilesFromDiff(diff)
	want := []DiffFile{
		{OldPath: "db/migrations/V1__a.sql", NewPath: "db/migrations/V1__a.sql", Added: []Range{{2, 3}, {12, 13}}, Removed: true},
		{OldPath: "x/old.sql", NewPath: "x/new.sql", Added: []Range{{3, 3}}, Removed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if !got[1].Existed() || !got[1].Changed() {
		t.Error("renamed and edited file: Existed/Changed")
	}
	if (DiffFile{OldPath: "a", NewPath: "a"}).Changed() {
		t.Error("no added lines, same path: not changed")
	}
	if (DiffFile{OldPath: "a", NewPath: "a", New: true}).Existed() {
		t.Error("a new file did not exist")
	}
	if got := FilesFromDiff("garbage\n+not a file\n"); len(got) != 0 {
		t.Errorf("lines before the first file header: %+v", got)
	}
}

// TestFilesFromDiffHeaderOnly: a header without ---/+++ lines (pure mode
// change or binary) still yields its paths; a quoted header does not.
func TestFilesFromDiffHeaderOnly(t *testing.T) {
	got := FilesFromDiff("diff --git a/m/V1__a.sql b/m/V1__a.sql\nold mode 100644\nnew mode 100755\n" +
		"diff --git \"a/m/x y.sql\" \"b/m/x y.sql\"\nold mode 100644\nnew mode 100755\n")
	want := []DiffFile{{OldPath: "m/V1__a.sql", NewPath: "m/V1__a.sql"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestInputAdded(t *testing.T) {
	in := Input{Added: []Range{{3, 5}, {9, 9}}}
	for line, want := range map[int]bool{2: false, 3: true, 4: true, 5: true, 6: false, 8: false, 9: true, 10: false} {
		if in.added(line) != want {
			t.Errorf("added(%d) = %v", line, !want)
		}
	}
	if !(Input{New: true}).added(42) {
		t.Error("new file: every line is added")
	}
}

// TestAnalyzeSameMRSkips: operations on a table created in the merge
// request (any file, any case) are skipped; drops, renames and type changes
// of a column it adds are skipped, but other operations on that column and
// operations on other columns are kept.
func TestAnalyzeSameMRSkips(t *testing.T) {
	res := Analyze([]Input{
		{Path: "db/migrations/2.up.sql", New: true, Content: []byte(
			"ALTER TABLE Fresh DROP COLUMN a;\n" +
				"ALTER TABLE orders RENAME COLUMN Added_Col TO b;\n" +
				"ALTER TABLE orders ALTER COLUMN added_col TYPE text;\n" +
				"ALTER TABLE orders DROP COLUMN added_col;\n" +
				"ALTER TABLE orders ALTER COLUMN added_col SET NOT NULL;\n" +
				"ALTER TABLE other ALTER COLUMN added_col TYPE text;\n" +
				"DROP TABLE public.fresh;\n")},
		{Path: "db/migrations/1.up.sql", New: true, Content: []byte(
			"CREATE TABLE fresh (id int);\nALTER TABLE orders ADD COLUMN added_col int;\n")},
	}, Config{})
	want := "major set_not_null orders.added_col@5; major alter_type other.added_col@6"
	if got := findingsStr(res.Findings); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestAnalyzeModifiedMigration: an edited pre-existing migration is
// reported at its first added line (line 1 for a pure rename); a new file
// never is.
func TestAnalyzeModifiedMigration(t *testing.T) {
	res := Analyze([]Input{
		{Path: "db/migrations/1.up.sql", Existed: true, Added: []Range{{4, 5}, {8, 8}}, Content: []byte("SELECT 1;\n")},
		{Path: "db/migrations/2.up.sql", Existed: true, Content: []byte("SELECT 1;\n")},
		{Path: "db/migrations/3.up.sql", New: true, Existed: true, Content: []byte("SELECT 1;\n")},
	}, Config{})
	want := "major modified_migration @4; major modified_migration @1"
	if got := findingsStr(res.Findings); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if !res.Files[0].Modified || !res.Files[1].Modified || res.Files[2].Modified {
		t.Errorf("modified flags %+v", res.Files)
	}
}

// TestAnalyzeDedupAndOrder: the same operation reported twice on one line
// is kept once; findings are sorted by level, then path, then line.
func TestAnalyzeDedupAndOrder(t *testing.T) {
	res := Analyze([]Input{
		{Path: "db/migrations/b.up.sql", New: true, Content: []byte("UPDATE t SET a = 1 WHERE id = 1;\nCREATE INDEX i ON t (a);\n")},
		{Path: "db/migrations/a.up.sql", New: true, Content: []byte(
			"UPDATE t SET a = 1 WHERE id = 1;\nCREATE INDEX i ON t (a); CREATE INDEX j ON T (b);\nCREATE INDEX k ON u (c);\n")},
	}, Config{})
	got := ""
	for _, f := range res.Findings {
		got += string(f.Level) + " " + f.Path + ":" + opStr(f.Op) + "; "
	}
	want := "major db/migrations/a.up.sql:create_index_not_concurrently t@2; " +
		"major db/migrations/a.up.sql:create_index_not_concurrently u@3; " +
		"major db/migrations/b.up.sql:create_index_not_concurrently t@2; " +
		"minor db/migrations/a.up.sql:data_backfill t@1; " +
		"minor db/migrations/b.up.sql:data_backfill t@1; "
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	for _, f := range res.Findings {
		if f.Framework != FwGolangMigrate {
			t.Errorf("framework %q", f.Framework)
		}
	}
}

// TestRemovalOnlyEditIsModification: deleting lines from an existing
// migration changes it as much as adding lines does (environments that
// already ran it keep the old schema); a mode-only change does not.
func TestRemovalOnlyEditIsModification(t *testing.T) {
	diff := `diff --git a/db/migrations/V1__a.sql b/db/migrations/V1__a.sql
index 1..2 100644
--- a/db/migrations/V1__a.sql
+++ b/db/migrations/V1__a.sql
@@ -1,2 +1,1 @@
 CREATE TABLE a (id int);
-ALTER TABLE a DROP COLUMN b;
diff --git a/db/migrations/V2__b.sql b/db/migrations/V2__b.sql
old mode 100644
new mode 100755
`
	fs := FilesFromDiff(diff)
	if len(fs) != 2 || !fs[0].Removed || !fs[0].Changed() || !fs[0].Existed() || fs[1].Changed() {
		t.Fatalf("files %+v", fs)
	}
	var inputs []Input
	for _, f := range fs {
		inputs = append(inputs, Input{Path: f.NewPath, Content: []byte("CREATE TABLE a (id int);\n"), New: f.New,
			Existed: f.Existed() && f.Changed(), Added: f.Added})
	}
	if got := findingsStr(Analyze(inputs, Config{}).Findings); got != "major modified_migration @1" {
		t.Errorf("got %s", got)
	}
}

// TestFilesFromDiffEmptyFiles: an empty file added or deleted has no
// ---/+++ lines; the mode lines alone decide that it is new / deleted (a
// new empty migration is not a modified old one).
func TestFilesFromDiffEmptyFiles(t *testing.T) {
	diff := "diff --git a/db/migrations/V9__empty.sql b/db/migrations/V9__empty.sql\nnew file mode 100644\nindex 0000000..e69de29\n" +
		"diff --git a/db/migrations/V1__gone.sql b/db/migrations/V1__gone.sql\ndeleted file mode 100644\nindex e69de29..0000000\n"
	got := FilesFromDiff(diff)
	want := []DiffFile{{NewPath: "db/migrations/V9__empty.sql", New: true}, {OldPath: "db/migrations/V1__gone.sql", Deleted: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if got[0].Existed() {
		t.Error("new empty file existed")
	}
}

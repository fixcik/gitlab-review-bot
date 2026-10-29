package migrations

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func findingsStr(fs []Finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, string(f.Level)+" "+opStr(f.Op))
	}
	return strings.Join(out, "; ")
}

func TestFilesFromDiff(t *testing.T) {
	diff := `diff --git a/db/migrations/V1__init.sql b/db/migrations/V1__init.sql
index 1..2 100644
--- a/db/migrations/V1__init.sql
+++ b/db/migrations/V1__init.sql
@@ -1,2 +1,3 @@
 CREATE TABLE a (id int);
+ALTER TABLE a ADD b int;
 -- end
diff --git a/db/migrations/V2__new.sql b/db/migrations/V2__new.sql
new file mode 100644
--- /dev/null
+++ b/db/migrations/V2__new.sql
@@ -0,0 +1,2 @@
+DROP TABLE x;
+DROP TABLE y;
diff --git a/db/migrations/V3__old.sql b/db/migrations/V3__renamed.sql
similarity index 100%
rename from db/migrations/V3__old.sql
rename to db/migrations/V3__renamed.sql
diff --git a/gone.sql b/gone.sql
deleted file mode 100644
--- a/gone.sql
+++ /dev/null
@@ -1 +0,0 @@
-x
`
	got := FilesFromDiff(diff)
	want := []DiffFile{
		{OldPath: "db/migrations/V1__init.sql", NewPath: "db/migrations/V1__init.sql", Added: []Range{{2, 2}}},
		{NewPath: "db/migrations/V2__new.sql", New: true, Added: []Range{{1, 2}}},
		{OldPath: "db/migrations/V3__old.sql", NewPath: "db/migrations/V3__renamed.sql"},
		{OldPath: "gone.sql", Deleted: true, Removed: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if !got[0].Existed() || !got[0].Changed() || got[1].Existed() || !got[2].Existed() || !got[2].Changed() {
		t.Error("Existed/Changed")
	}
}

func TestAnalyzeScopes(t *testing.T) {
	inputs := []Input{
		// New Flyway migration: everything counts; the index on a table
		// created in the same MR (other file) is skipped.
		{Path: "db/migration/V5__changes.sql", New: true, Content: []byte(
			"ALTER TABLE orders DROP COLUMN created_by;\nCREATE INDEX i ON audit (id);\nALTER TABLE orders DROP COLUMN fresh;\nALTER TABLE orders ALTER COLUMN fresh2 SET NOT NULL;\n")},
		{Path: "db/migration/V4__audit.sql", New: true, Content: []byte(
			"CREATE TABLE audit (id int);\nALTER TABLE orders ADD COLUMN fresh int;\nALTER TABLE orders ADD COLUMN fresh2 int;\n")},
		// Modified old migration: only its added line is analysed, plus
		// the modified-migration finding.
		{Path: "db/migration/V1__init.sql", Existed: true, Added: []Range{{3, 3}}, Content: []byte(
			"DROP TABLE old_one;\nCREATE TABLE b (id int);\nCREATE INDEX j ON users (email);\n")},
		// Not a migration.
		{Path: "src/app.py", New: true, Content: []byte("op.drop_table('x')")},
	}
	res := Analyze(inputs, Config{})
	got := findingsStr(res.Findings)
	want := "major modified_migration @3; major create_index_not_concurrently users@3; major drop_column orders.created_by@1; major set_not_null orders.fresh2@4"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if len(res.Files) != 3 || !res.IsMigration("db/migration/V1__init.sql") || res.IsMigration("src/app.py") {
		t.Errorf("files %+v", res.Files)
	}
	if !res.Files[2].Modified || res.Files[0].Modified {
		t.Errorf("modified flags %+v", res.Files)
	}

	// MySQL: online-DDL kinds are not reported.
	res = Analyze([]Input{{Path: "db/migrations/1.up.sql", New: true, Content: []byte("CREATE INDEX i ON t (a);\nALTER TABLE t ADD UNIQUE (b);\nALTER TABLE t MODIFY c bigint;")}},
		Config{Dialect: MySQL})
	if got := findingsStr(res.Findings); got != "major alter_type t.c@3" {
		t.Errorf("mysql: %s", got)
	}

	// max_files.
	res = Analyze([]Input{
		{Path: "db/migrations/1.up.sql", New: true, Content: []byte("DROP TABLE a;")},
		{Path: "db/migrations/2.up.sql", New: true, Content: []byte("DROP TABLE b;")},
	}, Config{MaxFiles: 1})
	if len(res.Files) != 1 || res.SkippedFiles != 1 {
		t.Errorf("max files: %+v skipped %d", res.Files, res.SkippedFiles)
	}
}

func TestNames(t *testing.T) {
	if got := columnVariants("created_by"); !reflect.DeepEqual(got, []string{"created_by", "createdBy", "CreatedBy"}) {
		t.Errorf("column created_by: %v", got)
	}
	if got := columnVariants("createdBy"); !reflect.DeepEqual(got, []string{"createdBy", "created_by", "CreatedBy"}) {
		t.Errorf("column createdBy: %v", got)
	}
	if got := tableVariants("order_items"); !reflect.DeepEqual(got, []string{"order_items", "orderItems", "OrderItems", "OrderItem"}) {
		t.Errorf("table order_items: %v", got)
	}
	if got := tableVariants("categories"); !reflect.DeepEqual(got, []string{"categories", "Category"}) {
		t.Errorf("table categories: %v", got)
	}
	if got := tableVariants("Order"); !reflect.DeepEqual(got, []string{"Order"}) {
		t.Errorf("table Order: %v", got)
	}
	for n, want := range map[string]bool{"status": true, "id": true, "ab": true, "createdAt": true, "created_by": false, "legacy_flag": false} {
		if generic(n) != want {
			t.Errorf("generic(%q) = %v", n, !want)
		}
	}
}

// fakeTree is a commit tree per revision.
type fakeTree map[string]map[string]string

func (f fakeTree) ListFiles(_ context.Context, rev string) ([]string, error) {
	var out []string
	for p := range f[rev] {
		out = append(out, p)
	}
	return out, nil
}

func (f fakeTree) ShowFile(_ context.Context, rev, p string) ([]byte, error) {
	s, ok := f[rev][p]
	if !ok {
		return nil, core.ErrNotFound
	}
	return []byte(s), nil
}

func TestCheckUsages(t *testing.T) {
	tree := fakeTree{
		"base": {
			"src/Entity/Order.php":                 "<?php\nclass Order {\n    #[ORM\\Column(name: 'created_by')]\n    private ?int $createdBy;\n    private string $status;\n}\n",
			"src/Report.php":                       "<?php\n$status = 'ok';\n",
			"src/Legacy.go":                        "type Row struct { CreatedBy int }\n",
			"tests/OrderTest.php":                  "<?php $o->createdBy = 1;\n",
			"migrations/Version1.php":              "<?php $this->addSql('ALTER TABLE orders ADD created_by INT');\n",
			"vendor/lib/x.php":                     "<?php $created_by = 1;\n",
			"src/Users.php":                        "<?php\n// legacy_flag is gone\n /* legacy_flag */\n",
			"templates/order.html.twig":            "{{ order.createdBy }}\n",
			"config/doctrine/Order.orm.xml":        "<field name=\"createdBy\" column=\"created_by\"/>\n",
			"config/services.yaml":                 "created_by: x\n",
			"db/migration/V5__changes.sql":         "ALTER TABLE orders DROP COLUMN created_by;\n",
			"src/Search/sessions_cleanup_task.php": "<?php // nothing\n",
		},
		"head": {
			"src/Entity/Order.php": "<?php\nclass Order {\n    private string $status;\n}\n",
			"src/Report.php":       "<?php\n$status = 'ok';\n",
		},
	}
	res := Analyze([]Input{{Path: "db/migration/V5__changes.sql", New: true, Content: []byte(
		"ALTER TABLE orders DROP COLUMN created_by;\nALTER TABLE orders DROP COLUMN status;\nALTER TABLE users DROP COLUMN legacy_flag;\nDROP TABLE sessions;\n")}}, Config{})
	cfg := Config{}
	CheckUsages(context.Background(), TreeSearcher{Tree: tree}, res, UsageOptions{Base: "base", Head: "head", MaxUsages: 3,
		Keep: func(p string) bool { return !cfg.IsMigrationPath(p) }})

	by := map[string]Finding{}
	for _, f := range res.Findings {
		by[f.Object()] = f
	}
	cb := by["orders.created_by"]
	if cb.Level != LevelCritical || !cb.BreaksOldCode() || cb.Usage.BaseTotal != 5 || len(cb.Usage.Base) != 3 || cb.Usage.HeadTotal != 0 || cb.Usage.Ambiguous {
		t.Fatalf("created_by: %+v %+v", cb, cb.Usage)
	}
	if u := cb.Usage.Base[0]; u.Path != "config/doctrine/Order.orm.xml" || u.Line != 1 {
		t.Errorf("first usage %+v", u)
	}
	var paths []string
	for _, u := range cb.Usage.Base {
		paths = append(paths, u.Path)
	}
	if strings.Contains(strings.Join(paths, ","), "tests/") || strings.Contains(strings.Join(paths, ","), "vendor/") {
		t.Errorf("tests or vendor counted: %v", paths)
	}
	// Generic name: only files mentioning orders/Order count (not Report.php),
	// and the merge request itself still uses it.
	st := by["orders.status"]
	if !st.Usage.Ambiguous || st.Usage.BaseTotal != 1 || st.Usage.HeadTotal != 1 || st.Level != LevelMajor || !st.BreaksOldCode() {
		t.Errorf("status: %+v %+v", st, st.Usage)
	}
	if lf := by["users.legacy_flag"]; lf.Level != LevelInfo || lf.Usage.BaseTotal != 0 {
		t.Errorf("legacy_flag (mentioned in a comment): %+v %+v", lf, lf.Usage)
	}
	if ss := by["sessions"]; ss.Level != LevelInfo || ss.Usage.BaseTotal != 0 || !ss.Usage.Checked {
		t.Errorf("sessions: %+v %+v", ss, ss.Usage)
	}
	if res.Findings[0].Object() != "orders.created_by" {
		t.Errorf("order: %s", findingsStr(res.Findings))
	}

	// A failing searcher leaves the check undone: major, never critical.
	res = Analyze([]Input{{Path: "db/migration/V5__changes.sql", New: true, Content: []byte("ALTER TABLE orders DROP COLUMN created_by;")}}, Config{})
	CheckUsages(context.Background(), errSearcher{}, res, UsageOptions{Base: "base", Head: "head"})
	if f := res.Findings[0]; f.Level != LevelMajor || f.Usage.Checked || !strings.Contains(f.Usage.Err, "boom") || f.BreaksOldCode() {
		t.Errorf("failed search: %+v %+v", f, f.Usage)
	}
	res = Analyze([]Input{{Path: "db/migration/V5__changes.sql", New: true, Content: []byte("ALTER TABLE orders DROP COLUMN created_by;")}}, Config{})
	CheckUsages(context.Background(), nil, res, UsageOptions{Base: "base"})
	if f := res.Findings[0]; f.Level != LevelMajor || f.Usage.Checked {
		t.Errorf("no searcher: %+v", f)
	}
}

type errSearcher struct{}

func (errSearcher) Search(context.Context, string, []string) ([]Hit, bool, error) {
	return nil, false, errors.New("boom")
}

type fakeGrepper struct {
	words, specs []string
	out          []core.GrepMatch
}

func (g *fakeGrepper) GrepWords(_ context.Context, _ string, words, specs []string, _ int) ([]core.GrepMatch, bool, error) {
	g.words, g.specs = words, specs
	return g.out, true, nil
}

func TestGrepSearcher(t *testing.T) {
	g := &fakeGrepper{out: []core.GrepMatch{{Path: "src/a.php", Line: 3, Text: "$x->createdBy"}}}
	hits, partial, err := GrepSearcher{G: g}.Search(context.Background(), "abc", []string{"created_by", "createdBy"})
	if err != nil || !partial || len(hits) != 1 || hits[0].Path != "src/a.php" || hits[0].Line != 3 {
		t.Fatalf("hits %+v %v %v", hits, partial, err)
	}
	if !reflect.DeepEqual(g.words, []string{"created_by", "createdBy"}) || len(g.specs) == 0 || g.specs[0] != "*.php" {
		t.Errorf("args %v %v", g.words, g.specs)
	}
	for p, want := range map[string]bool{"src/a.php": true, "app/models.py": true, "tests/a.php": false, "a_test.go": false,
		"vendor/x/a.php": false, "config/x.yaml": false, "config/x.orm.yml": true, "db/schema.sql": false, "sql/q.sql": true, "README.md": false} {
		if IsSourceFile(p) != want {
			t.Errorf("IsSourceFile(%q) = %v", p, !want)
		}
	}
}

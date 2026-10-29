package migrations

import "testing"

func TestCandidate(t *testing.T) {
	cfg := Config{Exclude: []string{"db/seeds/**", "  ", ""}}
	for p, want := range map[string]bool{
		"db/migrations/1.sql": true, "db/migrations/1.SQL": true, "a/Version1.php": true, "a/0001_x.py": true,
		"db/seeds/1.sql": false, "a/b.go": false, "a/sql": false, "README.md": false,
	} {
		if cfg.Candidate(p) != want {
			t.Errorf("Candidate(%q) = %v", p, !want)
		}
	}
}

// TestIsMigrationPath: what counts as a migration by path alone — such
// files never count as deployed usage of a dropped object.
func TestIsMigrationPath(t *testing.T) {
	cfg := Config{Paths: []string{"schema/changes/*.sql"}}
	for p, want := range map[string]bool{
		"db/migrations/x.sql":                         true,
		"app/Migrations/Version1.php":                 true,
		"src/Version20261020120000.php":               true,
		"database/2026_10_20_120000_add_orders.php":   true,
		"shop/0042_order_changes.py":                  true,
		"alembic_dir/1a2b3c4d5e6f_drop_created_by.py": true,
		"db/migrate/20261020_add.rb":                  true,
		"sqitch/deploy/x.sql":                         true,
		"x/V3__init.sql":                              true,
		"x/001.up.sql":                                true,
		"schema/changes/42.sql":                       true,
		"schema/other/42.sql":                         false,
		"src/Entity/Order.php":                        false,
		"src/Version.php":                             false,
		"app/models.py":                               false,
		"x/1a2b3c_short.py":                           false,
		"src/deploy/nested/x.sql":                     false,
	} {
		if cfg.IsMigrationPath(p) != want {
			t.Errorf("IsMigrationPath(%q) = %v", p, !want)
		}
	}
}

func TestDetectFrameworks(t *testing.T) {
	cases := []struct {
		path, text string
		fw         Framework
		ok         bool
	}{
		// SQL markers win over the path, and work anywhere.
		{"tools/x.sql", "-- +goose Up\nDROP TABLE a;", FwGoose, true},
		{"tools/x.sql", "  --  +GOOSE   UP\nDROP TABLE a;", FwGoose, true},
		{"tools/x.sql", "-- migrate:up\nDROP TABLE a;", FwDbmate, true},
		{"db/migrations/V1__x.sql", "-- +goose Up\n", FwGoose, true},
		{"db/migrations/x/up.sql", "DROP TABLE a;", FwGolangMigrate, true},
		{"db/migrations/R__views.sql", "DROP TABLE a;", FwFlyway, true},
		{"db/migrations/V1.2.3__x.sql", "DROP TABLE a;", FwFlyway, true},
		{"db/migrations/Vx__x.sql", "DROP TABLE a;", FwSQL, true},
		{"db/migrations/plain.sql", "DROP TABLE a;", FwSQL, true},
		{"tools/x.sql", "-- goose Up\nDROP TABLE a;", "", false},
		// Reverse direction is never analysed, even with markers.
		{"db/migrations/1.down.sql", "-- +goose Up\nDROP TABLE a;", "", false},
		{"db/migrations/U2__x.sql", "DROP TABLE a;", "", false},
		{"db/revert/x.sql", "-- migrate:up\nDROP TABLE a;", "", false},
		{"verify/x.sql", "DROP TABLE a;", "", false},
		// PHP.
		{"src/Anything.php", "<?php class V extends AbstractMigration { function up() {} }", FwDoctrine, true},
		{"src/Anything.php", "<?php use Doctrine\\Migrations\\Version;", FwDoctrine, true},
		{"src/Anything.php", "<?php Schema::table('x', fn($t) => 1); function up() {}", FwLaravel, true},
		{"src/Anything.php", "<?php use Illuminate\\Database\\Schema\\Blueprint; public function UP () {}", FwLaravel, true},
		{"src/Anything.php", "<?php Schema::table('x', fn($t) => 1);", "", false},
		{"db/migrations/Thing.php", "<?php function up() { }", FwDoctrine, true},
		{"src/Thing.php", "<?php function up() { }", "", false},
		{"db/migrations/Thing.php", "<?php function upgrade() { }", "", false},
		// Python.
		{"anywhere/x.py", "from django.db import migrations\nclass Migration( migrations.Migration ):\n", FwDjango, true},
		{"anywhere/x.py", "import alembic\n", FwAlembic, true},
		{"anywhere/x.py", "    from alembic import op\n", FwAlembic, true},
		{"anywhere/x.py", "# from alembic import op is not needed\n", "", false},
		{"db/migrations/x.py", "def upgrade():\n    pass\n", "", false},
		// Other extensions.
		{"db/migrations/x.rb", "drop_table :a", "", false},
	}
	for _, c := range cases {
		fw, ok := Config{}.Detect(c.path, []byte(c.text))
		if ok != c.ok || fw != c.fw {
			t.Errorf("Detect(%q, %q) = %q %v, want %q %v", c.path, c.text, fw, ok, c.fw, c.ok)
		}
	}
}

func TestForwardSQL(t *testing.T) {
	cases := []struct {
		fw         Framework
		text, want string
		line       int
	}{
		{FwGoose, "-- comment\n\n-- +goose Up\nA;\n-- +goose Down\nB;\n", "\nA;\n", 3},
		{FwGoose, "-- +goose Down\nB;\n-- +goose Up\nA;\n", "\nA;\n", 3},
		{FwGoose, "-- +goose Up\nA;\n", "\nA;\n", 1},
		{FwGoose, "A;\n", "A;\n", 1},
		{FwDbmate, "-- migrate:up\nA;\n-- migrate:down\nB;\n", "\nA;\n", 1},
		{FwDbmate, "-- +goose Up\nA;\n-- +goose Down\nB;\n", "-- +goose Up\nA;\n-- +goose Down\nB;\n", 1},
		{FwSQL, "-- +goose Up\nA;\n-- +goose Down\nB;\n", "-- +goose Up\nA;\n-- +goose Down\nB;\n", 1},
	}
	for _, c := range cases {
		got, line := forwardSQL(c.fw, c.text)
		if got != c.want || line != c.line {
			t.Errorf("forwardSQL(%s, %q) = %q @%d, want %q @%d", c.fw, c.text, got, line, c.want, c.line)
		}
	}
	// Line numbers of the Up section are file lines.
	ops := extract(FwGoose, "-- x\n-- +goose Up\n\nDROP TABLE a;\n-- +goose Down\nDROP TABLE b;\n", Postgres)
	if got := opsStr(ops); got != "drop_table a@4" {
		t.Errorf("goose ops: %s", got)
	}
}

func TestConfigDefaults(t *testing.T) {
	if (Config{}).dialect() != Postgres || (Config{Dialect: "oracle"}).dialect() != Postgres || (Config{Dialect: MySQL}).dialect() != MySQL {
		t.Error("dialect")
	}
	if (Config{}).maxFiles() != 50 || (Config{MaxFiles: -1}).maxFiles() != 50 || (Config{MaxFiles: 3}).maxFiles() != 3 {
		t.Error("maxFiles")
	}
	if (Config{}).maxUsages() != 5 || (Config{MaxUsages: 2}).maxUsages() != 2 {
		t.Error("maxUsages")
	}
}

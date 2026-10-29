package migrations

import (
	"strings"
	"testing"
)

// TestAnalyzeSQLEdges pins tokenizer and statement-splitting details that
// decide which operation (and which table) a statement yields.
func TestAnalyzeSQLEdges(t *testing.T) {
	cases := []struct {
		name, sql string
		d         Dialect
		want      string
	}{
		{"statements without space after semicolon", "DROP TABLE a;DROP TABLE b;ALTER TABLE c DROP COLUMN d", Postgres,
			"drop_table a@1; drop_table b@1; drop_column c.d@1"},
		{"empty string literal keeps the rest of the statement", "UPDATE t SET a = '' WHERE id = 1;", Postgres,
			"data_backfill t@1"},
		{"string literal then comma without space", "ALTER TABLE t ADD COLUMN a text DEFAULT 'x',DROP COLUMN b;", Postgres,
			"add_column t.a@1; drop_column t.b@1"},
		{"mysql rename without TO", "ALTER TABLE a RENAME b; ALTER TABLE c RENAME AS d;", MySQL,
			"rename_table a>b@1; rename_table c>d@1"},
		{"inline unique column", "ALTER TABLE t ADD COLUMN e text UNIQUE, ADD f int PRIMARY KEY;", Postgres,
			"add_column t.e@1; add_unique_constraint t.e@1; add_column t.f@1; add_unique_constraint t.f@1"},
		{"insert select with where only", "INSERT INTO b (x) SELECT x FROM a WHERE x > 0;", Postgres,
			"data_backfill b@1"},
		{"insert select with limit only", "INSERT INTO b (x) SELECT x FROM a LIMIT 10;", Postgres,
			"data_backfill b@1"},
		{"non-ascii identifiers", "ALTER TABLE заказы DROP COLUMN автор;", Postgres,
			"drop_column заказы.автор@1"},
		{"dollar in identifier", "DROP TABLE tmp$old;", Postgres, "drop_table tmp$old@1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := opsStr(analyzeSQL(c.sql, 1, c.d)); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestAnalyzeSQLTruncated: statements cut off at any token (a migration
// being edited, a heuristic split) never panic and yield no operation
// (or only an unnamed lock/rewrite).
func TestAnalyzeSQLTruncated(t *testing.T) {
	for _, s := range []string{
		"ALTER", "ALTER TABLE", "ALTER TABLE IF", "ALTER TABLE IF EXISTS", "ALTER TABLE t",
		"ALTER TABLE t DROP", "ALTER TABLE t DROP COLUMN", "ALTER TABLE t DROP COLUMN IF",
		"ALTER TABLE t RENAME", "ALTER TABLE t RENAME COLUMN", "ALTER TABLE t RENAME COLUMN a TO",
		"ALTER TABLE t RENAME TO", "ALTER TABLE t ALTER", "ALTER TABLE t ALTER COLUMN c SET",
		"ALTER TABLE t ALTER COLUMN c SET NOT", "ALTER TABLE t MODIFY", "ALTER TABLE t CHANGE",
		"ALTER TABLE t ADD", "ALTER TABLE t ADD CONSTRAINT", "ALTER TABLE t ADD CONSTRAINT x",
		"ALTER TABLE t ADD COLUMN", "ALTER TABLE t ADD COLUMN IF NOT",
		"DROP", "DROP TABLE", "DROP TABLE IF", "RENAME", "RENAME TABLE", "RENAME TABLE a TO",
		"CREATE", "CREATE OR", "CREATE OR REPLACE", "CREATE TEMP", "CREATE TABLE", "CREATE TABLE IF NOT",
		"CREATE INDEX", "CREATE INDEX i ON", "CREATE UNIQUE INDEX i ON ONLY",
		"UPDATE", "UPDATE ONLY", "DELETE", "DELETE FROM", "DELETE FROM ONLY", "INSERT", "INSERT INTO",
		"TRUNCATE", "TRUNCATE TABLE", "LOCK", "LOCK TABLE", "OPTIMIZE", "OPTIMIZE TABLE", "CLUSTER", "CLUSTER VERBOSE",
	} {
		t.Run(s, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic: %v", r)
				}
			}()
			ops := analyzeSQL(s+";", 1, Postgres)
			if strings.HasSuffix(s, " IF") || strings.HasSuffix(s, " NOT") {
				return // "IF" / "NOT" may be read as a name: only no panic matters
			}
			for _, o := range ops {
				if o.Kind != LockTable && o.Kind != VacuumFull && o.Kind != Cluster {
					t.Errorf("unexpected op %s", opStr(o))
				}
			}
		})
	}
}

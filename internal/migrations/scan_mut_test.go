package migrations

import "testing"

// TestMaskSQLEdges: comments, strings and dollar bodies hide exactly their
// own text — no more (real SQL right after them is analysed), no less (SQL
// inside them never is).
func TestMaskSQLEdges(t *testing.T) {
	cases := []struct {
		name, sql, want string
	}{
		{"block comment right before a statement", "/* note */DROP TABLE a;", "drop_table a@1"},
		{"doubled quote right before the closing one", "UPDATE t SET a = 'x''' WHERE id = 1;", "data_backfill t@1"},
		{"dollar body after code with a statement list inside",
			"CREATE FUNCTION f() RETURNS void AS $$ BEGIN UPDATE x SET a = 1; DROP TABLE y; END $$ LANGUAGE plpgsql;\nDROP TABLE z;",
			"drop_table z@2"},
		{"positional parameter is not a dollar tag", "UPDATE t SET a = $1 WHERE id = $2; DROP TABLE b;", "data_backfill t@1; drop_table b@1"},
		{"unterminated block comment", "DROP TABLE a; /* DROP TABLE b;", "drop_table a@1"},
		{"slash at the end", "DROP TABLE a; /", "drop_table a@1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := opsStr(analyzeSQL(c.sql, 1, Postgres)); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestMaskPHPEdges: heredocs closed at column 0 (PHP < 7.3 style), escaped
// line breaks, sources ending abruptly.
func TestMaskPHPEdges(t *testing.T) {
	src := `<?php
final class Version4 extends AbstractMigration
{
    public function up(Schema $schema): void
    {
        $this->addSql(<<<SQL
ALTER TABLE orders DROP COLUMN a
SQL
        );
        $this->addSql("ALTER TABLE orders\nDROP COLUMN b");
        $this->addSql(<<<'SQL'
            DROP TABLE c
            SQL);
    }
}
`
	if got := opsStr(analyzePHP(src, Postgres)); got != "drop_column orders.a@7; drop_column orders.b@10; drop_table c@12" {
		t.Errorf("got %s", got)
	}
	for _, s := range []string{"<?php $x = 1; /", "<?php $x = 'abc", "<?php <<<", "<?php <<<SQL\nDROP TABLE x", "<?php <<<'SQL"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%q: panic %v", s, r)
				}
			}()
			analyzePHP(s, Postgres)
		}()
	}
}

// TestMaskPythonEdges: arguments without spaces after commas, escaped line
// breaks, unterminated strings at the end of the file.
func TestMaskPythonEdges(t *testing.T) {
	src := "from alembic import op\n\n\ndef upgrade():\n    op.drop_column(\"a\",\"b\")\n    op.execute(\"UPDATE a SET x = 1;\\nDROP TABLE c\")\n"
	if got := opsStr(analyzeAlembic(src, Postgres)); got != "unbounded_dml a@6; drop_table c@6; drop_column a.b@5" {
		t.Errorf("got %s", got)
	}
	for _, s := range []string{"def upgrade():\n    x = \"abc", "def upgrade():\n    x = '''abc", "def upgrade():\n    x = 'a\\"} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%q: panic %v", s, r)
				}
			}()
			analyzeAlembic(s, Postgres)
		}()
	}
}

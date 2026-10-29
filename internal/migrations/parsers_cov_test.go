package migrations

import (
	"context"
	"errors"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Backslash escapes of PHP double-quoted and Python strings: the common ones
// decoded, others kept as written, \n a space (one source line).
func TestUnescape(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:                 "plain",
		`a\tb\rc\nd`:            "a\tb\rc d",
		"\\\\ \\\" \\' \\$ \\`": "\\ \" ' $ `",
		`\q \x41`:               `\q \x41`,
		`trailing\`:             `trailing\`,
		`DROP \"t\"\tnow`:       "DROP \"t\"\tnow",
		`\\\\server\\path`:      `\\server\path`,
	} {
		if got := unescape(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// What the masks hide from the extractors (comments, unterminated strings)
// and what they keep (escaped SQL in double-quoted strings, heredocs).
func TestMaskedSources(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		fw        Framework
		want      string
	}{
		{"php block comment", `<?php
/* $this->addSql('DROP TABLE commented'); */
$this->addSql("ALTER TABLE \"orders\"\tDROP COLUMN \$note");
$this->addSql('DROP TABLE after');
`, FwDoctrine, "drop_column orders.$note@3; drop_table after@4"},
		{"php heredoc with a space", "<?php\n$this->addSql(<<< SQL\nDROP TABLE spaced\nSQL);\n", FwDoctrine, "drop_table spaced@3"},
		{"php heredoc without a body", "<?php\n$this->addSql(<<<SQL", FwDoctrine, ""},
		{"php unclosed call", "<?php\n$this->addSql('DROP TABLE x'\n", FwDoctrine, ""},
		{"php method without arguments", "<?php\nSchema::drop();\nSchema::table();\n$s->dropTable();\n$s->getTable('t')->dropColumn();\n", FwLaravel, ""},
		{"php sql of a variable", "<?php\n$this->addSql($sql);\n$this->addSql('DROP TABLE ' . $name);\n", FwDoctrine, ""},
		{"python comment and raw string", `from alembic import op


def upgrade():
    # op.drop_table("commented")
    op.execute(r"DROP TABLE raw\_name")
    op.execute(R'DROP TABLE upper_r')
`, FwAlembic, "drop_table raw@6; drop_table upper_r@7"},
		{"python unterminated string", "from alembic import op\n\n\ndef upgrade():\n    op.execute(\"DROP TABLE x\n    op.drop_table(\"y\")\n", FwAlembic, `drop_table y@6`},
		{"sql unterminated quoted identifier", "DROP TABLE a;\nALTER TABLE \"b DROP COLUMN c;\n", FwSQL, "drop_table a@1"},
		{"sql unterminated backtick", "DROP TABLE a;\nALTER TABLE `b DROP COLUMN c;\n", FwSQL, "drop_table a@1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := opsStr(extract(tc.fw, tc.src, Postgres)); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// Calls the extractors cannot read completely are skipped, never guessed.
func TestIncompleteFrameworkCalls(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		fw        Framework
		want      string
	}{
		{"doctrine change/add without a column", `<?php
final class V extends AbstractMigration
{
    public function up(Schema $s): void
    {
        $t = $s->getTable('c');
        $t->changeColumn($name, []);
        $t->modifyColumn();
        $t->addColumn($name, 'integer');
        $t->dropColumn('kept');
    }
}
`, FwDoctrine, "drop_column c.kept@10"},
		{"doctrine getTable without a name", `<?php
final class V extends AbstractMigration
{
    public function up(Schema $s): void
    {
        $t = $s->getTable($name);
        $t->dropColumn('x');
        $s->dropTable('y');
    }
}
`, FwDoctrine, "drop_table y@8"},
		{"laravel table without a closure", `<?php
return new class extends Migration {
    public function up(): void
    {
        Schema::table('orders', $callback);
        Schema::table('orders', function (Blueprint $table) {
            $table->dropColumn('z');
        });
    }
};
`, FwLaravel, "drop_column orders.z@7"},
		{"laravel unclosed up", "<?php\nclass M { public function up() { Schema::drop('t');\n", FwLaravel, "drop_table t@2"},
		{"php up without a body", "<?php\n$this->addSql('DROP TABLE t');\nabstract function up()", FwDoctrine, "drop_table t@2"},
		{"php up with unclosed parameters", "<?php\n$this->addSql('DROP TABLE t');\nfunction up(Schema $s", FwDoctrine, "drop_table t@2"},
		{"php up with an unclosed body", "<?php\nfunction up() {\n$this->addSql('DROP TABLE t');\n", FwDoctrine, "drop_table t@3"},
		{"alembic incomplete", `from alembic import op
import sqlalchemy as sa


def upgrade():
    op.add_column(sa.Column("c", sa.Integer()))
    op.add_column("t")
    op.add_column("t", sa.Column(name, sa.Integer(), nullable=False))
    op.alter_column("t")
    op.alter_column(table, "c", new_column_name="d")
    with op.batch_alter_table() as b:
        b.drop_column("x")
    op.execute(sql)
    op.drop_column("t", "kept")
`, FwAlembic, "drop_column t.kept@14"},
		{"alembic batch outside upgrade", `from alembic import op


with op.batch_alter_table("outside") as b:
    b.drop_column("x")


def upgrade():
    op.drop_table("t")
`, FwAlembic, "drop_table t@9"},
		{"django incomplete", `from django.db import migrations, models


class Migration(migrations.Migration):
    operations = [
        migrations.AlterField(model_name="order", field=models.IntegerField()),
        migrations.AlterField("order", "total"),
        migrations.AddConstraint(constraint=models.UniqueConstraint(fields=["a"], name="u")),
        migrations.RunSQL(sql),
        migrations.RemoveField(model_name="order", name="old"),
    ]
`, FwDjango, "alter_type order.total weak@7; drop_column order.old@10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := opsStr(extract(tc.fw, tc.src, Postgres)); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// SQL forms: inheritance star, no action, MySQL inline indexes, RENAME
// TABLE lists, unique indexes.
func TestAnalyzeSQLMoreForms(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		d         Dialect
		want      string
	}{
		{"star", "ALTER TABLE orders * DROP COLUMN note;", Postgres, "drop_column orders.note@1"},
		{"no action", "ALTER TABLE orders;", Postgres, ""},
		{"mysql add index", "ALTER TABLE orders ADD INDEX i (a), ADD KEY k (b), ADD FULLTEXT f (c);", MySQL, ""},
		{"rename list", "RENAME TABLE a TO b, broken, c TO d;", MySQL, "rename_table a>b@1; rename_table c>d@1"},
		{"unique index", "CREATE UNIQUE INDEX u ON orders (a);", Postgres, "create_index_not_concurrently orders@1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := opsStr(analyzeSQL(tc.sql, 1, tc.d)); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
	ops := analyzeSQL("CREATE UNIQUE INDEX u ON orders (a);", 1, Postgres)
	if len(ops) != 1 || ops[0].Detail != "UNIQUE" {
		t.Errorf("unique detail: %+v", ops)
	}
}

func TestGitHeaderPathsMalformed(t *testing.T) {
	for _, s := range []string{"x/a b/a", "a/only-one-path", `"a/q" "b/q"`} {
		if a, b, ok := gitHeaderPaths(s); ok {
			t.Errorf("%q: %q %q", s, a, b)
		}
	}
	if a, b, ok := gitHeaderPaths("a/x y b/z"); !ok || a != "x y" || b != "z" {
		t.Errorf("%q %q %v", a, b, ok)
	}
}

type errTree struct{}

func (errTree) ListFiles(context.Context, string) ([]string, error) {
	return nil, errors.New("ls-tree: bad object")
}
func (errTree) ShowFile(context.Context, string, string) ([]byte, error) { return nil, nil }

type errGrepper struct{}

func (errGrepper) GrepWords(context.Context, string, []string, []string, int) ([]core.GrepMatch, bool, error) {
	return nil, false, errors.New("git grep: killed")
}

// A failed search is an error, not "no usages": the drop stays major and
// the search is reported as not done.
func TestSearchersFail(t *testing.T) {
	for name, s := range map[string]Searcher{
		"tree": TreeSearcher{Tree: errTree{}, MaxFiles: 10, MaxBytes: 1 << 20, MaxFileBytes: 1 << 20},
		"grep": GrepSearcher{G: errGrepper{}, MaxBytes: 1 << 20},
	} {
		if hits, _, err := s.Search(context.Background(), "base", []string{"created_by"}); err == nil || hits != nil {
			t.Errorf("%s: %v %v", name, hits, err)
		}
		res := Analyze([]Input{{Path: "db/migrations/1.up.sql", New: true, Content: []byte("ALTER TABLE orders DROP COLUMN created_by;")}}, Config{})
		CheckUsages(context.Background(), s, res, UsageOptions{Base: "base", Head: "head"})
		f := res.Findings[0]
		if f.Usage.Checked || f.Level != LevelMajor || f.BreaksOldCode() || f.Usage.Err == "" {
			t.Errorf("%s: %+v %+v", name, f, f.Usage)
		}
	}
}

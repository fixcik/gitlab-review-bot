package migrations

import "testing"

func TestDoctrineDetails(t *testing.T) {
	src := `<?php
final class Version2 extends AbstractMigration
{
    public function up(Schema $s): void
    {
        $this->connection->executeStatement('DROP TABLE a');
        $schema->dropTable('not_the_schema_var');
        $s->dropTable('b');
        $t = $s->getTable('c');
        $t->addColumn('d', 'integer', ['default' => 0]);
        $t->addColumn('e', 'string', ['length' => 10]);
        $t->addColumn('f', 'string', ['notnull' => false]);
        $t->addColumn('g', 'integer', ['autoincrement' => true]);
        $t->modifyColumn('h', ['type' => Type::getType('bigint')]);
        $t->changeColumn('i', ['comment' => 'x']);
        $t->addIndex(['d']);
        $t->addUniqueIndex(['e']);
        $t->addUniqueConstraint(['e']);
        $t->addForeignKeyConstraint('users', ['d'], ['id']);
        $other->dropColumn('j');
        $n = $s->createTable('fresh');
        $n->addColumn('id', 'integer');
    }
}
`
	want := "drop_table a@6; drop_table b@8; create_table fresh@21; add_column c.d@10; add_column c.e@11; add_column_not_null_no_default c.e@11; " +
		"add_column c.f@12; add_column c.g@13; alter_type c.h@14; create_index_not_concurrently c@16; create_index_not_concurrently c@17; " +
		"add_unique_constraint c@18; add_fk_validated c@19; add_column fresh.id@22; add_column_not_null_no_default fresh.id@22"
	if got := opsStr(analyzePHP(src, Postgres)); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestPHPWithoutUp: a migration without up() is analysed whole, except its
// down() body.
func TestPHPWithoutUp(t *testing.T) {
	src := `<?php
$this->addSql('DROP TABLE before');
function down() {
    $this->addSql('DROP TABLE in_down');
}
$this->addSql('DROP TABLE after');
`
	if got := opsStr(analyzePHP(src, Postgres)); got != "drop_table before@2; drop_table after@6" {
		t.Errorf("got %s", got)
	}
}

func TestLaravelBlueprintMore(t *testing.T) {
	src := `<?php
return new class extends Migration {
    public function up(): void
    {
        Schema::table('orders', function (Blueprint $table) {
            $table->dropMorphs('owner');
            $table->morphs('taggable');
            $table->rename('purchases');
            $table->unique('code');
            $table->foreign('user_id')->references('id')->on('users');
            $table->timestamp('paid_at')->useCurrent();
            $table->bigInteger('seq')->autoIncrement();
            $table->string('ref')->nullable(false);
            $table->string('opt')->nullable(true);
            $table->foreignId('shop_id')->references('id')->on('shops')->default(1);
            $table->string('slug')->default('')->unique()->index();
            $table->dropRememberToken();
            $table->dropSoftDeletesTz();
            $other->dropColumn('nope');
            $table->unknownMacro('x');
        });
        DB::unprepared('TRUNCATE orders');
    }
};
`
	want := "truncate orders@22; drop_column orders.owner_id@6; drop_column orders.owner_type@6; add_column orders.taggable_id@7; " +
		"add_column_not_null_no_default orders.taggable_id@7; rename_table orders>purchases@8; add_unique_constraint orders@9; " +
		"add_fk_validated orders@10; add_column orders.paid_at@11; add_column orders.seq@12; add_column orders.ref@13; " +
		"add_column_not_null_no_default orders.ref@13; add_column orders.opt@14; add_column orders.shop_id@15; add_fk_validated orders.shop_id@15; " +
		"add_column orders.slug@16; add_unique_constraint orders.slug@16; create_index_not_concurrently orders.slug@16; " +
		"drop_column orders.remember_token@17; drop_column orders.deleted_at@18"
	if got := opsStr(analyzePHP(src, Postgres)); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestAlembicDetails(t *testing.T) {
	src := `from alembic import op
import sqlalchemy as sa


def upgrade():
    op.drop_column(table_name="a", column_name="b")
    op.add_column("a", sa.Column("c", sa.Integer(), nullable=False, server_default=None))
    op.add_column("a", sa.Column("d", sa.Integer(), sa.ForeignKey("u.id"), nullable=True))
    op.create_foreign_key("fk", "a", "u", ["d"], ["id"], postgresql_not_valid=True)
    op.create_foreign_key("fk2", "a", "u", ["d"], ["id"])
    op.create_check_constraint("ck", "a", "c > 0")
    op.create_unique_constraint("uq", "a", ["c"])
    with op.batch_alter_table("e") as b:
        b.alter_column("f", new_column_name="g")
        b.add_column(sa.Column("h", sa.Text(), nullable=False))
    other.drop_table("x")
    op.drop_table("y")


def upgrade_data():
    op.execute("DELETE FROM a WHERE c = 0")
    def helper():
        op.drop_table("nested")
    op.drop_table("z")


def downgrade():
    op.drop_table("down")
`
	want := "data_backfill a@21; drop_column a.b@6; add_column a.c@7; add_column_not_null_no_default a.c@7; add_column a.d@8; add_fk_validated a.d@8; " +
		"add_fk_validated a@10; add_check_validated a@11; add_unique_constraint a@12; rename_column e.f>g@14; add_column e.h@15; " +
		"add_column_not_null_no_default e.h@15; drop_table y@17; drop_table nested@23; drop_table z@24"
	if got := opsStr(analyzeAlembic(src, Postgres)); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestDjangoDetails(t *testing.T) {
	src := `from django.db import migrations, models

SQL = "DROP TABLE from_var"
migrations.DeleteModel(name="Outside")


class Migration(migrations.Migration):
    operations = [
        migrations.RunSQL(SQL),
        migrations.RunSQL(["DROP TABLE one", "DROP TABLE two"]),
        migrations.AddField("order", "flag", models.BooleanField(default=False)),
        migrations.AddField("order", "owner", models.ForeignKey(to="auth.user", on_delete=models.CASCADE)),
        migrations.AddConstraint("order", models.UniqueConstraint(fields=["a"], name="u")),
        migrations.AddConstraint(model_name="order", constraint=models.CheckConstraint(check=models.Q(a__gt=0), name="c")),
        migrations.AlterUniqueTogether(name="order", unique_together={("a", "b")}),
        migrations.RenameModel("Cart", "Basket"),
        migrations.RemoveField(model_name="order", name="old"),
    ]
`
	want := "drop_table one@10; drop_table two@10; add_column order.flag@11; add_column order.owner@12; add_column_not_null_no_default order.owner@12; " +
		"add_unique_constraint order@13; add_check_validated order@14; add_unique_constraint order weak@15; rename_table Cart>Basket@16; drop_column order.old@17"
	if got := opsStr(analyzeDjango(src, Postgres)); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestSQLLiteralLines: SQL split over several literals (concatenation,
// statement lists, after a heredoc) keeps file line numbers, so ops land on
// the line that holds them — which also decides whether an op is on an
// added line of a modified migration.
func TestSQLLiteralLines(t *testing.T) {
	php := `<?php
final class Version3 extends AbstractMigration
{
    public function up(Schema $schema): void
    {
        $this->addSql('ALTER TABLE orders ' .
            'DROP COLUMN a, ' .

            'DROP COLUMN b');
        $this->addSql(<<<SQL
            DROP TABLE c;
            SQL . 'DROP TABLE d');
    }
}
`
	if got := opsStr(analyzePHP(php, Postgres)); got != "drop_column orders.a@7; drop_column orders.b@9; drop_table c@11; drop_table d@12" {
		t.Errorf("php: %s", got)
	}
	py := `from alembic import op


def upgrade():
    op.execute(
        "UPDATE orders SET a = 1;"
        "DROP TABLE x"
    )
    op.execute(["DROP TABLE y", "DROP TABLE z",
                "DROP TABLE w"])
`
	if got := opsStr(analyzeAlembic(py, Postgres)); got != "unbounded_dml orders@6; drop_table x@7; drop_table y@9; drop_table z@9; drop_table w@10" {
		t.Errorf("python: %s", got)
	}
	res := Analyze([]Input{{Path: "migrations/Version3.php", Existed: true, Added: []Range{{9, 9}}, Content: []byte(php)}}, Config{})
	if got := findingsStr(res.Findings); got != "major modified_migration @9; major drop_column orders.b@9" {
		t.Errorf("modified migration scope: %s", got)
	}
}

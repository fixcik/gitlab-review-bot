package migrations

import (
	"strings"
	"testing"
)

const doctrineMigration = `<?php
declare(strict_types=1);

namespace DoctrineMigrations;

use Doctrine\DBAL\Schema\Schema;
use Doctrine\Migrations\AbstractMigration;

final class Version20261020120000 extends AbstractMigration
{
    public function up(Schema $schema): void
    {
        // $this->addSql('DROP TABLE commented_out');
        $this->addSql('ALTER TABLE orders DROP COLUMN created_by');
        $this->addSql("ALTER TABLE orders RENAME COLUMN total TO amount");
        $this->addSql(<<<'SQL'
            CREATE INDEX idx_orders_status
                ON orders (status)
            SQL);
        $this->addSql('CREATE TABLE audit (id INT NOT NULL)');
        $this->addSql('CREATE INDEX idx_audit ON audit (id)');
        $table = $schema->getTable('users');
        $table->dropColumn('legacy_flag');
        $table->addColumn('nickname', 'string', ['notnull' => false]);
        $table->addColumn('score', 'integer');
        $table->changeColumn('email', ['notnull' => true]);
        $schema->dropTable('sessions');
        $schema->renameTable('carts', 'baskets');
        $schema->getTable('items')->renameColumn('qty', 'quantity');
    }

    public function down(Schema $schema): void
    {
        $this->addSql('ALTER TABLE orders ADD created_by INT');
        $this->addSql('DROP TABLE everything');
        $schema->dropTable('baskets');
    }
}
`

const laravelMigration = `<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        Schema::create('invoices', function (Blueprint $table) {
            $table->id();
            $table->string('number');
            $table->index('number');
        });
        Schema::table('orders', function (Blueprint $table) {
            $table->dropColumn(['created_by', 'note']);
            $table->renameColumn('total', 'amount');
            $table->string('code');
            $table->string('comment')->nullable();
            $table->integer('priority')->default(0);
            $table->string('email')->nullable(false)->change();
            $table->text('body')->change();
            $table->foreignId('customer_id')->nullable()->constrained();
            $table->index('status');
            $table->dropTimestamps();
        });
        Schema::dropIfExists('legacy');
        Schema::rename('carts', 'baskets');
        DB::statement('UPDATE orders SET archived = false');
    }

    public function down(): void
    {
        Schema::drop('invoices');
        Schema::table('orders', function (Blueprint $table) {
            $table->dropColumn('code');
        });
    }
};
`

const alembicMigration = `"""drop created_by

Revision ID: 1a2b3c4d5e6f
"""
from alembic import op
import sqlalchemy as sa

revision = "1a2b3c4d5e6f"
down_revision = "0f9e8d7c6b5a"


def upgrade() -> None:
    op.drop_column("orders", "created_by")
    op.alter_column("orders", "total", new_column_name="amount")
    op.alter_column("orders", "email", existing_type=sa.String(), nullable=False)
    op.alter_column("orders", "code", type_=sa.String(64))
    op.add_column("orders", sa.Column("region", sa.String(), nullable=False))
    op.add_column("orders", sa.Column("flag", sa.Boolean(), nullable=False, server_default=sa.false()))
    op.create_index("ix_orders_status", "orders", ["status"])
    op.create_index("ix_orders_code", "orders", ["code"], postgresql_concurrently=True)
    op.create_table("audit", sa.Column("id", sa.Integer()))
    op.create_index("ix_audit_id", "audit", ["id"])
    op.rename_table("carts", "baskets")
    with op.batch_alter_table("users") as batch_op:
        batch_op.drop_column("legacy")
        batch_op.create_index("ix_users_name", ["name"])
    op.execute("""
        UPDATE orders SET archived = false
    """)
    op.drop_table("sessions")


def downgrade() -> None:
    op.add_column("orders", sa.Column("created_by", sa.Integer()))
    op.drop_table("everything")
    op.execute("DROP TABLE orders")
`

const djangoMigration = `from django.db import migrations, models
import django.db.models.deletion


class Migration(migrations.Migration):

    dependencies = [("shop", "0041_auto")]

    operations = [
        migrations.RemoveField(
            model_name="order",
            name="created_by",
        ),
        migrations.RenameField("order", "total", "amount"),
        migrations.AlterField(
            model_name="order",
            name="email",
            field=models.CharField(max_length=200, null=False),
        ),
        migrations.AlterField(model_name="order", name="code", field=models.CharField(max_length=64)),
        migrations.AddField(model_name="order", name="region", field=models.CharField(max_length=10)),
        migrations.AddField(model_name="order", name="note", field=models.TextField(null=True)),
        migrations.AddField(model_name="order", name="tags", field=models.ManyToManyField(to="shop.tag")),
        migrations.AddField(model_name="order", name="customer", field=models.ForeignKey(null=True, on_delete=django.db.models.deletion.SET_NULL, to="shop.customer")),
        migrations.AddIndex(model_name="order", index=models.Index(fields=["status"], name="order_status_idx")),
        migrations.CreateModel(name="Audit", fields=[("id", models.AutoField(primary_key=True))]),
        migrations.AddIndex(model_name="audit", index=models.Index(fields=["id"], name="audit_idx")),
        migrations.DeleteModel(name="Session"),
        migrations.RenameModel(old_name="Cart", new_name="Basket"),
        migrations.RunSQL(
            sql="UPDATE shop_order SET archived = false",
            reverse_sql="DROP TABLE shop_order",
        ),
    ]
`

func TestExtractors(t *testing.T) {
	cases := []struct {
		name, path, text string
		fw               Framework
		want             string
	}{
		{"doctrine: up only, sql and schema api", "migrations/Version20261020120000.php", doctrineMigration, FwDoctrine,
			"drop_column orders.created_by@14; rename_column orders.total>amount@15; create_index_not_concurrently orders@17; " +
				"create_table audit@20; create_index_not_concurrently audit@21; " +
				"drop_table sessions@27; rename_table carts>baskets@28; rename_column items.qty>quantity@29; " +
				"drop_column users.legacy_flag@23; add_column users.nickname@24; add_column users.score@25; " +
				"add_column_not_null_no_default users.score@25; set_not_null users.email@26"},
		{"laravel: up only, blueprint", "database/migrations/2026_10_20_120000_change_orders.php", laravelMigration, FwLaravel,
			"unbounded_dml orders@31; create_table invoices@12; drop_column orders.created_by@18; drop_column orders.note@18; " +
				"rename_column orders.total>amount@19; add_column orders.code@20; add_column_not_null_no_default orders.code@20; " +
				"add_column orders.comment@21; add_column orders.priority@22; set_not_null orders.email@23; alter_type orders.body weak@24; " +
				"add_column orders.customer_id@25; add_fk_validated orders.customer_id@25; create_index_not_concurrently orders@26; " +
				"drop_column orders.created_at@27; drop_column orders.updated_at@27; drop_table legacy@29; rename_table carts>baskets@30"},
		{"alembic: upgrade only", "alembic/versions/1a2b3c4d5e6f_drop_created_by.py", alembicMigration, FwAlembic,
			"unbounded_dml orders@28; drop_column orders.created_by@13; rename_column orders.total>amount@14; set_not_null orders.email@15; " +
				"alter_type orders.code@16; add_column orders.region@17; add_column_not_null_no_default orders.region@17; add_column orders.flag@18; " +
				"create_index_not_concurrently orders@19; create_table audit@21; create_index_not_concurrently audit@22; " +
				"rename_table carts>baskets@23; drop_column users.legacy@25; create_index_not_concurrently users@26; drop_table sessions@30"},
		{"django: operations, reverse_sql ignored", "shop/migrations/0042_order_changes.py", djangoMigration, FwDjango,
			"drop_column order.created_by@10; rename_column order.total>amount@14; set_not_null order.email@15; " +
				"alter_type order.code weak@20; add_column order.region@21; add_column_not_null_no_default order.region@21; " +
				"add_column order.note@22; add_column order.customer@24; add_fk_validated order.customer@24; " +
				"create_index_not_concurrently order@25; create_table Audit@26; create_index_not_concurrently audit@27; " +
				"drop_table Session@28; rename_table Cart>Basket@29; unbounded_dml shop_order@31"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fw, ok := Config{}.Detect(c.path, []byte(c.text))
			if !ok || fw != c.fw {
				t.Fatalf("detect: %q %v", fw, ok)
			}
			if got := opsStr(extract(fw, c.text, Postgres)); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestSQLFrameworks(t *testing.T) {
	goose := "-- +goose Up\n-- +goose StatementBegin\nALTER TABLE orders DROP COLUMN created_by;\n-- +goose StatementEnd\n\n-- +goose Down\nALTER TABLE orders ADD COLUMN created_by int;\nDROP TABLE orders;\n"
	dbmate := "-- migrate:up\nALTER TABLE a RENAME TO b;\n-- migrate:down\nDROP TABLE b;\n"
	cases := []struct {
		path, text string
		fw         Framework
		ok         bool
		want       string
	}{
		{"db/migrations/20261020_drop.sql", goose, FwGoose, true, "drop_column orders.created_by@3"},
		{"anywhere/dbmate.sql", dbmate, FwDbmate, true, "rename_table a>b@2"},
		{"db/migrations/000042_drop.up.sql", "DROP TABLE x;", FwGolangMigrate, true, "drop_table x@1"},
		{"db/migrations/000042_drop.down.sql", "DROP TABLE x;", "", false, ""},
		{"src/main/resources/db/migration/V42__drop.sql", "DROP TABLE x;", FwFlyway, true, "drop_table x@1"},
		{"src/main/resources/db/migration/V1_2__x.sql", "DROP TABLE x;", FwFlyway, true, "drop_table x@1"},
		{"src/main/resources/db/migration/U42__drop.sql", "CREATE TABLE x (id int);", "", false, ""},
		{"sqitch/deploy/drop_x.sql", "BEGIN; DROP TABLE x; COMMIT;", FwSqitch, true, "drop_table x@1"},
		{"sqitch/revert/drop_x.sql", "CREATE TABLE x (id int);", "", false, ""},
		{"sqitch/verify/drop_x.sql", "SELECT 1/0 FROM x;", "", false, ""},
		{"diesel/migrations/2026_x/down.sql", "DROP TABLE x;", "", false, ""},
		{"sql/queries/orders.sql", "DELETE FROM orders;", "", false, ""}, // not a migration path
		{"app/migrations/__init__.py", "", "", false, ""},
		{"app/migrations/helpers.php", "<?php function x() {}", "", false, ""},
		{"config/schema.sql", "DROP TABLE x;", "", false, ""},
	}
	for _, c := range cases {
		fw, ok := Config{}.Detect(c.path, []byte(c.text))
		if ok != c.ok || fw != c.fw {
			t.Errorf("%s: detect %q %v, want %q %v", c.path, fw, ok, c.fw, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got := opsStr(extract(fw, c.text, Postgres)); got != c.want {
			t.Errorf("%s: got %s, want %s", c.path, got, c.want)
		}
	}
	// Extra paths and excludes.
	cfg := Config{Paths: []string{"schema/changes/*.sql"}, Exclude: []string{"db/migrations/legacy/**"}}
	if fw, ok := cfg.Detect("schema/changes/42.sql", []byte("DROP TABLE x")); !ok || fw != FwSQL {
		t.Errorf("extra path: %q %v", fw, ok)
	}
	if _, ok := cfg.Detect("db/migrations/legacy/1.sql", []byte("DROP TABLE x")); ok {
		t.Error("excluded path detected")
	}
}

func TestLaravelDetails(t *testing.T) {
	got := opsStr(extract(FwLaravel, laravelMigration, Postgres))
	for _, want := range []string{
		"create_table invoices@12", "drop_column orders.created_by@18", "drop_column orders.note@18",
		"rename_column orders.total>amount@19", "add_column_not_null_no_default orders.code@20",
		"set_not_null orders.email@23", "alter_type orders.body weak@24", "add_fk_validated orders.customer_id@25",
		"create_index_not_concurrently orders@26", "drop_column orders.created_at@27", "drop_column orders.updated_at@27",
		"drop_table legacy@29", "rename_table carts>baskets@30", "unbounded_dml orders@31",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	for _, bad := range []string{"add_column_not_null_no_default orders.comment", "add_column_not_null_no_default orders.priority",
		"add_column_not_null_no_default orders.customer_id", "drop_table invoices", "orders.code@3 ", "@36", "@37", "@38"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in\n%s", bad, got)
		}
	}
}

func TestLaravelArrowClosureAndFacadeVariants(t *testing.T) {
	src := `<?php
return new class extends Migration {
    public function up(): void
    {
        Schema::table('orders', fn (Blueprint $t) => $t->dropColumn('created_by'));
        Schema::connection('pgsql')->table('users', static function (Blueprint $table): void {
            $table->dropSoftDeletes();
        });
        Schema::dropColumns('carts', ['a', 'b']);
    }
};
`
	got := opsStr(extract(FwLaravel, src, Postgres))
	want := "drop_column orders.created_by@5; drop_column users.deleted_at@7; drop_column carts.a@9; drop_column carts.b@9"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

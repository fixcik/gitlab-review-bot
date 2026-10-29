package migrations

import (
	"fmt"
	"strings"
	"testing"
)

// opStr renders an operation compactly: "kind table.column>new@line".
func opStr(o Op) string {
	s := string(o.Kind) + " " + o.Table
	if o.Column != "" {
		s += "." + o.Column
	}
	if o.NewName != "" {
		s += ">" + o.NewName
	}
	if o.Weak {
		s += " weak"
	}
	return fmt.Sprintf("%s@%d", s, o.Line)
}

func opsStr(ops []Op) string {
	var out []string
	for _, o := range ops {
		out = append(out, opStr(o))
	}
	return strings.Join(out, "; ")
}

func TestAnalyzeSQL(t *testing.T) {
	cases := []struct {
		name, sql string
		d         Dialect
		want      string
	}{
		{"drop column if exists, quoted, schema", `ALTER TABLE IF EXISTS public."Orders" DROP COLUMN IF EXISTS "created_by";`, Postgres,
			"drop_column Orders.created_by@1"},
		{"several actions", "alter table orders\n  drop column a,\n  DROP b cascade,\n  drop constraint x;", Postgres,
			"drop_column orders.a@2; drop_column orders.b@3"},
		{"rename column", "ALTER TABLE orders RENAME COLUMN a TO b", Postgres, "rename_column orders.a>b@1"},
		{"rename column without keyword", "ALTER TABLE orders RENAME a TO b", Postgres, "rename_column orders.a>b@1"},
		{"rename table", "ALTER TABLE a RENAME TO b; RENAME TABLE c TO d, e TO f;", MySQL,
			"rename_table a>b@1; rename_table c>d@1; rename_table e>f@1"},
		{"rename constraint ignored", "ALTER TABLE a RENAME CONSTRAINT x TO y", Postgres, ""},
		{"drop tables", "DROP TABLE IF EXISTS a, public.b CASCADE;", Postgres, "drop_table a@1; drop_table b@1"},
		{"alter type and not null", "ALTER TABLE t ALTER COLUMN c TYPE bigint, ALTER c SET NOT NULL, ALTER COLUMN d SET DATA TYPE text, ALTER e DROP NOT NULL;", Postgres,
			"alter_type t.c@1; set_not_null t.c@1; alter_type t.d@1"},
		{"add column not null", "ALTER TABLE t ADD COLUMN a int NOT NULL, ADD b int NOT NULL DEFAULT 0, ADD COLUMN IF NOT EXISTS c bigserial NOT NULL, ADD d text;", Postgres,
			"add_column t.a@1; add_column_not_null_no_default t.a@1; add_column t.b@1; add_column t.c@1; add_column t.d@1"},
		{"add column references", "ALTER TABLE t ADD COLUMN u int REFERENCES users(id)", Postgres, "add_column t.u@1; add_fk_validated t.u@1"},
		{"constraints", `ALTER TABLE t ADD CONSTRAINT fk FOREIGN KEY (u) REFERENCES users(id);
ALTER TABLE t ADD CONSTRAINT fk2 FOREIGN KEY (u) REFERENCES users(id) NOT VALID;
ALTER TABLE t ADD CONSTRAINT ck CHECK (a > 0);
ALTER TABLE t ADD CHECK (a > 0) NOT VALID;
ALTER TABLE t ADD CONSTRAINT uq UNIQUE (a);
ALTER TABLE t ADD CONSTRAINT uq2 UNIQUE USING INDEX uq2_idx;`, Postgres,
			"add_fk_validated t@1; add_check_validated t@3; add_unique_constraint t@5"},
		{"indexes", `CREATE INDEX idx ON orders (a);
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS i2 ON orders (b);
create index on only public.items using btree (c);`, Postgres,
			"create_index_not_concurrently orders@1; create_index_not_concurrently items@3"},
		{"dml", `UPDATE orders SET a = 1;
UPDATE orders SET a = (SELECT 1 WHERE true) WHERE id < 10;
DELETE FROM logs;
INSERT INTO b (x) SELECT x FROM a;
INSERT INTO c VALUES (1);
TRUNCATE TABLE x, y;`, Postgres,
			"unbounded_dml orders@1; data_backfill orders@2; unbounded_dml logs@3; unbounded_dml b@4; truncate x@6; truncate y@6"},
		{"locks and rewrites", "LOCK TABLE a IN ACCESS EXCLUSIVE MODE; VACUUM FULL b; VACUUM c; CLUSTER d USING i; OPTIMIZE TABLE e;", Postgres,
			"lock_table a@1; vacuum_full b@1; cluster d@1; vacuum_full e@1"},
		{"comments and strings", `-- DROP TABLE nope;
/* ALTER TABLE x DROP COLUMN y; */
COMMENT ON COLUMN t.c IS 'DROP TABLE t; it''s fine';
SELECT $$ DROP TABLE z $$;
DO $body$ BEGIN DROP TABLE w; END $body$;
DROP TABLE real_one;`, Postgres, "drop_table real_one@6"},
		{"mysql modify/change", "ALTER TABLE t MODIFY COLUMN a bigint NOT NULL, CHANGE b c int, CHANGE d d varchar(10);", MySQL,
			"alter_type t.a@1; rename_column t.b>c@1; alter_type t.d@1"},
		{"mysql backslash string", `INSERT INTO t VALUES ('a\'; DROP TABLE x; --'); DROP TABLE y;`, MySQL, "drop_table y@1"},
		{"create table", "CREATE TABLE IF NOT EXISTS public.n (id int); CREATE UNLOGGED TABLE m (id int);", Postgres,
			"create_table n@1; create_table m@1"},
		{"multiline lines", "\n\nALTER TABLE t\n\n  DROP COLUMN x;\n", Postgres, "drop_column t.x@5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := opsStr(analyzeSQL(c.sql, 1, c.d)); got != c.want {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
}

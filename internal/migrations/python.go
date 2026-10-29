package migrations

// Python migrations: Alembic (op.*, batch_alter_table, op.execute; only
// the upgrade functions) and Django (migrations.* operations; RunSQL's
// reverse_sql is ignored).

import (
	"regexp"
	"strings"
)

var (
	pyUpgradeRe     = regexp.MustCompile(`(?m)^([ \t]*)def[ \t]+upgrade\w*[ \t]*\(`)
	pyOpRe          = regexp.MustCompile(`\b(\w+)\s*\.\s*(\w+)\s*\(`)
	pyBatchRe       = regexp.MustCompile(`\bbatch_alter_table\s*\(`)
	pyExecuteRe     = regexp.MustCompile(`\.\s*execute\s*\(`)
	pyAsRe          = regexp.MustCompile(`^\s*as\s+(\w+)`)
	pyNullFalseRe   = regexp.MustCompile(`\bnullable\s*=\s*False\b`)
	pyServerDefRe   = regexp.MustCompile(`\bserver_default\s*=\s*[^N\s]`)
	pyConcurrentRe  = regexp.MustCompile(`\bpostgresql_concurrently\s*=\s*True\b`)
	pyNotValidRe    = regexp.MustCompile(`not_valid\s*=\s*True\b`)
	pyForeignKeyRe  = regexp.MustCompile(`\bForeignKey\s*\(`)
	djOperationsRe  = regexp.MustCompile(`\boperations\s*=\s*\[`)
	djNullTrueRe    = regexp.MustCompile(`\bnull\s*=\s*True\b`)
	djNullFalseRe   = regexp.MustCompile(`\bnull\s*=\s*False\b`)
	djDefaultRe     = regexp.MustCompile(`\b(?:db_)?default\s*=`)
	djM2MRe         = regexp.MustCompile(`\bManyToManyField\s*\(`)
	djFKRe          = regexp.MustCompile(`\b(?:ForeignKey|OneToOneField)\s*\(`)
	djUniqueConsRe  = regexp.MustCompile(`\bUniqueConstraint\s*\(`)
	djCheckConsRe   = regexp.MustCompile(`\bCheckConstraint\s*\(`)
	pyIdentifierArg = regexp.MustCompile(`^\w+$`)
)

// pyBodies returns the bodies of the top-level functions def upgrade*():
// from the def line to the next line indented no deeper than the def.
func pyBodies(s *src) []span {
	var out []span
	for _, m := range pyUpgradeRe.FindAllStringSubmatchIndex(s.code, -1) {
		indent := m[3] - m[2]
		start := m[0]
		end := len(s.code)
		pos := lineEnd(s.code, start)
		for pos < len(s.code) {
			ls := pos + 1
			le := lineEnd(s.code, ls)
			line := s.code[ls:le]
			if t := strings.TrimLeft(line, " \t"); t != "" && len(line)-len(t) <= indent {
				end = ls
				break
			}
			pos = le
		}
		out = append(out, span{start, end})
	}
	return out
}

type pyAnalyzer struct {
	s   *src
	d   Dialect
	ops []Op
}

func (a *pyAnalyzer) add(off int, k Kind, table, column, newName, detail string, weak bool) {
	a.ops = append(a.ops, Op{Kind: k, Line: a.s.line(off), Table: table, Column: column, NewName: newName, Detail: detail, Weak: weak})
}

func (a *pyAnalyzer) sql(sp span) {
	text, content, ok := a.s.sqlText(sp)
	if !ok {
		return
	}
	a.ops = append(a.ops, analyzeSQL(text, a.s.line(content), a.d)...)
}

func (a *pyAnalyzer) str(args []span, i int, kw string) string {
	sp, ok := a.s.arg(args, i, kw)
	if !ok {
		return ""
	}
	v, _ := a.s.str(sp)
	return v
}

// alembicTableArg: index of the table argument of each op.* method (batch
// operations take it from batch_alter_table).
var alembicTableArg = map[string]int{
	"drop_table": 0, "rename_table": 0, "create_table": 0, "drop_column": 0, "add_column": 0, "alter_column": 0,
	"create_index": 1, "create_unique_constraint": 1, "create_foreign_key": 1, "create_check_constraint": 1,
}

var alembicTableKw = map[string]string{
	"create_index": "table_name", "create_unique_constraint": "table_name", "create_foreign_key": "source_table",
	"create_check_constraint": "table_name", "drop_table": "table_name", "drop_column": "table_name",
	"add_column": "table_name", "alter_column": "table_name", "rename_table": "old_table_name",
}

// analyzeAlembic returns the operations of the upgrade functions.
func analyzeAlembic(raw string, d Dialect) []Op {
	s := maskPython(raw)
	a := &pyAnalyzer{s: s, d: d}
	regions := pyBodies(s)
	batch := a.alembicBatches(regions)
	// op.execute(...), op.get_bind().execute(...), conn.execute(text(...)).
	for _, m := range pyExecuteRe.FindAllStringIndex(s.code, -1) {
		if !regionsContain(regions, m[0]) {
			continue
		}
		if args := s.args(m[1] - 1); len(args) > 0 {
			a.sql(args[0])
		}
	}
	for _, m := range pyOpRe.FindAllStringSubmatchIndex(s.code, -1) {
		if !regionsContain(regions, m[0]) {
			continue
		}
		recv, method := s.code[m[2]:m[3]], s.code[m[4]:m[5]]
		open := m[1] - 1
		args := s.args(open)
		bt, isBatch := batch[recv]
		if recv != "op" && !isBatch {
			continue
		}
		ti, known := alembicTableArg[method]
		if !known {
			continue
		}
		table := ""
		if isBatch {
			table = bt
			// Batch methods have no table argument: shift positions.
			ins := min(ti, len(args))
			virt := append(append([]span{}, args[:ins]...), span{-1, -1})
			args = append(virt, args[ins:]...)
		} else {
			table = a.str(args, ti, alembicTableKw[method])
		}
		a.alembicOp(m[0], open, method, table, args)
	}
	return a.ops
}

// alembicBatches maps the variables of batch_alter_table blocks inside
// regions ("with op.batch_alter_table('t') as batch_op") to their tables.
func (a *pyAnalyzer) alembicBatches(regions []span) map[string]string {
	s := a.s
	batch := map[string]string{}
	for _, m := range pyBatchRe.FindAllStringIndex(s.code, -1) {
		if !regionsContain(regions, m[0]) {
			continue
		}
		open := m[1] - 1
		args := s.args(open)
		cl := s.matchClose(open)
		if len(args) == 0 || cl < 0 {
			continue
		}
		t := a.str(args, 0, "table_name")
		if am := pyAsRe.FindStringSubmatch(s.code[cl+1:]); am != nil && t != "" {
			batch[am[1]] = t
		}
	}
	return batch
}

// alembicOp handles one op.* (or batch_op.*) call at off whose arguments
// open at open; a batch call's args have a virtual table argument (a span
// with start -1, which yields no string).
func (a *pyAnalyzer) alembicOp(off, open int, method, table string, args []span) {
	s := a.s
	all := s.unmask(open, max(open+1, s.matchClose(open)))
	switch method {
	case "create_table":
		if table != "" {
			a.add(off, createTable, table, "", "", "", false)
		}
	case "drop_table":
		if table != "" {
			a.add(off, DropTable, table, "", "", "", false)
		}
	case "rename_table":
		if nn := a.str(args, 1, "new_table_name"); table != "" && nn != "" {
			a.add(off, RenameTable, table, "", nn, "", false)
		}
	case "drop_column":
		if c := a.str(args, 1, "column_name"); table != "" && c != "" {
			a.add(off, DropColumn, table, c, "", "", false)
		}
	case "add_column":
		sp, ok := s.arg(args, 1, "column")
		if !ok || sp.start < 0 || table == "" {
			return
		}
		col := ""
		if ls := s.litsIn(sp.start, sp.end); len(ls) > 0 {
			col = ls[0].value
		}
		if col == "" {
			return
		}
		ct := s.unmask(sp.start, sp.end)
		a.add(off, addColumn, table, col, "", "", false)
		if pyNullFalseRe.MatchString(ct) && !pyServerDefRe.MatchString(ct) {
			a.add(off, AddColumnNotNull, table, col, "", "", false)
		}
		if pyForeignKeyRe.MatchString(ct) {
			a.add(off, AddFK, table, col, "", "", false)
		}
	case "alter_column":
		c := a.str(args, 1, "column_name")
		if table == "" || c == "" {
			return
		}
		if sp, ok := s.kwarg(args, "new_column_name"); ok {
			if nn, ok := s.str(sp); ok {
				a.add(off, RenameColumn, table, c, nn, "", false)
			}
		}
		if sp, ok := s.kwarg(args, "nullable"); ok && strings.TrimSpace(s.text(sp)) == "False" {
			a.add(off, SetNotNull, table, c, "", "", false)
		}
		if _, ok := s.kwarg(args, "type_"); ok {
			a.add(off, AlterType, table, c, "", "", false)
		}
	case "create_index":
		if table != "" && !pyConcurrentRe.MatchString(all) {
			a.add(off, CreateIndex, table, "", "", "", false)
		}
	case "create_unique_constraint":
		if table != "" {
			a.add(off, AddUnique, table, "", "", "", false)
		}
	case "create_foreign_key":
		if table != "" && !pyNotValidRe.MatchString(all) {
			a.add(off, AddFK, table, "", "", "", false)
		}
	case "create_check_constraint":
		if table != "" && !pyNotValidRe.MatchString(all) {
			a.add(off, AddCheck, table, "", "", "", false)
		}
	}
}

// analyzeDjango returns the operations of a Django migration (the
// operations list is the forward direction). Tables are model names.
func analyzeDjango(raw string, d Dialect) []Op {
	s := maskPython(raw)
	a := &pyAnalyzer{s: s, d: d}
	region := span{0, len(s.code)}
	if m := djOperationsRe.FindStringIndex(s.code); m != nil {
		if cl := s.matchClose(m[1] - 1); cl > 0 {
			region = span{m[1], cl}
		}
	}
	for _, m := range pyOpRe.FindAllStringSubmatchIndex(s.code, -1) {
		if m[0] < region.start || m[0] >= region.end || s.code[m[2]:m[3]] != "migrations" {
			continue
		}
		a.djangoOp(m[0], s.code[m[4]:m[5]], s.args(m[1]-1))
	}
	return a.ops
}

// djangoOp handles one migrations.* operation at off.
func (a *pyAnalyzer) djangoOp(off int, method string, args []span) {
	s := a.s
	field := func(i int, kw string) string {
		sp, ok := s.arg(args, i, kw)
		if !ok {
			return ""
		}
		return s.unmask(sp.start, sp.end)
	}
	model := a.str(args, 0, "model_name")
	switch method {
	case "CreateModel":
		if n := a.str(args, 0, "name"); n != "" {
			a.add(off, createTable, n, "", "", "", false)
		}
	case "DeleteModel":
		if n := a.str(args, 0, "name"); n != "" {
			a.add(off, DropTable, n, "", "", "", false)
		}
	case "RenameModel":
		if o, nn := a.str(args, 0, "old_name"), a.str(args, 1, "new_name"); o != "" && nn != "" {
			a.add(off, RenameTable, o, "", nn, "", false)
		}
	case "RemoveField":
		if c := a.str(args, 1, "name"); model != "" && c != "" {
			a.add(off, DropColumn, model, c, "", "", false)
		}
	case "RenameField":
		if o, nn := a.str(args, 1, "old_name"), a.str(args, 2, "new_name"); model != "" && o != "" && nn != "" {
			a.add(off, RenameColumn, model, o, nn, "", false)
		}
	case "AlterField":
		c := a.str(args, 1, "name")
		f := field(2, "field")
		if model == "" || c == "" {
			return
		}
		if djNullFalseRe.MatchString(f) {
			a.add(off, SetNotNull, model, c, "", "", false)
		} else {
			a.add(off, AlterType, model, c, "", "AlterField: возможна смена типа или NOT NULL", true)
		}
	case "AddField":
		c := a.str(args, 1, "name")
		f := field(2, "field")
		if model == "" || c == "" || djM2MRe.MatchString(f) {
			return
		}
		a.add(off, addColumn, model, c, "", "", false)
		nullable := djNullTrueRe.MatchString(f)
		if !nullable && !djDefaultRe.MatchString(f) {
			a.add(off, AddColumnNotNull, model, c, "", "", false)
		} else if nullable && djFKRe.MatchString(f) {
			a.add(off, AddFK, model, c, "", "", false)
		}
	case "AddIndex":
		if model != "" {
			a.add(off, CreateIndex, model, "", "", "", false)
		}
	case "AddConstraint":
		f := field(1, "constraint")
		switch {
		case model == "":
		case djUniqueConsRe.MatchString(f):
			a.add(off, AddUnique, model, "", "", "", false)
		case djCheckConsRe.MatchString(f):
			a.add(off, AddCheck, model, "", "", "", false)
		}
	case "AlterUniqueTogether":
		if n := a.str(args, 0, "name"); n != "" {
			a.add(off, AddUnique, n, "", "", "unique_together", true)
		}
	case "RunSQL":
		if sp, ok := s.arg(args, 0, "sql"); ok {
			if pyIdentifierArg.MatchString(s.text(sp)) {
				return // a variable: not analysed
			}
			a.sql(sp)
		}
	}
}

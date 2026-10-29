package migrations

// SQL analysis: token based, no full parser. Statements are split at ";"
// (comments, strings and $$ bodies masked), each statement is tokenized and
// matched against the statement shapes migrations use.

import "strings"

// sqlParser collects the operations of one SQL text.
type sqlParser struct {
	s     *src
	d     Dialect
	line0 int // line of the text's first character in the file
	ops   []Op
}

// analyzeSQL returns the operations of text; line is the file line of
// text's first character.
func analyzeSQL(text string, line int, d Dialect) []Op {
	p := &sqlParser{s: maskSQL(text, d), d: d, line0: line}
	code := p.s.code
	from := 0
	for i := 0; i <= len(code); i++ {
		if i < len(code) && code[i] != ';' {
			continue
		}
		if ts := tokenize(code, from, i); len(ts) > 0 {
			p.statement(ts)
		}
		from = i + 1
	}
	return p.ops
}

func (p *sqlParser) lineOf(t tok) int { return p.line0 + p.s.line(t.off) - 1 }

func (p *sqlParser) add(t tok, k Kind, table, column, newName, detail string) {
	p.ops = append(p.ops, Op{Kind: k, Line: p.lineOf(t), Table: table, Column: column, NewName: newName, Detail: detail})
}

func (p *sqlParser) statement(ts []tok) {
	switch {
	case ts[0].is("ALTER"):
		p.alterTable(ts)
	case ts[0].is("DROP"):
		p.drop(ts)
	case ts[0].is("RENAME"):
		p.renameTables(ts)
	case ts[0].is("CREATE"):
		p.create(ts)
	case ts[0].is("UPDATE"):
		p.update(ts)
	case ts[0].is("DELETE"):
		p.delete(ts)
	case ts[0].is("INSERT", "REPLACE"):
		p.insert(ts)
	case ts[0].is("TRUNCATE"):
		i := skip(ts, 1, "TABLE", "ONLY")
		for _, part := range topLevel(ts[i:]) {
			if t, _ := name(part, skip(part, 0, "ONLY")); t != "" {
				p.add(ts[0], Truncate, t, "", "", "")
			}
		}
	case ts[0].is("LOCK"):
		i := skip(ts, 1, "TABLE", "TABLES", "ONLY")
		t, _ := name(ts, i)
		p.add(ts[0], LockTable, t, "", "", "")
	case ts[0].is("VACUUM"):
		if hasSeq(ts, "FULL") {
			p.add(ts[0], VacuumFull, lastIdent(ts, "FULL", "ANALYZE", "VERBOSE", "FREEZE", "VACUUM"), "", "", "VACUUM FULL")
		}
	case ts[0].is("OPTIMIZE"):
		t, _ := name(ts, skip(ts, 1, "NO_WRITE_TO_BINLOG", "LOCAL", "TABLE"))
		p.add(ts[0], VacuumFull, t, "", "", "OPTIMIZE TABLE")
	case ts[0].is("CLUSTER"):
		t, _ := name(ts, skip(ts, 1, "VERBOSE"))
		p.add(ts[0], Cluster, t, "", "", "")
	}
}

func (p *sqlParser) alterTable(ts []tok) {
	i := skip(ts, 1, "ONLINE", "OFFLINE", "IGNORE")
	if i >= len(ts) || !ts[i].is("TABLE") {
		return
	}
	i = skipIf(ts, i+1, "IF", "EXISTS")
	i = skip(ts, i, "ONLY")
	table, i := name(ts, i)
	if table == "" {
		return
	}
	if i < len(ts) && ts[i].punct("*") {
		i++
	}
	for _, a := range topLevel(ts[i:]) {
		p.alterAction(table, a)
	}
}

// constraintWords follow DROP / ADD when the action is not about a column.
var constraintWords = []string{"CONSTRAINT", "INDEX", "KEY", "PRIMARY", "FOREIGN", "CHECK", "UNIQUE", "PARTITION",
	"DEFAULT", "SYSTEM", "PERIOD", "FULLTEXT", "SPATIAL", "EXCLUDE"}

func (p *sqlParser) alterAction(table string, a []tok) {
	if len(a) == 0 {
		return
	}
	at := a[0]
	switch {
	case at.is("DROP"):
		j := 1
		if j < len(a) && a[j].is(constraintWords...) {
			return
		}
		j = skip(a, j, "COLUMN")
		j = skipIf(a, j, "IF", "EXISTS")
		if col, _ := name(a, j); col != "" {
			p.add(at, DropColumn, table, col, "", "")
		}
	case at.is("RENAME"):
		j := 1
		switch {
		case j < len(a) && a[j].is("TO", "AS"):
			if nn, _ := name(a, j+1); nn != "" {
				p.add(at, RenameTable, table, "", nn, "")
			}
		case j < len(a) && a[j].is("CONSTRAINT", "INDEX", "KEY"):
		default:
			j = skip(a, j, "COLUMN")
			old, k := name(a, j)
			if old == "" {
				return
			}
			if k < len(a) && a[k].is("TO") {
				if nn, _ := name(a, k+1); nn != "" {
					p.add(at, RenameColumn, table, old, nn, "")
				}
			} else if k == len(a) { // MySQL: RENAME new_name
				p.add(at, RenameTable, table, "", old, "")
			}
		}
	case at.is("ALTER"):
		j := skip(a, 1, "COLUMN")
		col, k := name(a, j)
		if col == "" {
			return
		}
		switch {
		case hasSeqAt(a, k, "TYPE"), hasSeqAt(a, k, "SET", "DATA", "TYPE"):
			p.add(at, AlterType, table, col, "", "")
		case hasSeqAt(a, k, "SET", "NOT", "NULL"):
			p.add(at, SetNotNull, table, col, "", "")
		}
	case at.is("MODIFY"):
		j := skip(a, 1, "COLUMN")
		if col, _ := name(a, j); col != "" {
			p.add(at, AlterType, table, col, "", "MODIFY")
		}
	case at.is("CHANGE"):
		j := skip(a, 1, "COLUMN")
		old, k := name(a, j)
		nn, _ := name(a, k)
		switch {
		case old == "":
		case nn != "" && !strings.EqualFold(old, nn):
			p.add(at, RenameColumn, table, old, nn, "CHANGE")
		default:
			p.add(at, AlterType, table, old, "", "CHANGE")
		}
	case at.is("ADD"):
		p.addAction(table, a)
	}
}

func (p *sqlParser) addAction(table string, a []tok) {
	at := a[0]
	j := 1
	if j < len(a) && a[j].is("CONSTRAINT") {
		j++
		if j < len(a) && a[j].ident() && !a[j].is("FOREIGN", "CHECK", "UNIQUE", "PRIMARY", "EXCLUDE") {
			j++
		}
	}
	if j >= len(a) {
		return
	}
	rest := a[j:]
	notValid := hasSeq(rest, "NOT", "VALID")
	switch {
	case a[j].is("FOREIGN"):
		if !notValid {
			p.add(at, AddFK, table, "", "", "")
		}
		return
	case a[j].is("CHECK"):
		if !notValid {
			p.add(at, AddCheck, table, "", "", "")
		}
		return
	case a[j].is("UNIQUE", "PRIMARY"):
		if !hasSeq(rest, "USING", "INDEX") {
			p.add(at, AddUnique, table, "", "", "")
		}
		return
	case a[j].is("INDEX", "KEY", "FULLTEXT", "SPATIAL", "EXCLUDE", "PARTITION"):
		return
	}
	j = skip(a, j, "COLUMN")
	j = skipIf(a, j, "IF", "NOT", "EXISTS")
	col, k := name(a, j)
	if col == "" {
		return
	}
	def := a[k:]
	p.add(at, addColumn, table, col, "", "")
	serial := k < len(a) && a[k].is("SERIAL", "BIGSERIAL", "SMALLSERIAL", "SERIAL4", "SERIAL8", "SERIAL2")
	if hasSeq(def, "NOT", "NULL") && !hasSeq(def, "DEFAULT") && !hasSeq(def, "GENERATED") &&
		!hasSeq(def, "AUTO_INCREMENT") && !hasSeq(def, "IDENTITY") && !serial {
		p.add(at, AddColumnNotNull, table, col, "", "")
	}
	if hasSeq(def, "REFERENCES") {
		p.add(at, AddFK, table, col, "", "")
	}
	if hasSeq(def, "UNIQUE") || hasSeq(def, "PRIMARY", "KEY") {
		p.add(at, AddUnique, table, col, "", "")
	}
}

func (p *sqlParser) drop(ts []tok) {
	i := 1
	if i >= len(ts) || !ts[i].is("TABLE") {
		return
	}
	i = skipIf(ts, i+1, "IF", "EXISTS")
	for _, part := range topLevel(ts[i:]) {
		if t, _ := name(part, 0); t != "" && !part[0].is("CASCADE", "RESTRICT") {
			p.add(ts[0], DropTable, t, "", "", "")
		}
	}
}

// renameTables: MySQL RENAME TABLE a TO b[, c TO d].
func (p *sqlParser) renameTables(ts []tok) {
	if len(ts) < 2 || !ts[1].is("TABLE", "TABLES") {
		return
	}
	for _, part := range topLevel(ts[2:]) {
		old, k := name(part, 0)
		if old == "" || k >= len(part) || !part[k].is("TO") {
			continue
		}
		if nn, _ := name(part, k+1); nn != "" {
			p.add(ts[0], RenameTable, old, "", nn, "")
		}
	}
}

func (p *sqlParser) create(ts []tok) {
	i := skipIf(ts, 1, "OR", "REPLACE")
	i = skip(ts, i, "TEMP", "TEMPORARY", "UNLOGGED", "GLOBAL", "LOCAL")
	if i >= len(ts) {
		return
	}
	switch {
	case ts[i].is("TABLE"):
		j := skipIf(ts, i+1, "IF", "NOT", "EXISTS")
		if t, _ := name(ts, j); t != "" {
			p.add(ts[0], createTable, t, "", "", "")
		}
	case ts[i].is("UNIQUE", "INDEX"):
		unique := ts[i].is("UNIQUE")
		concurrently := false
		for j := i; j < len(ts); j++ {
			if ts[j].is("CONCURRENTLY") {
				concurrently = true
			}
			if ts[j].is("ON") {
				t, _ := name(ts, skip(ts, j+1, "ONLY"))
				if t != "" && !concurrently {
					d := ""
					if unique {
						d = "UNIQUE"
					}
					p.add(ts[0], CreateIndex, t, "", "", d)
				}
				return
			}
		}
	}
}

func (p *sqlParser) update(ts []tok) {
	i := skip(ts, 1, "ONLY", "LOW_PRIORITY", "IGNORE")
	t, _ := name(ts, i)
	if t == "" {
		return
	}
	if hasTopKw(ts, "WHERE") {
		p.add(ts[0], DataBackfill, t, "", "", "UPDATE")
	} else {
		p.add(ts[0], UnboundedDML, t, "", "", "UPDATE без WHERE")
	}
}

func (p *sqlParser) delete(ts []tok) {
	i := skip(ts, 1, "LOW_PRIORITY", "QUICK", "IGNORE")
	if i >= len(ts) || !ts[i].is("FROM") {
		return
	}
	t, _ := name(ts, skip(ts, i+1, "ONLY"))
	if t == "" {
		return
	}
	if hasTopKw(ts, "WHERE") {
		p.add(ts[0], DataBackfill, t, "", "", "DELETE")
	} else {
		p.add(ts[0], UnboundedDML, t, "", "", "DELETE без WHERE")
	}
}

// insert: INSERT INTO t ... SELECT ... FROM copies data; without WHERE /
// LIMIT it copies a whole table.
func (p *sqlParser) insert(ts []tok) {
	i := skip(ts, 1, "LOW_PRIORITY", "DELAYED", "HIGH_PRIORITY", "IGNORE", "INTO")
	t, _ := name(ts, i)
	if t == "" || !hasTopKw(ts, "SELECT") || !hasTopKw(ts, "FROM") {
		return
	}
	if hasTopKw(ts, "WHERE") || hasTopKw(ts, "LIMIT") {
		p.add(ts[0], DataBackfill, t, "", "", "INSERT … SELECT")
	} else {
		p.add(ts[0], UnboundedDML, t, "", "", "INSERT … SELECT без WHERE")
	}
}

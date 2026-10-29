package migrations

// PHP migrations: Doctrine Migrations ($this->addSql, the Schema API) and
// Laravel (Schema facade, Blueprint, DB::statement). Only the body of up()
// is analysed; without an up() the whole file except down() is.

import (
	"regexp"
	"strings"
)

var (
	phpUpRe       = regexp.MustCompile(`(?i)\bfunction\s+up\s*\(`)
	phpDownRe     = regexp.MustCompile(`(?i)\bfunction\s+down\s*\(`)
	phpUpParamRe  = regexp.MustCompile(`^\(\s*(?:\??[\w\\]+\s+)?\$(\w+)`)
	phpSQLCallRe  = regexp.MustCompile(`(?:->\s*(?:addSql|executeStatement|executeQuery|executeUpdate|exec)|\bDB\s*::\s*(?:statement|unprepared|update|delete|insert))\s*\(`)
	phpFacadeRe   = regexp.MustCompile(`\bSchema\s*::\s*(?:connection\s*\([^()]*\)\s*->\s*)?(create|table|drop|dropIfExists|rename|dropColumns)\s*\(`)
	phpSchemaRe   = regexp.MustCompile(`\$(\w+)\s*->\s*(dropTable|renameTable|createTable|getTable)\s*\(`)
	phpAssignRe   = regexp.MustCompile(`\$(\w+)\s*=\s*$`)
	phpTableOpRe  = regexp.MustCompile(`\$(\w+)\s*->\s*(dropColumn|renameColumn|changeColumn|modifyColumn|addColumn|addIndex|addUniqueIndex|addUniqueConstraint|addForeignKeyConstraint)\s*\(`)
	phpChainRe    = regexp.MustCompile(`^\s*->\s*(\w+)\s*\(`)
	phpClosureRe  = regexp.MustCompile(`^(?:static\s+)?(?:function|fn)\s*\(\s*(?:[\w\\]+\s+)?\$(\w+)`)
	phpNotNullFal = regexp.MustCompile(`(?i)'notnull'\s*=>\s*false`)
	phpNotNullTru = regexp.MustCompile(`(?i)'notnull'\s*=>\s*true`)
	phpDefaultRe  = regexp.MustCompile(`(?i)'(?:default|autoincrement)'\s*=>`)
	phpTypeOptRe  = regexp.MustCompile(`(?i)'(?:type|length|precision|scale|unsigned|fixed)'\s*=>|Type::`)
	phpFalseArgRe = regexp.MustCompile(`^(?i:false)$`)
)

// phpBodies returns the bodies ({...}, braces excluded) of the functions
// re matches, with the offset of each parameter list.
func phpBodies(s *src, re *regexp.Regexp) (bodies []span, params []int) {
	for _, m := range re.FindAllStringIndex(s.code, -1) {
		open := m[1] - 1
		cl := s.matchClose(open)
		if cl < 0 {
			continue
		}
		b := strings.IndexByte(s.code[cl:], '{')
		if b < 0 {
			continue
		}
		bo := cl + b
		bc := s.matchClose(bo)
		if bc < 0 {
			continue
		}
		bodies = append(bodies, span{bo + 1, bc})
		params = append(params, open)
	}
	return bodies, params
}

// laravelTypes are Blueprint column methods (adding a column).
var laravelTypes = map[string]bool{}

func init() {
	for _, t := range strings.Fields(`bigInteger binary boolean char dateTimeTz dateTime date decimal double enum float
		foreignId foreignIdFor foreignUlid foreignUuid geometry geography integer ipAddress json jsonb longText macAddress
		mediumInteger mediumText smallInteger string text timeTz time timestampTz timestamp tinyInteger tinyText
		unsignedBigInteger unsignedInteger unsignedMediumInteger unsignedSmallInteger unsignedTinyInteger ulid uuid year
		set point polygon lineString multiPoint multiPolygon multiLineString vector`) {
		laravelTypes[t] = true
	}
}

type phpCall struct {
	name string
	open int
	args []span
}

// phpChain reads "->a(...)->b(...)" starting at pos (up to end).
func phpChain(s *src, pos, end int) []phpCall {
	var out []phpCall
	for pos < end {
		m := phpChainRe.FindStringSubmatchIndex(s.code[pos:end])
		if m == nil {
			break
		}
		open := pos + m[1] - 1
		cl := s.matchClose(open)
		if cl < 0 || cl >= end {
			break
		}
		out = append(out, phpCall{name: s.code[pos+m[2] : pos+m[3]], open: open, args: s.split(open+1, cl, ',')})
		pos = cl + 1
	}
	return out
}

type phpAnalyzer struct {
	s   *src
	d   Dialect
	ops []Op
}

func (a *phpAnalyzer) add(off int, k Kind, table, column, newName, detail string, weak bool) {
	a.ops = append(a.ops, Op{Kind: k, Line: a.s.line(off), Table: table, Column: column, NewName: newName, Detail: detail, Weak: weak})
}

func (a *phpAnalyzer) sql(sp span) {
	text, content, ok := a.s.sqlText(sp)
	if !ok {
		return
	}
	a.ops = append(a.ops, analyzeSQL(text, a.s.line(content), a.d)...)
}

// analyzePHP returns the operations of a Doctrine or Laravel migration.
func analyzePHP(raw string, d Dialect) []Op {
	s := maskPHP(raw)
	a := &phpAnalyzer{s: s, d: d}
	regions, schemaVar := phpRegions(s)

	// Raw SQL.
	for _, m := range phpSQLCallRe.FindAllStringIndex(s.code, -1) {
		if !regionsContain(regions, m[0]) {
			continue
		}
		if args := s.args(m[1] - 1); len(args) > 0 {
			a.sql(args[0])
		}
	}
	closures := a.laravelFacade(regions)
	a.doctrineSchema(regions, closures, schemaVar)
	return a.ops
}

// phpRegions returns the parts of the file to analyse — the bodies of up(),
// or without an up() the whole file except the bodies of down() — and the
// name of up()'s schema parameter (Doctrine; "schema" by default).
func phpRegions(s *src) (regions []span, schemaVar string) {
	ups, params := phpBodies(s, phpUpRe)
	regions = ups
	if len(regions) == 0 {
		downs, _ := phpBodies(s, phpDownRe)
		regions = []span{{0, len(s.code)}}
		for _, dn := range downs {
			regions = cutSpan(regions, dn)
		}
	}
	schemaVar = "schema"
	if len(params) > 0 {
		if m := phpUpParamRe.FindStringSubmatch(s.code[params[0]:]); m != nil {
			schemaVar = m[1]
		}
	}
	return regions, schemaVar
}

// cutSpan removes cut from every span of regions.
func cutSpan(regions []span, cut span) []span {
	var next []span
	for _, r := range regions {
		if cut.start >= r.end || cut.end <= r.start {
			next = append(next, r)
			continue
		}
		if cut.start > r.start {
			next = append(next, span{r.start, cut.start})
		}
		if cut.end < r.end {
			next = append(next, span{cut.end, r.end})
		}
	}
	return next
}

// laravelFacade handles the Schema facade calls inside regions and their
// Blueprint closures, which it returns.
func (a *phpAnalyzer) laravelFacade(regions []span) (closures []span) {
	s := a.s
	for _, m := range phpFacadeRe.FindAllStringSubmatchIndex(s.code, -1) {
		if !regionsContain(regions, m[0]) {
			continue
		}
		method := s.code[m[2]:m[3]]
		args := s.args(m[1] - 1)
		if len(args) == 0 {
			continue
		}
		table, _ := s.str(args[0])
		switch method {
		case "create":
			if table != "" {
				a.add(m[0], createTable, table, "", "", "", false)
			}
			if len(args) > 1 {
				closures = append(closures, args[1])
			}
		case "drop", "dropIfExists":
			if table != "" {
				a.add(m[0], DropTable, table, "", "", "", false)
			}
		case "rename":
			if len(args) > 1 {
				if nn, ok := s.str(args[1]); ok && table != "" {
					a.add(m[0], RenameTable, table, "", nn, "", false)
				}
			}
		case "dropColumns":
			for _, ar := range args[1:] {
				for _, c := range s.strs(ar) {
					a.add(m[0], DropColumn, table, c, "", "", false)
				}
			}
		case "table":
			if len(args) > 1 && table != "" {
				closures = append(closures, args[1])
				a.blueprint(table, args[1])
			}
		}
	}
	return closures
}

// doctrineSchema handles the Doctrine Schema API on schemaVar inside
// regions (outside Laravel closures): table drops and renames, and the
// operations on Table objects it returns.
func (a *phpAnalyzer) doctrineSchema(regions, closures []span, schemaVar string) {
	s := a.s
	vars := map[string]string{} // variable holding a Table -> table name
	for _, m := range phpSchemaRe.FindAllStringSubmatchIndex(s.code, -1) {
		if !regionsContain(regions, m[0]) || regionsContain(closures, m[0]) || s.code[m[2]:m[3]] != schemaVar {
			continue
		}
		method := s.code[m[4]:m[5]]
		open := m[1] - 1
		args := s.args(open)
		if len(args) == 0 {
			continue
		}
		table, _ := s.str(args[0])
		if table == "" {
			continue
		}
		switch method {
		case "dropTable":
			a.add(m[0], DropTable, table, "", "", "", false)
		case "renameTable":
			if len(args) > 1 {
				if nn, ok := s.str(args[1]); ok {
					a.add(m[0], RenameTable, table, "", nn, "", false)
				}
			}
		case "createTable", "getTable":
			if method == "createTable" {
				a.add(m[0], createTable, table, "", "", "", false)
			}
			if am := phpAssignRe.FindStringSubmatch(s.code[max(0, m[0]-80):m[0]]); am != nil {
				vars[am[1]] = table
			}
			if cl := s.matchClose(open); cl > 0 {
				for _, c := range phpChain(s, cl+1, len(s.code)) {
					a.doctrineTableOp(table, c)
				}
			}
		}
	}
	for _, m := range phpTableOpRe.FindAllStringSubmatchIndex(s.code, -1) {
		if !regionsContain(regions, m[0]) || regionsContain(closures, m[0]) {
			continue
		}
		table, ok := vars[s.code[m[2]:m[3]]]
		if !ok {
			continue
		}
		open := m[1] - 1
		cl := s.matchClose(open)
		if cl < 0 {
			continue
		}
		a.doctrineTableOp(table, phpCall{name: s.code[m[4]:m[5]], open: open, args: s.split(open+1, cl, ',')})
	}
}

// doctrineTableOp handles one call on a Doctrine Table object.
func (a *phpAnalyzer) doctrineTableOp(table string, c phpCall) {
	s := a.s
	first := ""
	if len(c.args) > 0 {
		first, _ = s.str(c.args[0])
	}
	opts := ""
	if len(c.args) > 1 {
		last := c.args[len(c.args)-1]
		opts = s.unmask(last.start, last.end)
	}
	switch c.name {
	case "dropColumn":
		if first != "" {
			a.add(c.open, DropColumn, table, first, "", "", false)
		}
	case "renameColumn":
		if len(c.args) > 1 {
			if nn, ok := s.str(c.args[1]); ok && first != "" {
				a.add(c.open, RenameColumn, table, first, nn, "", false)
			}
		}
	case "changeColumn", "modifyColumn":
		if first == "" {
			return
		}
		if phpNotNullTru.MatchString(opts) {
			a.add(c.open, SetNotNull, table, first, "", "", false)
		}
		if phpTypeOptRe.MatchString(opts) {
			a.add(c.open, AlterType, table, first, "", "", false)
		}
	case "addColumn":
		if first == "" {
			return
		}
		a.add(c.open, addColumn, table, first, "", "", false)
		// DBAL columns are NOT NULL unless 'notnull' => false.
		if len(c.args) < 3 || !phpNotNullFal.MatchString(opts) && !phpDefaultRe.MatchString(opts) {
			a.add(c.open, AddColumnNotNull, table, first, "", "", false)
		}
	case "addIndex":
		a.add(c.open, CreateIndex, table, "", "", "", false)
	case "addUniqueIndex":
		a.add(c.open, CreateIndex, table, "", "", "UNIQUE", false)
	case "addUniqueConstraint":
		a.add(c.open, AddUnique, table, "", "", "", false)
	case "addForeignKeyConstraint":
		a.add(c.open, AddFK, table, "", "", "", false)
	}
}

// blueprint handles the closure of Schema::table('t', function (Blueprint $table) {...}).
func (a *phpAnalyzer) blueprint(table string, arg span) {
	s := a.s
	cm := phpClosureRe.FindStringSubmatch(s.text(arg))
	if cm == nil {
		return
	}
	v := cm[1]
	body := arg
	if bo := strings.IndexByte(s.text(arg), '{'); bo >= 0 {
		if bc := s.matchClose(arg.start + bo); bc > 0 {
			body = span{arg.start + bo + 1, bc}
		}
	} else if fa := strings.Index(s.text(arg), "=>"); fa >= 0 {
		body = span{arg.start + fa + 2, arg.end}
	}
	re := regexp.MustCompile(`\$` + regexp.QuoteMeta(v) + `\b`)
	for _, st := range s.split(body.start, body.end, ';') {
		loc := re.FindStringIndex(s.text(st))
		if loc == nil || strings.TrimSpace(s.text(st)[:loc[0]]) != "" {
			continue
		}
		calls := phpChain(s, st.start+loc[1], st.end)
		if len(calls) > 0 {
			a.blueprintStmt(table, calls)
		}
	}
}

func (a *phpAnalyzer) blueprintStmt(table string, calls []phpCall) {
	s := a.s
	c := calls[0]
	off := c.open
	var first string
	if len(c.args) > 0 {
		first, _ = s.str(c.args[0])
	}
	mods := map[string]phpCall{}
	for _, m := range calls[1:] {
		mods[m.name] = m
	}
	switch c.name {
	case "dropColumn":
		for _, ar := range c.args {
			for _, col := range s.strs(ar) {
				a.add(off, DropColumn, table, col, "", "", false)
			}
		}
		return
	case "renameColumn":
		if len(c.args) > 1 {
			if nn, ok := s.str(c.args[1]); ok && first != "" {
				a.add(off, RenameColumn, table, first, nn, "", false)
			}
		}
		return
	case "dropTimestamps", "dropTimestampsTz":
		a.add(off, DropColumn, table, "created_at", "", "", false)
		a.add(off, DropColumn, table, "updated_at", "", "", false)
		return
	case "dropSoftDeletes", "dropSoftDeletesTz":
		a.add(off, DropColumn, table, "deleted_at", "", "", false)
		return
	case "dropRememberToken":
		a.add(off, DropColumn, table, "remember_token", "", "", false)
		return
	case "dropMorphs":
		if first != "" {
			a.add(off, DropColumn, table, first+"_id", "", "", false)
			a.add(off, DropColumn, table, first+"_type", "", "", false)
		}
		return
	case "rename":
		if first != "" {
			a.add(off, RenameTable, table, "", first, "", false)
		}
		return
	case "index", "fullText", "spatialIndex", "rawIndex":
		a.add(off, CreateIndex, table, "", "", "Laravel", false)
		return
	case "unique":
		a.add(off, AddUnique, table, "", "", "", false)
		return
	case "foreign":
		a.add(off, AddFK, table, "", "", "", false)
		return
	case "morphs", "uuidMorphs", "ulidMorphs":
		if first != "" {
			a.add(off, addColumn, table, first+"_id", "", "", false)
			if _, ok := mods["change"]; !ok {
				a.add(off, AddColumnNotNull, table, first+"_id", "", "", false)
			}
		}
		return
	}
	if !laravelTypes[c.name] || first == "" {
		return
	}
	nullable := false
	if m, ok := mods["nullable"]; ok {
		nullable = len(m.args) == 0 || !phpFalseArgRe.MatchString(s.text(m.args[0]))
	}
	if _, ok := mods["change"]; ok {
		if m, ok := mods["nullable"]; ok && len(m.args) > 0 && phpFalseArgRe.MatchString(s.text(m.args[0])) {
			a.add(off, SetNotNull, table, first, "", "", false)
			return
		}
		a.add(off, AlterType, table, first, "", "->change(): возможна смена типа", true)
		return
	}
	a.add(off, addColumn, table, first, "", "", false)
	_, hasDefault := mods["default"]
	_, useCurrent := mods["useCurrent"]
	_, autoInc := mods["autoIncrement"]
	if !nullable && !hasDefault && !useCurrent && !autoInc {
		a.add(off, AddColumnNotNull, table, first, "", "", false)
	}
	if _, ok := mods["constrained"]; ok {
		a.add(off, AddFK, table, first, "", "", false)
	} else if _, ok := mods["references"]; ok {
		a.add(off, AddFK, table, first, "", "", false)
	}
	if _, ok := mods["unique"]; ok {
		a.add(off, AddUnique, table, first, "", "", false)
	}
	if _, ok := mods["index"]; ok {
		a.add(off, CreateIndex, table, first, "", "Laravel", false)
	}
}

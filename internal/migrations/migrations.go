// Package migrations finds database migrations in a merge request and flags
// operations that are unsafe for trunk-based development: code and
// migrations roll out separately, so while (and after) a migration runs the
// previous version of the code — the merge base on the target branch — is
// still serving traffic.
//
// The package is pure: detection works on paths and file contents, the
// extractors (raw SQL, Flyway, goose, golang-migrate, sqitch, dbmate,
// Doctrine, Laravel, Alembic, Django) turn the forward direction of a
// migration into operations, and a table of rules turns operations into
// findings. The usage check (usage.go) searches a repository tree through a
// small read-only interface. Nothing here runs a program or writes a file.
package migrations

import (
	"sort"
	"strings"
)

// Dialect is the SQL dialect whose locking rules apply.
type Dialect string

const (
	Postgres Dialect = "postgres"
	MySQL    Dialect = "mysql"
)

// Framework is the migration tool a file belongs to.
type Framework string

const (
	FwSQL           Framework = "sql"
	FwFlyway        Framework = "flyway"
	FwGoose         Framework = "goose"
	FwGolangMigrate Framework = "golang-migrate"
	FwSqitch        Framework = "sqitch"
	FwDbmate        Framework = "dbmate"
	FwDoctrine      Framework = "doctrine"
	FwLaravel       Framework = "laravel"
	FwAlembic       Framework = "alembic"
	FwDjango        Framework = "django"
)

// Title is the human name of the framework.
func (f Framework) Title() string {
	switch f {
	case FwSQL:
		return "SQL"
	case FwFlyway:
		return "Flyway"
	case FwGoose:
		return "goose"
	case FwGolangMigrate:
		return "golang-migrate"
	case FwSqitch:
		return "Sqitch"
	case FwDbmate:
		return "dbmate"
	case FwDoctrine:
		return "Doctrine"
	case FwLaravel:
		return "Laravel"
	case FwAlembic:
		return "Alembic"
	case FwDjango:
		return "Django"
	}
	return string(f)
}

// Kind is what an operation does.
type Kind string

const (
	DropTable         Kind = "drop_table"
	DropColumn        Kind = "drop_column"
	RenameTable       Kind = "rename_table"
	RenameColumn      Kind = "rename_column"
	AlterType         Kind = "alter_type"
	SetNotNull        Kind = "set_not_null"
	AddColumnNotNull  Kind = "add_column_not_null_no_default"
	CreateIndex       Kind = "create_index_not_concurrently"
	AddFK             Kind = "add_fk_validated"
	AddCheck          Kind = "add_check_validated"
	AddUnique         Kind = "add_unique_constraint"
	UnboundedDML      Kind = "unbounded_dml"
	DataBackfill      Kind = "data_backfill"
	Truncate          Kind = "truncate"
	LockTable         Kind = "lock_table"
	VacuumFull        Kind = "vacuum_full"
	Cluster           Kind = "cluster"
	ModifiedMigration Kind = "modified_migration"
	createTable       Kind = "create_table" // internal: tables created in the MR
	addColumn         Kind = "add_column"   // internal: columns added in the MR
)

// Level is how bad a finding is. Only LevelCritical may block the merge,
// and only for a contract operation whose object the deployed code uses.
type Level string

const (
	LevelCritical Level = "critical"
	LevelMajor    Level = "major"
	LevelMinor    Level = "minor"
	LevelInfo     Level = "info"
)

// Rank orders levels, most severe first.
func (l Level) Rank() int {
	switch l {
	case LevelCritical:
		return 0
	case LevelMajor:
		return 1
	case LevelMinor:
		return 2
	}
	return 3
}

// Rule describes one risky kind of operation.
type Rule struct {
	Kind Kind
	// Level of the finding. Contract rules compute it from the usage check.
	Level Level
	// Contract: dropping or renaming something the old code may use.
	Contract bool
	// Title (Russian) names the operation in the summary.
	Title string
	// Risk (Russian) says what goes wrong.
	Risk string
	// Safer (Russian) is the safe multi-step alternative.
	Safer string
	// SaferMySQL replaces Safer for MySQL ("" = same).
	SaferMySQL string
	// WhyEN is the risk for the prompt.
	WhyEN string
	// SkipMySQL: not a problem with MySQL/InnoDB online DDL.
	SkipMySQL bool
}

// Rules is the rule table, in display order. Extend it to add a check (the
// extractors emit the Kind).
var Rules = []Rule{
	{Kind: DropTable, Contract: true, Title: "удаление таблицы",
		Risk:  "старый код, который ещё работает во время выкатки, обращается к таблице",
		Safer: "expand–contract: сначала выкатить код без обращений к таблице, удалить её отдельным MR после деплоя",
		WhyEN: "drops a table the old code may still read or write during the rollout"},
	{Kind: DropColumn, Contract: true, Title: "удаление колонки",
		Risk:  "старый код (и ORM-маппинг) ещё читает или пишет колонку во время выкатки",
		Safer: "expand–contract: убрать колонку из кода и маппинга модели, задеплоить, удалить колонку отдельным MR",
		WhyEN: "drops a column the old code (or its ORM mapping) may still read or write during the rollout"},
	{Kind: RenameTable, Contract: true, Title: "переименование таблицы",
		Risk:  "старый код обращается к таблице по старому имени",
		Safer: "новая таблица (или view со старым именем) → запись в обе → перенос данных → переключение чтения → удаление старой отдельным MR",
		WhyEN: "renames a table: the old code still uses the old name"},
	{Kind: RenameColumn, Contract: true, Title: "переименование колонки",
		Risk:  "старый код обращается к колонке по старому имени",
		Safer: "добавить новую колонку → писать в обе → бэкфилл батчами → читать новую → удалить старую отдельным MR",
		WhyEN: "renames a column: the old code still uses the old name"},
	{Kind: AlterType, Level: LevelMajor, Title: "смена типа колонки",
		Risk:       "перезапись таблицы под ACCESS EXCLUSIVE-блокировкой; старый код может не понять новый тип",
		Safer:      "новая колонка нужного типа → двойная запись → бэкфилл батчами → переключение; или совместимое расширение без перезаписи (varchar(n) → text)",
		SaferMySQL: "новая колонка → двойная запись → бэкфилл; для больших таблиц — gh-ost / pt-online-schema-change",
		WhyEN:      "changes a column type: table rewrite under an ACCESS EXCLUSIVE lock, and the old code may not handle the new type"},
	{Kind: SetNotNull, Level: LevelMajor, Title: "SET NOT NULL на существующей колонке",
		Risk:       "полный проход по таблице под блокировкой; вставки старого кода с NULL начнут падать",
		Safer:      "ADD CONSTRAINT … CHECK (col IS NOT NULL) NOT VALID → VALIDATE CONSTRAINT → SET NOT NULL (PG12+ без сканирования) → DROP CONSTRAINT; сначала убедиться, что старый код не пишет NULL",
		SaferMySQL: "сначала код, который всегда пишет значение, и бэкфилл; NOT NULL — отдельной миграцией (для больших таблиц — gh-ost / pt-online-schema-change)",
		WhyEN:      "sets NOT NULL on an existing column: full table scan under lock, and inserts of the old code that leave it NULL start failing"},
	{Kind: AddColumnNotNull, Level: LevelMajor, Title: "NOT NULL-колонка без DEFAULT",
		Risk:  "падает на непустой таблице, а вставки старого кода (без новой колонки) ломаются",
		Safer: "добавить колонку nullable или с DEFAULT → код пишет значение → бэкфилл → NOT NULL отдельным шагом",
		WhyEN: "adds a NOT NULL column without a default: fails on a non-empty table, and inserts of the old code (which does not know the column) break"},
	{Kind: CreateIndex, Level: LevelMajor, Title: "индекс без CONCURRENTLY", SkipMySQL: true,
		Risk:  "CREATE INDEX блокирует запись в таблицу на всё время построения",
		Safer: "CREATE INDEX CONCURRENTLY вне транзакции (Alembic: postgresql_concurrently=True в autocommit_block(); Django: AddIndexConcurrently и atomic = False; Doctrine: isTransactional() → false)",
		WhyEN: "creates an index without CONCURRENTLY: writes to the table are blocked while it builds (CONCURRENTLY cannot run inside a transaction)"},
	{Kind: AddFK, Level: LevelMajor, Title: "внешний ключ с проверкой",
		Risk:       "проверка всех строк под блокировкой обеих таблиц",
		Safer:      "ADD CONSTRAINT … FOREIGN KEY … NOT VALID, затем VALIDATE CONSTRAINT отдельной миграцией",
		SaferMySQL: "добавлять FK на большие таблицы через gh-ost / pt-online-schema-change или с foreign_key_checks=0 и ALGORITHM=INPLACE после проверки данных",
		WhyEN:      "adds a validated foreign key: all rows are checked while both tables are locked"},
	{Kind: AddCheck, Level: LevelMajor, Title: "CHECK-ограничение с проверкой",
		Risk:       "проверка всех строк под блокировкой; вставки старого кода могут нарушать условие",
		Safer:      "ADD CONSTRAINT … CHECK (…) NOT VALID, затем VALIDATE CONSTRAINT отдельной миграцией",
		SaferMySQL: "для больших таблиц — gh-ost / pt-online-schema-change; сначала код, который соблюдает условие",
		WhyEN:      "adds a validated CHECK constraint: all rows are checked under lock, and inserts of the old code may violate it"},
	{Kind: AddUnique, Level: LevelMajor, Title: "UNIQUE-ограничение", SkipMySQL: true,
		Risk:  "уникальный индекс строится под блокировкой записи",
		Safer: "CREATE UNIQUE INDEX CONCURRENTLY, затем ADD CONSTRAINT … UNIQUE USING INDEX",
		WhyEN: "adds a UNIQUE constraint without building the unique index concurrently first: writes are blocked while it builds"},
	{Kind: UnboundedDML, Level: LevelMajor, Title: "изменение всех строк таблицы",
		Risk:  "долгая транзакция и блокировки строк в миграции схемы",
		Safer: "батчи по ключу в отдельной data-миграции или джобе, вне миграции схемы",
		WhyEN: "changes every row (UPDATE/DELETE without WHERE, INSERT … SELECT of a whole table) inside a schema migration: long transaction and row locks"},
	{Kind: DataBackfill, Level: LevelMinor, Title: "изменение данных в миграции схемы",
		Risk:  "на большой таблице — долгая транзакция",
		Safer: "для больших таблиц — батчами вне транзакции миграции",
		WhyEN: "changes data inside a schema migration: may run long on a big table"},
	{Kind: Truncate, Level: LevelMajor, Title: "TRUNCATE",
		Risk:  "ACCESS EXCLUSIVE-блокировка и потеря данных, которые ещё использует старый код",
		Safer: "не очищать используемые таблицы в миграции; удалять данные батчами отдельной джобой",
		WhyEN: "truncates a table: ACCESS EXCLUSIVE lock and loss of data the old code still uses"},
	{Kind: LockTable, Level: LevelMajor, Title: "явная блокировка таблицы",
		Risk:  "запросы приложения ждут конца миграции",
		Safer: "обойтись без LOCK TABLE: короткие операции с lock_timeout и повтором",
		WhyEN: "locks a table explicitly: application queries wait until the migration ends"},
	{Kind: VacuumFull, Level: LevelMajor, Title: "VACUUM FULL / перестройка таблицы",
		Risk:  "перезапись таблицы под ACCESS EXCLUSIVE-блокировкой",
		Safer: "pg_repack или обслуживание вне миграций",
		WhyEN: "rewrites the table under an ACCESS EXCLUSIVE lock (VACUUM FULL / OPTIMIZE TABLE)"},
	{Kind: Cluster, Level: LevelMajor, Title: "CLUSTER",
		Risk:  "перезапись таблицы под ACCESS EXCLUSIVE-блокировкой",
		Safer: "pg_repack или обслуживание вне миграций",
		WhyEN: "CLUSTER rewrites the table under an ACCESS EXCLUSIVE lock"},
	{Kind: ModifiedMigration, Level: LevelMajor, Title: "изменена уже существующая миграция",
		Risk:  "применённые миграции не перезапускаются: схема в окружениях разойдётся с кодом",
		Safer: "оставить старую миграцию как есть и добавить новую",
		WhyEN: "edits a migration that already exists on the target branch: applied migrations do not run again, so environments drift"},
}

var ruleByKind = func() map[Kind]*Rule {
	m := map[Kind]*Rule{}
	for i := range Rules {
		m[Rules[i].Kind] = &Rules[i]
	}
	return m
}()

// RuleFor returns the rule of a kind (nil for internal kinds).
func RuleFor(k Kind) *Rule { return ruleByKind[k] }

// Op is one operation of a migration.
type Op struct {
	Kind      Kind
	Path      string
	Line      int
	Framework Framework
	// Table is the table (unqualified; Django: the model name), Column the
	// column (Django: the field) when the operation is about one.
	Table  string
	Column string
	// NewName is the new name of a rename.
	NewName string
	// Weak: the extractor cannot tell whether the operation really does
	// this (Django AlterField, Laravel ->change()): reported as minor.
	Weak bool
	// Detail is a short note (Russian), e.g. "UPDATE без WHERE".
	Detail string
}

// Object is the name a contract operation removes ("orders.created_by").
func (o Op) Object() string {
	switch {
	case o.Column != "" && o.Table != "":
		return o.Table + "." + o.Column
	case o.Column != "":
		return o.Column
	}
	return o.Table
}

// Usage is one place that refers to an object.
type Usage struct {
	Path    string
	Line    int
	Snippet string
}

// UsageCheck is the result of looking for a contract operation's object in
// the code.
type UsageCheck struct {
	// Checked: the search ran (Err == "").
	Checked bool
	Err     string
	// Ambiguous: the name is generic; only files that also mention the
	// table (or its entity) were counted.
	Ambiguous bool
	// Words are the identifiers searched for (name variants).
	Words []string
	// Base: uses in the deployed code (merge base); BaseTotal counts all.
	Base      []Usage
	BaseTotal int
	// Head: uses left in the merge request's own code.
	Head      []Usage
	HeadTotal int
	// Partial: a search limit was hit, counts are a lower bound.
	Partial bool
}

// Finding is a risky operation.
type Finding struct {
	Op
	Level Level
	Usage *UsageCheck // contract operations only
}

// Rule returns the finding's rule.
func (f Finding) Rule() *Rule { return RuleFor(f.Kind) }

// BreaksOldCode: a contract operation whose object the deployed code uses.
func (f Finding) BreaksOldCode() bool {
	r := f.Rule()
	return r != nil && r.Contract && f.Usage != nil && f.Usage.Checked && f.Usage.BaseTotal > 0
}

// File is a migration file of the merge request.
type File struct {
	Path      string
	Framework Framework
	New       bool
	Modified  bool
}

// Result is the analysis of one merge request.
type Result struct {
	Dialect  Dialect
	Files    []File
	Findings []Finding
	// SkippedFiles: migration files beyond max_files, not analysed.
	SkippedFiles int
}

// IsMigration reports whether path is one of the analysed migration files.
func (r *Result) IsMigration(path string) bool {
	if r == nil {
		return false
	}
	for _, f := range r.Files {
		if f.Path == path {
			return true
		}
	}
	return false
}

// setLevels computes the level of every finding and sorts them, most
// severe first, then by file and line.
func (r *Result) setLevels() {
	for i := range r.Findings {
		f := &r.Findings[i]
		f.Level = levelOf(*f)
	}
	sort.SliceStable(r.Findings, func(i, j int) bool {
		a, b := r.Findings[i], r.Findings[j]
		if a.Level.Rank() != b.Level.Rank() {
			return a.Level.Rank() < b.Level.Rank()
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
}

func levelOf(f Finding) Level {
	rule := f.Rule()
	if rule == nil {
		return LevelInfo
	}
	if !rule.Contract {
		if f.Weak && rule.Level.Rank() < LevelMinor.Rank() {
			return LevelMinor
		}
		return rule.Level
	}
	u := f.Usage
	switch {
	case u == nil || !u.Checked:
		return LevelMajor // could not check: a human has to
	case u.BaseTotal > 0 && !u.Ambiguous:
		return LevelCritical
	case u.BaseTotal > 0:
		return LevelMajor // generic name: likely, but needs reading
	case u.HeadTotal > 0:
		return LevelMajor // the merge request's own code still uses it
	}
	return LevelInfo // expand–contract done right
}

// lowerName is how names are compared: unquoted, unqualified, lower case.
func lowerName(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	}
	return strings.ToLower(strings.Trim(s, "\"`[] "))
}

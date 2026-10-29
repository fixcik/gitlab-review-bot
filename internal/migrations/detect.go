package migrations

import (
	"path"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Config is the analysis configuration (review.migrations).
type Config struct {
	// Paths are extra doublestar globs of migration files.
	Paths []string
	// Exclude are globs of files never treated as migrations.
	Exclude []string
	// Dialect: postgres (default) or mysql.
	Dialect Dialect
	// MaxFiles caps the migration files analysed (default 50).
	MaxFiles int
	// MaxUsages caps the usage locations kept per object (default 5).
	MaxUsages int
}

func (c Config) dialect() Dialect {
	if c.Dialect == MySQL {
		return MySQL
	}
	return Postgres
}

func (c Config) maxFiles() int {
	if c.MaxFiles <= 0 {
		return 50
	}
	return c.MaxFiles
}

func (c Config) maxUsages() int {
	if c.MaxUsages <= 0 {
		return 5
	}
	return c.MaxUsages
}

// DefaultPaths are the built-in migration path globs (doublestar). Files
// outside them are still recognised by content (Doctrine, Laravel,
// Alembic, Django, goose, dbmate markers).
var DefaultPaths = []string{
	"**/migrations/**", "**/migration/**", "**/Migrations/**", "**/Migration/**",
	"**/alembic/versions/**", "**/db/migrate/**",
	"**/deploy/*.sql", "**/revert/*.sql", "**/verify/*.sql",
	"**/V[0-9]*__*.sql", "**/R__*.sql", "**/U[0-9]*__*.sql",
	"**/*.up.sql", "**/*.down.sql",
}

var (
	flywayRe       = regexp.MustCompile(`^(?:V[0-9][0-9._]*|R)__.+\.sql$`)
	flywayUndoRe   = regexp.MustCompile(`^U[0-9][0-9._]*__.+\.sql$`)
	gooseUpRe      = regexp.MustCompile(`(?im)^\s*--\s*\+goose\s+up\b`)
	gooseDownRe    = regexp.MustCompile(`(?im)^\s*--\s*\+goose\s+down\b`)
	dbmateUpRe     = regexp.MustCompile(`(?im)^\s*--\s*migrate:up\b`)
	dbmateDownRe   = regexp.MustCompile(`(?im)^\s*--\s*migrate:down\b`)
	doctrineRe     = regexp.MustCompile(`\bAbstractMigration\b|Doctrine\\Migrations`)
	laravelRe      = regexp.MustCompile(`Illuminate\\Database\\(?:Migrations|Schema)|\bSchema\s*::\s*\w+\s*\(`)
	phpUpFuncRe    = regexp.MustCompile(`(?i)\bfunction\s+up\s*\(`)
	alembicRe      = regexp.MustCompile(`(?m)^\s*(?:from\s+alembic\s+import|import\s+alembic)\b`)
	djangoRe       = regexp.MustCompile(`\bclass\s+Migration\s*\(\s*migrations\.Migration\s*\)`)
	migrationBases = []*regexp.Regexp{
		regexp.MustCompile(`^Version\d+\.php$`),                  // Doctrine
		regexp.MustCompile(`^\d{4}_\d{2}_\d{2}_\d{6}_\w+\.php$`), // Laravel
		regexp.MustCompile(`^\d{4}_\w+\.py$`),                    // Django
		regexp.MustCompile(`^[0-9a-f]{12}_\w+\.py$`),             // Alembic
	}
)

func globMatch(globs []string, p string) bool {
	for _, g := range globs {
		if g = strings.TrimSpace(g); g == "" {
			continue
		}
		if ok, err := doublestar.Match(g, p); err == nil && ok {
			return true
		}
	}
	return false
}

// Candidate reports a changed file worth reading to decide whether it is a
// migration: a .sql, .php or .py file that is not excluded.
func (c Config) Candidate(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".sql", ".php", ".py":
	default:
		return false
	}
	return !globMatch(c.Exclude, p)
}

// IsMigrationPath reports a path that looks like a migration by its
// location or name alone (used to keep migrations out of the usage check).
func (c Config) IsMigrationPath(p string) bool {
	if c.inMigrationPaths(p) {
		return true
	}
	base := path.Base(p)
	for _, re := range migrationBases {
		if re.MatchString(base) {
			return true
		}
	}
	return false
}

// inMigrationPaths: p matches a built-in or configured migration glob.
func (c Config) inMigrationPaths(p string) bool {
	return globMatch(DefaultPaths, p) || globMatch(c.Paths, p)
}

// reverse reports a migration file of the backward direction: never
// analysed (golang-migrate *.down.sql, Flyway undo U*.sql, sqitch revert/
// and verify/, Diesel down.sql).
func reverse(p string) bool {
	base := path.Base(p)
	if strings.HasSuffix(base, ".down.sql") || base == "down.sql" || flywayUndoRe.MatchString(base) {
		return true
	}
	if strings.HasSuffix(base, ".sql") {
		dir := "/" + path.Dir(p) + "/"
		return strings.Contains(dir, "/revert/") || strings.Contains(dir, "/verify/")
	}
	return false
}

// Detect decides whether path (with its content at the head) is a forward
// migration file and which framework it belongs to.
func (c Config) Detect(p string, content []byte) (Framework, bool) {
	if !c.Candidate(p) || reverse(p) {
		return "", false
	}
	text := string(content)
	byPath := c.inMigrationPaths(p)
	base := path.Base(p)
	switch strings.ToLower(path.Ext(p)) {
	case ".sql":
		switch {
		case gooseUpRe.MatchString(text):
			return FwGoose, true
		case dbmateUpRe.MatchString(text):
			return FwDbmate, true
		case !byPath:
			return "", false
		case flywayRe.MatchString(base):
			return FwFlyway, true
		case strings.HasSuffix(base, ".up.sql") || base == "up.sql":
			return FwGolangMigrate, true
		case strings.Contains("/"+path.Dir(p)+"/", "/deploy/"):
			return FwSqitch, true
		}
		return FwSQL, true
	case ".php":
		switch {
		case doctrineRe.MatchString(text):
			return FwDoctrine, true
		case laravelRe.MatchString(text) && phpUpFuncRe.MatchString(text):
			return FwLaravel, true
		case byPath && phpUpFuncRe.MatchString(text):
			return FwDoctrine, true
		}
	case ".py":
		switch {
		case djangoRe.MatchString(text):
			return FwDjango, true
		case alembicRe.MatchString(text):
			return FwAlembic, true
		}
	}
	return "", false
}

// forwardSQL cuts the forward part out of a SQL migration: goose's Up
// section, dbmate's migrate:up section, else the whole file. It returns the
// text and the line it starts at.
func forwardSQL(fw Framework, text string) (string, int) {
	var up, down *regexp.Regexp
	switch fw {
	case FwGoose:
		up, down = gooseUpRe, gooseDownRe
	case FwDbmate:
		up, down = dbmateUpRe, dbmateDownRe
	default:
		return text, 1
	}
	u := up.FindStringIndex(text)
	if u == nil {
		return text, 1
	}
	start := u[1]
	end := len(text)
	if d := down.FindStringIndex(text[start:]); d != nil {
		end = start + d[0]
	}
	return text[start:end], 1 + strings.Count(text[:start], "\n")
}

// extract returns the operations of one migration file.
func extract(fw Framework, text string, d Dialect) []Op {
	switch fw {
	case FwDoctrine, FwLaravel:
		return analyzePHP(text, d)
	case FwAlembic:
		return analyzeAlembic(text, d)
	case FwDjango:
		return analyzeDjango(text, d)
	}
	body, line := forwardSQL(fw, text)
	return analyzeSQL(body, line, d)
}

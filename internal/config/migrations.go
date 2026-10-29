package config

import (
	"fmt"
	"strings"
)

// Migrations configures the migration safety check (review.migrations):
// database migrations in the MR are analysed for operations that are
// unsafe while the previous version of the code is still deployed
// (trunk-based development). Detection works without any configuration.
type Migrations struct {
	// Enabled turns the check on (default true).
	Enabled bool `yaml:"enabled"`
	// Paths are extra doublestar globs of migration files, on top of the
	// built-in ones (migrations/, db/migration/, alembic/versions/, Flyway
	// names, *.up.sql, ...) and content detection.
	Paths []string `yaml:"paths"`
	// Exclude are doublestar globs of files never treated as migrations.
	Exclude []string `yaml:"exclude"`
	// Dialect selects the locking rules: postgres (default) or mysql.
	Dialect string `yaml:"dialect"`
	// MaxFiles caps the migration files analysed per MR (default 50).
	MaxFiles int `yaml:"max_files"`
	// MaxUsages caps the usage locations shown per dropped or renamed
	// object (default 5).
	MaxUsages int `yaml:"max_usages"`
}

func defaultMigrations() Migrations {
	return Migrations{Enabled: true, Dialect: "postgres", MaxFiles: 50, MaxUsages: 5}
}

func validDialect(d string) bool {
	switch strings.ToLower(strings.TrimSpace(d)) {
	case "", "postgres", "mysql":
		return true
	}
	return false
}

func (m Migrations) validate() []string {
	var errs []string
	if !validDialect(m.Dialect) {
		errs = append(errs, "review.migrations.dialect must be postgres or mysql")
	}
	if m.MaxFiles < 1 || m.MaxFiles > 500 {
		errs = append(errs, "review.migrations.max_files must be between 1 and 500")
	}
	if m.MaxUsages < 1 || m.MaxUsages > 50 {
		errs = append(errs, "review.migrations.max_usages must be between 1 and 50")
	}
	errs = append(errs, validateGlobs("review.migrations.paths", normalizeGlobs(m.Paths))...)
	errs = append(errs, validateGlobs("review.migrations.exclude", normalizeGlobs(m.Exclude))...)
	return errs
}

// RepoMigrations is the migrations section of .reviewbot.yaml.
type RepoMigrations struct {
	// Enabled=false turns the check off for the repository.
	Enabled *bool `yaml:"enabled"`
	// Paths and Exclude are added to the service globs.
	Paths   []string `yaml:"paths"`
	Exclude []string `yaml:"exclude"`
	// Dialect replaces review.migrations.dialect for the repository.
	Dialect string `yaml:"dialect"`
}

func (r *RepoMigrations) normalize() error {
	if r == nil {
		return nil
	}
	r.Paths, r.Exclude = normalizeGlobs(r.Paths), normalizeGlobs(r.Exclude)
	if errs := append(validateGlobs("migrations.paths", r.Paths), validateGlobs("migrations.exclude", r.Exclude)...); len(errs) > 0 {
		return fmt.Errorf("%s", errs[0])
	}
	r.Dialect = strings.ToLower(strings.TrimSpace(r.Dialect))
	if !validDialect(r.Dialect) {
		return fmt.Errorf("migrations.dialect must be postgres or mysql")
	}
	return nil
}

// mergeMigrations applies the repo's migrations section to the service one.
func mergeMigrations(svc Migrations, repo *Repo) Migrations {
	m := svc
	m.Dialect = strings.ToLower(strings.TrimSpace(m.Dialect))
	m.Paths = normalizeGlobs(svc.Paths)
	m.Exclude = normalizeGlobs(svc.Exclude)
	if repo == nil || repo.Migrations == nil {
		return m
	}
	r := repo.Migrations
	m.Enabled = svc.Enabled && defaultTrue(r.Enabled)
	m.Paths = append(m.Paths, r.Paths...)
	m.Exclude = append(m.Exclude, r.Exclude...)
	if r.Dialect != "" {
		m.Dialect = r.Dialect
	}
	return m
}

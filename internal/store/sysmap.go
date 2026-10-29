package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/sysmap/model"
)

// The system map (internal/sysmap): per-repository scan state, per-file
// extraction results (entries as JSON) and the extraction run log (budget
// and cost). The consumer -> provider matching is rebuilt in memory from
// these rows (sysmap.Build), so nothing derived is stored.
func init() {
	addNamedMigration("sysmap_v1", `
	CREATE TABLE IF NOT EXISTS sysmap_repos (
		repo       TEXT    PRIMARY KEY,
		sha        TEXT    NOT NULL DEFAULT '',
		branch     TEXT    NOT NULL DEFAULT '',
		scanned_at INTEGER NOT NULL DEFAULT 0,
		status     TEXT    NOT NULL DEFAULT '',
		error      TEXT    NOT NULL DEFAULT '',
		candidates INTEGER NOT NULL DEFAULT 0,
		specs      INTEGER NOT NULL DEFAULT 0,
		pending    INTEGER NOT NULL DEFAULT 0,
		capped     INTEGER NOT NULL DEFAULT 0,
		too_large  INTEGER NOT NULL DEFAULT 0,
		version    TEXT    NOT NULL DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS sysmap_files (
		repo         TEXT    NOT NULL,
		path         TEXT    NOT NULL,
		hash         TEXT    NOT NULL,
		version      TEXT    NOT NULL,
		entries      TEXT    NOT NULL DEFAULT '[]',
		error        TEXT    NOT NULL DEFAULT '',
		extracted_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (repo, path)
	);
	CREATE TABLE IF NOT EXISTS sysmap_runs (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		at          INTEGER NOT NULL,
		repo        TEXT    NOT NULL DEFAULT '',
		files       INTEGER NOT NULL DEFAULT 0,
		ok          INTEGER NOT NULL DEFAULT 0,
		error       TEXT    NOT NULL DEFAULT '',
		cost_usd    REAL    NOT NULL DEFAULT 0,
		tokens      INTEGER NOT NULL DEFAULT 0,
		duration_ms INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS sysmap_runs_at ON sysmap_runs(at);`)
	// Extraction results of merge-request files by content hash (phase B:
	// the consumer side of a review). Shared across repositories and
	// branches; pruned after sysmapCacheKeep.
	addNamedMigration("sysmap_cache_v1", `
	CREATE TABLE IF NOT EXISTS sysmap_cache (
		hash         TEXT    NOT NULL,
		version      TEXT    NOT NULL,
		entries      TEXT    NOT NULL DEFAULT '[]',
		error        TEXT    NOT NULL DEFAULT '',
		extracted_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (hash, version)
	);
	CREATE INDEX IF NOT EXISTS sysmap_cache_at ON sysmap_cache(extracted_at);
	CREATE INDEX IF NOT EXISTS sysmap_files_hash ON sysmap_files(hash);`)
	// Run kind: the map of main branches ('') and phase B ('deps') have
	// separate hourly budgets.
	addNamedMigration("sysmap_runs_kind_v1", `
	ALTER TABLE sysmap_runs ADD COLUMN kind TEXT NOT NULL DEFAULT '';`)
	addNamedMigration("sysmap_runs_kind_idx_v1", `
	CREATE INDEX IF NOT EXISTS sysmap_runs_kind_at ON sysmap_runs(kind, at);`)
}

var _ model.Store = (*SQLite)(nil)

// sysmapRunsKeep: run records older than this are pruned on insert.
const sysmapRunsKeep = 400 * 24 * time.Hour

// sysmapCacheKeep: merge-request extraction results older than this are
// pruned on insert.
const sysmapCacheKeep = 30 * 24 * time.Hour

// ListSysmapRepos returns every repository's scan state, by path.
func (s *SQLite) ListSysmapRepos(ctx context.Context) ([]model.RepoState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repo, sha, branch, scanned_at, status, error, candidates, specs, pending, capped, too_large, version
		FROM sysmap_repos ORDER BY repo`)
	if err != nil {
		return nil, fmt.Errorf("store: list sysmap repos: %w", err)
	}
	defer rows.Close()
	var out []model.RepoState
	for rows.Next() {
		var r model.RepoState
		var at int64
		if err := rows.Scan(&r.Path, &r.SHA, &r.Branch, &at, &r.Status, &r.Error, &r.Candidates, &r.Specs, &r.Pending, &r.Capped, &r.TooLarge, &r.Version); err != nil {
			return nil, fmt.Errorf("store: scan sysmap repo: %w", err)
		}
		r.ScannedAt = fromNanos(at)
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveSysmapRepo inserts or replaces a repository's scan state.
func (s *SQLite) SaveSysmapRepo(ctx context.Context, r model.RepoState) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sysmap_repos(repo, sha, branch, scanned_at, status, error, candidates, specs, pending, capped, too_large, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo) DO UPDATE SET sha = excluded.sha, branch = excluded.branch, scanned_at = excluded.scanned_at,
			status = excluded.status, error = excluded.error, candidates = excluded.candidates, specs = excluded.specs,
			pending = excluded.pending, capped = excluded.capped, too_large = excluded.too_large, version = excluded.version`,
		r.Path, r.SHA, r.Branch, toNanos(r.ScannedAt), r.Status, r.Error, r.Candidates, r.Specs, r.Pending, r.Capped, r.TooLarge, r.Version)
	if err != nil {
		return fmt.Errorf("store: save sysmap repo: %w", err)
	}
	return nil
}

// DeleteSysmapRepo removes a repository and its files.
func (s *SQLite) DeleteSysmapRepo(ctx context.Context, repo string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: delete sysmap repo: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `DELETE FROM sysmap_files WHERE repo = ?`, repo); err != nil {
		return fmt.Errorf("store: delete sysmap files: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sysmap_repos WHERE repo = ?`, repo); err != nil {
		return fmt.Errorf("store: delete sysmap repo: %w", err)
	}
	return tx.Commit()
}

const sysmapFileCols = `repo, path, hash, version, entries, error, extracted_at`

func scanSysmapFiles(rows *sql.Rows) (map[string][]model.FileRecord, error) {
	defer rows.Close()
	out := map[string][]model.FileRecord{}
	for rows.Next() {
		var repo, entries string
		var f model.FileRecord
		var at int64
		if err := rows.Scan(&repo, &f.Path, &f.Hash, &f.Version, &entries, &f.Error, &at); err != nil {
			return nil, fmt.Errorf("store: scan sysmap file: %w", err)
		}
		f.ExtractedAt = fromNanos(at)
		if err := json.Unmarshal([]byte(entries), &f.Entries); err != nil {
			// A broken row is re-extracted: an empty hash never matches.
			f.Hash, f.Entries = "", nil
		}
		out[repo] = append(out[repo], f)
	}
	return out, rows.Err()
}

// ListSysmapFiles returns the files of one repository, by path.
func (s *SQLite) ListSysmapFiles(ctx context.Context, repo string) ([]model.FileRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sysmapFileCols+` FROM sysmap_files WHERE repo = ? ORDER BY path`, repo)
	if err != nil {
		return nil, fmt.Errorf("store: list sysmap files: %w", err)
	}
	m, err := scanSysmapFiles(rows)
	if err != nil {
		return nil, err
	}
	return m[repo], nil
}

// ListAllSysmapFiles returns the files of every repository.
func (s *SQLite) ListAllSysmapFiles(ctx context.Context) (map[string][]model.FileRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sysmapFileCols+` FROM sysmap_files ORDER BY repo, path`)
	if err != nil {
		return nil, fmt.Errorf("store: list sysmap files: %w", err)
	}
	return scanSysmapFiles(rows)
}

// entriesJSON encodes extracted entries (nil as an empty list).
func entriesJSON(entries []model.Entry) ([]byte, error) {
	if entries == nil {
		entries = []model.Entry{}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("store: encode sysmap entries: %w", err)
	}
	return b, nil
}

// SaveSysmapFiles inserts or replaces file records of a repository.
func (s *SQLite) SaveSysmapFiles(ctx context.Context, repo string, files []model.FileRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: save sysmap files: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	for _, f := range files {
		b, err := entriesJSON(f.Entries)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sysmap_files(`+sysmapFileCols+`) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repo, path) DO UPDATE SET hash = excluded.hash, version = excluded.version, entries = excluded.entries,
				error = excluded.error, extracted_at = excluded.extracted_at`,
			repo, f.Path, f.Hash, f.Version, string(b), f.Error, toNanos(f.ExtractedAt)); err != nil {
			return fmt.Errorf("store: save sysmap file: %w", err)
		}
	}
	return tx.Commit()
}

// DeleteSysmapFiles removes file records of a repository.
func (s *SQLite) DeleteSysmapFiles(ctx context.Context, repo string, paths []string) error {
	for len(paths) > 0 {
		n := min(len(paths), 400)
		args := []any{repo}
		for _, p := range paths[:n] {
			args = append(args, p)
		}
		q := `DELETE FROM sysmap_files WHERE repo = ? AND path IN ` + inPlaceholders(n)
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("store: delete sysmap files: %w", err)
		}
		paths = paths[n:]
	}
	return nil
}

// RecordSysmapRun stores one extraction run (and prunes very old ones).
func (s *SQLite) RecordSysmapRun(ctx context.Context, r model.RunRecord) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sysmap_runs(at, repo, files, ok, error, cost_usd, tokens, duration_ms, kind) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		toNanos(r.At), r.Repo, r.Files, boolInt(r.OK), r.Error, r.CostUSD, r.Tokens, r.Duration.Milliseconds(), r.Kind); err != nil {
		return fmt.Errorf("store: record sysmap run: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sysmap_runs WHERE at < ?`, toNanos(r.At.Add(-sysmapRunsKeep))); err != nil {
		return fmt.Errorf("store: prune sysmap runs: %w", err)
	}
	return nil
}

// SysmapRunStats aggregates the extraction runs since t (zero = all).
func (s *SQLite) SysmapRunStats(ctx context.Context, since time.Time) (model.RunStats, error) {
	var st model.RunStats
	var failed sql.NullInt64
	var cost sql.NullFloat64
	var tokens sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(1 - ok), SUM(cost_usd), SUM(tokens) FROM sysmap_runs WHERE at >= ?`, toNanos(since)).
		Scan(&st.Runs, &failed, &cost, &tokens)
	if err != nil {
		return st, fmt.Errorf("store: sysmap run stats: %w", err)
	}
	st.Failed, st.CostUSD, st.Tokens = int(failed.Int64), cost.Float64, tokens.Int64
	return st, nil
}

// SysmapRunCount counts the extraction runs of one kind since t.
func (s *SQLite) SysmapRunCount(ctx context.Context, since time.Time, kind string) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sysmap_runs WHERE kind = ? AND at >= ?`, kind, toNanos(since)).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: sysmap run count: %w", err)
	}
	return n, nil
}

// SysmapEntriesByHash returns stored extraction results by content hash
// for one extractor version: default-branch files of any repository and
// the merge-request cache. Path is "" (the caller knows its file); a row
// with an unusable answer comes back with Error set.
func (s *SQLite) SysmapEntriesByHash(ctx context.Context, version string, hashes []string) (map[string]model.FileRecord, error) {
	out := map[string]model.FileRecord{}
	for len(hashes) > 0 {
		n := min(len(hashes), 400)
		args := []any{version}
		for _, h := range hashes[:n] {
			args = append(args, h)
		}
		in := inPlaceholders(n)
		q := `SELECT hash, entries, error, extracted_at FROM sysmap_files WHERE version = ? AND hash IN ` + in + `
			UNION ALL SELECT hash, entries, error, extracted_at FROM sysmap_cache WHERE version = ? AND hash IN ` + in
		rows, err := s.db.QueryContext(ctx, q, append(args, args...)...)
		if err != nil {
			return nil, fmt.Errorf("store: sysmap entries by hash: %w", err)
		}
		for rows.Next() {
			var f model.FileRecord
			var entries string
			var at int64
			if err := rows.Scan(&f.Hash, &entries, &f.Error, &at); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: scan sysmap entries by hash: %w", err)
			}
			if json.Unmarshal([]byte(entries), &f.Entries) != nil {
				continue
			}
			f.Version, f.ExtractedAt = version, fromNanos(at)
			// Prefer a usable answer over an unusable one.
			if old, ok := out[f.Hash]; ok && old.Error == "" {
				continue
			}
			out[f.Hash] = f
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("store: sysmap entries by hash: %w", err)
		}
		hashes = hashes[n:]
	}
	return out, nil
}

// SaveSysmapCache stores merge-request extraction results by content hash
// (and prunes old ones).
func (s *SQLite) SaveSysmapCache(ctx context.Context, recs []model.FileRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: save sysmap cache: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var newest time.Time
	for _, f := range recs {
		b, err := entriesJSON(f.Entries)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sysmap_cache(hash, version, entries, error, extracted_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(hash, version) DO UPDATE SET entries = excluded.entries, error = excluded.error, extracted_at = excluded.extracted_at`,
			f.Hash, f.Version, string(b), f.Error, toNanos(f.ExtractedAt)); err != nil {
			return fmt.Errorf("store: save sysmap cache: %w", err)
		}
		if f.ExtractedAt.After(newest) {
			newest = f.ExtractedAt
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sysmap_cache WHERE extracted_at < ?`, toNanos(newest.Add(-sysmapCacheKeep))); err != nil {
		return fmt.Errorf("store: prune sysmap cache: %w", err)
	}
	return tx.Commit()
}

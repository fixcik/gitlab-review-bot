package store

// Claude run accounting: one row per core.Runner.Run in claude_runs, plus
// its per-model split in claude_run_models (a run with no breakdown gets one
// model row carrying the run totals), so totals by user/project/kind/day
// count runs once and the by-model view still sums exactly.
//
// The schema is created idempotently (CREATE ... IF NOT EXISTS) on first use
// instead of through the numbered `migrations` list. That keeps this feature
// out of the shared schema_version sequence: other branches can append
// migrations without renumbering, and an older binary opening the database
// is not refused because of these tables.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
)

const runsSchema = `
CREATE TABLE IF NOT EXISTS claude_runs (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	at                 INTEGER NOT NULL,
	kind               TEXT    NOT NULL DEFAULT '',
	project_id         INTEGER NOT NULL DEFAULT 0,
	project_path       TEXT    NOT NULL DEFAULT '',
	mr_iid             INTEGER NOT NULL DEFAULT 0,
	mr_title           TEXT    NOT NULL DEFAULT '',
	username           TEXT    NOT NULL DEFAULT '',
	user_id            INTEGER NOT NULL DEFAULT 0,
	model              TEXT    NOT NULL DEFAULT '',
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd           REAL    NOT NULL DEFAULT 0,
	duration_ms        INTEGER NOT NULL DEFAULT 0,
	num_turns          INTEGER NOT NULL DEFAULT 0,
	ok                 INTEGER NOT NULL DEFAULT 1,
	error_class        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS claude_runs_at ON claude_runs(at);
CREATE INDEX IF NOT EXISTS claude_runs_user_at ON claude_runs(username, at);
CREATE INDEX IF NOT EXISTS claude_runs_project_at ON claude_runs(project_path, at);
CREATE INDEX IF NOT EXISTS claude_runs_mr ON claude_runs(project_path, mr_iid);
CREATE TABLE IF NOT EXISTS claude_run_models (
	run_id             INTEGER NOT NULL REFERENCES claude_runs(id) ON DELETE CASCADE,
	model              TEXT    NOT NULL DEFAULT '',
	input_tokens       INTEGER NOT NULL DEFAULT 0,
	output_tokens      INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd           REAL    NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS claude_run_models_run ON claude_run_models(run_id);
`

// runsReady remembers, per open database, that runsSchema was applied.
var runsReady sync.Map // *sql.DB -> *runsInit

type runsInit struct {
	once sync.Once
	err  error
}

func (s *SQLite) ensureRuns(ctx context.Context) error {
	v, _ := runsReady.LoadOrStore(s.db, &runsInit{})
	ri := v.(*runsInit)
	ri.once.Do(func() {
		if _, err := s.db.ExecContext(ctx, runsSchema); err != nil {
			ri.err = fmt.Errorf("store: create claude_runs: %w", err)
		}
	})
	if ri.err != nil {
		// Allow a retry on the next call (e.g. a cancelled context).
		runsReady.Delete(s.db)
	}
	return ri.err
}

// RecordRun stores one run. A zero At is replaced with now.
func (s *SQLite) RecordRun(ctx context.Context, r meter.ClaudeRun) error {
	if err := s.ensureRuns(ctx); err != nil {
		return err
	}
	if r.At.IsZero() {
		r.At = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: record run: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `INSERT INTO claude_runs(at, kind, project_id, project_path, mr_iid, mr_title,
		username, user_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
		cost_usd, duration_ms, num_turns, ok, error_class)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		toNanos(r.At), r.Kind, r.ProjectID, r.ProjectPath, r.MRIID, r.MRTitle,
		r.Username, r.UserID, r.Model, r.Usage.Input, r.Usage.Output, r.Usage.CacheRead, r.Usage.CacheWrite,
		r.CostUSD, r.Duration.Milliseconds(), r.NumTurns, boolInt(r.OK), r.ErrorClass)
	if err != nil {
		return fmt.Errorf("store: record run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("store: record run: %w", err)
	}
	models := r.Models
	if len(models) == 0 {
		models = map[string]core.ModelUsage{r.Model: {TokenUsage: r.Usage, CostUSD: r.CostUSD}}
	}
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m := models[n]
		if _, err := tx.ExecContext(ctx, `INSERT INTO claude_run_models(run_id, model, input_tokens, output_tokens,
			cache_read_tokens, cache_write_tokens, cost_usd) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id, n, m.Input, m.Output, m.CacheRead, m.CacheWrite, m.CostUSD); err != nil {
			return fmt.Errorf("store: record run model: %w", err)
		}
	}
	return tx.Commit()
}

// PruneRuns deletes runs strictly older than before and returns how many.
func (s *SQLite) PruneRuns(ctx context.Context, before time.Time) (int64, error) {
	if err := s.ensureRuns(ctx); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: prune runs: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	b := toNanos(before)
	if _, err := tx.ExecContext(ctx, `DELETE FROM claude_run_models WHERE run_id IN (SELECT id FROM claude_runs WHERE at < ?)`, b); err != nil {
		return 0, fmt.Errorf("store: prune runs: %w", err)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM claude_runs WHERE at < ?`, b)
	if err != nil {
		return 0, fmt.Errorf("store: prune runs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// runWhere is the WHERE clause of f.
func runWhere(f meter.RunFilter) (string, []any) {
	var w whereClause
	if !f.Since.IsZero() {
		w.add("r.at >= ?", toNanos(f.Since))
	}
	if !f.Until.IsZero() {
		w.add("r.at < ?", toNanos(f.Until))
	}
	if f.ProjectPath != "" {
		w.add("r.project_path = ?", f.ProjectPath)
	}
	if f.Username != nil {
		w.add("r.username = ?", *f.Username)
	}
	if f.Kind != "" {
		w.add("r.kind = ?", f.Kind)
	}
	if f.MRIID > 0 {
		w.add("r.project_path = ? AND r.mr_iid = ?", f.MRProjectPath, f.MRIID)
	}
	if f.HasModel {
		w.add("EXISTS (SELECT 1 FROM claude_run_models fm WHERE fm.run_id = r.id AND fm.model = ?)", f.Model)
	}
	return w.sql(), w.args
}

const statCols = `COUNT(DISTINCT r.id),
	COUNT(DISTINCT CASE WHEN r.ok = 0 THEN r.id END),
	COALESCE(SUM(%[1]s.input_tokens), 0), COALESCE(SUM(%[1]s.output_tokens), 0),
	COALESCE(SUM(%[1]s.cache_read_tokens), 0), COALESCE(SUM(%[1]s.cache_write_tokens), 0),
	COALESCE(SUM(%[1]s.cost_usd), 0), %[2]s`

// RunTotals sums every run matching f.
func (s *SQLite) RunTotals(ctx context.Context, f meter.RunFilter) (meter.RunStats, error) {
	var st meter.RunStats
	if err := s.ensureRuns(ctx); err != nil {
		return st, err
	}
	where, args := runWhere(f)
	q := `SELECT ` + fmt.Sprintf(statCols, "r", "COALESCE(SUM(r.duration_ms), 0)") + ` FROM claude_runs r` + where
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&st.Runs, &st.Failed, &st.Usage.Input, &st.Usage.Output,
		&st.Usage.CacheRead, &st.Usage.CacheWrite, &st.CostUSD, &st.DurationMS)
	if err != nil {
		return st, fmt.Errorf("store: run totals: %w", err)
	}
	return st, nil
}

// RunsBy aggregates runs matching f by dim. Days come in date order, the
// other dimensions by total tokens, largest first.
func (s *SQLite) RunsBy(ctx context.Context, f meter.RunFilter, dim meter.Dimension) ([]meter.RunGroup, error) {
	if err := s.ensureRuns(ctx); err != nil {
		return nil, err
	}
	where, args := runWhere(f)
	tbl, from, key, id := "r", " FROM claude_runs r", "", "0"
	dur := "COALESCE(SUM(r.duration_ms), 0)"
	switch dim {
	case meter.ByUser:
		key, id = "r.username", "MAX(r.user_id)"
	case meter.ByProject:
		key, id = "r.project_path", "MAX(r.project_id)"
	case meter.ByKind:
		key = "r.kind"
	case meter.ByDay:
		loc := f.Location
		if loc == nil {
			loc = time.UTC
		}
		ref := f.Until
		if ref.IsZero() {
			ref = time.Now()
		}
		_, off := ref.In(loc).Zone()
		key = fmt.Sprintf("date(r.at / 1000000000, 'unixepoch', '%+d seconds')", off)
	case meter.ByModel:
		// Duration belongs to the run, not to a model: not split.
		tbl, from, key, dur = "m", " FROM claude_run_models m JOIN claude_runs r ON r.id = m.run_id", "m.model", "0"
	default:
		return nil, fmt.Errorf("store: unknown dimension %q", dim)
	}
	order := "(SUM(" + tbl + ".input_tokens) + SUM(" + tbl + ".output_tokens) + SUM(" + tbl + ".cache_read_tokens) + SUM(" + tbl + ".cache_write_tokens)) DESC, k ASC"
	if dim == meter.ByDay {
		order = "k ASC"
	}
	q := `SELECT ` + key + ` AS k, ` + id + `, ` + fmt.Sprintf(statCols, tbl, dur) + from + where +
		` GROUP BY k ORDER BY ` + order
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: runs by %s: %w", dim, err)
	}
	defer rows.Close()
	var out []meter.RunGroup
	for rows.Next() {
		var g meter.RunGroup
		var k sql.NullString
		if err := rows.Scan(&k, &g.ID, &g.Runs, &g.Failed, &g.Usage.Input, &g.Usage.Output,
			&g.Usage.CacheRead, &g.Usage.CacheWrite, &g.CostUSD, &g.DurationMS); err != nil {
			return nil, fmt.Errorf("store: runs by %s: %w", dim, err)
		}
		g.Key = k.String
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: runs by %s: %w", dim, err)
	}
	return out, nil
}

// TopMRs returns the n MRs with the most tokens among runs matching f.
func (s *SQLite) TopMRs(ctx context.Context, f meter.RunFilter, n int) ([]meter.MRStats, error) {
	if err := s.ensureRuns(ctx); err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, errors.New("store: top mrs: n must be > 0")
	}
	where, args := runWhere(f)
	if where == "" {
		where = " WHERE r.mr_iid > 0"
	} else {
		where += " AND r.mr_iid > 0"
	}
	q := `SELECT r.project_path, r.mr_iid, MAX(r.project_id),
		COALESCE((SELECT x.mr_title FROM claude_runs x WHERE x.project_path = r.project_path AND x.mr_iid = r.mr_iid
			AND x.mr_title <> '' ORDER BY x.id DESC LIMIT 1), ''), ` +
		fmt.Sprintf(statCols, "r", "COALESCE(SUM(r.duration_ms), 0)") + ` FROM claude_runs r` + where +
		` GROUP BY r.project_path, r.mr_iid
		ORDER BY (SUM(r.input_tokens) + SUM(r.output_tokens) + SUM(r.cache_read_tokens) + SUM(r.cache_write_tokens)) DESC,
			SUM(r.cost_usd) DESC, r.project_path, r.mr_iid LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, n)...)
	if err != nil {
		return nil, fmt.Errorf("store: top mrs: %w", err)
	}
	defer rows.Close()
	var out []meter.MRStats
	for rows.Next() {
		var m meter.MRStats
		if err := rows.Scan(&m.ProjectPath, &m.MRIID, &m.ProjectID, &m.Title, &m.Runs, &m.Failed,
			&m.Usage.Input, &m.Usage.Output, &m.Usage.CacheRead, &m.Usage.CacheWrite, &m.CostUSD, &m.DurationMS); err != nil {
			return nil, fmt.Errorf("store: top mrs: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: top mrs: %w", err)
	}
	return out, nil
}

// RunsHourly splits the runs of [since, until) into hours counted from
// since: element i sums the runs of [since+i h, since+(i+1) h). Every hour of
// the window has an element (zero when nothing ran); a partial last hour
// counts as one. The elements sum to RunTotals of the same window. An empty
// window (until <= since) gives nil.
func (s *SQLite) RunsHourly(ctx context.Context, since, until time.Time) ([]meter.RunStats, error) {
	if !until.After(since) {
		return nil, nil
	}
	if err := s.ensureRuns(ctx); err != nil {
		return nil, err
	}
	n := int((until.Sub(since) + time.Hour - 1) / time.Hour)
	out := make([]meter.RunStats, n)
	from := toNanos(since)
	q := `SELECT (r.at - ?) / ` + fmt.Sprint(int64(time.Hour)) + ` AS k, ` + fmt.Sprintf(statCols, "r", "COALESCE(SUM(r.duration_ms), 0)") +
		` FROM claude_runs r WHERE r.at >= ? AND r.at < ? GROUP BY k`
	rows, err := s.db.QueryContext(ctx, q, from, from, toNanos(until))
	if err != nil {
		return nil, fmt.Errorf("store: runs hourly: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k int64
		var st meter.RunStats
		if err := rows.Scan(&k, &st.Runs, &st.Failed, &st.Usage.Input, &st.Usage.Output, &st.Usage.CacheRead,
			&st.Usage.CacheWrite, &st.CostUSD, &st.DurationMS); err != nil {
			return nil, fmt.Errorf("store: runs hourly: %w", err)
		}
		if k >= 0 && k < int64(n) {
			out[k] = st
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: runs hourly: %w", err)
	}
	return out, nil
}

// modelSep separates the model names of a run in RunRecords' GROUP_CONCAT
// (the unit separator cannot be in a model name the bot passes to the CLI).
const modelSep = "\x1f"

// RunRecords returns the runs of [f.Since, f.Until) matching f, newest
// first, at most limit of them (limit > 0); truncated reports that more
// runs matched. Only metadata, for the usage page's per-run rules.
func (s *SQLite) RunRecords(ctx context.Context, f meter.RunFilter, limit int) (recs []meter.RunRecord, truncated bool, err error) {
	if err := s.ensureRuns(ctx); err != nil {
		return nil, false, err
	}
	if limit <= 0 {
		return nil, false, errors.New("store: run records: limit must be > 0")
	}
	where, args := runWhere(f)
	q := `SELECT r.at, r.kind, r.project_path, r.mr_iid, r.mr_title, r.username, r.model,
		r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens, r.cost_usd, r.duration_ms,
		r.num_turns, r.ok, r.error_class,
		COALESCE((SELECT GROUP_CONCAT(m.model, char(31)) FROM claude_run_models m WHERE m.run_id = r.id), '')
		FROM claude_runs r` + where + ` ORDER BY r.at DESC, r.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit+1)...)
	if err != nil {
		return nil, false, fmt.Errorf("store: run records: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r meter.RunRecord
		var at int64
		var ok int
		var models string
		if err := rows.Scan(&at, &r.Kind, &r.ProjectPath, &r.MRIID, &r.MRTitle, &r.Username, &r.Model,
			&r.Usage.Input, &r.Usage.Output, &r.Usage.CacheRead, &r.Usage.CacheWrite, &r.CostUSD, &r.DurationMS,
			&r.NumTurns, &ok, &r.ErrorClass, &models); err != nil {
			return nil, false, fmt.Errorf("store: run records: %w", err)
		}
		if len(recs) == limit {
			truncated = true
			break
		}
		r.At, r.OK = fromNanos(at), ok != 0
		r.Models = strings.Split(models, modelSep)
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: run records: %w", err)
	}
	return recs, truncated, nil
}

// FirstRunAt is the time of the oldest stored run (zero when there is none).
func (s *SQLite) FirstRunAt(ctx context.Context) (time.Time, error) {
	if err := s.ensureRuns(ctx); err != nil {
		return time.Time{}, err
	}
	var at sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(at) FROM claude_runs`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("store: first run: %w", err)
	}
	if !at.Valid {
		return time.Time{}, nil
	}
	return fromNanos(at.Int64), nil
}

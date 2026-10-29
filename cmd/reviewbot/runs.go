package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// runRuns implements `reviewbot runs` (the index of recent runs) and
// `reviewbot runs show <id>` (one run's timeline). It reads the same
// SQLite database and diag directory as the running service.
func runRuns(ctx context.Context, args []string, out io.Writer) error {
	show := len(args) > 0 && args[0] == "show"
	if show {
		args = args[1:]
	}
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	fs.SetOutput(out)
	cfgPath := fs.String("config", "", "path to config.yaml (data_dir, diag)")
	dataDirFlag := fs.String("data-dir", "", "data directory (overrides config)")
	since := fs.Duration("since", 0, "window, e.g. 6h (default diag.retention)")
	status := fs.String("status", "", "ok | skipped | postponed | failed | running | interrupted")
	kind := fs.String("kind", "", "job kind: "+jobKindList())
	project := fs.String("project", "", "project path (substring)")
	mr := fs.Int64("mr", 0, "MR IID")
	asJSON := fs.Bool("json", false, "print JSON")
	limit := fs.Int("limit", 200, "max rows")
	// Flags may follow the id: `runs show <id> -json`.
	var id string
	if show && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if show && id == "" {
		id = fs.Arg(0)
	}
	dataDir, dcfg, err := config.LoadDiag(*cfgPath)
	if err != nil {
		return err
	}
	if *dataDirFlag != "" {
		dataDir = *dataDirFlag
	}
	dbPath := filepath.Join(dataDir, "reviewbot.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("no database at %s (set -config or -data-dir): %w", dbPath, err)
	}
	st, err := store.OpenSQLite(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	rd := &diag.Reader{Root: filepath.Join(dataDir, "diag"), Index: st}
	if show {
		if id == "" {
			return errors.New("usage: reviewbot runs show <id> [-json]")
		}
		return printRun(ctx, out, rd, id, *asJSON)
	}
	win := *since
	if win <= 0 {
		win = dcfg.RetentionD()
	}
	rows, err := rd.List(ctx, diag.IndexFilter{Since: time.Now().Add(-win), Status: *status, Kind: *kind, ProjectPath: *project, MRIID: *mr, Limit: *limit})
	if err != nil {
		return err
	}
	return printRuns(out, rows, *asJSON)
}

func printRuns(out io.Writer, rows []diag.IndexRow, asJSON bool) error {
	if asJSON {
		if rows == nil {
			rows = []diag.IndexRow{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "no runs")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTARTED\tKIND\tMR\tUSER\tSTATUS\tDURATION\tCLAUDE\tTOKENS\tREASON/ERROR")
	for _, r := range rows {
		mr := r.ProjectPath
		if r.MRIID > 0 {
			mr += fmt.Sprintf("!%d", r.MRIID)
		}
		why := r.Error
		if why == "" {
			why = r.Reason
		}
		if r.Status == diag.StatusPostponed && !r.RetryAt.IsZero() {
			why = "until " + r.RetryAt.Local().Format("15:04") + ": " + why
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", r.ID, r.StartedAt.Local().Format("01-02 15:04:05"), r.Kind, dash(mr),
			dash(r.Username), r.Status, fmtDur(time.Duration(r.DurationMS)*time.Millisecond), r.ClaudeRuns, r.Tokens, oneLine(why, 100))
	}
	return tw.Flush()
}

func printRun(ctx context.Context, out io.Writer, rd *diag.Reader, id string, asJSON bool) error {
	row, rec, err := rd.Get(ctx, id)
	if err != nil {
		return err
	}
	if row == nil || rec == nil {
		return fmt.Errorf("run %q not found", id)
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rec)
	}
	j := rec.Job
	fmt.Fprintf(out, "Run %s  %s  %s\n", rec.ID, j.Kind, row.Status)
	if j.MRIID > 0 {
		fmt.Fprintf(out, "MR:       %s!%d %s\n", j.ProjectPath, j.MRIID, j.MRTitle)
	}
	if j.MRURL != "" {
		fmt.Fprintf(out, "URL:      %s\n", j.MRURL)
	}
	if j.Username != "" {
		fmt.Fprintf(out, "User:     @%s\n", j.Username)
	}
	fmt.Fprintf(out, "Started:  %s  duration %s  attempt %d\n", rec.StartedAt.Local().Format(time.DateTime),
		fmtDur(time.Duration(rec.DurationMS)*time.Millisecond), rec.Attempt)
	if rec.Reason != "" || row.Reason != "" {
		fmt.Fprintf(out, "Reason:   %s\n", firstNonEmpty(rec.Reason, row.Reason))
	}
	if !rec.RetryAt.IsZero() {
		fmt.Fprintf(out, "Retry at: %s\n", rec.RetryAt.Local().Format(time.DateTime))
	}
	if rec.Error != "" {
		fmt.Fprintf(out, "Error:    %s\n", rec.Error)
	}
	if rec.TraceID != "" {
		fmt.Fprintf(out, "Trace:    %s\n", rec.TraceID)
	}
	fmt.Fprintln(out, "\nTimeline:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, s := range rec.Steps {
		indent := ""
		if s.Parent > 0 {
			indent = "  "
		}
		d := fmtDur(time.Duration(s.DurationMS) * time.Millisecond)
		if s.Kind == diag.KindLog {
			d = ""
		}
		extra := firstNonEmpty(s.Error, s.Detail)
		fmt.Fprintf(tw, "  %s\t%s\t%s%s\t%s\t%s\t%s\n", s.Start.Local().Format("15:04:05"), s.Kind, indent, oneLine(s.Name, 80), s.Status, d, oneLine(extra, 100))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(rec.Claude) > 0 {
		fmt.Fprintln(out, "\nClaude runs:")
		for _, c := range rec.Claude {
			fmt.Fprintf(out, "  #%d %s model=%s status=%s duration=%s turns=%d tokens=%d cost=$%.3f events=%d transcript=%s\n",
				c.N, firstNonEmpty(c.Purpose, "claude"), firstNonEmpty(c.Model, "default"), c.Status,
				fmtDur(time.Duration(c.DurationMS)*time.Millisecond), c.NumTurns, c.Usage.Total(), c.CostUSD, c.Events,
				rd.TranscriptPath(row, c.N))
			if c.Error != "" {
				fmt.Fprintf(out, "     error: %s\n", oneLine(c.Error, 200))
			}
		}
	}
	return nil
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return d.Round(time.Second).String()
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// jobKindList is every job kind for the -kind help (core.AllJobKinds).
func jobKindList() string {
	var ks []string
	for _, k := range core.AllJobKinds() {
		ks = append(ks, string(k))
	}
	return strings.Join(ks, " | ")
}

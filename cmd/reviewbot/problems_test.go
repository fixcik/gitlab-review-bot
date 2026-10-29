package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

func TestProblemsWiring(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Defaults()
	cfg.GitLab.Token = "glpat-abcdefghij0123456789"
	tm := telemetry.New("test")
	svc := newProblems(&cfg, st, tm, slog.New(slog.DiscardHandler))
	if svc == nil {
		t.Fatal("on by default")
	}
	// Scan errors of the system map become problems of the repository;
	// other statuses only feed the metric.
	var seen []string
	f := sysmapProblems(svc, func(repo, status string) { seen = append(seen, repo+":"+status) })
	f("g/a", sysmap.StatusOK)
	f("g/b", sysmap.StatusError)
	rows, err := st.ListProblems(context.Background(), problems.Filter{})
	if err != nil || len(rows) != 1 || rows[0].ProjectPath != "g/b" || rows[0].Kind != problems.SysmapFailed || len(seen) != 2 {
		t.Fatalf("rows %+v seen %v err %v", rows, seen, err)
	}
	// Secrets of the config are scrubbed from stored text.
	ctx, run := svc.Start(context.Background(), core.Job{Kind: core.JobAutoReview, ProjectPath: "g/api", MRIID: 1})
	problems.Add(ctx, problems.Problem{Kind: problems.BaseUnavailable, Subject: "setup", Detail: "auth glpat-abcdefghij0123456789 failed"})
	svc.Finish(ctx, run)
	rows, _ = st.ListProblems(context.Background(), problems.Filter{Kind: problems.BaseUnavailable})
	if len(rows) != 1 || rows[0].Detail != "auth [скрыто] failed" {
		t.Fatalf("scrub: %+v", rows)
	}
	sysmapProblems(nil, nil)("g/c", sysmap.StatusError) // off: no-op
	cfg.Review.Problems.Enabled = false
	if newProblems(&cfg, st, tm, slog.New(slog.DiscardHandler)) != nil {
		t.Fatal("off")
	}
}

package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

type fakeProblems struct {
	rows []problems.Record
	err  error
}

func (f fakeProblems) ListProblems(context.Context, problems.Filter) ([]problems.Record, error) {
	return f.rows, f.err
}

// The header of every page: switched-off sections and the problems badge
// (red when a problem breaks reviews; an error shows no badge).
func TestWebNav(t *testing.T) {
	cfg := config.Defaults()
	off := false
	cfg.Usage.Enabled = &off
	log := slog.New(slog.DiscardHandler)
	n := webNav(&cfg, fakeProblems{rows: []problems.Record{{Kind: problems.LintFailed}, {Kind: problems.ClaudeAuthFailed}}}, false, true, true, log)
	if !n.UsageOff || !n.DiagOff || n.MapOff || n.ProblemsOff || n.Badge == nil {
		t.Fatalf("nav %+v", n)
	}
	if b := n.Badge(context.Background()); b != (webui.Badge{Count: "2", Bad: true}) {
		t.Errorf("badge %+v", b)
	}
	many := make([]problems.Record, navBadgeLimit)
	for i := range many {
		many[i].Kind = problems.LintFailed
	}
	if b := webNav(&cfg, fakeProblems{rows: many}, true, true, true, log).Badge(context.Background()); b != (webui.Badge{Count: "1000+"}) {
		t.Errorf("capped badge %+v", b)
	}
	for _, st := range []fakeProblems{{err: errors.New("db")}, {}} {
		if b := webNav(&cfg, st, true, true, true, log).Badge(context.Background()); b != (webui.Badge{}) {
			t.Errorf("no badge expected: %+v", b)
		}
	}
	if n := webNav(&cfg, fakeProblems{}, true, true, false, log); n.Badge != nil || !n.ProblemsOff {
		t.Errorf("problems off: %+v", n)
	}
}

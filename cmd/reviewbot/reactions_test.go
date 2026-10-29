package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

// forbiddenAwards answers every award with 403 (a bot without access).
type forbiddenAwards struct{}

func (forbiddenAwards) ListAwards(context.Context, int64, int64, int64) ([]core.Award, error) {
	return nil, nil
}
func (forbiddenAwards) AddAward(context.Context, int64, int64, int64, string) (core.Award, error) {
	return core.Award{}, fmt.Errorf("gitlab: HTTP 403: %w", core.ErrForbidden)
}
func (forbiddenAwards) DeleteAward(context.Context, int64, int64, int64, int64) error { return nil }

func TestReactionsWiring(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Defaults()
	tm := telemetry.New("test")
	log := slog.New(slog.DiscardHandler)
	probs := newProblems(&cfg, st, tm, log)
	rc := newReactions(&cfg, forbiddenAwards{}, 99, "reviewbot", tm, probs, nil, log)
	if rc == nil {
		t.Fatal("on by default")
	}
	for i := 0; i < 3; i++ {
		done := rc.Submit(context.Background(), reactions.Change{Event: reactions.EventPickedUp, ProjectPath: "g/api",
			Target: reactions.Target{ProjectID: 1, MRIID: 2, NoteID: 3}, Add: []string{"eyes"}})
		if !reactions.Wait(context.Background(), done, 2*time.Second) {
			t.Fatal("not applied")
		}
	}
	rows, err := st.ListProblems(context.Background(), problems.Filter{Kind: problems.ReactionsFailed})
	if err != nil || len(rows) != 1 || rows[0].ProjectPath != "g/api" || rows[0].Count != 1 {
		t.Fatalf("problems %+v %v", rows, err)
	}
	var b strings.Builder
	if err := tm.Registry.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `reviewbot_reactions_total{event="picked_up",result="forbidden"} 3`) {
		t.Errorf("metric missing:\n%s", b.String())
	}
	cfg.Review.Reactions.Enabled = false
	if newReactions(&cfg, forbiddenAwards{}, 99, "reviewbot", tm, probs, nil, log) != nil {
		t.Fatal("off")
	}
}

package reviewtest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// KindRunner records the meter attribution kind of every run.
type KindRunner struct {
	*FakeRunner
	Kinds []string
}

func (k *KindRunner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	a, _ := meter.From(ctx)
	k.Kinds = append(k.Kinds, a.Kind)
	return k.FakeRunner.Run(ctx, req)
}

type MemHarness struct {
	*Harness
	Db   *store.SQLite
	Kind *KindRunner
}

// NewMemHarness is New with a real SQLite memory store.
func NewMemHarness(t *testing.T) *MemHarness {
	t.Helper()
	h := New(t)
	db, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	kr := &KindRunner{FakeRunner: h.Runner}
	h.Deps = review.Deps{
		Compat: h.Compat,
		GitLab: h.GL, Repos: h.Repos, Runner: kr, Store: h.St, Limiter: h.Lim, Config: h.Cfg,
		Memory: db, ScratchDir: t.TempDir(),
		BuildIndex:    func([]core.FileDiff) env.DiffIndex { return h.Idx },
		RepoConfigTTL: -1, // tests change the fake .reviewbot.yaml between runs
		BotUserID:     BotID,
		BotUsername:   BotName,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           func() time.Time { return TestNow },
	}
	h.Reset()
	return &MemHarness{Harness: h, Db: db, Kind: kr}
}

func (h *MemHarness) Seed(t *testing.T, key, content string) {
	t.Helper()
	if err := h.Db.SaveMemorySection(context.Background(), memory.Section{ProjectID: PID, ProjectPath: "group/api", Key: key, Content: content, UpdatedByMR: 3}); err != nil {
		t.Fatal(err)
	}
}

func (h *MemHarness) Section(t *testing.T, key string) []string {
	t.Helper()
	secs, err := h.Db.ListMemory(context.Background(), PID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range secs {
		if s.Key == key {
			return memory.Bullets(s.Content)
		}
	}
	return nil
}

// ReviewOutMem is a review answer (with compatibility) plus memory_updates.
func ReviewOutMem(t *testing.T, updates []memory.Update) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(ReviewOutCompat(t, nil, nil)), &m); err != nil {
		t.Fatal(err)
	}
	if updates != nil {
		m["memory_updates"] = updates
	}
	return MustJSON(t, m)
}

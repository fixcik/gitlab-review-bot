package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

type dirSnapshots struct{ dirs []core.ContextDir }

func (d dirSnapshots) Acquire(context.Context, core.ContextRequest) ([]core.ContextDir, []core.ContextMissing, func()) {
	return d.dirs, nil, func() {}
}

type mapRunner struct{ innerRunner }

func (r *mapRunner) Run(ctx context.Context, req core.RunRequest) (*core.RunResult, error) {
	r.innerRunner.Run(ctx, req)
	return &core.RunResult{Output: `{"entries":[{"file":"app.py","line":1,"side":"provides","protocol":"http","method":"GET","path":"/health","event":"","base":"","raw":"@app.get('/health')"}]}`,
		CostUSD: 0.02}, nil
}

// serve's wiring of the system map: extraction runs go through the
// metered runner as kind "map", and /map serves the result.
func TestSysmapWiring(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "reviewbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap := filepath.Join(dir, "snap")
	os.MkdirAll(snap, 0o755)
	os.WriteFile(filepath.Join(snap, "app.py"), []byte("@app.get('/health')\ndef health(): pass\n"), 0o644)

	cfg := config.Defaults()
	cfg.GitLab.Groups = []string{"g"}
	inner := &mapRunner{}
	runner := meter.Wrap(inner, st, time.Now, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := startSysmap(ctx, &cfg, dirSnapshots{dirs: []core.ContextDir{{HostPath: snap, Name: "g/api", SHA: "abc", Branch: "main"}}}, st, runner, dir, nil, nil, logger)
	if m == nil {
		t.Fatal("map not started")
	}
	if err := m.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if len(inner.runs) != 1 || !inner.runs[0].NoTools || inner.runs[0].Model != "sonnet" {
		t.Fatalf("runs = %+v", inner.runs)
	}
	groups, err := st.RunsBy(ctx, meter.RunFilter{}, meter.ByKind)
	if err != nil || len(groups) != 1 || groups[0].Key != meter.KindMap {
		t.Fatalf("metered kinds = %+v, %v", groups, err)
	}
	if systemMapDep(m) == nil || systemMapDep(nil) != nil {
		t.Error("systemMapDep")
	}

	mux := http.NewServeMux()
	registerMapWeb(mux, &cfg, m, webui.Nav{}, logger)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map/data.json", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"/health"`) {
		t.Fatalf("/map/data.json: %d %s", w.Code, w.Body.String())
	}

	off := config.Defaults() // no groups: no context repositories, no map
	if startSysmap(ctx, &off, nil, st, runner, dir, nil, nil, logger) != nil {
		t.Error("map started without context repositories")
	}
	mux = http.NewServeMux()
	registerMapWeb(mux, &off, nil, webui.Nav{}, logger)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/map", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("/map when off: %d", w.Code)
	}
}

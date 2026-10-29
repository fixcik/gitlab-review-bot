package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/diagweb"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// /runs/data.json carries the queue of the store it was wired with; without
// a store (or with diagnostics off) there is no queue block / no page.
func TestRegisterDiagWebQueue(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "reviewbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, err := diag.New(diag.Options{Root: filepath.Join(dir, "diag"), Index: st})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := st.AddJob(context.Background(), queue.Stored{Job: core.Job{Kind: core.JobChat, ProjectID: 1, ProjectPath: "g/api", MRIID: 2, EnqueuedAt: now}, ReadyAt: now}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	get := func(mux *http.ServeMux) (int, diagweb.ListData) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, diagweb.DataPath, nil))
		var d diagweb.ListData
		_ = json.Unmarshal(w.Body.Bytes(), &d)
		return w.Code, d
	}
	mux := http.NewServeMux()
	registerDiagWeb(mux, &config.Service{}, rec, st, webui.Nav{}, log)
	if code, d := get(mux); code != http.StatusOK || d.Queue == nil || d.Queue.Waiting != 1 {
		t.Fatalf("with store: %d %+v", code, d.Queue)
	}
	mux = http.NewServeMux()
	registerDiagWeb(mux, &config.Service{}, rec, nil, webui.Nav{}, log)
	if code, d := get(mux); code != http.StatusOK || d.Queue != nil {
		t.Fatalf("without store: %d %+v", code, d.Queue)
	}
	mux = http.NewServeMux()
	registerDiagWeb(mux, &config.Service{}, nil, st, webui.Nav{}, log)
	if code, _ := get(mux); code != http.StatusNotFound {
		t.Fatalf("diagnostics off: %d", code)
	}
}

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
)

func TestRegisterTelemetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":1,"username":"bot"}`)
	}))
	defer srv.Close()
	gl, err := gitlab.New(srv.URL, "glpat-test-token-123456")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, enabled := range []bool{true, false} {
		cfg := config.Defaults()
		cfg.Sandbox.Mode = "local" // no docker check in tests
		cfg.Metrics.Enabled = &enabled
		tm := newTelemetry(&cfg)
		if (tm != nil) != enabled {
			t.Fatalf("enabled=%v: tm=%v", enabled, tm)
		}
		observeGitLab(tm, gl, nil)
		mux := http.NewServeMux()
		registerTelemetry(mux, tm, &cfg, gl, st, slog.New(slog.DiscardHandler))

		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var rep telemetry.ReadyReport
		_ = json.Unmarshal(w.Body.Bytes(), &rep)
		if w.Code != 200 || rep.Status != "ok" || len(rep.Checks) != 2 {
			t.Fatalf("readyz enabled=%v: %d %s", enabled, w.Code, w.Body.String())
		}

		w = httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if !enabled {
			if w.Code != http.StatusNotFound {
				t.Fatalf("disabled /metrics: %d", w.Code)
			}
			continue
		}
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `reviewbot_build_info{version="dev"`) ||
			!strings.Contains(body, `reviewbot_gitlab_requests_total{method="GET",code="2xx"} 1`) ||
			!strings.Contains(body, `reviewbot_readiness_check_ok{check="store"} 1`) {
			t.Fatalf("/metrics:\n%s", body)
		}
	}
}

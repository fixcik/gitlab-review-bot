package main

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/homeweb"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// registerHomeWeb mounts the main page "/" (every other route of the mux is
// more specific and wins). It uses the /usage token; links, cards and
// figures of switched-off pages are left out (diagOn: /runs is served, i.e.
// the diagnostics recorder started; sysMap nil = /map is off).
func registerHomeWeb(mux *http.ServeMux, cfg *config.Service, st *store.SQLite, diagOn bool, sysMap *sysmap.Map, problemsOn bool, nav webui.Nav, logger *slog.Logger) {
	o := homeweb.Options{Nav: nav,
		Store: st, Token: cfg.Usage.Token, Usage: cfg.Usage.On(), Diag: diagOn, Problems: problemsOn,
		Now: time.Now, Logger: logger,
	}
	if sysMap != nil {
		o.Map = sysMap
	}
	homeweb.Register(mux, o)
}

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/doctor"
	"github.com/fixcik/gitlab-review-bot/internal/setupweb"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// registerSetupWeb mounts /setup: the doctor checks on the running config
// (serve validated it, so the config check is left out), behind the /usage
// token.
func registerSetupWeb(mux *http.ServeMux, cfg *config.Service, nav webui.Nav, logger *slog.Logger) {
	in := doctorInputs("", cfg, nil, os.Getenv)
	setupweb.Register(mux, setupweb.Options{
		Nav: nav, Token: cfg.Usage.Token, Logger: logger, Now: time.Now, Timeout: doctorTimeout,
		Run: func(ctx context.Context) doctor.Report { return doctor.Run(ctx, time.Now(), in.Providers()...) },
	})
}

// Command server runs the users service.
package main

import (
	"database/sql"
	"log/slog"
	"net/http"
	"os"
	"time"

	"gitlab.local/reviewbot-sandbox/rb-backend/internal/events"
	"gitlab.local/reviewbot-sandbox/rb-backend/internal/httpx"
	"gitlab.local/reviewbot-sandbox/rb-backend/internal/notify"
	"gitlab.local/reviewbot-sandbox/rb-backend/internal/users"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	db, err := sql.Open("pgx", os.Getenv("USERS_DB_DSN"))
	if err != nil {
		logger.Error("open database", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	pub := events.NewLogPublisher(logger, events.TopicFromEnv())
	hub := notify.NewHub()

	mux := http.NewServeMux()
	httpx.RegisterHealth(mux)
	users.NewHandler(users.NewStore(db), pub, hub).Register(mux)
	mux.HandleFunc("GET /ws/notifications", hub.ServeWS)

	srv := &http.Server{
		Addr:              envOr("LISTEN_ADDR", ":8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	logger.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil {
		logger.Error("serve", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

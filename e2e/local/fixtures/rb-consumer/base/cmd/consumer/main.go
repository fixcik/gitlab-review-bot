// Command consumer reads user.created events (one JSON message per line on
// stdin in development; the bus adapter feeds the same JSON in production).
package main

import (
	"bufio"
	"context"
	"log/slog"
	"os"

	"gitlab.local/reviewbot-sandbox/rb-consumer/internal/welcome"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	topic := os.Getenv("EVENTS_TOPIC")
	if topic == "" {
		topic = "users.events"
	}
	logger.Info("consuming", "topic", topic)

	h := welcome.NewHandler(welcome.LogMailer{Logger: logger})
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		if err := h.Handle(context.Background(), sc.Bytes()); err != nil {
			logger.Error("handle event", "err", err)
		}
	}
}

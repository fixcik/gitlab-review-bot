// Package events publishes domain events of the users service. The JSON
// schema of UserCreated is a contract: rb-consumer decodes it (see also
// proto/users.proto).
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
)

// DefaultTopic is used when EVENTS_TOPIC is not set.
const DefaultTopic = "users.events"

// UserCreated is published after a user is created.
type UserCreated struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Publisher sends events to the message bus.
type Publisher interface {
	Publish(ctx context.Context, e UserCreated) error
}

// TopicFromEnv returns the topic name from EVENTS_TOPIC.
func TopicFromEnv() string {
	if t := os.Getenv("EVENTS_TOPIC"); t != "" {
		return t
	}
	return DefaultTopic
}

// LogPublisher writes events to the log (the bus adapter is wired in
// production; the format on the wire is the same JSON).
type LogPublisher struct {
	logger *slog.Logger
	topic  string
}

// NewLogPublisher returns a LogPublisher.
func NewLogPublisher(l *slog.Logger, topic string) *LogPublisher {
	return &LogPublisher{logger: l, topic: topic}
}

// Publish implements Publisher.
func (p *LogPublisher) Publish(_ context.Context, e UserCreated) error {
	b, err := json.Marshal(struct {
		Type string      `json:"type"`
		Data UserCreated `json:"data"`
	}{"user.created", e})
	if err != nil {
		return err
	}
	p.logger.Info("event", "topic", p.topic, "payload", string(b))
	return nil
}

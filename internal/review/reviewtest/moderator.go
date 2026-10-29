package reviewtest

import (
	"context"
	"sync"
)

type FakeModerator struct {
	mu        sync.Mutex
	Resolved  []string
	Reviewers []int64
}

func (m *FakeModerator) ResolveDiscussion(_ context.Context, _, _ int64, id string, resolved bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if resolved {
		m.Resolved = append(m.Resolved, id)
	}
	return nil
}

func (m *FakeModerator) AddReviewer(_ context.Context, _, _ int64, userID int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.Reviewers {
		if id == userID {
			return false, nil
		}
	}
	m.Reviewers = append(m.Reviewers, userID)
	return true, nil
}

func WithModerator(h *Harness) *FakeModerator {
	m := &FakeModerator{}
	h.Env().Mod = m
	h.GL.PublishMakesThreads = true
	h.GL.ForbidResolve = true // the bot itself is a Reporter
	return m
}

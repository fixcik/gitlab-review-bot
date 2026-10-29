package reviewtest

import (
	"context"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// FlagStore is the harness store plus MRFlags.
type FlagStore struct {
	*FakeStore
	fmu    sync.Mutex
	Paused map[string]string // mr key -> by
	at     map[string]time.Time
}

func (s *FlagStore) GetMRPause(_ context.Context, p, i int64) (bool, string, time.Time, error) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	by, ok := s.Paused[core.MRKey(p, i)]
	return ok, by, s.at[core.MRKey(p, i)], nil
}

func (s *FlagStore) SetMRPause(_ context.Context, p, i int64, by string, at time.Time) error {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	s.Paused[core.MRKey(p, i)], s.at[core.MRKey(p, i)] = by, at
	return nil
}

func (s *FlagStore) ClearMRPause(_ context.Context, p, i int64) (bool, error) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	_, ok := s.Paused[core.MRKey(p, i)]
	delete(s.Paused, core.MRKey(p, i))
	return ok, nil
}

func (s *FlagStore) IsPaused() bool {
	p, _, _, _ := s.GetMRPause(context.Background(), PID, IID)
	return p
}

func WithFlags(h *Harness) *FlagStore {
	fs := &FlagStore{FakeStore: h.St, Paused: map[string]string{}, at: map[string]time.Time{}}
	h.Deps.MRFlags = fs
	return fs
}

func CmdJob(kind core.JobKind) core.Job { return ManualJob(kind) }

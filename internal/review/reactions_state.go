package review

// The reviewer's reaction state: notes that wait for their job's final
// award, and the cached reactions.enabled switch per MR.

import (
	"context"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
)

// reactor is the reviewer's reaction state.
type reactor struct {
	rc *reactions.Reactor

	mu sync.Mutex
	// notes: notes that got 👀 (or ⏳) and whose job has not ended yet;
	// postponed: of those, the ones waiting with ⏳.
	notes     map[int64]time.Time
	postponed map[int64]bool
	// repos caches the repository switch per MR (effectiveConfig fills it).
	repos map[string]repoReact
}

type repoReact struct {
	on bool
	at time.Time
}

func newReactor(rc *reactions.Reactor) *reactor {
	if rc == nil {
		return nil
	}
	return &reactor{rc: rc, notes: map[int64]time.Time{}, postponed: map[int64]bool{}, repos: map[string]repoReact{}}
}

// reactState is one job's reaction bookkeeping (in its context).
type reactState struct {
	mu    sync.Mutex
	job   core.Job
	note  bool          // the job's note carries the 👀 … ✅ lifecycle
	quiet bool          // "done" only removes 👀 (thread replies)
	marks *jobctx.Marks // denied (🚫), learned (🧠)
	last  <-chan struct{}
}

type reactKey struct{}

func reactFrom(ctx context.Context) *reactState {
	st, _ := ctx.Value(reactKey{}).(*reactState)
	return st
}

// rememberReactions caches the repository switch (effectiveConfig).
func (r *Reviewer) rememberReactions(projectID, mrIID int64, eff config.Effective) {
	if r.rx == nil {
		return
	}
	r.rx.mu.Lock()
	defer r.rx.mu.Unlock()
	now := r.env.Now()
	if len(r.rx.repos) > reactMaxNotes {
		for k, v := range r.rx.repos {
			if now.Sub(v.at) > reactRepoTTL {
				delete(r.rx.repos, k)
			}
		}
	}
	r.rx.repos[core.MRKey(projectID, mrIID)] = repoReact{on: eff.Reactions.Enabled, at: now}
}

func (x *reactor) repo(key string, now time.Time) (bool, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	v, ok := x.repos[key]
	if !ok || now.Sub(v.at) > reactRepoTTL || now.Before(v.at) {
		return false, false
	}
	return v.on, true
}

func (x *reactor) track(note int64, now time.Time) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.notes) >= reactMaxNotes {
		for n, at := range x.notes {
			if now.Sub(at) > 24*time.Hour {
				delete(x.notes, n)
				delete(x.postponed, n)
			}
		}
	}
	x.notes[note] = now
	delete(x.postponed, note)
}

func (x *reactor) state(note int64) (seen, postponed bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	_, seen = x.notes[note]
	return seen, x.postponed[note]
}

func (x *reactor) setPostponed(note int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.postponed[note] = true
}

func (x *reactor) forget(note int64) {
	x.mu.Lock()
	defer x.mu.Unlock()
	delete(x.notes, note)
	delete(x.postponed, note)
}

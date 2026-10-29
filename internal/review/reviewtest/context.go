package reviewtest

import (
	"context"
	"strings"
	"sync"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// FakeContextRepos returns a ContextDir for every known project the filter
// lets through, related ones first; related paths not in projects (or in
// unknown) are reported missing.
type FakeContextRepos struct {
	mu       sync.Mutex
	Projects []string
	Calls    int
	Released int
	asked    []string // projects passed through the filter
	Related  []string // last req.Related
}

func (f *FakeContextRepos) Acquire(_ context.Context, req core.ContextRequest) ([]core.ContextDir, []core.ContextMissing, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.Related = append([]string(nil), req.Related...)
	mk := func(p string) core.ContextDir {
		return core.ContextDir{HostPath: "/data/snapshots/" + p, Name: p, SHA: "c0ffee" + strings.Repeat("0", 34), Branch: "main"}
	}
	var out []core.ContextDir
	var missing []core.ContextMissing
	taken := map[string]bool{}
	for _, r := range req.Related {
		known := false
		for _, p := range f.Projects {
			if strings.EqualFold(p, r) {
				known = true
			}
		}
		if !known && !strings.HasPrefix(r, "outside/") {
			missing = append(missing, core.ContextMissing{Path: r, Reason: "не найден или у бота нет доступа на чтение"})
			continue
		}
		taken[strings.ToLower(r)] = true
		out = append(out, mk(r))
	}
	for _, p := range f.Projects {
		if taken[strings.ToLower(p)] || (req.Exclude != nil && req.Exclude(p)) {
			continue
		}
		f.asked = append(f.asked, p)
		out = append(out, mk(p))
	}
	return out, missing, func() {
		f.mu.Lock()
		f.Released++
		f.mu.Unlock()
	}
}

package reviewtest

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// FakeMRLabels is the MR's label state on the fake GitLab, shared by the
// bot's and the service token's clients.
type FakeMRLabels struct {
	Mu      sync.Mutex
	State   string
	Labels  []string
	Project []string // labels that exist in the project / groups
}

// FakeLabelClient is one token's client.
type FakeLabelClient struct {
	Mr      *FakeMRLabels
	Err     error // UpdateMRLabels fails with it
	Ignore  bool  // 200 without applying anything (no right to set metadata)
	ListErr error
	Updates [][2][]string
	Reads   int
}

func (c *FakeLabelClient) MRLabels(context.Context, int64, int64) (string, []string, error) {
	c.Mr.Mu.Lock()
	defer c.Mr.Mu.Unlock()
	c.Reads++
	return c.Mr.State, append([]string(nil), c.Mr.Labels...), nil
}

func (c *FakeLabelClient) UpdateMRLabels(_ context.Context, _, _ int64, add, remove []string) ([]string, error) {
	c.Mr.Mu.Lock()
	defer c.Mr.Mu.Unlock()
	c.Updates = append(c.Updates, [2][]string{append([]string(nil), add...), append([]string(nil), remove...)})
	if c.Err != nil {
		return nil, c.Err
	}
	if !c.Ignore {
		var keep []string
		for _, l := range c.Mr.Labels {
			drop := slices.Contains(remove, l)
			for _, a := range add { // GitLab: one label per scope
				if sc := config.LabelScope(a); sc != "" && sc == config.LabelScope(l) && a != l {
					drop = true
				}
			}
			if !drop {
				keep = append(keep, l)
			}
		}
		for _, a := range add {
			if !slices.Contains(keep, a) {
				keep = append(keep, a)
			}
		}
		c.Mr.Labels = keep
	}
	return append([]string(nil), c.Mr.Labels...), nil
}

func (c *FakeLabelClient) ListProjectLabels(context.Context, int64) ([]string, error) {
	if c.ListErr != nil {
		return nil, c.ListErr
	}
	c.Mr.Mu.Lock()
	defer c.Mr.Mu.Unlock()
	return append([]string(nil), c.Mr.Project...), nil
}

// LabelFakeStore adds LabelStore to the harness store.
type LabelFakeStore struct {
	*FakeStore
	mu               sync.Mutex
	Added, Suppresed []string
	Saves            int
}

func (s *LabelFakeStore) BotLabels(context.Context, int64, int64) ([]string, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.Added...), append([]string(nil), s.Suppresed...), nil
}

func (s *LabelFakeStore) SaveBotLabels(_ context.Context, _, _ int64, added, suppressed []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Added, s.Suppresed = added, suppressed
	s.Saves++
	return nil
}

type LabelHarness struct {
	*Harness
	Mr       *FakeMRLabels
	Bot, Svc *FakeLabelClient
	Ls       *LabelFakeStore
	Metrics  []string
	mmu      sync.Mutex
}

const LabelRepoConfig = `labels:
  rules:
    - name: backend
      paths: ["**/*.go"]
    - name: db-migration
      when_fact: migrations
    - name: type::bug
      instruction: "MR fixes incorrect behaviour (not a new feature, not a refactoring)."
    - name: needs-design-review
      instruction: "Changes public API shape or cross-service contracts."
`

func NewLabelHarness(t *testing.T) *LabelHarness {
	t.Helper()
	h := New(t)
	h.Cfg.Review.Lint.Enabled = false
	h.GL.RepoConfig = LabelRepoConfig
	lh := &LabelHarness{Harness: h, Mr: &FakeMRLabels{State: "opened",
		Project: []string{"backend", "db-migration", "type::bug", "type::feature", "needs-design-review"}}}
	lh.Bot = &FakeLabelClient{Mr: lh.Mr, Err: fmt.Errorf("403: %w", core.ErrForbidden)} // Reporter bot
	lh.Svc = &FakeLabelClient{Mr: lh.Mr}
	lh.Ls = &LabelFakeStore{FakeStore: h.St}
	h.Deps.LabelStore = lh.Ls
	h.Deps.Labels, h.Deps.ServiceLabels = lh.Bot, lh.Svc
	h.Deps.OnLabel = func(action, result string) {
		lh.mmu.Lock()
		defer lh.mmu.Unlock()
		lh.Metrics = append(lh.Metrics, action+":"+result)
	}
	return lh
}

func (lh *LabelHarness) Review(t *testing.T, job core.Job, out string) {
	t.Helper()
	lh.Runner.Resps = []RunResp{{Out: out}}
	if err := lh.R().Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func (lh *LabelHarness) Summary(t *testing.T) string {
	t.Helper()
	lh.GL.Mu.Lock()
	defer lh.GL.Mu.Unlock()
	var best int64
	for id, b := range lh.GL.Notes {
		if strings.Contains(b, summary.Title) && id > best {
			best = id
		}
	}
	if best == 0 {
		t.Fatal("no summary note")
	}
	return lh.GL.Notes[best]
}

func (lh *LabelHarness) Labels() []string {
	lh.Mr.Mu.Lock()
	defer lh.Mr.Mu.Unlock()
	out := append([]string(nil), lh.Mr.Labels...)
	sort.Strings(out)
	return out
}

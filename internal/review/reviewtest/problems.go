package reviewtest

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// ProbStore is an in-memory problems.Store.
type ProbStore struct {
	mu   sync.Mutex
	Rows map[string]*problems.Record
}

func (s *ProbStore) UpsertProblem(_ context.Context, r problems.Record) (problems.Upserted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.Rows[r.Fingerprint]
	if !ok {
		c := r
		c.Count = 1
		s.Rows[r.Fingerprint] = &c
		return problems.Upserted{New: true}, nil
	}
	up := problems.Upserted{Reappeared: !old.AckedAt.IsZero() || !old.ResolvedAt.IsZero(), NotifiedAt: old.NotifiedAt}
	old.Count++
	old.Detail, old.LastSeen, old.AckedAt, old.ResolvedAt = r.Detail, r.LastSeen, time.Time{}, time.Time{}
	return up, nil
}

func (s *ProbStore) MarkProblemsNotified(_ context.Context, fps []string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, fp := range fps {
		if r := s.Rows[fp]; r != nil {
			r.NotifiedAt = at
		}
	}
	return nil
}

func (s *ProbStore) ListProblems(context.Context, problems.Filter) ([]problems.Record, error) {
	return nil, nil
}

func (s *ProbStore) AckProblem(_ context.Context, fp string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.Rows[fp]; r != nil {
		r.AckedAt = at
		return true, nil
	}
	return false, nil
}

func (s *ProbStore) ResolveProblem(_ context.Context, fp string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.Rows[fp]; r != nil && r.ResolvedAt.IsZero() {
		r.ResolvedAt = at
		return true, nil
	}
	return false, nil
}

func (s *ProbStore) PruneProblems(context.Context, time.Time) (int64, error) { return 0, nil }

func (s *ProbStore) Kinds() map[problems.Kind][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[problems.Kind][]string{}
	for _, r := range s.Rows {
		out[r.Kind] = append(out[r.Kind], r.Subject)
	}
	return out
}

// ProbHarness is a harness with a problems service; now is its clock.
type ProbHarness struct {
	*Harness
	Svc *problems.Service
	St  *ProbStore
	Now time.Time
}

func NewProbHarness(t *testing.T, mode string, notify ...string) *ProbHarness {
	t.Helper()
	ph := &ProbHarness{Harness: New(t), St: &ProbStore{Rows: map[string]*problems.Record{}}, Now: TestNow}
	ph.Cfg.Review.Lint.Enabled = false // the harness clone has no .git: gitleaks would be a problem in every test
	ph.Svc = problems.New(problems.Options{Store: ph.St, Notify: notify, NotifyMode: mode, PageURL: "https://bot.example.com",
		Now: func() time.Time { return ph.Now }})
	return ph
}

// review runs one auto review inside the problems wrapper.
func (ph *ProbHarness) Review(t *testing.T, resps ...RunResp) error {
	t.Helper()
	if len(resps) == 0 {
		resps = []RunResp{{Out: ReviewOut(t, nil, nil)}}
	}
	ph.Runner.Resps = resps
	return ph.Svc.Handler(ph.R().Handle)(context.Background(), AutoJob())
}

// summary is the body of the summary note.
func (ph *ProbHarness) Summary(t *testing.T) string {
	t.Helper()
	ph.GL.Mu.Lock()
	defer ph.GL.Mu.Unlock()
	// The newest one: the fake discussions do not list notes, so every
	// review creates a fresh summary. Test SHAs are not hex: no marker.IsSummaryNote.
	var best int64
	for id, b := range ph.GL.Notes {
		if strings.Contains(b, summary.Title) && id > best {
			best = id
		}
	}
	if best == 0 {
		t.Fatal("no summary note")
	}
	return ph.GL.Notes[best]
}

func ProblemsBlockOf(body string) string {
	i, j := strings.Index(body, marker.ProblemsStart), strings.Index(body, marker.ProblemsEnd)
	if i < 0 || j < i {
		return ""
	}
	return body[i : j+len(marker.ProblemsEnd)]
}

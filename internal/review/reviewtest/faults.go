package reviewtest

import (
	"context"
	"sync"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Fault injection for the error paths of the reviewer's I/O: FaultGitLab,
// FaultStore and FaultRepos wrap the fakes and fail the calls a test picks.
// Install them through Deps before the first R (NewFaults does).

// FaultFunc decides whether a call of method fails: nil lets it through.
// n is the number of calls of method so far, this one included (the
// reviewer's per-job memo answers repeated reads of an MR, so only real
// calls are counted).
type FaultFunc func(method string, n int) error

// Faults counts calls by method and asks Fail about each.
type Faults struct {
	mu    sync.Mutex
	Fail  FaultFunc
	calls map[string]int
}

func (f *Faults) fault(method string) error {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[method]++
	n, fail := f.calls[method], f.Fail
	f.mu.Unlock()
	if fail == nil {
		return nil
	}
	return fail(method, n)
}

// Calls is the number of calls of method so far.
func (f *Faults) Calls(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// FailOn fails every call of the methods with err.
func FailOn(err error, methods ...string) FaultFunc {
	return func(m string, _ int) error {
		for _, x := range methods {
			if x == m {
				return err
			}
		}
		return nil
	}
}

// FailNth fails the nth call of method (only that one) with err.
func FailNth(method string, nth int, err error) FaultFunc {
	return func(m string, n int) error {
		if m == method && n == nth {
			return err
		}
		return nil
	}
}

// FaultGitLab is FakeGitLab with injected failures (method names as in
// core.GitLab: "GetMR", "ListDiscussions", "UpdateNote", …). The fake's
// capabilities (CI jobs, reports, open MRs) are promoted.
type FaultGitLab struct {
	*FakeGitLab
	Faults
}

func (g *FaultGitLab) GetMR(ctx context.Context, p, i int64) (*core.MergeRequest, error) {
	if err := g.fault("GetMR"); err != nil {
		return nil, err
	}
	return g.FakeGitLab.GetMR(ctx, p, i)
}

func (g *FaultGitLab) GetMRDiffs(ctx context.Context, p, i int64) ([]core.FileDiff, error) {
	if err := g.fault("GetMRDiffs"); err != nil {
		return nil, err
	}
	return g.FakeGitLab.GetMRDiffs(ctx, p, i)
}

func (g *FaultGitLab) ListDiscussions(ctx context.Context, p, i int64) ([]core.Discussion, error) {
	if err := g.fault("ListDiscussions"); err != nil {
		return nil, err
	}
	return g.FakeGitLab.ListDiscussions(ctx, p, i)
}

func (g *FaultGitLab) GetDiscussion(ctx context.Context, p, i int64, id string) (*core.Discussion, error) {
	if err := g.fault("GetDiscussion"); err != nil {
		return nil, err
	}
	return g.FakeGitLab.GetDiscussion(ctx, p, i, id)
}

func (g *FaultGitLab) CreateDraftNote(ctx context.Context, p, i int64, body string, pos *core.Position) (int64, error) {
	if err := g.fault("CreateDraftNote"); err != nil {
		return 0, err
	}
	return g.FakeGitLab.CreateDraftNote(ctx, p, i, body, pos)
}

func (g *FaultGitLab) PublishDraftNotes(ctx context.Context, p, i int64) error {
	if err := g.fault("PublishDraftNotes"); err != nil {
		return err
	}
	return g.FakeGitLab.PublishDraftNotes(ctx, p, i)
}

func (g *FaultGitLab) DeleteDraftNotes(ctx context.Context, p, i int64) error {
	if err := g.fault("DeleteDraftNotes"); err != nil {
		return err
	}
	return g.FakeGitLab.DeleteDraftNotes(ctx, p, i)
}

func (g *FaultGitLab) CreateNote(ctx context.Context, p, i int64, body string) (int64, error) {
	if err := g.fault("CreateNote"); err != nil {
		return 0, err
	}
	return g.FakeGitLab.CreateNote(ctx, p, i, body)
}

func (g *FaultGitLab) UpdateNote(ctx context.Context, p, i, noteID int64, body string) error {
	if err := g.fault("UpdateNote"); err != nil {
		return err
	}
	return g.FakeGitLab.UpdateNote(ctx, p, i, noteID, body)
}

func (g *FaultGitLab) ReplyToDiscussion(ctx context.Context, p, i int64, id, body string) error {
	if err := g.fault("ReplyToDiscussion"); err != nil {
		return err
	}
	return g.FakeGitLab.ReplyToDiscussion(ctx, p, i, id, body)
}

func (g *FaultGitLab) ResolveDiscussion(ctx context.Context, p, i int64, id string, resolved bool) error {
	if err := g.fault("ResolveDiscussion"); err != nil {
		return err
	}
	return g.FakeGitLab.ResolveDiscussion(ctx, p, i, id, resolved)
}

// FaultStore is FakeStore with injected failures ("GetMRState",
// "SaveMRState").
type FaultStore struct {
	*FakeStore
	Faults
}

func (s *FaultStore) GetMRState(ctx context.Context, p, i int64) (*core.MRState, error) {
	if err := s.fault("GetMRState"); err != nil {
		return nil, err
	}
	return s.FakeStore.GetMRState(ctx, p, i)
}

func (s *FaultStore) SaveMRState(ctx context.Context, st core.MRState) error {
	if err := s.fault("SaveMRState"); err != nil {
		return err
	}
	return s.FakeStore.SaveMRState(ctx, st)
}

// FaultRepos is FakeRepos with injected failures ("Checkout") and
// checkouts that fail theirs ("IsAncestor", "ChangedFiles", "Diff",
// "ShowFile").
type FaultRepos struct {
	*FakeRepos
	Faults
}

func (f *FaultRepos) Checkout(ctx context.Context, path string, mr *core.MergeRequest) (core.Checkout, error) {
	if err := f.fault("Checkout"); err != nil {
		return nil, err
	}
	co, err := f.FakeRepos.Checkout(ctx, path, mr)
	if err != nil {
		return nil, err
	}
	return &FaultCheckout{FakeCheckout: co.(*FakeCheckout), f: &f.Faults}, nil
}

// FaultCheckout is the checkout of FaultRepos (core.TreeExporter too).
type FaultCheckout struct {
	*FakeCheckout
	f *Faults
}

func (c *FaultCheckout) IsAncestor(ctx context.Context, commit string) (bool, error) {
	if err := c.f.fault("IsAncestor"); err != nil {
		return false, err
	}
	return c.FakeCheckout.IsAncestor(ctx, commit)
}

func (c *FaultCheckout) ChangedFiles(ctx context.Context, from string, ignore []string) ([]string, error) {
	if err := c.f.fault("ChangedFiles"); err != nil {
		return nil, err
	}
	return c.FakeCheckout.ChangedFiles(ctx, from, ignore)
}

func (c *FaultCheckout) Diff(ctx context.Context, from string, ignore []string) (string, error) {
	if err := c.f.fault("Diff"); err != nil {
		return "", err
	}
	return c.FakeCheckout.Diff(ctx, from, ignore)
}

func (c *FaultCheckout) ShowFile(ctx context.Context, rev, path string) ([]byte, error) {
	if err := c.f.fault("ShowFile"); err != nil {
		return nil, err
	}
	return c.FakeCheckout.ShowFile(ctx, rev, path)
}

// WithFaults installs the fault wrappers around h's GitLab, store and repos
// (before the first R) and returns them; the fakes stay reachable through
// h.GL, h.St and h.Repos.
func WithFaults(h *Harness) (*FaultGitLab, *FaultStore, *FaultRepos) {
	gl, st, rp := &FaultGitLab{FakeGitLab: h.GL}, &FaultStore{FakeStore: h.St}, &FaultRepos{FakeRepos: h.Repos}
	h.Deps.GitLab, h.Deps.Store, h.Deps.Repos = gl, st, rp
	return gl, st, rp
}

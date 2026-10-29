package diag

import (
	"context"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

type fakeLabels struct{ err error }

func (f fakeLabels) MRLabels(context.Context, int64, int64) (string, []string, error) {
	return "opened", []string{"a"}, f.err
}
func (f fakeLabels) UpdateMRLabels(_ context.Context, _, _ int64, add, _ []string) ([]string, error) {
	return add, f.err
}
func (f fakeLabels) ListProjectLabels(context.Context, int64) ([]string, error) {
	return []string{"a", "b"}, f.err
}

// Label calls of both tokens are steps of the job's timeline; the bot
// token's 403 is a warning (the service token takes over).
func TestWrapLabelsSteps(t *testing.T) {
	e := newEnv(t)
	hooks := Hooks{Rec: e.rec}
	if hooks.WrapLabels(nil, false) != nil || (Hooks{}).WrapLabels(fakeLabels{}, false) != (fakeLabels{}) {
		t.Fatal("nil client or diagnostics off must stay unwrapped")
	}
	bot := hooks.WrapLabels(fakeLabels{err: core.ErrForbidden}, false)
	svc := hooks.WrapLabels(fakeLabels{}, true)
	var id string
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		id = FromContext(ctx).ID()
		_, _, _ = bot.MRLabels(ctx, j.ProjectID, j.MRIID)
		_, _ = bot.UpdateMRLabels(ctx, j.ProjectID, j.MRIID, []string{"x"}, nil)
		_, _ = svc.UpdateMRLabels(ctx, j.ProjectID, j.MRIID, []string{"x"}, nil)
		_, _ = svc.ListProjectLabels(ctx, j.ProjectID)
		return nil
	})
	if err := h(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	_, r := e.record(t, id)
	byName := map[string]TimelineStep{}
	for _, s := range r.Steps {
		byName[s.Name] = s
	}
	for name, st := range map[string]string{
		"GET MR labels": StepWarn, "PUT MR labels": StepWarn,
		"PUT MR labels (service token)": StepOK, "GET project labels (service token)": StepOK,
	} {
		if s, ok := byName[name]; !ok || s.Status != st {
			t.Errorf("step %q: %+v (ok=%v), want %s; steps %v", name, s, ok, st, names(r.Steps))
		}
	}
}

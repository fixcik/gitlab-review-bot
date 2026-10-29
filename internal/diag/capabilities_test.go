package diag

import (
	"context"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// capCheckout is a checkout with the optional capabilities of the real one.
type capCheckout struct{ fakeCheckout }

func (capCheckout) ExportTree(context.Context, string, int64) (string, func(), error) {
	return "/tmp/x", func() {}, nil
}
func (capCheckout) GrepWords(context.Context, string, []string, []string, int) ([]core.GrepMatch, bool, error) {
	return nil, false, nil
}
func (capCheckout) LinesChanged(context.Context, string, string, string, int, int) (bool, error) {
	return false, nil
}

type capRepos struct{}

func (capRepos) Checkout(context.Context, string, *core.MergeRequest) (core.Checkout, error) {
	return capCheckout{}, nil
}

// The recorded checkout (diagnostics are on by default) must not hide the
// real checkout's optional capabilities: /base, the migration usage search
// and outcome classification look them up.
func TestWrappedCheckoutKeepsCapabilities(t *testing.T) {
	e := newEnv(t)
	repos := e.rec.WrapRepos(capRepos{})
	var co core.Checkout
	h := e.rec.Handler(func(ctx context.Context, j core.Job) error {
		var err error
		co, err = repos.Checkout(ctx, j.ProjectPath, &core.MergeRequest{})
		return err
	})
	if err := h(context.Background(), core.Job{Kind: core.JobReview, ProjectID: 1, MRIID: 2, ProjectPath: "g/p"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := co.(capCheckout); ok {
		t.Fatal("checkout not wrapped: the test proves nothing")
	}
	if _, ok := core.CheckoutAs[core.TreeExporter](co); !ok {
		t.Error("TreeExporter hidden by the diag wrapper (/base never mounted)")
	}
	if _, ok := core.CheckoutAs[core.WordGrepper](co); !ok {
		t.Error("WordGrepper hidden by the diag wrapper")
	}
	if _, ok := co.(linesChanger); !ok {
		t.Error("LinesChanged hidden by the diag wrapper")
	}
}

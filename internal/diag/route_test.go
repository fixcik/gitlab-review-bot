package diag

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestSetRoute(t *testing.T) {
	e := newEnv(t)
	tr, sink := newTraced(t)
	var id string
	h := TraceJobs(tr, nil, e.rec.Handler(func(ctx context.Context, j core.Job) error {
		id = FromContext(ctx).ID()
		SetRoute(ctx, "light", "haiku", "низкий риск: только docs, 3 строки token="+glToken)
		return nil
	}))
	if err := h(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	_, rec := e.record(t, id)
	if rec.Route == nil || rec.Route.Route != "light" || rec.Route.Model != "haiku" || rec.Route.Reason == "" {
		t.Fatalf("route %+v", rec.Route)
	}
	if got := rec.Route.Reason; strings.Contains(got, glToken) || !strings.Contains(got, "только docs") {
		t.Fatalf("reason not scrubbed: %q", got)
	}
	sp := sink.byName(t, "review.job")
	if attr(sp, AttrReviewRoute) != "light" || attr(sp, AttrReviewModel) != "haiku" {
		t.Fatalf("span attrs route=%q model=%q", attr(sp, AttrReviewRoute), attr(sp, AttrReviewModel))
	}
	// Neither a run nor a span: nothing happens.
	SetRoute(context.Background(), "normal", "sonnet", "")
}

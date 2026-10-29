package review_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

type traceSink struct {
	mu    sync.Mutex
	spans []*tracing.SpanData
}

func (s *traceSink) Enqueue(d *tracing.SpanData) {
	s.mu.Lock()
	s.spans = append(s.spans, d)
	s.mu.Unlock()
}

// TestReviewSpanTree runs a whole auto review inside a traced job (tracing
// on, diagnostics off) and checks the span tree: analyzer spans from this
// package and the bridged GitLab/limiter/checkout/Claude spans.
func TestReviewSpanTree(t *testing.T) {
	h := reviewtest.New(t)
	h.GL.RepoConfig = "language: ru\n"
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{
		{Path: "cache.go", Line: 2, Severity: "major", Title: "Race on map", Body: "The map is accessed without a lock."},
	}, nil)}}
	hooks := diag.Hooks{Trace: true}
	r := review.New(review.Deps{
		Compat: h.Compat,
		GitLab: hooks.WrapGitLab(h.GL), Repos: hooks.WrapRepos(h.Repos), Runner: hooks.WrapRunner(h.Runner), Store: h.St,
		Limiter: hooks.WrapLimiter(h.Lim), Config: h.Cfg,
		BuildIndex:    func([]core.FileDiff) env.DiffIndex { return h.Idx },
		RepoConfigTTL: -1,
		BotUserID:     reviewtest.BotID,
		BotUsername:   reviewtest.BotName,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:           func() time.Time { return reviewtest.TestNow },
	})
	sink := &traceSink{}
	tr := tracing.New(tracing.Options{Exporter: sink})
	if err := diag.TraceJobs(tr, nil, r.Handle)(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}

	byName := map[string][]*tracing.SpanData{}
	var root *tracing.SpanData
	for _, d := range sink.spans {
		byName[d.Name] = append(byName[d.Name], d)
		if d.Name == "review.job" {
			root = d
		}
	}
	if root == nil {
		t.Fatal("no job span")
	}
	ids := map[tracing.SpanID]*tracing.SpanData{}
	for _, d := range sink.spans {
		ids[d.SpanContext.SpanID] = d
		if d.SpanContext.TraceID != root.SpanContext.TraceID {
			t.Errorf("%s is in another trace", d.Name)
		}
	}
	for _, d := range sink.spans {
		if d != root && ids[d.Parent] == nil {
			t.Errorf("%s has a parent outside the trace", d.Name)
		}
	}
	for _, name := range []string{"gitlab.get_mr", "git.checkout", "git.changed_files", "git.diff", "limiter.reserve", "review.threads",
		"review.base", "review.compat", "review.lint", "review.task", "review.context", "review.coverage", "review.migrations",
		"claude review", "review.verify", "review.diff_index", "review.publish", "review.publish.drafts", "review.summary", "review.tbd",
		"review.labels", "gitlab.create_draft_note", "gitlab.publish_draft_notes"} {
		if len(byName[name]) == 0 {
			var all []string
			for n := range byName {
				all = append(all, n)
			}
			t.Errorf("missing span %q (have %s)", name, strings.Join(all, ", "))
		}
	}
	// The analyzers run concurrently (review.parallel_analyzers) and still
	// hang under the job span, each with its own span.
	for _, name := range []string{"review.base", "review.compat", "review.lint", "review.task", "review.context", "review.coverage", "review.migrations"} {
		if d := byName[name]; len(d) != 1 || d[0].Parent != root.SpanContext.SpanID {
			t.Errorf("%s: %d spans, not under the job span", name, len(d))
		}
	}
	// Bridged steps called from an analyzer nest under it.
	if d := byName["gitlab.create_draft_note"]; len(d) > 0 && ids[d[0].Parent].Name != "review.publish.drafts" {
		t.Errorf("draft note under %q", ids[d[0].Parent].Name)
	}
	if d := byName["review.publish.drafts"]; len(d) > 0 {
		if v, _ := d[0].AttrValue("reviewbot.drafts_created"); v.Int != 1 {
			t.Errorf("drafts_created = %d", v.Int)
		}
	}
	if v, _ := root.AttrValue(diag.AttrJobResult); v.Str != diag.StatusOK {
		t.Errorf("job result %q", v.Str)
	}
}

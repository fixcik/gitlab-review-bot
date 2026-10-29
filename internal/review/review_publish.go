package review

import (
	"context"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/publish"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// publishFindings posts findings as draft notes and publishes them. It
// returns the posted findings, the ones that go to the summary, and the
// number skipped as duplicates of open threads.
// A blocking finding whose thread could not be created or published is
// moved to the summary and counted in unpublished: it still blocks the
// merge (fail closed, see reviewRun.unpublished).
// Findings at or above review.block_min_severity stay open threads (they
// block the merge when the project requires resolved threads; a blocking
// finding outside the diff becomes a general thread so it still blocks).
// Lower findings are resolved right after publishing when a service token is
// configured, so they stay visible without blocking. newBlocking is the
// number of blocking threads posted.
func (r *Reviewer) publishFindings(ctx context.Context, job core.Job, mr *core.MergeRequest, idx env.DiffIndex, findings []output.Finding, open map[string]mrthread.OpenFinding) (t reviewTally) {
	ctx, tsp := tracing.Start(ctx, "review.publish")
	defer func() {
		tsp.SetAttr(tracing.Int("reviewbot.findings", len(findings)), tracing.Int("reviewbot.published", len(t.published)),
			tracing.Int("reviewbot.outside", len(t.outside)), tracing.Int("reviewbot.skipped", t.skipped), tracing.Int("reviewbot.blocking", t.newBlocking),
			tracing.Int("reviewbot.unpublished_blocking", t.unpublished))
		tsp.End()
	}()
	seen := map[string]bool{}
	nonBlocking := map[string]bool{}
	var batch []publish.Draft
	var batched []output.Finding
	var batchBlocking []bool
	for _, f := range findings {
		id := finding.ID(f.Path, f.Title)
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := open[id]; ok {
			t.skipped++
			continue
		}
		pl := r.pub.Place(idx, mr.DiffRefs, f) // the same decision submit_finding reports
		blocking, pos := pl.Blocking, pl.Pos
		if pl.Kind == publish.Summary {
			t.outside = append(t.outside, f)
			continue
		}
		opts := finding.NoteOpts{Blocking: blocking, NonBlocking: !blocking, OutsideDiff: pos == nil}
		batch = append(batch, publish.Draft{Body: finding.NoteBodyOpts(id, f, pl.Exact && pos != nil, pl.SnappedFrom, opts), Pos: pos, What: "finding " + id})
		batched = append(batched, f)
		batchBlocking = append(batchBlocking, blocking)
	}
	created, err := r.pub.Drafts(ctx, job, batch)
	for i, f := range batched {
		if !created[i] || err != nil {
			t.outside = append(t.outside, f) // moved to the summary
			if batchBlocking[i] {
				t.unpublished++ // still blocks the merge
			}
			continue
		}
		t.published = append(t.published, f)
		if batchBlocking[i] {
			t.newBlocking++
		} else {
			nonBlocking[finding.ID(f.Path, f.Title)] = true
		}
	}
	if err == nil {
		r.pub.ResolveNonBlocking(ctx, job, nonBlocking, "finding", marker.ParseFinding)
	}
	return t
}

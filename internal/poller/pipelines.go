package poller

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Pipeline polling (review.ci with gitlab.events poll/both): finished
// pipelines of the projects of gitlab.groups become the same
// core.JobPipeline values the Pipeline Hook produces (events.Router), so
// the CI failure digest works without inbound webhooks. Per project per tick
// the pipelines updated since the project's cursor (minus Overlap) are read
// page by page (100 per page, at most pipelinePagesPerTick pages); the
// project list is refreshed every ProjectsEvery. A project seen for the
// first time is only baselined (its cursor starts now): old failures are
// never replayed.

// PipelineSource is the part of the GitLab API the pipeline poller reads.
type PipelineSource interface {
	ListGroupProjectsDetails(ctx context.Context, group string) ([]gitlab.ProjectDetails, error)
	ListPipelines(ctx context.Context, projectID int64, q gitlab.PipelineQuery) ([]gitlab.PipelineInfo, error)
}

// PipelineOptions configures a PipelinePoller.
type PipelineOptions struct {
	Source  PipelineSource
	State   events.PollState // cursors only (scope "pipelines:<project id>")
	Router  *events.Router
	Enqueue func(core.Job)
	Groups  []string
	// Interval between ticks (default 1m); ProjectsEvery: how often the
	// project lists are re-read (default 10m).
	Interval, ProjectsEvery time.Duration
	Logger                  *slog.Logger
	Now                     func() time.Time
	// Tracer: see Options.Tracer ("poller.pipelines.tick").
	Tracer *tracing.Tracer
}

// PipelinePoller polls the pipelines of the group projects.
type PipelinePoller struct {
	o   PipelineOptions
	log *slog.Logger
	now func() time.Time

	mu       sync.Mutex
	projects map[string][]gitlab.ProjectDetails // by group; a failed refresh keeps the last list
	listedAt time.Time
}

const (
	// pipelinesPerCall is the page size of one project's pipeline listing.
	pipelinesPerCall = 100
	// pipelinePagesPerTick caps the pages read per project per tick. When
	// it is hit (a burst of pipelines), the cursor still moves past the last
	// pipeline read, without the overlap: the next tick goes on from there
	// and the poller never stalls on a window it cannot read in one tick.
	pipelinePagesPerTick = 10
)

// NewPipelines returns a PipelinePoller.
func NewPipelines(o PipelineOptions) *PipelinePoller {
	p := &PipelinePoller{o: o, log: o.Logger, now: o.Now, projects: map[string][]gitlab.ProjectDetails{}}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.o.Interval <= 0 {
		p.o.Interval = time.Minute
	}
	if p.o.ProjectsEvery <= 0 {
		p.o.ProjectsEvery = 10 * time.Minute
	}
	return p
}

// Run polls until ctx is done. The first tick runs immediately.
func (p *PipelinePoller) Run(ctx context.Context) {
	p.log.Info("poller: pipelines started", "interval", p.o.Interval, "groups", p.o.Groups)
	runTicks(ctx, p.o.Interval, func(ctx context.Context) { p.Tick(ctx) })
}

// Tick polls every project once and returns the number of jobs enqueued
// and the first error.
func (p *PipelinePoller) Tick(ctx context.Context) (int, error) {
	ctx, sp := p.o.Tracer.Start(ctx, "poller.pipelines.tick", tracing.WithNewRoot())
	jobs, err := p.tick(ctx)
	endTickSpan(sp, jobs, err)
	return jobs, err
}

func (p *PipelinePoller) tick(ctx context.Context) (int, error) {
	var first error
	jobs := 0
	for _, pr := range p.projectList(ctx) {
		if !p.o.Router.Accepts(pr.PathWithNamespace) {
			continue
		}
		n, err := p.pollProject(ctx, pr)
		jobs += n
		if err != nil {
			if ctx.Err() != nil {
				return jobs, ctx.Err()
			}
			p.log.Warn("poller: pipelines of a project failed, will retry", "project", pr.PathWithNamespace, "err", err)
			if first == nil {
				first = err
			}
		}
	}
	return jobs, first
}

// projectList returns the deduplicated projects of every group, re-read
// every ProjectsEvery.
func (p *PipelinePoller) projectList(ctx context.Context) []gitlab.ProjectDetails {
	p.mu.Lock()
	stale := p.listedAt.IsZero() || p.now().Sub(p.listedAt) >= p.o.ProjectsEvery
	p.mu.Unlock()
	if stale {
		p.refreshProjects(ctx)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[int64]bool{}
	var out []gitlab.ProjectDetails
	for _, g := range p.o.Groups {
		for _, pr := range p.projects[g] {
			if pr.Archived || seen[pr.ID] || pr.DefaultBranch == "" {
				continue
			}
			seen[pr.ID] = true
			out = append(out, pr)
		}
	}
	return out
}

// refreshProjects re-reads the project list of every group; a group whose
// listing fails keeps its last list.
func (p *PipelinePoller) refreshProjects(ctx context.Context) {
	for _, g := range p.o.Groups {
		ps, err := p.o.Source.ListGroupProjectsDetails(ctx, g)
		if err != nil {
			p.log.Warn("poller: list group projects for pipelines", "group", g, "err", err)
			continue
		}
		p.mu.Lock()
		p.projects[g] = ps
		p.mu.Unlock()
	}
	p.mu.Lock()
	p.listedAt = p.now()
	p.mu.Unlock()
}

func pipelineScope(projectID int64) string { return "pipelines:" + strconv.FormatInt(projectID, 10) }

func (p *PipelinePoller) pollProject(ctx context.Context, pr gitlab.ProjectDetails) (int, error) {
	scope := pipelineScope(pr.ID)
	cursor, ok, err := p.o.State.GetPollCursor(ctx, scope)
	if err != nil {
		return 0, err
	}
	if !ok {
		// First sight: start from now, never replay history.
		return 0, p.o.State.SetPollCursor(ctx, scope, p.now().Add(-Overlap))
	}
	ps, capped, err := p.listPipelines(ctx, pr.ID, cursor)
	if err != nil {
		return 0, err
	}
	next := cursor
	jobs := 0
	for _, pl := range ps {
		if pl.UpdatedAt.After(next) {
			next = pl.UpdatedAt
		}
		job, why := p.o.Router.Pipeline(events.PipelineEvent{ProjectID: pr.ID, ProjectPath: pr.PathWithNamespace,
			DefaultBranch: pr.DefaultBranch, PipelineID: pl.ID, Ref: pl.Ref, SHA: pl.SHA, Status: pl.Status,
			Source: pl.Source, At: pl.UpdatedAt, MRIID: gitlab.MRIIDFromRef(pl.Ref)})
		if job == nil {
			p.log.Debug("poller: pipeline ignored", "project", pr.PathWithNamespace, "pipeline", pl.ID, "reason", why)
			continue
		}
		p.log.Info("poller: job accepted", "kind", job.Kind, "mr", job.MRKey(), "pipeline", pl.ID, "status", pl.Status)
		job.TraceParent = tracing.TraceParent(ctx)
		if p.o.Enqueue != nil {
			p.o.Enqueue(*job)
		}
		jobs++
	}
	if capped { // capped: pipelinePagesPerTick full pages were read, ps is not empty
		// More pipelines than one tick reads: the next query (cursor -
		// Overlap) starts right after the last one read. Pipelines updated
		// at that very instant but not read yet are the price of never
		// stalling.
		last := ps[len(ps)-1].UpdatedAt.Add(Overlap)
		if !last.After(cursor) {
			last = cursor.Add(time.Second)
		}
		p.log.Warn("poller: too many pipelines for one tick, moving on", "project", pr.PathWithNamespace,
			"read", len(ps), "cursor", last)
		next = last
	}
	if next.After(cursor) {
		if err := p.o.State.SetPollCursor(ctx, scope, next); err != nil {
			return jobs, err
		}
	}
	return jobs, nil
}

// listPipelines reads the pipelines of a project updated since cursor (minus
// Overlap), oldest update first; capped reports that pipelinePagesPerTick
// full pages were read and more may follow.
func (p *PipelinePoller) listPipelines(ctx context.Context, projectID int64, cursor time.Time) (ps []gitlab.PipelineInfo, capped bool, err error) {
	for page := 1; ; page++ {
		batch, err := p.o.Source.ListPipelines(ctx, projectID, gitlab.PipelineQuery{UpdatedAfter: cursor.Add(-Overlap),
			OrderBy: "updated_at", Sort: "asc", Limit: pipelinesPerCall, Page: page})
		if err != nil {
			return nil, false, err
		}
		ps = append(ps, batch...)
		if len(batch) < pipelinesPerCall {
			return ps, false, nil
		}
		if page == pipelinePagesPerTick {
			return ps, true, nil
		}
	}
}

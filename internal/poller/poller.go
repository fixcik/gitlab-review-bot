// Package poller is the alternative to webhooks: it asks the GitLab API for
// recently updated MRs every interval and turns what changed (new MR,
// reopen, push, Draft → Ready, title change, new comment) into the same
// core.Job values the webhook handler produces, through events.Router.
//
// One API call per group per tick finds the updated MRs (GitLab bumps
// updated_at on pushes, state/title/draft changes and new comments, a comment
// at most once a minute); only for those the discussions are read to find new
// notes, and for HotWindow after the last bump on every tick. The last seen state
// of every MR and a cursor per group are kept in the store, so a restart
// neither replays old events nor loses new ones.
package poller

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Overlap is subtracted from the cursor on every query, so an MR updated
// while the previous query ran, or a small clock skew, is never missed.
// MRs seen again inside the overlap are skipped by their updated_at.
const Overlap = 2 * time.Minute

// HotWindow is how long after an MR's updated_at its discussions are read on
// every tick even when updated_at did not move. GitLab (19.4 at least) bumps
// updated_at for a comment only if the MR was not updated in the last minute,
// so a command written right after a push, or after the bot's own note, would
// stay invisible until the next bump (or forever). It must stay within
// Overlap: the MR is listed only while the cursor's overlap covers it.
const HotWindow = Overlap

// CatchUp is how far back the first poll of a process looks: the MRs updated
// within it have their discussions read once even if updated_at did not move
// since the last run, because a comment written while the bot was down may
// have been throttled (see HotWindow) and the MR has cooled off by now.
const CatchUp = time.Hour

// Source is the part of the GitLab API the poller reads.
type Source interface {
	ListUpdatedMRs(ctx context.Context, group string, since time.Time) ([]gitlab.MRSummary, error)
	ListDiscussions(ctx context.Context, projectID, mrIID int64) ([]core.Discussion, error)
}

// Options configures a Poller.
type Options struct {
	Source   Source
	State    events.PollState
	Router   *events.Router
	Enqueue  func(core.Job)
	Groups   []string // empty = every MR the bot token can see
	Interval time.Duration
	Logger   *slog.Logger
	Now      func() time.Time
	// Observe, when set, is called after every tick with its duration, the
	// number of jobs enqueued and the first error (metrics).
	Observe func(d time.Duration, jobs int, err error)
	// Tracer, when set, makes every tick a short trace ("poller.tick") and
	// the jobs it enqueues link to it (core.Job.TraceParent).
	Tracer *tracing.Tracer
}

// Poller polls GitLab for MR events.
type Poller struct {
	o   Options
	log *slog.Logger
	now func() time.Time
	// caughtUp: scopes whose first poll of this process (CatchUp) is done.
	caughtUp map[string]bool
}

// New returns a Poller.
func New(o Options) *Poller {
	p := &Poller{o: o, log: o.Logger, now: o.Now, caughtUp: map[string]bool{}}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.o.Interval <= 0 {
		p.o.Interval = 30 * time.Second
	}
	return p
}

// Run polls until ctx is done. The first tick runs immediately.
func (p *Poller) Run(ctx context.Context) {
	p.log.Info("poller: started", "interval", p.o.Interval, "groups", p.o.Groups)
	runTicks(ctx, p.o.Interval, func(ctx context.Context) { p.Tick(ctx) })
}

// runTicks calls tick at once and then every interval until ctx is done.
func runTicks(ctx context.Context, interval time.Duration, tick func(context.Context)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick polls every scope once and returns the number of jobs enqueued and
// the first error.
func (p *Poller) Tick(ctx context.Context) (int, error) {
	ctx, sp := p.o.Tracer.Start(ctx, "poller.tick", tracing.WithNewRoot(),
		tracing.WithAttrs(tracing.Int("reviewbot.poll.scopes", max(len(p.o.Groups), 1))))
	jobs, err := p.tick(ctx)
	endTickSpan(sp, jobs, err)
	return jobs, err
}

// endTickSpan finishes the span of a poll round.
func endTickSpan(sp *tracing.Span, jobs int, err error) {
	sp.SetAttr(tracing.Int("reviewbot.poll.jobs", jobs))
	if err != nil {
		sp.RecordError(err, "poll")
	} else {
		sp.SetStatus(tracing.StatusOK, "")
	}
	sp.End()
}

func (p *Poller) tick(ctx context.Context) (int, error) {
	start := p.now()
	scopes := p.o.Groups
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	jobs := 0
	var first error
	for _, s := range scopes {
		n, err := p.pollScope(ctx, s)
		jobs += n
		if err != nil {
			if ctx.Err() != nil {
				return jobs, ctx.Err()
			}
			p.log.Warn("poller: poll failed", "group", s, "err", err)
			if first == nil {
				first = err
			}
		}
	}
	if p.o.Observe != nil {
		p.o.Observe(p.now().Sub(start), jobs, first)
	}
	return jobs, first
}

func (p *Poller) pollScope(ctx context.Context, scope string) (int, error) {
	cursor, ok, err := p.o.State.GetPollCursor(ctx, scope)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, p.bootstrap(ctx, scope)
	}
	window, rescan := Overlap, !p.caughtUp[scope]
	if rescan {
		window = CatchUp
	}
	mrs, err := p.o.Source.ListUpdatedMRs(ctx, scope, cursor.Add(-window))
	if err != nil {
		return 0, err
	}
	next := cursor
	var failedAt time.Time // earliest updated_at of an MR that failed
	jobs := 0
	for _, m := range mrs {
		n, err := p.processMR(ctx, m, cursor, rescan)
		jobs += n
		if err != nil {
			if ctx.Err() != nil {
				return jobs, ctx.Err()
			}
			p.log.Warn("poller: MR failed, will retry", "mr", mrKey(m), "err", err)
			if failedAt.IsZero() || m.UpdatedAt.Before(failedAt) {
				failedAt = m.UpdatedAt
			}
			continue
		}
		if m.UpdatedAt.After(next) {
			next = m.UpdatedAt
		}
	}
	// Never move past an MR that failed: it is retried on the next tick.
	if !failedAt.IsZero() && failedAt.Before(next) {
		next = failedAt
	}
	if next.After(cursor) {
		if err := p.o.State.SetPollCursor(ctx, scope, next); err != nil {
			return jobs, err
		}
	}
	if failedAt.IsZero() {
		p.caughtUp[scope] = true
	}
	return jobs, nil
}

// bootstrap records every open MR as it is now, without enqueuing anything:
// the poller starts from the present, it does not review the backlog.
func (p *Poller) bootstrap(ctx context.Context, scope string) error {
	mrs, err := p.o.Source.ListUpdatedMRs(ctx, scope, time.Time{})
	if err != nil {
		return err
	}
	now := p.now()
	cursor := now.Add(-Overlap)
	for _, m := range mrs {
		if err := p.o.State.SavePollMR(ctx, recordOf(m, 0, now)); err != nil {
			return err
		}
		if m.UpdatedAt.After(cursor) {
			cursor = m.UpdatedAt
		}
	}
	p.log.Info("poller: baseline recorded", "group", scope, "open_mrs", len(mrs))
	return p.o.State.SetPollCursor(ctx, scope, cursor)
}

func recordOf(m gitlab.MRSummary, lastNote int64, notesSince time.Time) events.PollMR {
	return events.PollMR{ProjectID: m.ProjectID, IID: m.IID, State: m.State, Draft: m.Draft, SHA: m.SHA, Title: m.Title,
		UpdatedAt: m.UpdatedAt, LastNoteID: lastNote, NotesSince: notesSince}
}

func mrKey(m gitlab.MRSummary) string {
	return m.ProjectPath + "!" + strconv.FormatInt(m.IID, 10)
}

// processMR compares m with the stored record, enqueues the MR job and the
// note jobs, and saves the new record. cursor is the scope's cursor before
// this tick; rescan reads the discussions even of an unchanged, cold MR (the
// first poll of the process).
func (p *Poller) processMR(ctx context.Context, m gitlab.MRSummary, cursor time.Time, rescan bool) (int, error) {
	if !p.o.Router.Accepts(m.ProjectPath) {
		return 0, nil // excluded or outside the groups: not even its notes are read
	}
	prev, err := p.o.State.GetPollMR(ctx, m.ProjectID, m.IID)
	if err != nil {
		return 0, err
	}
	if prev != nil && prev.UpdatedAt.Equal(m.UpdatedAt) && !rescan && p.now().Sub(m.UpdatedAt) >= HotWindow {
		return 0, nil // seen again inside the overlap, and quiet
	}

	var (
		mrJobs     []core.Job
		notesSince time.Time
		lastNote   int64
	)
	if prev == nil {
		// Unknown MR: created (or reopened, or moved into the group) since
		// the poller started. Its notes are new unless it is old.
		if m.CreatedAt.Before(cursor.Add(-Overlap)) {
			notesSince = cursor.Add(-Overlap)
		}
		if m.State == "opened" {
			mrJobs = p.mrJob(events.MREvent{Action: "open"}, m)
		}
	} else {
		notesSince, lastNote = prev.NotesSince, prev.LastNoteID
		mrJobs = p.mrJob(diff(prev, m), m)
	}

	disc, err := p.o.Source.ListDiscussions(ctx, m.ProjectID, m.IID)
	if err != nil {
		return 0, err
	}
	noteJobs, maxNote := p.noteJobs(m, disc, lastNote, notesSince)

	jobs := 0
	for _, j := range append(mrJobs, noteJobs...) {
		p.log.Info("poller: job accepted", "kind", j.Kind, "mr", j.MRKey(), "user", j.Username)
		j.TraceParent = tracing.TraceParent(ctx) // the job's trace links to this round
		if p.o.Enqueue != nil {
			p.o.Enqueue(j)
		}
		jobs++
	}
	return jobs, p.o.State.SavePollMR(ctx, recordOf(m, maxNote, notesSince))
}

// diff turns the change between the stored record and the current MR into
// the webhook event GitLab would have sent ("Action" empty = nothing to do
// for the MR itself).
func diff(prev *events.PollMR, m gitlab.MRSummary) events.MREvent {
	var e events.MREvent
	switch {
	case m.State != "opened":
		return e // closed/merged: nothing to review
	case prev.State != "opened":
		e.Action = "reopen"
		return e
	}
	e.Action = "update"
	if prev.Draft && !m.Draft {
		e.BecameReady = true
	}
	if prev.SHA != "" && m.SHA != "" && prev.SHA != m.SHA {
		e.OldRev = prev.SHA
	}
	if prev.Title != m.Title {
		e.OldTitle, e.NewTitle = prev.Title, m.Title
	}
	if !e.BecameReady && e.OldRev == "" && e.OldTitle == e.NewTitle {
		e.Action = ""
	}
	return e
}

func (p *Poller) mrJob(e events.MREvent, m gitlab.MRSummary) []core.Job {
	if e.Action == "" {
		return nil
	}
	e.ProjectID, e.ProjectPath, e.IID, e.State, e.Draft, e.HeadSHA = m.ProjectID, m.ProjectPath, m.IID, m.State, m.Draft, m.SHA
	job, why := p.o.Router.MR(e)
	if job == nil {
		p.log.Debug("poller: MR event ignored", "mr", mrKey(m), "action", e.Action, "reason", why)
		return nil
	}
	return []core.Job{*job}
}

type flatNote struct {
	n        core.Note
	discID   string
	threaded bool
}

// noteJobs returns the jobs for notes newer than lastNote (and not older
// than since), oldest first, and the highest note ID seen.
func (p *Poller) noteJobs(m gitlab.MRSummary, disc []core.Discussion, lastNote int64, since time.Time) ([]core.Job, int64) {
	maxID := lastNote
	var fresh []flatNote
	for _, d := range disc {
		for _, n := range d.Notes {
			if n.ID > maxID {
				maxID = n.ID
			}
			if n.ID <= lastNote || (!since.IsZero() && n.CreatedAt.Before(since)) {
				continue
			}
			fresh = append(fresh, flatNote{n: n, discID: d.ID, threaded: !d.Individual})
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].n.ID < fresh[j].n.ID })
	var jobs []core.Job
	for _, f := range fresh {
		job, why := p.o.Router.Note(events.NoteEvent{
			ProjectID: m.ProjectID, ProjectPath: m.ProjectPath, IID: m.IID,
			NoteID: f.n.ID, DiscussionID: f.discID, Body: f.n.Body, System: f.n.System, Threaded: f.threaded,
			UserID: f.n.AuthorID, Username: f.n.AuthorUsername,
		})
		if job == nil {
			p.log.Debug("poller: note ignored", "mr", mrKey(m), "note", f.n.ID, "reason", why)
			continue
		}
		jobs = append(jobs, *job)
	}
	return jobs, maxID
}

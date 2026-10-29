// Package jobctx holds the per-job state the review features keep in the
// job's context.Context: bot problems (and the per-step buffer of the
// parallel analyzers), token attribution, the files Claude read, the
// "bypass the GitLab memo" flag and the reaction marks of the job's note.
// It is a leaf: it must not depend on internal/review or any of its feature
// packages.
package jobctx

import (
	"context"
	"sync"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
)

// ---------------------------------------------------------------------------
// Bot problems
// ---------------------------------------------------------------------------

// BotProblem records a problem of the bot's own steps.
func BotProblem(ctx context.Context, kind problems.Kind, subject, detail string) {
	p := problems.Problem{Kind: kind, Source: problems.SourceBot, Subject: subject, Detail: detail}
	if bufferProblem(ctx, p) { // a parallel analyzer step
		return
	}
	problems.Add(ctx, p)
}

// ProblemBuf holds the bot problems of one step until the group flushes.
type ProblemBuf struct {
	mu sync.Mutex
	ps []problems.Problem
}

type problemBufKey struct{}

// WithProblemBuf makes BotProblem keep the problems of ctx in b.
func WithProblemBuf(ctx context.Context, b *ProblemBuf) context.Context {
	return context.WithValue(ctx, problemBufKey{}, b)
}

// bufferProblem keeps p for the step's flush; false = no step buffer in ctx.
func bufferProblem(ctx context.Context, p problems.Problem) bool {
	b, _ := ctx.Value(problemBufKey{}).(*ProblemBuf)
	if b == nil {
		return false
	}
	b.mu.Lock()
	b.ps = append(b.ps, p)
	b.mu.Unlock()
	return true
}

// Flush records the buffered problems in ctx.
func (b *ProblemBuf) Flush(ctx context.Context) {
	b.mu.Lock()
	ps := b.ps
	b.ps = nil
	b.mu.Unlock()
	for _, p := range ps {
		problems.Add(ctx, p) // ctx: the group's, without the buffer
	}
}

// ---------------------------------------------------------------------------
// Token accounting (internal/meter)
// ---------------------------------------------------------------------------

// Every Claude run made while handling a job is charged to the attribution
// put into the context here:
//   - manual jobs (commands, chat, learning) -> the user who issued them;
//   - auto reviews -> the MR author, filled in once the MR is fetched.

// AttributeJob returns ctx carrying the job's attribution.
func AttributeJob(ctx context.Context, job core.Job) context.Context {
	return meter.With(ctx, meter.ForJob(job))
}

// AttributeMR enriches the attribution with the MR title and, for auto
// reviews, charges the run to the MR author.
func AttributeMR(ctx context.Context, job core.Job, mr *core.MergeRequest) {
	meter.Update(ctx, func(a *meter.Attribution) {
		a.MRTitle = mr.Title
		if job.Kind == core.JobAutoReview {
			a.Username, a.UserID = mr.AuthorUsername, 0
		}
	})
}

// AttributeKind sets the run kind for the following runs (e.g.
// meter.KindVerification before a verification run).
func AttributeKind(ctx context.Context, kind string) {
	meter.Update(ctx, func(a *meter.Attribution) { a.Kind = kind })
}

// ---------------------------------------------------------------------------
// Files Claude read (skills)
// ---------------------------------------------------------------------------

// Reads gathers RunResult.ReadFiles of the runs made with a ctx from
// WithReads (the structured Claude run adds them).
type Reads struct {
	mu    sync.Mutex
	paths []string
}

type readsKey struct{}

// WithReads makes the runs of ctx record the files they read in c.
func WithReads(ctx context.Context, c *Reads) context.Context {
	return context.WithValue(ctx, readsKey{}, c)
}

func (c *Reads) add(paths []string) {
	c.mu.Lock()
	c.paths = append(c.paths, paths...)
	c.mu.Unlock()
}

// Files are the files read so far.
func (c *Reads) Files() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.paths...)
}

// CollectReads records the files a run read, when ctx asks for it.
func CollectReads(ctx context.Context, res *core.RunResult) {
	if res == nil {
		return
	}
	if c, ok := ctx.Value(readsKey{}).(*Reads); ok && c != nil {
		c.add(res.ReadFiles)
	}
}

// ---------------------------------------------------------------------------
// GitLab memo
// ---------------------------------------------------------------------------

type freshKey struct{}

// WithFreshGitLab makes the reads in ctx bypass the job's memo of GitLab
// reads (the result replaces the memoized one).
func WithFreshGitLab(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshKey{}, true)
}

// FreshGitLab reports whether ctx asks for real GitLab reads.
func FreshGitLab(ctx context.Context) bool {
	fresh, _ := ctx.Value(freshKey{}).(bool)
	return fresh
}

// ---------------------------------------------------------------------------
// Reactions on the job's note
// ---------------------------------------------------------------------------

// Marks are the outcomes of a job its note's reactions show beyond done /
// failed: refused (🚫) and a team rule learned (🧠).
type Marks struct {
	mu      sync.Mutex
	denied  bool
	learned bool
	noted   bool
}

type marksKey struct{}

// WithMarks puts m into ctx (the job's reaction state).
func WithMarks(ctx context.Context, m *Marks) context.Context {
	return context.WithValue(ctx, marksKey{}, m)
}

func marksFrom(ctx context.Context) *Marks {
	m, _ := ctx.Value(marksKey{}).(*Marks)
	return m
}

// Denied marks the job as refused (no access, a per-user limit): its note
// gets 🚫 instead of ✅.
func Denied(ctx context.Context) {
	if m := marksFrom(ctx); m != nil {
		m.mu.Lock()
		m.denied = true
		m.mu.Unlock()
	}
}

// Learned marks that a team rule was learned from the job's note (🧠).
func Learned(ctx context.Context) {
	if m := marksFrom(ctx); m != nil {
		m.mu.Lock()
		m.learned = true
		m.mu.Unlock()
	}
}

// Noted marks that the job's note, a reply in a bot thread, was read and led
// to nothing (👍 stays on it).
func Noted(ctx context.Context) {
	if m := marksFrom(ctx); m != nil {
		m.mu.Lock()
		m.noted = true
		m.mu.Unlock()
	}
}

// IsNoted reports whether Noted was called (false on nil).
func (m *Marks) IsNoted() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.noted
}

// Get returns the marks (zero on nil).
func (m *Marks) Get() (denied, learned bool) {
	if m == nil {
		return false, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.denied, m.learned
}

package diag

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// The decorators below embed the interface they wrap, so a method added to
// core.GitLab / core.Limiter / core.Repos / core.Checkout later is forwarded
// automatically (unrecorded) instead of breaking the build. Only the calls
// worth seeing in a timeline are overridden. Note bodies are never stored:
// only ids, counts and sizes.
//
// Each overridden call is a step (startStep): a timeline entry of the
// job's Run and, for a traced job, a span whose context the wrapped call
// receives (so the GitLab client's HTTP spans nest under it).

// Hooks selects what the decorators feed: the diagnostics Recorder (nil =
// diagnostics off) and/or spans of the job's trace (Trace = tracing on).
// With both off every Wrap* returns its argument unchanged.
type Hooks struct {
	Rec   *Recorder
	Trace bool
}

func (h Hooks) off() bool { return h.Rec == nil && !h.Trace }

// GitLab wraps core.GitLab.
type GitLab struct {
	core.GitLab
}

// Unwrap returns the wrapped client, so optional capabilities of the real
// client (CI artifacts, open MR lists, …) stay reachable through
// core.GitLabAs even though this wrapper does not implement them.
func (g *GitLab) Unwrap() core.GitLab { return g.GitLab }

// WrapGitLab returns gl recorded into the Run in each call's context.
func (rec *Recorder) WrapGitLab(gl core.GitLab) core.GitLab { return Hooks{Rec: rec}.WrapGitLab(gl) }

// WrapGitLab returns gl with its calls recorded (unchanged when off).
func (h Hooks) WrapGitLab(gl core.GitLab) core.GitLab {
	if h.off() || gl == nil {
		return gl
	}
	return &GitLab{GitLab: gl}
}

func gitlabStep(ctx context.Context, op, name string, f Fields) (context.Context, *pending) {
	return startStep(ctx, "gitlab."+op, KindGitLab, name, f)
}

// notFoundOK ends a step where ErrNotFound is an answer, not a failure.
func notFoundOK(p *pending, err error, f Fields) {
	if errors.Is(err, core.ErrNotFound) {
		p.end(StepInfo, "не найдено (404)", f)
		return
	}
	p.done(err, f)
}

func (g *GitLab) GetMR(ctx context.Context, projectID, mrIID int64) (*core.MergeRequest, error) {
	sctx, p := gitlabStep(ctx, "get_mr", "GET merge request", nil)
	mr, err := g.GitLab.GetMR(sctx, projectID, mrIID)
	var f Fields
	if mr != nil {
		FromContext(ctx).setMR(mr)
		setMRSpan(ctx, mr)
		f = Fields{"state": mr.State, "draft": mr.Draft, "head_sha": mr.DiffRefs.HeadSHA, "base_sha": mr.DiffRefs.BaseSHA,
			"start_sha": mr.DiffRefs.StartSHA, "source_branch": mr.SourceBranch, "target_branch": mr.TargetBranch,
			"author": mr.AuthorUsername}
	}
	p.done(err, f)
	return mr, err
}

func (g *GitLab) GetMRDiffs(ctx context.Context, projectID, mrIID int64) ([]core.FileDiff, error) {
	sctx, p := gitlabStep(ctx, "get_mr_diffs", "GET diffs", nil)
	ds, err := g.GitLab.GetMRDiffs(sctx, projectID, mrIID)
	bytes := 0
	for _, d := range ds {
		bytes += len(d.Diff)
	}
	p.done(err, Fields{"files": len(ds), "diff_bytes": bytes})
	return ds, err
}

func (g *GitLab) ListDiscussions(ctx context.Context, projectID, mrIID int64) ([]core.Discussion, error) {
	sctx, p := gitlabStep(ctx, "list_discussions", "GET discussions", nil)
	ds, err := g.GitLab.ListDiscussions(sctx, projectID, mrIID)
	open, notes := 0, 0
	for _, d := range ds {
		notes += len(d.Notes)
		if d.Resolvable && !d.Resolved {
			open++
		}
	}
	p.done(err, Fields{"discussions": len(ds), "unresolved": open, "notes": notes})
	return ds, err
}

func (g *GitLab) GetDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string) (*core.Discussion, error) {
	sctx, p := gitlabStep(ctx, "get_discussion", "GET discussion", Fields{"discussion_id": discussionID})
	d, err := g.GitLab.GetDiscussion(sctx, projectID, mrIID, discussionID)
	var f Fields
	if d != nil {
		f = Fields{"notes": len(d.Notes), "resolved": d.Resolved}
	}
	notFoundOK(p, err, f)
	return d, err
}

func (g *GitLab) AccessLevel(ctx context.Context, projectID, userID int64) (int, error) {
	sctx, p := gitlabStep(ctx, "access_level", "GET access level", Fields{"user_id": userID})
	lvl, err := g.GitLab.AccessLevel(sctx, projectID, userID)
	p.done(err, Fields{"level": lvl})
	return lvl, err
}

func (g *GitLab) GetRawFile(ctx context.Context, projectID int64, path, ref string) ([]byte, error) {
	sctx, p := gitlabStep(ctx, "get_raw_file", "GET file "+path, Fields{"path": path, "ref": ref})
	b, err := g.GitLab.GetRawFile(sctx, projectID, path, ref)
	notFoundOK(p, err, Fields{"bytes": len(b)})
	return b, err
}

func (g *GitLab) ListMRCommits(ctx context.Context, projectID, mrIID int64) ([]core.Commit, error) {
	sctx, p := gitlabStep(ctx, "list_mr_commits", "GET commits", nil)
	cs, err := g.GitLab.ListMRCommits(sctx, projectID, mrIID)
	run := FromContext(ctx)
	for _, c := range cs {
		run.addSafe(c.SHA)
	}
	p.done(err, Fields{"commits": len(cs)})
	return cs, err
}

func (g *GitLab) CreateDraftNote(ctx context.Context, projectID, mrIID int64, body string, pos *core.Position) (int64, error) {
	f := Fields{"body_bytes": len(body), "marker": noteMarker(body)}
	if pos != nil {
		f["position"] = fmt.Sprintf("%s:%d", cmp.Or(pos.NewPath, pos.OldPath), max(pos.NewLine, pos.OldLine))
	}
	sctx, p := gitlabStep(ctx, "create_draft_note", "POST draft note", f)
	id, err := g.GitLab.CreateDraftNote(sctx, projectID, mrIID, body, pos)
	p.done(err, Fields{"draft_note_id": id})
	return id, err
}

func (g *GitLab) PublishDraftNotes(ctx context.Context, projectID, mrIID int64) error {
	sctx, p := gitlabStep(ctx, "publish_draft_notes", "POST publish draft notes", nil)
	err := g.GitLab.PublishDraftNotes(sctx, projectID, mrIID)
	p.done(err, nil)
	return err
}

func (g *GitLab) DeleteDraftNotes(ctx context.Context, projectID, mrIID int64) error {
	sctx, p := gitlabStep(ctx, "delete_draft_notes", "DELETE leftover draft notes", nil)
	err := g.GitLab.DeleteDraftNotes(sctx, projectID, mrIID)
	p.done(err, nil)
	return err
}

func (g *GitLab) CreateNote(ctx context.Context, projectID, mrIID int64, body string) (int64, error) {
	sctx, p := gitlabStep(ctx, "create_note", "POST note", Fields{"body_bytes": len(body), "marker": noteMarker(body)})
	id, err := g.GitLab.CreateNote(sctx, projectID, mrIID, body)
	p.done(err, Fields{"note_id": id})
	return id, err
}

func (g *GitLab) UpdateNote(ctx context.Context, projectID, mrIID, noteID int64, body string) error {
	sctx, p := gitlabStep(ctx, "update_note", "PUT note", Fields{"note_id": noteID, "body_bytes": len(body), "marker": noteMarker(body)})
	err := g.GitLab.UpdateNote(sctx, projectID, mrIID, noteID, body)
	notFoundOK(p, err, nil)
	return err
}

func (g *GitLab) ReplyToDiscussion(ctx context.Context, projectID, mrIID int64, discussionID, body string) error {
	sctx, p := gitlabStep(ctx, "reply", "POST reply", Fields{"discussion_id": discussionID, "body_bytes": len(body), "marker": noteMarker(body)})
	err := g.GitLab.ReplyToDiscussion(sctx, projectID, mrIID, discussionID, body)
	p.done(err, nil)
	return err
}

func (g *GitLab) ResolveDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string, resolved bool) error {
	return resolveStep(ctx, "resolve_discussion", "PUT resolve discussion (bot token)", discussionID, resolved, func(ctx context.Context) error {
		return g.GitLab.ResolveDiscussion(ctx, projectID, mrIID, discussionID, resolved)
	})
}

func resolveStep(ctx context.Context, op, name, discussionID string, resolved bool, call func(context.Context) error) error {
	sctx, p := gitlabStep(ctx, op, name, Fields{"discussion_id": discussionID, "resolved": resolved})
	err := call(sctx)
	if errors.Is(err, core.ErrForbidden) {
		p.end(StepWarn, "403: нет прав (Reporter)", nil)
		return err
	}
	p.done(err, nil)
	return err
}

// noteMarker returns the reviewbot marker kind of a note body
// ("summary", "finding", "compat", "fixed", ...), "" if none.
func noteMarker(body string) string {
	i := strings.Index(body, "<!-- reviewbot:")
	if i < 0 {
		return ""
	}
	rest := body[i+len("<!-- reviewbot:"):]
	end := strings.IndexAny(rest, " ->")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

// Moderator is the service-token client interface of env.Moderator (package review/env).
type Moderator interface {
	ResolveDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string, resolved bool) error
	AddReviewer(ctx context.Context, projectID, mrIID, userID int64) (bool, error)
}

type moderator struct {
	Moderator
}

// WrapModerator returns m recorded (nil stays nil).
func (rec *Recorder) WrapModerator(m Moderator) Moderator { return Hooks{Rec: rec}.WrapModerator(m) }

// WrapModerator returns m recorded (nil stays nil; unchanged when off).
func (h Hooks) WrapModerator(m Moderator) Moderator {
	if h.off() || m == nil {
		return m
	}
	return &moderator{Moderator: m}
}

func (m *moderator) ResolveDiscussion(ctx context.Context, projectID, mrIID int64, discussionID string, resolved bool) error {
	return resolveStep(ctx, "resolve_discussion_service", "PUT resolve discussion (service token)", discussionID, resolved, func(ctx context.Context) error {
		return m.Moderator.ResolveDiscussion(ctx, projectID, mrIID, discussionID, resolved)
	})
}

func (m *moderator) AddReviewer(ctx context.Context, projectID, mrIID, userID int64) (bool, error) {
	sctx, p := gitlabStep(ctx, "add_reviewer", "PUT add bot as reviewer (service token)", Fields{"user_id": userID})
	added, err := m.Moderator.AddReviewer(sctx, projectID, mrIID, userID)
	p.done(err, Fields{"added": added})
	return added, err
}

// Labels is the MR label client interface of review/labels.Client
// (the bot's own client and the service-token one).
type Labels interface {
	MRLabels(ctx context.Context, projectID, mrIID int64) (state string, labels []string, err error)
	UpdateMRLabels(ctx context.Context, projectID, mrIID int64, add, remove []string) ([]string, error)
	ListProjectLabels(ctx context.Context, projectID int64) ([]string, error)
}

type labelsClient struct {
	Labels
	who string // "" (bot token) or " (service token)"
}

// WrapLabels returns c with its calls recorded (nil stays nil; unchanged
// when off). service marks the service-token client in step names. A 403
// is a warning, as for resolving threads: the bot's Reporter token gets it,
// and the labels feature then retries with the service token.
func (h Hooks) WrapLabels(c Labels, service bool) Labels {
	if h.off() || c == nil {
		return c
	}
	l := &labelsClient{Labels: c}
	if service {
		l.who = " (service token)"
	}
	return l
}

func (l *labelsClient) op(op string) string {
	if l.who != "" {
		return op + "_service"
	}
	return op
}

// forbiddenWarn ends a step where ErrForbidden is an expected answer.
func forbiddenWarn(p *pending, err error, f Fields) {
	if errors.Is(err, core.ErrForbidden) {
		p.end(StepWarn, "403: нет прав (Reporter)", nil)
		return
	}
	p.done(err, f)
}

func (l *labelsClient) MRLabels(ctx context.Context, projectID, mrIID int64) (string, []string, error) {
	sctx, p := gitlabStep(ctx, l.op("mr_labels"), "GET MR labels"+l.who, nil)
	state, labels, err := l.Labels.MRLabels(sctx, projectID, mrIID)
	forbiddenWarn(p, err, Fields{"labels": len(labels)})
	return state, labels, err
}

func (l *labelsClient) UpdateMRLabels(ctx context.Context, projectID, mrIID int64, add, remove []string) ([]string, error) {
	sctx, p := gitlabStep(ctx, l.op("update_mr_labels"), "PUT MR labels"+l.who, Fields{"add": add, "remove": remove})
	labels, err := l.Labels.UpdateMRLabels(sctx, projectID, mrIID, add, remove)
	forbiddenWarn(p, err, Fields{"labels": len(labels)})
	return labels, err
}

func (l *labelsClient) ListProjectLabels(ctx context.Context, projectID int64) ([]string, error) {
	sctx, p := gitlabStep(ctx, l.op("list_project_labels"), "GET project labels"+l.who, nil)
	labels, err := l.Labels.ListProjectLabels(sctx, projectID)
	forbiddenWarn(p, err, Fields{"labels": len(labels)})
	return labels, err
}

// ---------------------------------------------------------------------------
// Limiter
// ---------------------------------------------------------------------------

type limiter struct {
	core.Limiter
}

// WrapLimiter returns l with its decisions recorded.
func (rec *Recorder) WrapLimiter(l core.Limiter) core.Limiter { return Hooks{Rec: rec}.WrapLimiter(l) }

// WrapLimiter returns l with its decisions recorded (unchanged when off).
func (h Hooks) WrapLimiter(l core.Limiter) core.Limiter {
	if h.off() || l == nil {
		return l
	}
	return &limiter{Limiter: l}
}

func decisionEnd(p *pending, dec core.Decision, err error) {
	if err != nil {
		p.done(err, nil)
		return
	}
	p.spanAttrs(tracing.Bool("reviewbot.limiter.allowed", dec.Allowed))
	if dec.Allowed {
		p.end(StepOK, "разрешено", nil)
		return
	}
	g := Fields{"reason": dec.Reason}
	if !dec.RetryAt.IsZero() {
		g["retry_at"] = dec.RetryAt
	}
	p.end(StepDenied, "отказ: "+dec.Reason, g)
}

func (l *limiter) CheckUser(ctx context.Context, userID int64, username string, kind core.UsageKind, mrKey string, lim core.UserLimits) (core.Decision, error) {
	sctx, p := startStep(ctx, "limiter.check_user", KindLimiter, "лимит пользователя ("+string(kind)+")", Fields{"user": username, "kind": string(kind),
		"limits": fmt.Sprintf("reruns/h=%d chat/h=%d cooldown=%s", lim.RerunsPerHour, lim.ChatPerHour, lim.MRRerunCooldown)})
	dec, err := l.Limiter.CheckUser(sctx, userID, username, kind, mrKey, lim)
	decisionEnd(p, dec, err)
	return dec, err
}

func (l *limiter) CheckGlobal(ctx context.Context) (core.Decision, error) {
	sctx, p := startStep(ctx, "limiter.check_global", KindLimiter, "глобальный лимит", nil)
	dec, err := l.Limiter.CheckGlobal(sctx)
	decisionEnd(p, dec, err)
	return dec, err
}

func (l *limiter) Reserve(ctx context.Context, req core.ReserveRequest) (core.Decision, core.Reservation, error) {
	scope := "глобальный"
	if req.User != nil {
		scope = "пользователь + глобальный"
	}
	if req.KindPerHour > 0 {
		scope += fmt.Sprintf(" + %d/ч на вид", req.KindPerHour)
	}
	sctx, p := startStep(ctx, "limiter.reserve", KindLimiter, "бронь запуска ("+string(req.Usage.Kind)+", "+scope+")",
		Fields{"user": req.Username, "kind": string(req.Usage.Kind)})
	dec, res, err := l.Limiter.Reserve(sctx, req)
	decisionEnd(p, dec, err)
	return dec, res, err
}

func (l *limiter) Record(ctx context.Context, u core.Usage) error {
	sctx, p := startStep(ctx, "limiter.record", KindLimiter, "учёт запуска ("+string(u.Kind)+")", Fields{"kind": string(u.Kind), "user_id": u.UserID})
	err := l.Limiter.Record(sctx, u)
	p.done(err, nil)
	return err
}

// ---------------------------------------------------------------------------
// Repos / Checkout
// ---------------------------------------------------------------------------

type repos struct {
	core.Repos
	maxDiff int
}

// WrapRepos returns r with checkouts and diffs recorded.
func (rec *Recorder) WrapRepos(r core.Repos) core.Repos { return Hooks{Rec: rec}.WrapRepos(r) }

// WrapRepos returns r with checkouts and diffs recorded (unchanged when off).
func (h Hooks) WrapRepos(r core.Repos) core.Repos {
	if h.off() || r == nil {
		return r
	}
	maxDiff := 0
	if h.Rec != nil {
		maxDiff = h.Rec.o.MaxDiffBytes
	}
	return &repos{Repos: r, maxDiff: maxDiff}
}

func (r *repos) Checkout(ctx context.Context, projectPath string, mr *core.MergeRequest) (core.Checkout, error) {
	run := FromContext(ctx)
	var f Fields
	if mr != nil {
		run.addSafe(mr.DiffRefs.BaseSHA, mr.DiffRefs.StartSHA, mr.DiffRefs.HeadSHA)
		f = Fields{"project": projectPath, "head_sha": mr.DiffRefs.HeadSHA, "base_sha": mr.DiffRefs.BaseSHA, "start_sha": mr.DiffRefs.StartSHA}
	}
	sctx, p := startStep(ctx, "git.checkout", KindCheckout, "checkout", f)
	co, err := r.Repos.Checkout(sctx, projectPath, mr)
	if err != nil || co == nil || p == nil {
		p.done(err, nil)
		return co, err
	}
	p.done(nil, Fields{"dir": co.Dir()})
	c := &checkout{Checkout: co, maxDiff: r.maxDiff}
	if lc, ok := co.(linesChanger); ok {
		// Optional methods of the concrete checkout must survive the
		// wrapper: the reviewer finds them by type assertion.
		return &linesCheckout{checkout: c, lc: lc}, nil
	}
	return c, nil
}

type checkout struct {
	core.Checkout
	maxDiff int
}

// linesChanger is the optional LinesChanged method of *repo.Checkout, used
// by the reviewer to classify finding outcomes
// (review/learning/outcomes_classify.go).
type linesChanger interface {
	LinesChanged(ctx context.Context, from, to, path string, start, end int) (bool, error)
}

// Unwrap returns the wrapped checkout, so optional capabilities of the
// real checkout (ExportTree, GrepWords, …) stay reachable through
// core.CheckoutAs even though this wrapper does not implement them.
func (c *checkout) Unwrap() core.Checkout { return c.Checkout }

// linesCheckout is a recorded checkout whose underlying checkout also
// answers LinesChanged.
type linesCheckout struct {
	*checkout
	lc linesChanger
}

func (c *linesCheckout) LinesChanged(ctx context.Context, from, to, path string, start, end int) (bool, error) {
	return c.lc.LinesChanged(ctx, from, to, path, start, end)
}

func (c *checkout) Diff(ctx context.Context, from string, ignore []string) (string, error) {
	FromContext(ctx).addSafe(from)
	sctx, p := startStep(ctx, "git.diff", KindGit, "git diff", Fields{"from": from, "to": "HEAD", "ignore": ignore})
	d, err := c.Checkout.Diff(sctx, from, ignore)
	f := Fields{"diff_bytes": len(d), "files": strings.Count(d, "diff --git ")}
	if c.maxDiff > 0 && len(d) > c.maxDiff {
		f["capped_to_bytes"] = c.maxDiff
		p.end(StepWarn, fmt.Sprintf("дифф %d байт больше review.max_diff_bytes=%d: в промпт попадёт часть файлов", len(d), c.maxDiff), f)
		return d, err
	}
	p.done(err, f)
	return d, err
}

func (c *checkout) ChangedFiles(ctx context.Context, from string, ignore []string) ([]string, error) {
	sctx, p := startStep(ctx, "git.changed_files", KindGit, "git changed files", Fields{"from": from})
	files, err := c.Checkout.ChangedFiles(sctx, from, ignore)
	f := Fields{"count": len(files)}
	if len(files) > 0 {
		shown := files
		if len(shown) > 100 {
			shown = shown[:100]
		}
		f["files"] = shown
	}
	p.done(err, f)
	return files, err
}

func (c *checkout) IsAncestor(ctx context.Context, commit string) (bool, error) {
	FromContext(ctx).addSafe(commit)
	sctx, p := startStep(ctx, "git.is_ancestor", KindGit, "git is-ancestor", Fields{"commit": commit})
	ok, err := c.Checkout.IsAncestor(sctx, commit)
	p.done(err, Fields{"ancestor": ok})
	return ok, err
}

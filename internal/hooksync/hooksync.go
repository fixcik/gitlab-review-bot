// Package hooksync keeps the bot's project webhook present and correct in
// every project of the configured GitLab groups.
//
// GitLab Free/CE has no group webhooks, so the bot manages one project hook
// per project instead, with the service token (whose user is a Maintainer or
// Owner in those groups; the bot account itself stays Reporter). The hook is
// identified by its URL; GitLab never returns hook tokens, so a token
// rotation is propagated by re-writing the hooks (resetTokens).
package hooksync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
)

// API is the part of *gitlab.Client the syncer needs.
type API interface {
	ListGroupProjects(ctx context.Context, groupPath string) ([]gitlab.Project, error)
	ListProjectHooks(ctx context.Context, projectID int64) ([]gitlab.Hook, error)
	AddProjectHook(ctx context.Context, projectID int64, s gitlab.HookSpec) (int64, error)
	EditProjectHook(ctx context.Context, projectID, hookID int64, s gitlab.HookSpec) error
}

var _ API = (*gitlab.Client)(nil)

// Options configures a Syncer.
type Options struct {
	API    API
	Groups []string // full group paths; subgroups are included
	URL    string   // the bot's webhook URL as GitLab sees it
	Secret string   // webhook secret token (X-Gitlab-Token); never logged
	// SSLVerify is enable_ssl_verification of the managed hooks.
	SSLVerify bool
	// PipelineEvents subscribes the hooks to pipeline events (review.ci:
	// the CI failure digest); a hook whose pipeline_events differs is
	// updated.
	PipelineEvents bool
	// DryRun computes the report without writing anything to GitLab.
	DryRun bool
	Logger *slog.Logger
	// Excluded, when set, skips projects excluded from review
	// (gitlab.exclude): no hook is created or fixed there (existing hooks
	// are left alone; events from them are ignored by the bot anyway).
	Excluded func(projectPath string) bool
}

// Report summarizes one Sync run.
//
// Projects is the number of distinct non-archived projects found. Every one of
// them ends up in exactly one of Created, Updated, Unchanged, Skipped or
// Failed. Failed additionally counts groups whose project list could not be
// read. In dry-run mode Created/Updated mean "would be created/updated".
type Report struct {
	Projects  int
	Created   int
	Updated   int
	Unchanged int
	Skipped   int // no permission (403) or not found (404): the service token's user is not Maintainer
	Excluded  int // gitlab.exclude
	Failed    int
	Errors    []string // human-readable, one per failure/skip; no secrets
}

// Syncer reconciles project hooks.
type Syncer struct {
	o   Options
	log *slog.Logger
}

// New returns a Syncer.
func New(o Options) *Syncer {
	l := o.Logger
	if l == nil {
		l = slog.New(slog.DiscardHandler)
	}
	return &Syncer{o: o, log: l.With("component", "hooksync")}
}

func (s *Syncer) spec() gitlab.HookSpec {
	return gitlab.HookSpec{
		URL:                   s.o.URL,
		Token:                 s.o.Secret,
		MergeRequestsEvents:   true,
		NoteEvents:            true,
		PipelineEvents:        s.o.PipelineEvents,
		EnableSSLVerification: s.o.SSLVerify,
	}
}

// Sync makes one pass over all projects of all groups. With resetTokens every
// existing bot hook is re-written even if it looks correct, so that a changed
// secret reaches GitLab. Per-project problems are recorded in the report and
// do not stop the pass; an error is returned only when ctx is cancelled or
// when listing projects failed for every group.
func (s *Syncer) Sync(ctx context.Context, resetTokens bool) (Report, error) {
	var rep Report
	if s.o.API == nil {
		return rep, errors.New("hooksync: nil API")
	}
	if strings.TrimSpace(s.o.URL) == "" {
		return rep, errors.New("hooksync: empty webhook URL")
	}
	groups := normalizeGroups(s.o.Groups)
	if len(groups) == 0 {
		return rep, errors.New("hooksync: no groups configured")
	}

	projects, groupErrs, err := s.listProjects(ctx, groups, &rep)
	if err != nil {
		return rep, err
	}
	rep.Projects = len(projects)

	wantURLKey := urlKey(s.o.URL)
	for _, p := range projects {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if s.o.Excluded != nil && s.o.Excluded(p.PathWithNamespace) {
			rep.Excluded++
			continue
		}
		s.syncProject(ctx, p, wantURLKey, resetTokens, &rep)
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	if len(groupErrs) == len(groups) {
		return rep, fmt.Errorf("hooksync: listing projects failed for every group: %w", errors.Join(groupErrs...))
	}
	return rep, nil
}

// listProjects returns the distinct non-archived projects of groups. A group
// whose listing fails is recorded in rep and in groupErrs; err is only the
// cancellation of ctx.
func (s *Syncer) listProjects(ctx context.Context, groups []string, rep *Report) (projects []gitlab.Project, groupErrs []error, err error) {
	seen := map[int64]bool{}
	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		ps, err := s.o.API.ListGroupProjects(ctx, g)
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			rep.Failed++
			rep.Errors = append(rep.Errors, fmt.Sprintf("группа %s: не удалось получить проекты: %v", g, err))
			groupErrs = append(groupErrs, fmt.Errorf("group %s: %w", g, err))
			s.log.Error("hooksync: cannot list group projects", "group", g, "err", err)
			continue
		}
		for _, p := range ps {
			if p.Archived || seen[p.ID] {
				continue
			}
			seen[p.ID] = true
			projects = append(projects, p)
		}
	}
	return projects, groupErrs, nil
}

func (s *Syncer) syncProject(ctx context.Context, p gitlab.Project, wantURLKey string, resetTokens bool, rep *Report) {
	log := s.log.With("project", p.PathWithNamespace, "project_id", p.ID)
	if s.o.DryRun {
		log = log.With("dry_run", true)
	}
	fail := func(op string, err error) {
		if ctx.Err() != nil {
			return // cancellation is reported by Sync
		}
		if noAccess(err) {
			rep.Skipped++
			rep.Errors = append(rep.Errors, fmt.Sprintf("%s: пропущен, нет прав (пользователю служебного токена нужен Maintainer): %v", p.PathWithNamespace, err))
			log.Warn("hooksync: project skipped, the service token's user needs Maintainer to manage hooks", "op", op, "err", err)
			return
		}
		rep.Failed++
		rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %s: %v", p.PathWithNamespace, op, err))
		log.Error("hooksync: project failed", "op", op, "err", err)
	}

	hooks, err := s.o.API.ListProjectHooks(ctx, p.ID)
	if err != nil {
		fail("list hooks", err)
		return
	}
	h, ok := botHook(hooks, wantURLKey, log)
	if !ok {
		if s.o.DryRun {
			rep.Created++
			log.Info("hooksync: would create hook")
			return
		}
		hid, err := s.o.API.AddProjectHook(ctx, p.ID, s.spec())
		if err != nil {
			fail("create hook", err)
			return
		}
		rep.Created++
		log.Info("hooksync: hook created", "hook_id", hid)
		return
	}

	drift := !h.MergeRequestsEvents || !h.NoteEvents || h.PipelineEvents != s.o.PipelineEvents || h.EnableSSLVerification != s.o.SSLVerify
	if !drift && !resetTokens {
		rep.Unchanged++
		log.Debug("hooksync: hook up to date", "hook_id", h.ID)
		return
	}
	reason := "settings drift"
	if !drift {
		reason = "token reset"
	}
	if s.o.DryRun {
		rep.Updated++
		log.Info("hooksync: would update hook", "hook_id", h.ID, "reason", reason)
		return
	}
	if err := s.o.API.EditProjectHook(ctx, p.ID, h.ID, s.spec()); err != nil {
		fail("update hook", err)
		return
	}
	rep.Updated++
	log.Info("hooksync: hook updated", "hook_id", h.ID, "reason", reason)
}

// botHook is the first of the project's hooks that points to the bot (by
// URL); duplicates are left alone with a warning.
func botHook(hooks []gitlab.Hook, wantURLKey string, log *slog.Logger) (gitlab.Hook, bool) {
	var mine []gitlab.Hook
	for _, h := range hooks {
		if urlKey(h.URL) == wantURLKey {
			mine = append(mine, h)
		}
	}
	if len(mine) == 0 {
		return gitlab.Hook{}, false
	}
	if len(mine) > 1 {
		ids := make([]int64, 0, len(mine))
		for _, h := range mine {
			ids = append(ids, h.ID)
		}
		log.Warn("hooksync: several hooks point to the bot; keeping the first, delete the others by hand", "hook_ids", ids)
	}
	return mine[0], true
}

// Run syncs immediately with resetTokens=true (so a rotated secret propagates
// on restart), then every interval with resetTokens=false, until ctx is done.
func (s *Syncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	s.runOnce(ctx, true)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.runOnce(ctx, false)
		}
	}
}

func (s *Syncer) runOnce(ctx context.Context, resetTokens bool) {
	start := time.Now()
	rep, err := s.Sync(ctx, resetTokens)
	if ctx.Err() != nil {
		return
	}
	attrs := []any{
		"reset_tokens", resetTokens,
		"projects", rep.Projects,
		"created", rep.Created,
		"updated", rep.Updated,
		"unchanged", rep.Unchanged,
		"skipped", rep.Skipped,
		"failed", rep.Failed,
		"duration", time.Since(start).Round(time.Millisecond),
	}
	switch {
	case err != nil:
		s.log.Error("hooksync: sync failed", append(attrs, "err", err)...)
	case rep.Failed > 0 || rep.Skipped > 0:
		s.log.Warn("hooksync: sync finished with problems", attrs...)
	default:
		s.log.Info("hooksync: sync finished", attrs...)
	}
}

// noAccess reports a 403/404 from GitLab: the bot cannot manage hooks there.
func noAccess(err error) bool {
	switch gitlab.StatusCode(err) {
	case http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return errors.Is(err, core.ErrNotFound)
}

// urlKey normalizes a webhook URL for comparison: scheme and host are
// lowercased, trailing slashes of the path are dropped. Query and the path's
// case are kept.
func urlKey(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimRight(raw, "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = strings.TrimRight(u.RawPath, "/")
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

func normalizeGroups(groups []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range groups {
		g = strings.TrimSpace(strings.Trim(strings.TrimSpace(g), "/"))
		if g == "" || seen[strings.ToLower(g)] {
			continue
		}
		seen[strings.ToLower(g)] = true
		out = append(out, g)
	}
	return out
}

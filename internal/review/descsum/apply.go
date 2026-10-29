package descsum

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// Client reads and writes an MR description (*gitlab.Client: the bot's
// own token, and the service token on 403).
type Client interface {
	GetMR(ctx context.Context, projectID, mrIID int64) (*core.MergeRequest, error)
	UpdateMRDescription(ctx context.Context, projectID, mrIID int64, description string) error
}

// denyFor: after a 403 the token is not tried on that project for this long.
const denyFor = time.Hour

// maxDescriptionBytes: GitLab's limit of a description (1 MB).
const maxDescriptionBytes = 1_000_000

// Feature writes the summary block into MR descriptions.
type Feature struct {
	env      *env.Env
	bot, svc Client

	mu   sync.Mutex
	deny map[string]time.Time // "bot|<project>" / "service|<project>" -> last 403
}

// New returns the feature; bot and svc may be nil (svc: no service token).
func New(e *env.Env, bot, svc Client) *Feature {
	return &Feature{env: e, bot: bot, svc: svc, deny: map[string]time.Time{}}
}

func (f *Feature) denied(who string, projectID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.deny[key(who, projectID)]
	return ok && f.env.Now().Sub(at) < denyFor
}

func (f *Feature) denyNow(who string, projectID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deny[key(who, projectID)] = f.env.Now()
}

func key(who string, projectID int64) string { return who + "|" + strconv.FormatInt(projectID, 10) }

// Apply writes the summary of out into the MR's description and returns
// what the summary note keeps: the Written marker while the description
// holds the bot's block (so a block the author removed is not added again),
// else "". Read-modify-write on the description read right before the
// update; nothing is written for a draft or a closed MR, an unchanged
// description or an empty summary. Never fails the review.
func (f *Feature) Apply(ctx context.Context, job core.Job, eff config.Effective, lang string, out *output.ReviewOutput, prevSummary string) string {
	if !eff.DescriptionSummary || out == nil {
		return ""
	}
	written := strings.Contains(prevSummary, Written)
	keep := ""
	if written {
		keep = Written + "\n"
	}
	log := f.env.Log.With("mr", job.MRKey())
	block := Render(out.DescriptionSummary, lang)
	read := f.bot
	if read == nil {
		read = f.svc
	}
	if block == "" || read == nil {
		return keep
	}
	mr, err := read.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: description summary: read mr", "err", err)
		return keep
	}
	if mr.State != "opened" || mr.Draft {
		return keep
	}
	desc, why := Merge(mr.Description, block, written)
	switch {
	case why == WhySame:
		return Written + "\n"
	case why == WhyOff:
		log.Info("review: description summary: the author opted out")
		return ""
	case why != "":
		log.Info("review: description summary: description left alone", "why", why)
		return keep
	case len(desc) > maxDescriptionBytes:
		log.Info("review: description summary: the description would exceed 1 MB")
		return keep
	}
	if f.push(ctx, job, desc) {
		return Written + "\n"
	}
	return keep
}

// push writes the description: the bot's token first, the service token on
// 403. It reports whether a write succeeded; a failure is a problem of the
// job (once per run), never a failed review.
func (f *Feature) push(ctx context.Context, job core.Job, desc string) bool {
	log := f.env.Log.With("mr", job.MRKey())
	type client struct {
		who string
		cl  Client
	}
	var clients []client
	for _, c := range []client{{"bot", f.bot}, {"service", f.svc}} {
		if c.cl != nil && !f.denied(c.who, job.ProjectID) {
			clients = append(clients, c)
		}
	}
	if len(clients) == 0 {
		return false // every token was refused within the hour: reported then
	}
	var lastErr error
	forbidden := true
	for _, c := range clients {
		err := c.cl.UpdateMRDescription(ctx, job.ProjectID, job.MRIID, desc)
		if err == nil {
			log.Info("review: description summary written", "token", c.who)
			return true
		}
		lastErr = err
		if errors.Is(err, core.ErrForbidden) || errors.Is(err, core.ErrNotFound) {
			f.denyNow(c.who, job.ProjectID)
		} else {
			forbidden = false
		}
		log.Info("review: description summary: update failed", "token", c.who, "err", err)
	}
	if forbidden {
		detail := "у токена нет права менять описание MR (нужна роль Developer или автор MR)"
		if f.svc == nil {
			detail += "; задайте GITLAB_SERVICE_TOKEN или выключите review.description_summary"
		}
		jobctx.BotProblem(ctx, problems.DescriptionSummaryFailed, "описание MR: нет прав", detail+" ("+lastErr.Error()+")")
		return false
	}
	jobctx.BotProblem(ctx, problems.DescriptionSummaryFailed, "описание MR: ошибка GitLab API", lastErr.Error())
	return false
}

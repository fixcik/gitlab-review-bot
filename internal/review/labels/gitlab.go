package labels

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
)

// labelDenyFor: after a 403 on the bot's own token, labels of that project
// go straight to the service token for this long.
const labelDenyFor = time.Hour

// Label metric values (reviewbot_labels_total).
const (
	labelAdd    = "add"
	labelRemove = "remove"

	labelOK        = "ok"
	labelForbidden = "forbidden"
	labelMissing   = "missing"
	labelIgnored   = "ignored"
	labelError     = "error"
)

// Reader is the client that reads labels: the bot's own, else the
// service one (nil when neither is set).
func (lb *Feature) Reader() Client {
	if lb.bot != nil {
		return lb.bot
	}
	return lb.svc
}

// observeLabel counts one label change (nil-safe).
func (lb *Feature) observeLabel(action, result string) {
	if lb.onLabel != nil {
		lb.onLabel(action, result)
	}
}

// existingLabels keeps the labels that exist in the project or its groups
// (create_missing off); missing ones are reported.
func (lb *Feature) existingLabels(ctx context.Context, job core.Job, add []string, out *labelOutcome) []string {
	names, err := lb.Reader().ListProjectLabels(ctx, job.ProjectID)
	if err != nil {
		lb.env.Log.Warn("review: labels: list project labels", "mr", job.MRKey(), "err", err)
		jobctx.BotProblem(ctx, problems.LabelsFailed, "метки MR: не удалось получить список меток проекта", err.Error())
		for range add {
			lb.observeLabel(labelAdd, labelError)
		}
		out.failed = append(out.failed, add...)
		return nil
	}
	have := toSet(names)
	var keep []string
	for _, a := range add {
		if have[a] {
			keep = append(keep, a)
			continue
		}
		lb.observeLabel(labelAdd, labelMissing)
		out.failed = append(out.failed, a)
		jobctx.BotProblem(ctx, problems.LabelsFailed, "метки нет в проекте и его группах: "+a,
			"create_missing выключен: создайте метку в проекте или группе, либо включите labels.create_missing")
	}
	return keep
}

// botLabelsDenied reports whether the bot's token recently got 403 on this
// project's labels.
func (lb *Feature) botLabelsDenied(projectID int64) bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	at, ok := lb.labelDeny[projectID]
	return ok && lb.env.Now().Sub(at) < labelDenyFor
}

// denyBotLabels remembers that the bot's token got 403 on this project's
// labels now.
func (lb *Feature) denyBotLabels(projectID int64) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if lb.labelDeny == nil {
		lb.labelDeny = map[int64]time.Time{}
	}
	lb.labelDeny[projectID] = lb.env.Now()
}

// pushLabels sends the changes: the bot's own token first (labels are MR
// metadata; GitLab requires Developer, a Reporter gets 403), then the
// service token for whatever is still not applied. It returns the MR's
// labels after the last successful request (nil when none succeeded).
func (lb *Feature) pushLabels(ctx context.Context, job core.Job, add, remove []string, out *labelOutcome) []string {
	log := lb.env.Log.With("mr", job.MRKey())
	type client struct {
		name string
		cl   Client
	}
	var clients []client
	if lb.bot != nil && !lb.botLabelsDenied(job.ProjectID) {
		clients = append(clients, client{"bot", lb.bot})
	}
	if lb.svc != nil {
		clients = append(clients, client{"service", lb.svc})
	}
	var final []string
	pendAdd, pendRemove := add, remove
	lastResult, lastErr := labelForbidden, error(nil)
	for _, c := range clients {
		labels, err := c.cl.UpdateMRLabels(ctx, job.ProjectID, job.MRIID, pendAdd, pendRemove)
		if err != nil {
			lastErr = err
			if errors.Is(err, core.ErrForbidden) {
				lastResult = labelForbidden
				if c.name == "bot" {
					lb.denyBotLabels(job.ProjectID)
				}
			} else {
				lastResult = labelError
			}
			log.Info("review: labels: update failed", "token", c.name, "err", err)
			continue
		}
		final = labels
		fin := toSet(labels)
		var nextAdd, nextRemove []string
		for _, a := range pendAdd {
			if fin[a] {
				out.added = append(out.added, a)
				lb.observeLabel(labelAdd, labelOK)
			} else {
				nextAdd = append(nextAdd, a)
			}
		}
		for _, d := range pendRemove {
			if !fin[d] {
				out.removed = append(out.removed, d)
				lb.observeLabel(labelRemove, labelOK)
			} else {
				nextRemove = append(nextRemove, d)
			}
		}
		pendAdd, pendRemove = nextAdd, nextRemove
		lastResult, lastErr = labelIgnored, nil
		if len(pendAdd) == 0 && len(pendRemove) == 0 {
			return final
		}
	}
	if len(pendAdd) > 0 || len(pendRemove) > 0 {
		lb.labelsNotApplied(ctx, pendAdd, pendRemove, len(clients) == 0, lastResult, lastErr, out)
	}
	return final
}

// labelsNotApplied counts and reports the label changes no client could
// apply; result and lastErr are of the last attempt.
func (lb *Feature) labelsNotApplied(ctx context.Context, add, remove []string, noClients bool, result string, lastErr error, out *labelOutcome) {
	for range add {
		lb.observeLabel(labelAdd, result)
	}
	for range remove {
		lb.observeLabel(labelRemove, result)
	}
	out.failed = append(out.failed, add...)
	out.failed = append(out.failed, remove...)
	names := strings.Join(append(append([]string(nil), add...), remove...), ", ")
	switch {
	case noClients:
		jobctx.BotProblem(ctx, problems.LabelsFailed, "метки MR: нет токена с правом менять метки",
			"токен бота получил 403 (Reporter), служебный токен не задан; не применено: "+names)
	case result == labelForbidden:
		detail := "нет прав менять метки MR (нужна роль Developer); не применено: " + names
		if lb.svc == nil {
			detail += "; задайте GITLAB_SERVICE_TOKEN"
		}
		if lastErr != nil {
			detail += " (" + lastErr.Error() + ")"
		}
		jobctx.BotProblem(ctx, problems.LabelsFailed, "метки MR: нет прав", detail)
	case result == labelIgnored:
		jobctx.BotProblem(ctx, problems.LabelsFailed, "метки MR: GitLab не применил изменения",
			"запрос прошёл, но метки не изменились (нет права создать метку или менять метаданные MR): "+names)
	default:
		detail := "не применено: " + names
		if lastErr != nil {
			detail += " (" + lastErr.Error() + ")"
		}
		jobctx.BotProblem(ctx, problems.LabelsFailed, "метки MR: ошибка GitLab API", detail)
	}
}

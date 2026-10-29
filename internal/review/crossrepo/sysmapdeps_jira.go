package crossrepo

// System map, phase B: the Jira story or epic a linked counterpart merge
// request shares with the reviewed one.

import (
	"context"
	"log/slog"

	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
	"github.com/fixcik/gitlab-review-bot/internal/tasklink"
)

// depsShared finds the Jira story or epic a linked merge request shares
// with the reviewed one (a supporting signal, only for merge requests that
// already touch the same contract).
func (xr *Feature) depsShared(ctx context.Context, log *slog.Logger, mrs []*openMR, tc *task.Context) {
	if xr.task.Jira == nil || tc == nil || len(tc.Issues) == 0 || len(mrs) == 0 {
		return
	}
	fetched := map[string]*jira.Issue{}
	lookups := 0
	for _, m := range mrs {
		found := tasklink.Find(tasklink.Sources{Title: tasklink.StripDraft(m.mr.Title), Branch: m.mr.SourceBranch, Description: m.mr.Description},
			xr.env.Cfg.Jira.ProjectKeys)
		var issues []*jira.Issue
		for _, key := range tasklink.Keys(found, 2) {
			is, ok := fetched[key]
			if !ok {
				if lookups >= depsMaxJira {
					continue
				}
				lookups++
				var err error
				is, err = xr.task.Jira.GetIssue(ctx, key)
				if err != nil {
					log.Warn("review: system map deps: jira issue", "key", key, "err", err)
					is = nil
				}
				fetched[key] = is
			}
			if is != nil {
				issues = append(issues, is)
			}
		}
		kind, key := sharedTask(tc.Issues, issues)
		if key == "" {
			continue
		}
		m.share = kind + " [" + key + "](" + xr.task.BrowseURL(key, nil) + ")"
		m.sharePrompt = map[string]string{"общая задача": "the same issue", "общая стори": "shares parent story", "общий эпик": "shares epic"}[kind] + " " + key
	}
}

func epicOf(is *jira.Issue) string {
	if is.EpicKey != "" {
		return is.EpicKey
	}
	if is.Epic != nil {
		return is.Epic.Key
	}
	return ""
}

// sharedTask: what two sets of issues have in common, most specific
// first: the same issue, a parent story, an epic. Keys are validated.
func sharedTask(a, b []*jira.Issue) (kind, key string) {
	best := 4
	for _, x := range a {
		for _, y := range b {
			if x == nil || y == nil {
				continue
			}
			px, py := "", ""
			if x.Parent != nil {
				px = x.Parent.Key
			}
			if y.Parent != nil {
				py = y.Parent.Key
			}
			ex, ey := epicOf(x), epicOf(y)
			switch {
			case x.Key == y.Key && tasklink.ValidKey(x.Key) && best > 1:
				best, kind, key = 1, "общая задача", x.Key
			case px != "" && px == py && tasklink.ValidKey(px) && best > 2:
				// A parent that is the epic of either issue (next-gen
				// projects) is an epic, not a story.
				if px == ex || px == ey {
					if best > 3 {
						best, kind, key = 3, "общий эпик", px
					}
				} else {
					best, kind, key = 2, "общая стори", px
				}
			case ex != "" && ex == ey && tasklink.ValidKey(ex) && best > 3:
				best, kind, key = 3, "общий эпик", ex
			}
		}
	}
	return kind, key
}

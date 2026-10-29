package main

import (
	"context"
	"log/slog"
	"strconv"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// navBadgeLimit caps the problems read for the header badge ("1000+").
const navBadgeLimit = 1000

// problemsLister is what the header badge reads (*store.SQLite).
type problemsLister interface {
	ListProblems(ctx context.Context, f problems.Filter) ([]problems.Record, error)
}

// webNav is the shared header of every page: the sections that are on and
// the active problems badge (one query per page view; an error shows no
// badge and never fails the page).
func webNav(cfg *config.Service, st problemsLister, diagOn, mapOn, problemsOn bool, logger *slog.Logger) webui.Nav {
	n := webui.Nav{UsageOff: !cfg.Usage.On(), DiagOff: !diagOn, MapOff: !mapOn, ProblemsOff: !problemsOn}
	if problemsOn && st != nil {
		n.Badge = func(ctx context.Context) webui.Badge {
			rows, err := st.ListProblems(ctx, problems.Filter{Status: problems.StatusNew, Limit: navBadgeLimit})
			if err != nil {
				logger.Warn("header: problems badge", "err", err)
				return webui.Badge{}
			}
			if len(rows) == 0 {
				return webui.Badge{}
			}
			b := webui.Badge{Count: strconv.Itoa(len(rows))}
			if len(rows) >= navBadgeLimit {
				b.Count += "+"
			}
			for _, r := range rows {
				if problems.InfoOf(r.Kind).Impact == problems.ImpactBreaks {
					b.Bad = true
					break
				}
			}
			return b
		}
	}
	return n
}

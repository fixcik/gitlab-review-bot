package review

// Model routing by MR risk (review.routing). After the deterministic
// analyses and before the review run, the reviewed range is classified:
//
//   - low risk: at most review.routing.max_lines changed lines, every
//     reviewed file matches review.routing.low_risk_paths (docs, tests,
//     harmless repository config; a renamed or copied file by its old and
//     its new path) and there is no risk signal: no sensitive path
//     (review.routing.sensitive_paths, old paths of renames included),
//     no migrations, no API spec change (compat / OpenAPI changelog, or a
//     compat check that failed), no gitleaks secret, no system map contract
//     touched, no cross-repository dependency (phase B), and the whole MR
//     is not over the TBD size warning (review.tbd.max_lines);
//   - heavy (review.routing.heavy_model set): a range that is not low-risk
//     with at least review.routing.heavy_min_lines changed lines or a file
//     (old paths of renames included) under review.routing.heavy_paths;
//   - everything else: the normal route.
//
// The light route has priority. The heavy route reviews with heavy_model
// without the advisor and the deep-review subagent (the model did not call
// them; the stronger model does the work itself); the search subagent and
// MCP stay as on the normal route.
//
// The light route reviews with the light model (review.routing.light_model,
// else claude.light_model), without the advisor; the search subagent stays.
// Nothing critical is expected there, so normally there is nothing to
// verify; if the light model still reports a critical/major finding (or a
// breaking change) the verification run happens as usual, with the normal
// model (verify/verify.go never uses the route).
//
// The route is recorded in the summary footer ("модель: haiku (низкий
// риск: только docs, 12 строк)"), on /runs (diag.SetRoute), on the job span
// (reviewbot.review.route / reviewbot.review.model) and in
// reviewbot_review_route_total{route}.

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
)

// Routes (metric label, /runs, span attribute).
const (
	routeLight  = "light"
	routeNormal = "normal"
	routeHeavy  = "heavy"
)

// reviewRoute is the routing decision of one review.
type reviewRoute struct {
	name  string // routeLight | routeNormal | routeHeavy
	model string
	// on: routing is enabled for the repository (the footer shows the route).
	on bool
	// lowRisk is the light route's reason ("только docs, 12 строк"); signal
	// is the first risk signal of an otherwise low-risk range ("миграции").
	lowRisk, signal string
	// heavy is the heavy route's reason ("1356 строк ≥ 300", "путь …").
	heavy string
	// reasons: why the range is not low-risk (log).
	reasons []string
}

func (rt reviewRoute) light() bool { return rt.name == routeLight }

// footer is the route part of the summary footer ("" when routing is off).
func (rt reviewRoute) footer() string {
	if !rt.on || rt.model == "" {
		return ""
	}
	s := "модель: " + rt.model
	switch {
	case rt.light():
		s += " (низкий риск: " + rt.lowRisk + ")"
	case rt.heavy != "":
		s += " (тяжёлый: " + rt.heavy + ")"
	case rt.signal != "":
		s += " (риск: " + rt.signal + ")"
	}
	return s
}

// routeInput is what the decision looks at.
type routeInput struct {
	co          core.Checkout
	mr          *core.MergeRequest
	files       []string
	diff        string // the reviewed diff (ignore globs applied)
	incremental bool
	smDiff      string // the system map's full MR diff ("" = none)
	mg          *migcheck.Run
	api         *compatcheck.APIRun
	cr          *compatcheck.Run
	lr          *linters.Run
	sm          *crossrepo.SysmapRun
	smDeps      *crossrepo.DepsRun
}

// routeReview decides the route and records it (log, metric, /runs, span).
func (r *Reviewer) routeReview(ctx context.Context, job core.Job, eff config.Effective, in routeInput) reviewRoute {
	log := r.env.Log.With("mr", job.MRKey())
	rt := decideRoute(eff, r.env.Cfg.Claude.Model, in)
	if rt.light() && eff.TBD.MaxLines > 0 {
		// The whole MR (an incremental range may be small in a big MR).
		if n, ok := r.fullMRLines(ctx, log, eff, in); !ok || n > eff.TBD.MaxLines {
			rt = normalRoute(eff, r.env.Cfg.Claude.Model, "большой MR")
			rt.reasons = []string{fmt.Sprintf("whole MR %d lines (tbd.max_lines %d, known %v)", n, eff.TBD.MaxLines, ok)}
		}
	}
	log.Info("review: model route", "route", rt.name, "model", rt.model, "routing", rt.on,
		"low_risk", rt.lowRisk, "reasons", strings.Join(rt.reasons, "; "))
	if r.onRoute != nil {
		r.onRoute(rt.name)
	}
	diag.SetRoute(ctx, rt.name, rt.model, rt.why())
	return rt
}

// why is the reason shown on /runs.
func (rt reviewRoute) why() string {
	switch {
	case rt.light():
		return "низкий риск: " + rt.lowRisk
	case rt.heavy != "":
		return "тяжёлый: " + rt.heavy
	case !rt.on:
		return "маршрутизация выключена"
	case rt.signal != "":
		return "риск: " + rt.signal
	case len(rt.reasons) > 0:
		return rt.reasons[0]
	}
	return ""
}

// fullMRLines counts the changed lines of the whole MR (merge base ..
// head, ignore globs applied).
func (r *Reviewer) fullMRLines(ctx context.Context, log *slog.Logger, eff config.Effective, in routeInput) (int, bool) {
	full := in.diff
	if in.incremental {
		full = in.smDiff
		if full == "" {
			d, err := in.co.Diff(ctx, diffcap.FullDiffBase(in.mr), eff.Ignore)
			if err != nil {
				log.Warn("review: routing: full diff", "err", err)
				return 0, false
			}
			full = d
		}
	}
	a, rm, _ := diffSize(full, eff.Ignore)
	return a + rm, true
}

func normalRoute(eff config.Effective, model, signal string) reviewRoute {
	return reviewRoute{name: routeNormal, model: model, on: eff.Routing.Enabled, signal: signal}
}

// decideRoute is the decision without I/O (the whole-MR size is checked by
// routeReview): the light route first, then the heavy one, else normal.
func decideRoute(eff config.Effective, model string, in routeInput) reviewRoute {
	rt := decideLight(eff, model, in)
	if rt.light() {
		return rt
	}
	if why := heavyReason(eff.Routing, eff.Ignore, in); why != "" {
		return reviewRoute{name: routeHeavy, model: eff.Routing.HeavyModel, on: true, heavy: why, reasons: append([]string{"heavy: " + why}, rt.reasons...)}
	}
	return rt
}

// heavyReason is why a range is heavy ("" = not, or no heavy route).
func heavyReason(rc config.EffRouting, ignore []string, in routeInput) string {
	if rc.HeavyModel == "" {
		return ""
	}
	a, rm, _ := diffSize(in.diff, ignore)
	if n := a + rm; n >= rc.HeavyMinLines {
		return fmt.Sprintf("%d %s ≥ %d", n, mdtext.LinesWord(n), rc.HeavyMinLines)
	}
	for _, f := range append(slices.Clone(in.files), oldPaths(in.diff)...) {
		if config.MatchAnyPath(rc.HeavyPaths, f) {
			return "путь " + f
		}
	}
	return ""
}

// decideLight is the light-or-normal decision.
func decideLight(eff config.Effective, model string, in routeInput) reviewRoute {
	rc := eff.Routing
	if !rc.Enabled {
		rt := normalRoute(eff, model, "")
		rt.reasons = []string{"routing off"}
		return rt
	}
	var shape, signals []string // shape: size and file kinds; signals: risk
	a, rm, _ := diffSize(in.diff, eff.Ignore)
	lines := a + rm
	if lines > rc.MaxLines {
		shape = append(shape, fmt.Sprintf("%d lines > %d", lines, rc.MaxLines))
	}
	// Both sides of a rename or copy: a file moved out of a sensitive or
	// code path is not low-risk because of its new name.
	paths := append(slices.Clone(in.files), oldPaths(in.diff)...)
	for _, f := range paths {
		if !config.MatchAnyPath(rc.LowRiskPaths, f) {
			shape = append(shape, "not low-risk: "+f)
			break
		}
	}
	if len(in.files) == 0 {
		shape = append(shape, "no files")
	}
	signal := func(ru, en string) {
		signals = append(signals, ru+" ("+en+")")
	}
	for _, f := range paths {
		if config.MatchAnyPath(rc.SensitivePaths, f) {
			signal("чувствительный путь", f)
			break
		}
	}
	if in.mg != nil {
		signal("миграции", "migrations")
	}
	if apiTouched(in.api, in.cr) {
		signal("изменения API", "api spec changed or compat check failed")
	}
	if hasSecrets(in.lr) {
		signal("секреты", "gitleaks")
	}
	if in.sm != nil && in.sm.Impact != nil && len(in.sm.Impact.Contracts) > 0 {
		signal("контракты карты системы", "sysmap contracts touched")
	}
	if in.smDeps.HasFacts() {
		signal("зависимости от других репозиториев", "sysmap phase B")
	}
	if len(shape) > 0 || len(signals) > 0 {
		rt := normalRoute(eff, model, "")
		if len(shape) == 0 {
			rt.signal, _, _ = strings.Cut(signals[0], " (")
		}
		rt.reasons = append(shape, signals...)
		return rt
	}
	return reviewRoute{name: routeLight, model: rc.LightModel, on: true,
		lowRisk: fmt.Sprintf("только %s, %d %s", fileKinds(in.files), lines, mdtext.LinesWord(lines))}
}

// apiTouched: a spec file changed (or the compat check could not tell).
func apiTouched(api *compatcheck.APIRun, cr *compatcheck.Run) bool {
	if cr != nil {
		if cr.Res == nil {
			return cr.Err != nil
		}
		if len(cr.Res.Specs)+len(cr.Res.Deleted)+len(cr.Res.Changes)+len(cr.Res.Warnings) > 0 {
			return true
		}
	}
	return api != nil && len(api.Specs) > 0
}

func hasSecrets(lr *linters.Run) bool {
	if lr == nil || lr.Res == nil {
		return false
	}
	for _, f := range lr.Res.Findings {
		if f.Secret() {
			return true
		}
	}
	return false
}

// routeDocGlobs / routeTestRe label the light route's files (wording only).
var routeDocGlobs = []string{"**/*.md", "**/*.markdown", "**/*.rst", "**/*.adoc", "**/*.txt", "docs/**", "doc/**",
	"**/LICENSE*", "**/CHANGELOG*", "**/AUTHORS*"}

// fileKinds: "docs", "тесты", "конфиги" or a combination ("docs и тесты").
func fileKinds(files []string) string {
	var docs, tests, other bool
	for _, f := range files {
		lower := strings.ToLower(f)
		switch {
		case config.MatchAnyPath(routeDocGlobs, f):
			docs = true
		case strings.Contains(lower, "test") || strings.Contains(lower, "spec"):
			tests = true
		default:
			other = true
		}
	}
	var ks []string
	if docs {
		ks = append(ks, "docs")
	}
	if tests {
		ks = append(ks, "тесты")
	}
	if other {
		ks = append(ks, "конфиги")
	}
	return strings.Join(ks, " и ")
}

// withRoute fills the models of the review run: the light and the heavy
// route get their model, no advisor and no deep-review subagent (the
// search subagent stays); the normal route is env.WithReviewModels.
func (r *Reviewer) withRoute(rt reviewRoute, req core.RunRequest) core.RunRequest {
	if !rt.light() && rt.name != routeHeavy {
		return r.env.WithReviewModels(req)
	}
	req.Model, req.Advisor, req.SearchModel, req.DeepReviewModel = rt.model, "", r.env.Cfg.Claude.SearchModel, ""
	req.SystemPrompt += env.ModelsNote(false, req.SearchModel != "", false)
	return req
}

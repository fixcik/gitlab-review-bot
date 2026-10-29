package verify_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"text/template"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// errGlobalLimiter fails the global limit check.
type errGlobalLimiter struct {
	*reviewtest.FakeLimiter
	err error
}

func (l errGlobalLimiter) CheckGlobal(context.Context) (core.Decision, error) {
	return core.Decision{}, l.err
}

func problemsCtx(ctx context.Context) (context.Context, *problems.Run) {
	return problems.New(problems.Options{}).Start(ctx, reviewtest.AutoJob())
}

// The global limit cannot be checked: no run, the claims are published
// unverified (they still block) with the reason, and a verify_failed
// problem is recorded — unless the job itself was cancelled.
func TestVerifyGlobalLimitCheckError(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		h := reviewtest.New(t)
		h.Deps.Limiter = errGlobalLimiter{FakeLimiter: h.Lim, err: errors.New("sqlite: database is locked")}
		ctx, run := problemsCtx(context.Background())
		if cancelled {
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		}
		cr := &compatcheck.Run{Enabled: true, Res: &compat.Result{}}
		out := &output.ReviewOutput{Compatibility: &output.Compatibility{Breaking: []output.BreakingChange{dropColumn}}}
		h.Internals().Verify.PlanCompat(ctx, reviewtest.AutoJob(), cr, out, &mrthread.Threads{}, t.TempDir(), "b", "h", "Russian")
		if len(h.Runner.Reqs) != 0 {
			t.Fatalf("cancelled=%v: runs %d", cancelled, len(h.Runner.Reqs))
		}
		if len(cr.Items) != 1 || !cr.Items[0].Unverified || cr.Items[0].VerifyNote != "перепроверка не удалась" || cr.Items[0].Title != dropColumn.Title {
			t.Fatalf("cancelled=%v: items %+v", cancelled, cr.Items)
		}
		ps := run.List()
		if cancelled {
			if len(ps) != 0 {
				t.Errorf("a cancelled job is not a bot problem: %+v", ps)
			}
			continue
		}
		if len(ps) != 1 || ps[0].Kind != problems.VerifyFailed || ps[0].Subject != "перепроверка: "+verify.WhyError || ps[0].Detail != "sqlite: database is locked" {
			t.Errorf("problems %+v", ps)
		}
	}
}

// The verification prompt cannot be rendered: no run, the findings are
// published as they are (a critical still blocks) and the summary says the
// verification did not run.
func TestVerifyRenderErrorPublishesFindings(t *testing.T) {
	verify.BreakTemplate(t, verify.VerifyUserTmpl, template.Must(template.New("bad").Parse("{{.NoSuchField}}")))
	h, obs := reviewtest.VerifyHarness(t)
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, []output.Finding{reviewtest.VfRace}, nil)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("runs %d, want only the review", len(h.Runner.Reqs))
	}
	race := threadFor(t, h, reviewtest.VfRace.Title)
	if !marker.HasBlocking(race.Body) || !strings.Contains(race.Body, "🔴 Critical") {
		t.Errorf("unverified critical must be published as is: %s", race.Body)
	}
	if b := verifyBlock(reviewtest.SummaryBody(t, h)); !strings.Contains(b, "🧪 Перепроверка не выполнена: "+verify.WhyError+" — 1 замечание опубликовано без перепроверки.") {
		t.Errorf("block %q", b)
	}
	if strings.Join(obs.Runs, ",") != verify.ResultError || strings.Join(obs.Verdicts, ",") != "unverified/critical" {
		t.Errorf("metrics %v %v", obs.Runs, obs.Verdicts)
	}
}

// A claim the merged run gives no verdict for is published unverified and
// keeps blocking; the answered one is published as confirmed.
func TestVerifyClaimWithoutVerdictStaysBlocking(t *testing.T) {
	h, obs := reviewtest.VerifyHarness(t)
	renamed := output.BreakingChange{Path: "cache.go", Line: 3, Kind: "api", Title: "Переименовано поле ответа", Explanation: "клиенты читают old_name"}
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.MustJSON(t, output.ReviewOutput{Summary: "s", Findings: []output.Finding{reviewtest.VfSQL},
			Compatibility: &output.Compatibility{Breaking: []output.BreakingChange{dropColumn, renamed}}})},
		{Out: reviewtest.MustJSON(t, verify.Output{
			Findings: []verify.FindingVerdict{{ID: "F1", Verdict: "confirm", Reason: "да"}},
			Verdicts: []output.CompatVerdict{{Index: 1, Verdict: "confirmed", Reason: "auth.go:3 читает email"}},
		})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	th := reviewtest.CompatThreads(h)
	if len(th) != 2 {
		t.Fatalf("compat threads %+v", th)
	}
	for _, d := range th {
		if !marker.HasBlocking(d.Body) {
			t.Errorf("a breaking change always blocks: %s", d.Body)
		}
		switch {
		case strings.Contains(d.Body, dropColumn.Title):
			if !strings.Contains(d.Body, "Подтверждено перепроверкой: auth.go:3 читает email") || strings.Contains(d.Body, "Не перепроверено") {
				t.Errorf("confirmed claim: %s", d.Body)
			}
		case strings.Contains(d.Body, renamed.Title):
			if !strings.Contains(d.Body, "Не перепроверено (перепроверка не дала ответа по этому пункту)") {
				t.Errorf("claim without a verdict: %s", d.Body)
			}
		default:
			t.Errorf("unexpected thread %s", d.Body)
		}
	}
	if got := strings.Join(obs.Verdicts, ","); got != "confirm/breaking,missing/breaking,confirm/critical" {
		t.Errorf("verdicts %s", got)
	}
}

// failingOutcomes cannot store outcomes of rejected findings.
type failingOutcomes struct{ *store.SQLite }

func (f failingOutcomes) RecordOutcome(ctx context.Context, o learn.Outcome) (int64, error) {
	if o.Status == learn.OutcomeRejectedByVerifier {
		return 0, errors.New("disk full")
	}
	return f.SQLite.RecordOutcome(ctx, o)
}

// A rejected finding whose outcome cannot be stored is still not
// published, and the rest of the review goes on.
func TestVerifyRejectedOutcomeStoreError(t *testing.T) {
	h, _ := reviewtest.OutcomesHarness(t)
	h.Deps.Outcomes = failingOutcomes{h.Db}
	h.Cfg.Review.Verify = config.Defaults().Review.Verify
	h.Runner.Resps = []reviewtest.RunResp{
		{Out: reviewtest.ReviewOut(t, []output.Finding{reviewtest.CritFinding, reviewtest.VfSQL}, nil)},
		{Out: reviewtest.VerdictsOut(t, verify.FindingVerdict{ID: "F1", Verdict: "reject", Reason: "нет"}, verify.FindingVerdict{ID: "F2", Verdict: "confirm", Reason: "да"})},
	}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	for _, d := range reviewtest.FindingThreads(h.Harness) {
		if strings.Contains(d.Body, reviewtest.CritFinding.Title) {
			t.Fatalf("rejected finding published: %s", d.Body)
		}
	}
	got := map[string]string{}
	for _, o := range reviewtest.Outcomes(t, h) {
		got[o.Title] = o.Status
	}
	if _, ok := got[reviewtest.CritFinding.Title]; ok || got[reviewtest.VfSQL.Title] != learn.OutcomeOpen {
		t.Errorf("outcomes %v", got)
	}
	if b := verifyBlock(reviewtest.SummaryBody(t, h.Harness)); !strings.Contains(b, "подтверждено 1, понижено 0, отклонено 1") {
		t.Errorf("block %q", b)
	}
}

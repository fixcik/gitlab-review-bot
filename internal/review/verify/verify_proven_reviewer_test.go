package verify_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// A migration critical whose deployed_usage passed the evidence check is
// not verified again; the model's opinion finding is.
func TestVerifySkipsProvenMigrationFinding(t *testing.T) {
	h := reviewtest.MigHarness(t)
	h.Cfg.Review.Verify = config.Defaults().Review.Verify
	obs := &reviewtest.FakeVerifyObs{}
	h.Deps.Verify = obs
	out := output.ReviewOutput{Summary: "Удаляет колонки.", Findings: []output.Finding{
		{Path: reviewtest.MigPath, Line: 7, Severity: "critical", Title: "Удаление created_by ломает задеплоенный код",
			Body: "Сущность Order всё ещё маппит createdBy.", DeployedUsage: &output.UsageRef{Path: "src/Entity/Order.php", Line: 4, Name: "createdBy"}},
		{Path: "cache.go", Line: 2, Severity: "critical", Title: "Гонка в кэше", Body: "map без блокировки"},
	}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, out)}, {Out: reviewtest.VerdictsOut(t, verify.FindingVerdict{ID: "F1", Verdict: verify.VerdictConfirm, Reason: "да"})}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 2 {
		t.Fatalf("runs %d, want review + verification", len(h.Runner.Reqs))
	}
	v := h.Runner.Reqs[1].Prompt
	if !strings.Contains(v, "Гонка в кэше") || strings.Contains(v, "Удаление created_by") {
		t.Fatalf("verification prompt must hold only the opinion finding:\n%s", v)
	}
	sum := reviewtest.SummaryBody(t, h)
	if !strings.Contains(sum, "🧪 Без перепроверки: 1 замечание доказано проверками бота (использование в задеплоенном коде).") ||
		!strings.Contains(sum, "подтверждено 1") {
		t.Fatalf("summary:\n%s", sum)
	}
	if !strings.Contains(strings.Join(obs.Verdicts, ","), "proven/critical") {
		t.Fatalf("verdict metric %v", obs.Verdicts)
	}
}

// Only proven findings: no verification run, the block still says why.
func TestVerifyOnlyProvenNoRun(t *testing.T) {
	h := reviewtest.MigHarness(t)
	h.Cfg.Review.Verify = config.Defaults().Review.Verify
	out := output.ReviewOutput{Summary: "x", Findings: []output.Finding{
		{Path: reviewtest.MigPath, Line: 7, Severity: "critical", Title: "Удаление created_by ломает задеплоенный код",
			Body: "x", DeployedUsage: &output.UsageRef{Path: "src/Entity/Order.php", Line: 4, Name: "createdBy"}},
	}}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.MustJSON(t, out)}}
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	if len(h.Runner.Reqs) != 1 {
		t.Fatalf("runs %d, want 1", len(h.Runner.Reqs))
	}
	if sum := reviewtest.SummaryBody(t, h); !strings.Contains(sum, marker.VerifyStart+"\n🧪 Без перепроверки: 1 замечание доказано") {
		t.Fatalf("summary:\n%s", sum)
	}
}

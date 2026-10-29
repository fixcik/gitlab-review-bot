package reviewtest

import (
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// Fixtures: lines 2..5 of cache.go are in the fake diff index.
var (
	VfRace   = output.Finding{Path: "cache.go", Line: 2, Severity: "critical", Title: "Гонка при записи в кэш", Body: "map пишется без блокировки"}
	VfLeak   = output.Finding{Path: "cache.go", Line: 3, Severity: "critical", Title: "Утечка памяти", Body: "записи никогда не вытесняются"}
	VfSQL    = output.Finding{Path: "cache.go", Line: 4, Severity: "critical", Title: "SQL-инъекция в ключе", Body: "ключ подставляется в запрос"}
	VfLimit  = output.Finding{Path: "cache.go", Line: 5, Severity: "major", Title: "Нет лимита размера", Body: "размер не ограничен"}
	VfTypo   = output.Finding{Path: "cache.go", Line: 5, Severity: "minor", Title: "Опечатка в комментарии", Body: "cahce"}
	VfNitBig = output.Finding{Path: "cache.go", Line: 4, Severity: "nit", Title: "Имя переменной", Body: "x"}
)

// VerifyHarness is New with review.verify on (the default config).
func VerifyHarness(t *testing.T) (*Harness, *FakeVerifyObs) {
	t.Helper()
	h := New(t)
	h.Cfg.Review.Verify = config.Defaults().Review.Verify
	obs := &FakeVerifyObs{}
	h.Deps.Verify = obs
	return h, obs
}

func VerdictsOut(t *testing.T, vs ...verify.FindingVerdict) string {
	return MustJSON(t, verify.Output{Findings: vs})
}

type FakeVerifyObs struct {
	mu       sync.Mutex
	Runs     []string
	Verdicts []string // verdict/severity
}

func (o *FakeVerifyObs) ObserveVerifyRun(result string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Runs = append(o.Runs, result)
}

func (o *FakeVerifyObs) ObserveVerifyVerdict(verdict, severity string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.Verdicts = append(o.Verdicts, verdict+"/"+severity)
}

func FindingThreads(h *Harness) []DraftNote {
	var out []DraftNote
	for _, d := range h.GL.Published {
		if _, ok := marker.ParseFinding(d.Body); ok {
			out = append(out, d)
		}
	}
	return out
}

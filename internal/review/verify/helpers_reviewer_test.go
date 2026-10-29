package verify_test

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

// verifyOut and dropColumn: as in compatcheck's tests (a compat-only
// verification answer, a breaking change Claude reports).

func verifyOut(t *testing.T, verdicts ...output.CompatVerdict) string {
	return reviewtest.MustJSON(t, output.CompatVerification{Verdicts: verdicts})
}

var dropColumn = output.BreakingChange{
	Path: "cache.go", Line: 2, Kind: "db", Title: "Удалена колонка users.email",
	Explanation:      "Предыдущая версия сервиса читает users.email при логине.",
	PreviousBehavior: "колонка users.email существует", NewBehavior: "колонки нет",
	SafeAlternative: "сначала перестать читать колонку, удалить её отдельным релизом",
}

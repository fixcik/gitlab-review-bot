package review

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

func TestRenderProblemsBlock(t *testing.T) {
	mk := func(kind problems.Kind, subject string) problems.Outcome {
		return problems.Outcome{Problem: problems.Problem{Kind: kind, Subject: subject}, Info: problems.InfoOf(kind)}
	}
	if renderProblemsBlock(nil, "cc @x") != "" {
		t.Fatal("empty")
	}
	three := []problems.Outcome{mk(problems.JiraFailed, "X-1"), mk(problems.InvalidOutput, "ответ"), mk(problems.LintFailed, "eslint: недоступен")}
	b := renderProblemsBlock(three, "")
	if strings.Contains(b, "<details>") || !strings.HasPrefix(b, marker.ProblemsStart+"\n⚠️ **Не удалось проверить:**\n- Ответ модели не разобран") {
		t.Fatalf("≤3 (severity order: error first):\n%s", b)
	}
	// Untrusted subjects: escaped, no mentions, no HTML.
	evil := renderProblemsBlock([]problems.Outcome{mk(problems.ModelOther, "@all <img src=x> [link](http://e) `x`")}, "")
	for _, want := range []string{"@\u200ball", "\\<img", "\\[link\\]", "\\`x\\`"} {
		if !strings.Contains(evil, want) {
			t.Errorf("%q not escaped in\n%s", want, evil)
		}
	}
	four := append(three, mk(problems.BaseUnavailable, "merge base"))
	if b := renderProblemsBlock(four, "cc @a"); !strings.Contains(b, "<summary>⚠️ Не удалось проверить: 4</summary>") ||
		!strings.Contains(b, "</details>\n\ncc @a\n"+marker.ProblemsEnd) || strings.Contains(b, "и ещё") {
		t.Fatalf(">3 collapsed:\n%s", b)
	}
}

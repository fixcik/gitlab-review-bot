package review

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

func TestMergeFollowUp(t *testing.T) {
	f := func(path string, line, end int, title, body string) output.Finding {
		return output.Finding{Path: path, Line: line, EndLine: end, Title: title, Body: body}
	}
	first := []output.Finding{
		f("a.go", 10, 12, "Нулевой указатель при пустом кэше", "Если кэш пуст, обработчик разыменует nil"),
		f("a.go", 11, 11, "Сдвиг на единицу в счётчике повторов", "Цикл делает на одну попытку больше"),
	}
	second := []output.Finding{
		f("a.go", 10, 13, "Нулевой указатель при пустом кэше", "Если кэш пуст, обработчик разыменует nil при запуске"), // repeat
		f("a.go", 10, 12, "Нет теста на ветку таймаута", "Ветка таймаута обработчика не покрыта тестами"),              // other problem, same lines
		f("a.go", 90, 91, "Нулевой указатель при пустом кэше", "Если кэш пуст, обработчик разыменует nil"),             // other place
		f("b.go", 10, 12, "Нулевой указатель при пустом кэше", "Если кэш пуст, обработчик разыменует nil"),             // other file
	}
	got := mergeFollowUp(first, second)
	if len(got) != 5 {
		t.Fatalf("got %d findings, want 5 (the repeat dropped): %+v", len(got), got)
	}
	if got[0].Line != 10 || got[1].Line != 11 {
		t.Errorf("the first review must stay as it is: %+v", got[:2])
	}
}

func TestFollowUpAndRecallOnByDefault(t *testing.T) {
	if r := config.Defaults().Review; !r.FollowUp || !r.Recall {
		t.Errorf("review.followup and review.recall are on by default: %+v", r)
	}
}

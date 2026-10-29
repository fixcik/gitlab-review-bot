package events

import (
	"reflect"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestParseMRCommand(t *testing.T) {
	tests := []struct {
		in   string
		want core.JobKind // "" = not an MR command
	}{
		{"pause", core.JobPause},
		{"Pause!", core.JobPause},
		{"PAUSE.", core.JobPause},
		{"пауза", core.JobPause},
		{"Пауза", core.JobPause},
		{"пауза — идёт рефакторинг", core.JobPause},
		{"pause: big refactoring", core.JobPause},
		{"resume", core.JobResume},
		{"Продолжить", core.JobResume},
		{"продолжить, всё готово", core.JobResume},
		{"ignore", core.JobIgnore},
		{"игнорировать", core.JobIgnore},
		{"resolve", core.JobResolveAll},
		{"Закрой треды", core.JobResolveAll},
		{"закрой   треды!", core.JobResolveAll},
		{"config", core.JobConfig},
		{"Конфиг?", core.JobConfig},
		{"настройки", core.JobConfig},
		{"check", core.JobCheck},
		{"Check!", core.JobCheck},
		{"recheck", core.JobCheck},
		{"RECHECK.", core.JobCheck},
		{"проверь", core.JobCheck},
		{"Проверь!", core.JobCheck},
		{"перепроверь", core.JobCheck},
		{"  Перепроверь  ", core.JobCheck},
		// Chat questions, not commands.
		{"check this function for races?", ""},
		{"проверь, пожалуйста, обработку ошибок", ""},
		{"проверь: почему тут nil", ""},
		{"перепроверь\nи ещё вопрос", ""},
		{"pause the worker before shutdown?", ""},
		{"why does resume() not lock?", ""},
		{"resolve, then explain", ""},
		{"config: why is this key needed", ""},
		{"пауза\nи ещё вопрос", ""},
		{"ignore errors here?", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got, ok := ParseMRCommand(tt.in)
		if tt.want == "" {
			if ok {
				t.Errorf("%q: unexpected %s", tt.in, got)
			}
			continue
		}
		if !ok || got != tt.want {
			t.Errorf("%q: got %q %v, want %s", tt.in, got, ok, tt.want)
		}
	}
}

// The command phrases derived from the kind table (core.KindInfo) are the
// ones the webhook recognized before the table.
func TestCommandTablesUnchanged(t *testing.T) {
	wantMention := map[string]core.JobKind{
		"review": core.JobReview, "ревью": core.JobReview,
		"full review": core.JobFullReview, "full-review": core.JobFullReview, "полное ревью": core.JobFullReview,
		"review force": core.JobForceReview, "force review": core.JobForceReview,
		"ревью принудительно": core.JobForceReview, "принудительное ревью": core.JobForceReview,
		"help": core.JobHelp, "помощь": core.JobHelp,
	}
	wantMR := map[string]core.JobKind{
		"pause": core.JobPause, "пауза": core.JobPause, "на паузу": core.JobPause, "поставь на паузу": core.JobPause,
		"resume": core.JobResume, "unpause": core.JobResume, "продолжить": core.JobResume, "продолжи": core.JobResume,
		"продолжай": core.JobResume, "сними паузу": core.JobResume,
		"ignore": core.JobIgnore, "игнорировать": core.JobIgnore, "игнорируй": core.JobIgnore,
		"resolve": core.JobResolveAll, "resolve all": core.JobResolveAll, "resolve threads": core.JobResolveAll,
		"закрой треды": core.JobResolveAll, "закрыть треды": core.JobResolveAll, "закрой все треды": core.JobResolveAll,
		"config": core.JobConfig, "configuration": core.JobConfig, "конфиг": core.JobConfig, "настройки": core.JobConfig,
		"check": core.JobCheck, "recheck": core.JobCheck, "проверь": core.JobCheck, "перепроверь": core.JobCheck,
	}
	if !reflect.DeepEqual(mentionCommands, wantMention) {
		t.Errorf("mentionCommands = %v", mentionCommands)
	}
	if !reflect.DeepEqual(mrCommandWords, wantMR) {
		t.Errorf("mrCommandWords = %v", mrCommandWords)
	}
	if !reflect.DeepEqual(withReason, map[core.JobKind]bool{core.JobPause: true, core.JobResume: true}) {
		t.Errorf("withReason = %v", withReason)
	}
}

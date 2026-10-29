package events

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestRouteMemoryCommands(t *testing.T) {
	cases := []struct {
		note string
		want core.JobKind
	}{
		{"@reviewbot память", core.JobMemory},
		{"@reviewbot memory", core.JobMemory},
		{"@reviewbot память очистить тесты", core.JobMemory},
		{"@reviewbot memory clear", core.JobMemory},
		{"@reviewbot память запомни линт: make lint", core.JobMemory},
		{"@reviewbot memory add test: make test-unit", core.JobMemory},
		{"@reviewbot запомни логи на английском", core.JobRules},
		{"@reviewbot правила", core.JobRules},
		{"@reviewbot а что с памятью в этом цикле?", core.JobChat},
		{"@reviewbot help", core.JobHelp},
	}
	for _, c := range cases {
		kind, q, ok := ParseCommand(c.note, "@reviewbot")
		if !ok {
			t.Fatalf("%q: not parsed", c.note)
		}
		kind = routeMemory(routeRules(kind, q), q)
		if kind != c.want {
			t.Errorf("%q -> %s, want %s", c.note, kind, c.want)
		}
	}
}

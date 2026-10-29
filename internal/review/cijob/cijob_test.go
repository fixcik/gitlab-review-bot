package cijob

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestMatchCIJobs(t *testing.T) {
	jobs := []core.CIJob{{Name: "phpstan"}, {Name: "phpstan 1/2"}, {Name: "phpstan-strict"}, {Name: "lint:ruff"}, {Name: "x phpstan"}}
	for pattern, want := range map[string]string{
		"phpstan":   "phpstan,phpstan 1/2",
		"phpstan*":  "phpstan,phpstan 1/2,phpstan-strict",
		"lint:*":    "lint:ruff",
		"*phpstan":  "phpstan,phpstan 1/2,x phpstan",
		"nothing":   "",
		"[bad":      "",
		"phpstan 1": "",
	} {
		var got []string
		for _, j := range Match(jobs, pattern) {
			got = append(got, j.Name)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%q: %v, want %s", pattern, got, want)
		}
	}
}

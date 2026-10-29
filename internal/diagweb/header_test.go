package diagweb

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

// The list and a run share the header (Диагностика highlighted); the run's
// path back to the list is inside the page.
func TestSharedHeader(t *testing.T) {
	f := newFixture(t, Options{Enabled: true})
	for _, p := range []string{"/runs", "/runs/" + f.ids["ok"]} {
		body := get(f.h, p, nil).Body.String()
		if pr := webui.HeaderProblems(body, webui.SectionRuns); len(pr) > 0 {
			t.Errorf("%s: %v", p, pr)
		}
	}
	run := get(f.h, "/runs/"+f.ids["ok"], nil).Body.String()
	if i, j := strings.Index(run, "</header>"), strings.Index(run, `<nav class="pagecrumbs"`); j < i || !strings.Contains(run, `<a href="/runs" id="back">Диагностика</a>`) {
		t.Error("the run page keeps its path to the list under the header")
	}
}

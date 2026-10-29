package usageweb

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

func TestSharedHeader(t *testing.T) {
	h := newHandler(t, Options{Enabled: true})
	body := get(h, "/usage", nil).Body.String()
	if p := webui.HeaderProblems(body, webui.SectionUsage); len(p) > 0 {
		t.Errorf("header: %v", p)
	}
}

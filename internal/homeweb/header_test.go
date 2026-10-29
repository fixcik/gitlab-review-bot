package homeweb

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

func TestSharedHeader(t *testing.T) {
	body := get(New(full(richStore())), "GET", "/", nil).Body.String()
	if p := webui.HeaderProblems(body, webui.SectionHome); len(p) > 0 {
		t.Errorf("header: %v", p)
	}
}

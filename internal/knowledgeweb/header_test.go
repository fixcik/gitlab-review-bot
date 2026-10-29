package knowledgeweb

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

func TestSharedHeader(t *testing.T) {
	h, _ := testHandler(t, "")
	body := do(h, "GET", "/knowledge", nil, "").Body.String()
	if p := webui.HeaderProblems(body, webui.SectionKnowledge); len(p) > 0 {
		t.Errorf("header: %v", p)
	}
}

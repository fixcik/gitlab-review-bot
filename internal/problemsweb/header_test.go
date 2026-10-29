package problemsweb

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

func TestSharedHeader(t *testing.T) {
	h, _ := impactHandler(t)
	if p := webui.HeaderProblems(get(t, h, "/problems"), webui.SectionProblems); len(p) > 0 {
		t.Errorf("header: %v", p)
	}
}

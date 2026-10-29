package mapweb

import (
	"context"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/webui"
)

func TestSharedHeader(t *testing.T) {
	body := get(newHandler(Options{Enabled: true, Nav: webui.Nav{Badge: func(context.Context) webui.Badge { return webui.Badge{Count: "4"} }}}), PagePath, nil).Body.String()
	if p := webui.HeaderProblems(body, webui.SectionMap); len(p) > 0 {
		t.Errorf("header: %v", p)
	}
	if !strings.Contains(body, `<span class="cnt" title="активных проблем">4</span>`) {
		t.Error("the problems badge is on every page")
	}
}

package pipeline

import (
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"strings"
	"testing"
	"time"
)

func TestTBDHelpers(t *testing.T) {
	if a, r := diffcap.CountLines("@@ -1,2 +1,3 @@\n ctx\n-old\n+new\n+more\n\\ No newline at end of file"); a != 2 || r != 1 {
		t.Errorf("count %d %d", a, r)
	}
	for d, want := range map[time.Duration]string{20 * time.Minute: "меньше часа", 5 * time.Hour: "5 часов", 47 * time.Hour: "47 часов", 50 * time.Hour: "2 дня", 7 * 24 * time.Hour: "7 дней"} {
		if got := humanAge(d); got != want {
			t.Errorf("humanAge(%v) = %q, want %q", d, got, want)
		}
	}
	body := "<!-- reviewbot:summary sha=x -->\n## 🤖 Ревью\n\ntext\n\n" + summary.CompatSectionTitle + "\n\nok"
	got := ReplaceTBD(body, marker.TBDStart+"\nA\n"+marker.TBDEnd)
	if !strings.Contains(got, "text\n\n"+marker.TBDStart+"\nA\n"+marker.TBDEnd+"\n\n"+summary.CompatSectionTitle) {
		t.Errorf("insert:\n%s", got)
	}
	if again := ReplaceTBD(got, marker.TBDStart+"\nB\n"+marker.TBDEnd); strings.Contains(again, "\nA\n") || strings.Count(again, marker.TBDStart) != 1 {
		t.Errorf("replace:\n%s", again)
	}
	if ReplaceTBD("plain", "") != "plain" {
		t.Error("empty block, no block: unchanged")
	}
	if pipelineLine(&core.Pipeline{Status: "success", SHA: "old", WebURL: "javascript:alert(1)"}, "new") != "✅ пайплайн прошёл (для предыдущего коммита)" {
		t.Error("non-http URL must not become a link; stale pipeline marked")
	}
}

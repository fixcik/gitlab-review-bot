package summary

import (
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// GateBlockedPrefix starts the gate line while blocking threads are open;
// GateClearLine is the gate line without them.
const (
	GateBlockedPrefix = "⛔ **Мерж заблокирован:**"
	GateClearLine     = "✅ Блокирующих замечаний нет."
)

// GateLine is the merge-gate line of the summary: "⛔ **Мерж
// заблокирован:** …" with the number of open blocking threads (and how many
// of them wait for a human to close them), or "✅ Блокирующих замечаний нет.".
func GateLine(blockingOpen, fixedPending int) string {
	if blockingOpen <= 0 {
		return GateClearLine
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %d %s", GateBlockedPrefix, blockingOpen,
		mdtext.Plural(blockingOpen, "блокирующее замечание", "блокирующих замечания", "блокирующих замечаний"))
	if p := min(fixedPending, blockingOpen); p > 0 {
		fmt.Fprintf(&b, " (из них %d %s, %s закрытия)", p, mdtext.Plural(p, "исправлено", "исправлены", "исправлены"),
			mdtext.Plural(p, "ждёт", "ждут", "ждут"))
	}
	b.WriteString(". Исправьте и закройте треды.")
	return b.String()
}

// SetGateLine replaces the merge-gate line of an existing summary body
// with line; ok is false when the body has no gate line.
func SetGateLine(body, line string) (string, bool) {
	lines := strings.Split(body, "\n")
	found := false
	for i, l := range lines {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, GateBlockedPrefix) || t == GateClearLine {
			lines[i], found = line, true
		}
	}
	if !found {
		return body, false
	}
	return strings.Join(lines, "\n"), true
}

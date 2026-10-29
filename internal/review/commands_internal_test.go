package review

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

func TestSetPauseLine(t *testing.T) {
	base := marker.Summary("x") + "\n" + summary.Title + "\n\nТекст."
	with := setPauseLine(base, "⏸ Авторевью на паузе (поставил @\u200bbob)")
	if with != marker.Summary("x")+"\n"+summary.Title+"\n\n⏸ Авторевью на паузе (поставил @\u200bbob)\n\nТекст." {
		t.Fatalf("with:\n%q", with)
	}
	if setPauseLine(with, "⏸ Авторевью на паузе (поставил @\u200beve)") != strings.Replace(with, "bob", "eve", 1) {
		t.Fatal("replace")
	}
	if got := setPauseLine(with, ""); got != base {
		t.Fatalf("remove:\n%q", got)
	}
	if quietUser("bo b\n<x>") != "@\u200bbobx" {
		t.Fatalf("quietUser %q", quietUser("bo b\n<x>"))
	}
}

package compatcheck

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/review/mrthread"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// Changes of one endpoint are grouped per method, in HTTP method order.
func TestBuildAPIViewMethodOrder(t *testing.T) {
	v := buildAPIView(compat.APIChanges{Spec: "a.yaml", Items: []compat.APIChange{
		{Level: 1, Rule: "r", Operation: "DELETE", Endpoint: "/x", Text: "d"},
		{Level: 1, Rule: "r", Operation: "POST", Endpoint: "/x", Text: "p"},
		{Level: 1, Rule: "r", Operation: "GET", Endpoint: "/x", Text: "g"},
	}}, 10)
	var got []string
	for _, g := range v.Groups {
		got = append(got, g.Operation+" "+g.Endpoint)
	}
	if strings.Join(got, ",") != "GET /x,POST /x,DELETE /x" {
		t.Errorf("groups %v", got)
	}
}

// The source line of a Claude breaking change names its kind; a note
// anchored on a nearby line says which line it is about.
func TestNoteBodyKindsAndSnap(t *testing.T) {
	for kind, want := range map[string]string{
		output.CompatKindAPI: "(API)", output.CompatKindEvent: "(события/сообщения)", output.CompatKindDB: "(миграция БД)",
		output.CompatKindConfig: "(конфигурация)", "weird": "(другое)",
	} {
		b := noteBody(Item{ID: "i", Source: SourceClaude, Kind: kind, Path: "a.go", Line: 3, Title: "t"}, false, 0)
		if !strings.Contains(b, "_Источник: анализ Claude "+want+"._") {
			t.Errorf("%s:\n%s", kind, b)
		}
	}
	b := noteBody(Item{ID: "i", Source: SourceClaude, Path: "a.go", Line: 3, Title: "t"}, false, 5)
	if !strings.Contains(b, "\n_Относится к строке 5._\n") || strings.Contains(b, "вне диффа") || strings.Contains(b, "_Источник: анализ Claude (") {
		t.Errorf("snapped:\n%s", b)
	}
}

// An open thread whose note carries no compat marker gets the id a new
// item of the same source, path and title would get: the item of this run
// is listed once.
func TestCompatListOpenThreadWithoutMarker(t *testing.T) {
	id := compat.ID(compat.SourceBuf, "a.proto", "field deleted")
	if gotID, src := openCompatID(mrthread.OpenCompat{Source: compat.SourceBuf, Path: "a.proto", Title: "field deleted"}); gotID != id || src != compat.SourceBuf {
		t.Fatalf("%q %q", gotID, src)
	}
	cr := &Run{Enabled: true,
		Items: []Item{{ID: id, Source: compat.SourceBuf, Path: "a.proto", Title: "field deleted"}},
		Remaining: []mrthread.OpenCompat{
			{Source: compat.SourceBuf, Path: "a.proto", Title: "field deleted"},
			{Source: compat.SourceBuf, Path: "b.proto", Line: 4, Title: "enum value removed"},
		}}
	sum := RenderSection(cr)
	if strings.Count(sum, "field deleted") != 1 || !strings.Contains(sum, "**Найдено 2 несовместимых изменения**") ||
		!strings.Contains(sum, "**buf** · `b.proto:4` — enum value removed _(найдено ранее, тред открыт)_") {
		t.Errorf("section:\n%s", sum)
	}
}

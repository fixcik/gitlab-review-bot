package mrthread

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

const botID = 99

func isBot(n core.Note) bool { return n.AuthorID == botID }

func botNote(id int64, body string) core.Note { return core.Note{ID: id, AuthorID: botID, Body: body} }

func TestCIThreadOfOnlyBotFirstNote(t *testing.T) {
	ci := marker.CIThread(12, "abc", 3, false) + "\nexplained"
	for name, d := range map[string]core.Discussion{
		"no notes":      {ID: "d"},
		"system note":   {ID: "d", Notes: []core.Note{{ID: 1, AuthorID: botID, System: true, Body: ci}}},
		"person's note": {ID: "d", Notes: []core.Note{{ID: 1, AuthorID: 5, Body: ci}}},
		"not a CI note": {ID: "d", Notes: []core.Note{botNote(1, marker.Finding("ab")+"\nx")}},
	} {
		if th := CIThreadOf(d, isBot); th != nil {
			t.Errorf("%s: %+v", name, th)
		}
	}
	th := CIThreadOf(core.Discussion{ID: "d", Resolved: true, Notes: []core.Note{botNote(7, ci)}}, isBot)
	if th == nil || th.DiscussionID != "d" || th.NoteID != 7 || th.Pipeline != 12 || th.FP != "abc" || th.Explained != 3 || th.Green || !th.Resolved {
		t.Fatalf("ci thread: %+v", th)
	}
	if th.Active() || !th.About(12, "abc") || th.About(13, "abc") || th.About(12, "abd") {
		t.Fatalf("active/about: %+v", th)
	}
}

// The same finding id posted twice (a re-post after a lost publish): the
// first open thread is the finding's thread.
func TestScanKeepsFirstThreadOfAFinding(t *testing.T) {
	body := marker.Finding("f1") + "\n" + marker.Blocking + "\n**Title**\n\nbody"
	ds := []core.Discussion{
		{ID: "first", Notes: []core.Note{botNote(1, body)}},
		{ID: "second", Notes: []core.Note{botNote(2, body)}},
	}
	th := Scan(ds, 0, isBot)
	if len(th.OpenOrder) != 1 || th.Open["f1"].DiscussionID != "first" || !th.Open["f1"].Blocking {
		t.Fatalf("open %+v order %v", th.Open, th.OpenOrder)
	}
}

func TestNoteLocationOfDeletedFile(t *testing.T) {
	n := core.Note{Position: &core.Position{OldPath: "gone.go", OldLine: 8, NewLine: 0}}
	if p, l := NoteLocation(n); p != "gone.go" || l != 8 {
		t.Fatalf("NoteLocation = %s:%d", p, l)
	}
	if p, l := NotePathLine(n); p != "gone.go" || l != 0 {
		t.Fatalf("NotePathLine = %s:%d", p, l)
	}
	if p, l := NoteLocation(core.Note{}); p != "" || l != 0 {
		t.Fatalf("general note: %s:%d", p, l)
	}
	if p, l := NoteLocation(core.Note{Position: &core.Position{OldPath: "a.go", NewPath: "b.go", OldLine: 1, NewLine: 2}}); p != "b.go" || l != 2 {
		t.Fatalf("new side: %s:%d", p, l)
	}
}

func TestParseCompatNote(t *testing.T) {
	for body, want := range map[string]string{
		"":                                  "",
		"<!-- reviewbot:compat -->\n\n  \n": "",
		"<!-- m -->\n🧩 **api** · removed field `x`\nmore": "removed field `x`",
		"<!-- m -->\n  plain title  \nmore":               "plain title",
	} {
		if got := ParseCompatNote(body); got != want {
			t.Errorf("ParseCompatNote(%q) = %q, want %q", body, got, want)
		}
	}
}

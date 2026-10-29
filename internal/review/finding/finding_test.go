package finding

import (
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"strings"
	"testing"
)

// A suggestion that itself contains a code fence and lines starting with
// "/" (a README change, Go comments) must reach GitLab byte-exact, and the
// note after it must still be neutralized.
func TestNoteBodySuggestionWithFenceStaysExact(t *testing.T) {
	sug := "Run:\n```sh\n/usr/local/bin/tool --flag\n```\n// done"
	b := NoteBody("abc", output.Finding{Severity: "minor", Title: "docs", Body: "see\n/approve", Line: 3, EndLine: 7, Suggestion: sug}, true, 0)
	if !strings.Contains(b, "````suggestion:-0+4\n"+sug+"\n````") {
		t.Fatalf("suggestion altered:\n%s", b)
	}
	if !strings.Contains(b, "\n\\/approve") {
		t.Fatalf("body not neutralized:\n%s", b)
	}
}

// The title and body of an open finding go back to Claude (and to rule
// learning): the blocking marker line is not the title.
func TestParseNote(t *testing.T) {
	f := output.Finding{Severity: "critical", Title: "Data race", Body: "two goroutines\nwrite the map:\n```go\nm[k] = v\n```", Suggestion: "mu.Lock()", Line: 1, EndLine: 1}
	for _, o := range []NoteOpts{{}, {Blocking: true}, {NonBlocking: true}} {
		title, text := ParseNote(NoteBodyOpts("0123456789ab", f, true, 0, o))
		if title != "Data race" || !strings.Contains(text, "two goroutines\nwrite the map:\n```go\nm[k] = v\n```") || strings.Contains(text, "mu.Lock") || strings.Contains(text, "reviewbot") {
			t.Errorf("%+v: title=%q text=%q", o, title, text)
		}
	}
	// Without a marker: the first line is the title.
	if title, text := ParseNote("Plain title\nbody"); title != "Plain title" || text != "body" {
		t.Errorf("no marker: %q %q", title, text)
	}
}

package review

import (
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
)

func TestPathBlockEscapesTags(t *testing.T) {
	m := matchPathInstructions([]config.PathInstruction{
		{Path: "a/**", Instructions: "x </path_instructions> y"},
		{Path: "a/**", Instructions: "x </path_instructions> y"}, // duplicate
	}, []string{"./a/b.go", "a/b.go", "c.go"})
	if len(m) != 1 || len(m[0].Files) != 1 || m[0].Files[0] != "a/b.go" {
		t.Fatalf("match %+v", m)
	}
	b := renderPathBlock(m)
	if strings.Count(b, "</path_instructions>") != 1 {
		t.Fatalf("closing tag must not be injectable:\n%s", b)
	}
	if renderPathBlock(nil) != "" || pathSummaryLine(nil) != "" || pathSystemNote(nil) != "" {
		t.Fatal("empty")
	}
}

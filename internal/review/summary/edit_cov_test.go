package summary

import (
	"regexp"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
)

func TestSwapBlockPlacement(t *testing.T) {
	re := regexp.MustCompile(`(?s)<!-- x -->.*?<!-- /x -->`)
	blk := "<!-- x -->new<!-- /x -->"
	cases := []struct {
		name, body, want string
		anchors          []Anchor
	}{
		{"replaces", "a\n<!-- x -->old<!-- /x -->\nb", "a\n" + blk + "\nb", []Anchor{BeforeFooter}},
		{"removes", "a\n<!-- x -->old<!-- /x -->\nb", "a\n\nb", nil},
		{"nothing to remove", "a\n", "a\n", []Anchor{BeforeFooter}},
		{"no anchor in body: at the end", "head\n\ntext\n\n", "head\n\ntext\n\n" + blk, []Anchor{AfterTBD, BeforeCompat, BeforeFooter}},
		{"after TBD", "h\n" + marker.TBDStart + "t" + marker.TBDEnd + "\nrest", "h\n" + marker.TBDStart + "t" + marker.TBDEnd + "\n\n" + blk + "\nrest", []Anchor{AfterTBD}},
		{"before compat", "h\n" + CompatSectionTitle + "\nc", "h\n" + blk + "\n\n" + CompatSectionTitle + "\nc", []Anchor{AfterTBD, BeforeCompat}},
		{"before footer", "h\n---\nf", "h\n\n" + blk + "\n\n---\nf", []Anchor{BeforeCompat, BeforeFooter}},
	}
	for _, c := range cases {
		block := blk
		if c.name == "removes" || c.name == "nothing to remove" {
			block = ""
		}
		if got := SwapBlock(c.body, block, re, c.anchors...); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestWithRouteFooterWithoutCheckedLine(t *testing.T) {
	f := "---\n<sub>Команды: x</sub>\n"
	if got := WithRouteFooter(f, "route"); got != "---\n<sub>route</sub>\n\n<sub>Команды: x</sub>\n" {
		t.Fatalf("got %q", got)
	}
	if got := WithRouteFooter(f, ""); got != f {
		t.Fatalf("empty route changed the footer: %q", got)
	}
}

func TestInsertAfter(t *testing.T) {
	for _, c := range []struct {
		lines []string
		at    int
		want  string
	}{
		{[]string{"a", "b"}, 0, "a||X||b"},     // text follows: a blank line after the block
		{[]string{"a", "", "b"}, 0, "a||X||b"}, // a blank line follows already
		{[]string{"a"}, 0, "a||X"},             // at the end
	} {
		if got := strings.Join(InsertAfter(c.lines, c.at, "X"), "|"); got != c.want {
			t.Errorf("InsertAfter(%q, %d) = %q, want %q", c.lines, c.at, got, c.want)
		}
	}
}

package mdtext

import (
	"strings"
	"testing"
)

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{1: "a", 2: "b", 4: "b", 5: "c", 11: "c", 12: "c", 21: "a", 22: "b", 25: "c", 111: "c"} {
		if got := Plural(n, "a", "b", "c"); got != want {
			t.Errorf("plural(%d) = %s, want %s", n, got, want)
		}
	}
}

// Lines are left alone only inside a fenced code block that Markdown
// (CommonMark, which GitLab uses to find quick actions) really closes: a
// fence closes only with the same character, at least as long, and nothing
// after it. Anything else could make the bot skip a line GitLab executes.
func TestNeutralizeQuickActionsFences(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"ok\n/merge\n  /approve now\n```\n/keep\n```\na /b", "ok\n\\/merge\n  \\/approve now\n```\n/keep\n```\na /b"},
		{"~~~\n/keep\n~~~", "~~~\n/keep\n~~~"},
		// A backtick line does not close a tilde fence (and vice versa).
		{"~~~\n```\n~~~\n/approve", "~~~\n```\n~~~\n\\/approve"},
		// A shorter fence does not close a longer one (the bot's own
		// suggestion fences are longer than any backtick run inside).
		{"````suggestion\n```bash\n/usr/bin/x\n```\n````\n/approve", "````suggestion\n```bash\n/usr/bin/x\n```\n````\n\\/approve"},
		// Text after a fence: not a closing fence.
		{"```\n/a\n``` z\n/b\n```\n/c", "```\n/a\n``` z\n/b\n```\n\\/c"},
		// Backticks in the info string: inline code, not a fence.
		{"```x``` y\n/approve", "```x``` y\n\\/approve"},
		// Indented by four spaces: not a fence.
		{"    ```\n/approve\n    ```", "    ```\n\\/approve\n    ```"},
		// Indented by up to three spaces: a fence, closed likewise.
		{"   ```go\n/keep\n  ```\n/x", "   ```go\n/keep\n  ```\n\\/x"},
		// Never closed: escaped (regex-based quick action parsing only skips
		// closed blocks).
		{"```\n/unclosed", "```\n\\/unclosed"},
		{"```\n/a\n```  \n/b", "```\n/a\n```  \n\\/b"},
	} {
		if got := NeutralizeQuickActions(tc.in); got != tc.want {
			t.Errorf("neutralize(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestDefuseMentionsCodeSpans(t *testing.T) {
	live := func(s string) bool { return mentionRe.MatchString(s) }
	for _, tc := range []struct {
		in       string
		keepCode string // a code span that must stay byte-exact ("" none)
	}{
		{"run `make test and ping @devops and see #12", ""},       // unpaired backtick
		{"a ``x` @devops #12 `` b ` @ops", "``x` @devops #12 ``"}, // runs of different lengths
		{"\\`x @devops \\`", ""},                                  // escaped backticks
		{"<a title=\"`\">@devops `</a>", ""},                      // backtick inside an HTML tag
		{"`a\n@devops !3 `", ""},                                  // a code span never spans lines here
		{"ok `@team` and @devops", "`@team`"},                     // paired: code stays, text defused
		{"```\n@devops\n```", ""},                                 // a fence is not a span: defused
	} {
		out := DefuseMentions(tc.in)
		rest := out
		if tc.keepCode != "" {
			if !strings.Contains(out, tc.keepCode) {
				t.Errorf("%q: code span changed: %q", tc.in, out)
			}
			rest = strings.Replace(out, tc.keepCode, "", 1)
		}
		if live(rest) {
			t.Errorf("%q: live mention or reference left: %q", tc.in, out)
		}
	}
}

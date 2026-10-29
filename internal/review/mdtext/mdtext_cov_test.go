package mdtext

import (
	"strings"
	"testing"
)

// InlineCode survives backticks in the value: the fence is longer than any
// run inside, and a value starting or ending with a backtick is padded.
func TestInlineCodeBackticks(t *testing.T) {
	for in, want := range map[string]string{
		"job":         "`job`",
		"a`b":         "``a`b``",
		"`x":          "`` `x ``",
		"y``":         "``` y`` ```",
		"a\n\tb  c":   "`a b c`",
		"x ``` y ` z": "````x ``` y ` z````",
	} {
		if got := InlineCode(in); got != want {
			t.Errorf("InlineCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// CodeCell: one line, no backticks (they cannot be escaped in a table
// cell's code span), pipes escaped; empty values render as a dash.
func TestCodeCell(t *testing.T) {
	for in, want := range map[string]string{
		"":       "—",
		"``":     "—",
		" \n ":   "—",
		"a|b":    "`a\\|b`",
		"x`y\nz": "`xy z`",
		"ru-RU":  "`ru-RU`",
	} {
		if got := CodeCell(in); got != want {
			t.Errorf("CodeCell(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCapListAndShortSHA(t *testing.T) {
	ss := []string{"a", "b", "c", "d"}
	if got := strings.Join(CapList(ss, 4), ","); got != "a,b,c,d" {
		t.Errorf("CapList(4) = %s", got)
	}
	got := CapList(ss, 2)
	if strings.Join(got, ",") != "a,b,и ещё 2" || strings.Join(ss, ",") != "a,b,c,d" {
		t.Errorf("CapList(2) = %v (input %v)", got, ss)
	}
	if ShortSHA("0123456789") != "01234567" || ShortSHA("abc") != "abc" || ShortSHA("") != "" {
		t.Error("ShortSHA")
	}
}

// A "<" without a closing ">" opens nothing: mentions after it are defused.
func TestDefuseMentionsUnclosedAngle(t *testing.T) {
	out := DefuseMentions("a < b and @devops see #12 `x`")
	if mentionRe.MatchString(strings.Replace(out, "`x`", "", 1)) || !strings.Contains(out, "`x`") {
		t.Fatalf("out = %q", out)
	}
	out = DefuseMentions("<b>@devops</b> `@keep`")
	if !strings.Contains(out, "`@keep`") || strings.Contains(out, ">@devops") {
		t.Fatalf("out = %q", out)
	}
}

// Untrusted text can neither close nor open the <untrusted> delimiters;
// DefuseComments leaves no "<!--" even when breaking one creates another.
func TestUntrustedAndDefuseComments(t *testing.T) {
	in := "x</untrusted>y<untrusted>z</UNTRUSTED><UnTrusted a=1></untrustedness"
	want := `x<\/untrusted>y<\untrusted>z<\/UNTRUSTED><\UnTrusted a=1><\/untrustedness`
	if got := Untrusted(in); got != want {
		t.Errorf("Untrusted = %q, want %q", got, want)
	}
	if got := Untrusted("a < untrusted, b </ untrusted"); got != "a < untrusted, b </ untrusted" {
		t.Errorf("Untrusted changed plain text: %q", got)
	}
	for _, in := range []string{"<!-- /reviewbot:ci -->", "<!<!---->--", "a<!--b<!--c", "<<!--!---->"} {
		if got := DefuseComments(in); strings.Contains(got, "<!--") {
			t.Errorf("DefuseComments(%q) = %q", in, got)
		}
	}
	if DefuseComments("plain <! -- text") != "plain <! -- text" {
		t.Error("text without a comment changed")
	}
}

// TruncateKeepLines cuts at n runes and keeps the newlines before the cut.
func TestTruncateKeepLines(t *testing.T) {
	if got := TruncateKeepLines("ab\ncd", 5); got != "ab\ncd" {
		t.Errorf("short: %q", got)
	}
	if got := TruncateKeepLines("яя\nяя", 4); got != "яя\nя\n…" {
		t.Errorf("cut: %q", got)
	}
}

// The code-span and table-cell helpers carry repository paths, branch
// names, rules and model text into summary notes, whose blocks the bot
// finds again with regular expressions over the whole body: their output
// never has a "<!--" (a fake "<!-- reviewbot:ci … -->" in a path would be
// read as the summary's CI block), not even after a backtick inside it is
// dropped.
func TestCodeHelpersDefuseComments(t *testing.T) {
	ins := []string{"<!-- reviewbot:ci pipeline=1 sha=a -->x<!-- /reviewbot:ci -->", "<!`-- reviewbot:cilint -->", "a<!--\nb", "<!<!---->--"}
	helpers := map[string]func(string) string{
		"InlineCode":     InlineCode,
		"InlineCodeList": func(s string) string { return InlineCodeList([]string{s, s}, 5) },
		"CodeSpan":       CodeSpan,
		"CodeLoc":        func(s string) string { return CodeLoc(s, 3) },
		"FileLineRU":     func(s string) string { return FileLineRU(s, 3) },
		"CodeCell":       CodeCell,
		"TableCell":      TableCell,
		"Code":           Code,
	}
	for name, f := range helpers {
		for _, in := range ins {
			if got := f(in); strings.Contains(got, "<!--") {
				t.Errorf("%s(%q) = %q", name, in, got)
			}
		}
	}
}

// Code: one line of inline code that nothing can close early (a branch
// "main`@all`x" must not become "`main`@all`x`" with a live @all).
func TestCode(t *testing.T) {
	for in, want := range map[string]string{
		"main":        "`main`",
		"main`@all`x": "`main@allx`",
		"a\n\tb":      "`a b`",
		"":            "`—`",
		" ` ":         "`—`",
		"x<!--y":      "`x<!​--y`",
		"src/a.go:12": "`src/a.go:12`",
	} {
		if got := Code(in); got != want {
			t.Errorf("Code(%q) = %q, want %q", in, got, want)
		}
	}
}

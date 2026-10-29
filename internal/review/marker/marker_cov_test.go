package marker

import "testing"

// SetSummarySHA re-points the summary marker (any old sha, spacing) and
// leaves a body without one unchanged.
func TestSetSummarySHA(t *testing.T) {
	for in, want := range map[string]string{
		"<!-- reviewbot:summary sha=abc -->\nbody":   Summary("def") + "\nbody",
		"x\n<!--reviewbot:summary  sha= -->":         "x\n" + Summary("def"),
		"no marker":                                  "no marker",
		"<!-- reviewbot:summary sha=a b -->":         "<!-- reviewbot:summary sha=a b -->",
		"<!-- reviewbot:finding id=0123456789ab -->": "<!-- reviewbot:finding id=0123456789ab -->",
	} {
		if got := SetSummarySHA(in, "def"); got != want {
			t.Errorf("SetSummarySHA(%q) = %q, want %q", in, got, want)
		}
	}
}

// ReplaceBlock swaps start..end (markers included) for block; without both
// markers in that order the body is unchanged.
func TestReplaceBlock(t *testing.T) {
	const s, e = "<!-- s -->", "<!-- e -->"
	for in, want := range map[string]string{
		"a" + s + "old" + e + "b":         "aNEWb",
		"a" + s + "old":                   "a" + s + "old",
		"a" + e + "old" + s + "b":         "a" + e + "old" + s + "b",
		"no markers":                      "no markers",
		s + "1" + e + "mid" + s + "2" + e: "NEWmid" + s + "2" + e,
		"x" + e + s + "y" + e + "z":       "x" + e + "NEWz",
	} {
		if got := ReplaceBlock(in, s, e, "NEW"); got != want {
			t.Errorf("ReplaceBlock(%q) = %q, want %q", in, got, want)
		}
	}
}

// Thread markers parse only from the marker header: the same marker inside
// untrusted text below it (a model answer, a linter report) is ignored.
// Ids are lower-cased.
func TestThreadMarkersHeaderOnly(t *testing.T) {
	for _, tc := range []struct {
		body       string
		id, second string
		ok         bool
		parse      func(string) (string, string, bool)
	}{
		{Compat("ABCDEF12", "oasdiff") + "\ntext", "abcdef12", "oasdiff", true, ParseCompat},
		{"text\n" + Compat("abcdef12", "oasdiff"), "", "", false, ParseCompat},
		{Lint("0A1B", "golangci-lint:v2") + "\n" + Blocking + "\nx", "0a1b", "golangci-lint:v2", true, ParseLint},
		{"> quoted\n" + Lint("0a1b", "x"), "", "", false, ParseLint},
		{Finding("0123456789ab") + "\n", "", "", false, ParseLint},
	} {
		id, second, ok := tc.parse(tc.body)
		if id != tc.id || second != tc.second || ok != tc.ok {
			t.Errorf("%q: %q %q %v", tc.body, id, second, ok)
		}
	}
	if id, ok := ParseFinding(" " + Finding("ABCDEF") + "\nx"); !ok || id != "abcdef" {
		t.Errorf("finding: %q %v", id, ok)
	}
	if !IsSummaryNote(Summary("abc")+"\nbody") || IsSummaryNote("body\n"+Summary("abc")) {
		t.Error("IsSummaryNote")
	}
	if FP("") != "-" || FP("ab") != "ab" {
		t.Error("FP")
	}
	if got := CIThread(7, "", 3, true); !CIThreadRe.MatchString(got) || got != "<!-- reviewbot:ci-explain pipeline=7 fp=- explained=3 state=green -->" {
		t.Errorf("CIThread = %q", got)
	}
}

// StripBot removes every marker, including ones reassembled when an inner
// marker goes; StripNoteMarkers removes the thread and summary markers.
func TestStripMarkers(t *testing.T) {
	for in, want := range map[string]string{
		"<!-- reviewbot:fixed -->hi":                              "hi",
		"<!-<!--reviewbot:x-->- reviewbot:ci a -->text":           "text",
		"a <!--\n/reviewbot:ci\n--> b":                            "a  b",
		"<!-- other comment --> kept":                             "<!-- other comment --> kept",
		"<!-<!-<!--reviewbot:a-->-reviewbot:b-->-reviewbot:c-->z": "z",
	} {
		if got := StripBot(in); got != want {
			t.Errorf("StripBot(%q) = %q, want %q", in, got, want)
		}
	}
	in := Finding("ab") + "\n" + Compat("cd", "x") + Lint("ef", "t") + Summary("01") + "\n body \n"
	if got := StripNoteMarkers(in); got != "body" {
		t.Errorf("StripNoteMarkers = %q", got)
	}
}

// The marker header ends at the first line that is not a comment, a blank
// line included: a marker after it is untrusted text.
func TestHeaderEndsAtBlankLine(t *testing.T) {
	body := Finding("ab") + "\n\n" + Fixed + "\n" + Blocking
	if got := Header(body); got != Finding("ab")+"\n" {
		t.Fatalf("Header = %q", got)
	}
	if HasFixed(body) || HasBlocking(body) {
		t.Fatal("marker after a blank line recognized")
	}
	if got := Header("  " + Fixed + "\n\t" + Blocking + "\ntext"); !HasFixed(got) || !HasBlocking(got) {
		t.Fatalf("indented markers: %q", got)
	}
}

package tasklink

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractKeysBoundariesAndFallback(t *testing.T) {
	for _, tc := range []struct {
		text     string
		prefixes []string
		want     []string
	}{
		// A rejected candidate does not stop the scan.
		{"aPROJ-1 PROJ-2", nil, []string{"PROJ-2"}},
		{"OPS-1 PROJ-2", []string{"PROJ"}, []string{"PROJ-2"}},
		// Right boundary: any letter or digit glued to the number.
		{"PROJ-1A PROJ-1Z PROJ-1a", nil, nil},
		// Configured prefix after "_" inside a longer upper-case token.
		{"OLD_PROJ-1", []string{"PROJ"}, []string{"PROJ-1"}},
		{"FEATURE_X_PROJ-5", []string{"PROJ"}, []string{"PROJ-5"}},
		{"OLD__PROJ-6", []string{"PROJ"}, []string{"PROJ-6"}},
		{"OLD_OPS-1", []string{"PROJ"}, nil},
		{"OLD_PROJ-1", []string{"OPS", "PROJ"}, []string{"PROJ-1"}},
	} {
		if got := ExtractKeys(tc.text, tc.prefixes); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ExtractKeys(%q, %v) = %v, want %v", tc.text, tc.prefixes, got, tc.want)
		}
	}
}

func TestKeyPrefixAndAllowed(t *testing.T) {
	for in, want := range map[string]string{"PROJ-123": "PROJ", "MY_P-1": "MY_P", "NOKEY": "", "-1": "", "": ""} {
		if got := KeyPrefix(in); got != want {
			t.Errorf("KeyPrefix(%q) = %q, want %q", in, got, want)
		}
	}
	for _, k := range []string{"proj-1", "PROJ-", "PROJ", "C-1", "PROJ-1a"} {
		if Allowed(k, nil) {
			t.Errorf("Allowed(%q, nil) = true", k)
		}
	}
	if !Allowed("PROJ-1", nil) || Allowed("OPS-1", []string{"PROJ"}) || !Allowed("OPS-1", []string{"PROJ", "OPS"}) {
		t.Error("Allowed prefix filter")
	}
}

func TestKeysCap(t *testing.T) {
	found := []Found{{"A1-1", WhereTitle}, {"A1-2", WhereBranch}, {"A1-3", WhereCommits}, {"A1-4", WhereCommits}}
	if got := Keys(found, 0); len(got) != 4 {
		t.Errorf("n=0 must return all: %v", got)
	}
	if got := Keys(found, MaxIssues); strings.Join(got, ",") != "A1-1,A1-2,A1-3" {
		t.Errorf("cap: %v", got)
	}
}

func TestBuildHintKeyChoice(t *testing.T) {
	base := HintInput{Types: types, ProjectKeys: []string{"PROJ"}, RequireKey: true}

	// A key of a foreign project in the title does not count.
	in := base
	in.Title, in.Found = "feat: x (OPS-1)", []Found{{"PROJ-7", WhereBranch}}
	h := BuildHint(in)
	if h.Empty() || h.SuggestedTitle != "feat: x (PROJ-7)" {
		t.Errorf("foreign key in title: %+v", h)
	}

	// The first key found outside the title is the one suggested.
	in = base
	in.Title, in.Found = "fix: x", []Found{{"PROJ-7", WhereBranch}, {"PROJ-8", WhereCommits}}
	h = BuildHint(in)
	if h.SuggestedTitle != "fix: x (PROJ-7)" || !strings.Contains(strings.Join(h.Messages, "\n"), "в названии ветки") {
		t.Errorf("first non-title key: %+v", h)
	}
}

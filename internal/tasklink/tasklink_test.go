package tasklink

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
)

var types = config.DefaultTitleTypes

func TestExtractKeys(t *testing.T) {
	for _, tc := range []struct {
		text     string
		prefixes []string
		want     []string
	}{
		{"feat: cache (PROJ-123)", nil, []string{"PROJ-123"}},
		{"PROJ-1 and PROJ-1 again, OPS-77", nil, []string{"PROJ-1", "OPS-77"}},
		{"PROJ-1 OPS-77", []string{"PROJ"}, []string{"PROJ-1"}},
		{"feature/PROJ-15-add-cache", nil, []string{"PROJ-15"}},
		{"feature_PROJ-15_cache", []string{"PROJ"}, []string{"PROJ-15"}},
		{"MY_PROJ-9", nil, []string{"MY_PROJ-9"}},
		{"lowercase proj-1 is not a key", nil, nil},
		{"aPROJ-1 PROJ-1b X-1", nil, nil},
		// Without configured prefixes anything key-shaped matches.
		{"UTF-8", nil, []string{"UTF-8"}},
		{"A1-5", nil, []string{"A1-5"}},
		{"see https://jira.example.com/browse/PROJ-42.", nil, []string{"PROJ-42"}},
		{"version ISO-8601 is not an issue", []string{"PROJ"}, nil},
		{"", nil, nil},
		{"PROJ-", nil, nil},
		{"(PROJ-7, PROJ-8)", []string{"PROJ"}, []string{"PROJ-7", "PROJ-8"}},
	} {
		got := ExtractKeys(tc.text, tc.prefixes)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ExtractKeys(%q, %v) = %v, want %v", tc.text, tc.prefixes, got, tc.want)
		}
	}
}

func TestStripDraft(t *testing.T) {
	for in, want := range map[string]string{
		"Draft: feat: x":           "feat: x",
		"draft:feat: x":            "feat: x",
		"[Draft] feat: x":          "feat: x",
		"(Draft) feat: x":          "feat: x",
		"WIP: feat: x":             "feat: x",
		"[WIP] Draft: feat: x":     "feat: x",
		"  feat: x  ":              "feat: x",
		"feat: drafting the draft": "feat: drafting the draft",
		"Drafts: x":                "Drafts: x",
	} {
		if got := StripDraft(in); got != want {
			t.Errorf("StripDraft(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTitle(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Title
	}{
		{"feat: описание (PROJ-123)", Title{Valid: true, Type: "feat", Description: "описание", Key: "PROJ-123", Keys: []string{"PROJ-123"}}},
		{"Draft: fix(auth)!: вход по SSO (PROJ-5)", Title{Valid: true, Type: "fix", Scope: "auth", Breaking: true, Description: "вход по SSO", Key: "PROJ-5", Keys: []string{"PROJ-5"}}},
		{"feat: две задачи (PROJ-1, PROJ-2)", Title{Valid: true, Type: "feat", Description: "две задачи", Key: "PROJ-1", Keys: []string{"PROJ-1", "PROJ-2"}}},
		{"feat: без ключа", Title{Valid: true, Type: "feat", Description: "без ключа"}},
		{"Add cache", Title{Description: "Add cache"}},
		{"Add cache (PROJ-9)", Title{Description: "Add cache", Key: "PROJ-9", Keys: []string{"PROJ-9"}}},
		{"feature: x (PROJ-1)", Title{Type: "feature", Description: "x", Key: "PROJ-1", Keys: []string{"PROJ-1"}}},
		{"Feat: x", Title{Type: "Feat", Description: "x"}},
		{"feat(): x", Title{Type: "feat", Description: "x"}},
		{"feat:x", Title{Description: "feat:x"}},
		{"feat: (PROJ-1)", Title{Description: "feat:", Key: "PROJ-1", Keys: []string{"PROJ-1"}}},
		{"PROJ-1 feat: x", Title{Description: "PROJ-1 feat: x"}},
	} {
		got := ParseTitle(tc.in, types)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseTitle(%q) =\n %+v\nwant\n %+v", tc.in, got, tc.want)
		}
	}
}

func TestFindPriorityAndDedupe(t *testing.T) {
	src := Sources{
		Title:       "feat: x (PROJ-3)",
		Branch:      "feature/proj-1-cache",
		Description: "Closes PROJ-2, relates to PROJ-3 and OPS-1",
		Commits:     []string{"fix: y (PROJ-4)\n\nAlso PROJ-1", "chore: z PROJ-5"},
	}
	got := Find(src, []string{"PROJ"})
	want := []Found{
		{"PROJ-3", WhereTitle}, {"PROJ-1", WhereBranch}, {"PROJ-2", WhereDescription},
		{"PROJ-4", WhereCommits}, {"PROJ-5", WhereCommits},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if k := Keys(got, MaxIssues); strings.Join(k, ",") != "PROJ-3,PROJ-1,PROJ-2" {
		t.Fatalf("keys %v", k)
	}
	// Without configured prefixes lowercase branches are not matched (too
	// many false positives such as "utf-8").
	if got := Find(Sources{Branch: "feature/proj-1"}, nil); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	if got := Find(Sources{}, nil); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestBuildHint(t *testing.T) {
	keys := []string{"PROJ"}
	for _, tc := range []struct {
		name      string
		in        HintInput
		empty     bool
		suggested string
		contains  []string
	}{
		{
			name:  "valid with key",
			in:    HintInput{Title: "feat: кэш (PROJ-1)", Found: []Found{{"PROJ-1", WhereTitle}}},
			empty: true,
		},
		{
			name:  "valid with key, draft",
			in:    HintInput{Title: "Draft: feat: кэш (PROJ-1)", Found: []Found{{"PROJ-1", WhereTitle}}},
			empty: true,
		},
		{
			name:      "key only in branch",
			in:        HintInput{Title: "feat(cache): LRU-кэш", Found: []Found{{"PROJ-7", WhereBranch}}},
			suggested: "feat(cache): LRU-кэш (PROJ-7)",
			contains:  []string{"**PROJ-7**", "в названии ветки"},
		},
		{
			name:      "key only in commits",
			in:        HintInput{Title: "fix: x", Found: []Found{{"PROJ-8", WhereCommits}}},
			suggested: "fix: x (PROJ-8)",
			contains:  []string{"в сообщениях коммитов"},
		},
		{
			name:      "no key anywhere",
			in:        HintInput{Title: "feat: x"},
			suggested: "feat: x (PROJ-?)",
			contains:  []string{"не найден ключ задачи"},
		},
		{
			name:      "key in title but not at the end",
			in:        HintInput{Title: "feat: PROJ-4 cache", Found: []Found{{"PROJ-4", WhereTitle}}},
			suggested: "feat: PROJ-4 cache (PROJ-4)",
			contains:  []string{"в конце заголовка"},
		},
		{
			name:      "invalid format, claude suggestion",
			in:        HintInput{Title: "Add cache", Found: []Found{{"PROJ-1", WhereBranch}}, Suggestion: &Suggestion{Type: "feat", Scope: "cache", Description: "добавить LRU-кэш"}},
			suggested: "feat(cache): добавить LRU-кэш (PROJ-1)",
			contains:  []string{"Conventional Commits", "feat, fix", "PROJ-123", "в названии ветки"},
		},
		{
			name:      "invalid format with key at end, no suggestion",
			in:        HintInput{Title: "Add cache (PROJ-1)", Found: []Found{{"PROJ-1", WhereTitle}}},
			suggested: "type: описание (PROJ-1)",
			contains:  []string{"Conventional Commits"},
		},
		{
			name:      "alias type",
			in:        HintInput{Title: "feature: cache (PROJ-1)", Found: []Found{{"PROJ-1", WhereTitle}}},
			suggested: "feat: cache (PROJ-1)",
		},
		{
			name:      "bad claude suggestion type ignored",
			in:        HintInput{Title: "Add cache", Suggestion: &Suggestion{Type: "feature", Description: "x"}},
			suggested: "type: описание (PROJ-?)",
		},
		{
			name:      "claude suggestion sanitized",
			in:        HintInput{Title: "x", Suggestion: &Suggestion{Type: " FEAT ", Description: "a `b`\n(c)"}},
			suggested: "feat: a 'b' c (PROJ-?)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			in.Types, in.ProjectKeys, in.RequireKey = types, keys, true
			h := BuildHint(in)
			if h.Empty() != tc.empty {
				t.Fatalf("empty = %v: %+v", h.Empty(), h)
			}
			if tc.empty {
				return
			}
			if h.SuggestedTitle != tc.suggested {
				t.Errorf("suggested %q, want %q", h.SuggestedTitle, tc.suggested)
			}
			all := strings.Join(h.Messages, "\n")
			for _, s := range tc.contains {
				if !strings.Contains(all, s) {
					t.Errorf("messages lack %q: %s", s, all)
				}
			}
			// Suggested titles are always valid when a real type is known.
			if !strings.HasPrefix(h.SuggestedTitle, "type") {
				if p := ParseTitle(h.SuggestedTitle, types); !p.Valid || p.Key == "" && !strings.Contains(h.SuggestedTitle, "-?") {
					t.Errorf("suggested title does not parse: %+v", p)
				}
			}
		})
	}
}

func TestBuildHintKeyNotRequired(t *testing.T) {
	h := BuildHint(HintInput{Title: "feat: x", Types: types, RequireKey: false})
	if !h.Empty() {
		t.Fatalf("%+v", h)
	}
	h = BuildHint(HintInput{Title: "Add x", Types: types, RequireKey: false, Found: []Found{{"PROJ-1", WhereBranch}}})
	if h.Empty() || h.SuggestedTitle != "type: описание (PROJ-1)" || len(h.Messages) != 1 {
		t.Fatalf("%+v", h)
	}
}

func TestPlaceholderAndNeedsSuggestion(t *testing.T) {
	if Placeholder(nil) != "KEY-?" || Placeholder([]string{"OPS", "PROJ"}) != "OPS-?" {
		t.Fatal("placeholder")
	}
	if NeedsSuggestion("feat: x", types) || !NeedsSuggestion("Add x (PROJ-1)", types) {
		t.Fatal("needs suggestion")
	}
}

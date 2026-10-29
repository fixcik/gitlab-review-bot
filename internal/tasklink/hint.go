package tasklink

import (
	"fmt"
	"slices"
	"strings"
)

// Suggestion is a proposed title without the key part (it comes from
// Claude or from the current title).
type Suggestion struct {
	Type        string `json:"type"`
	Scope       string `json:"scope,omitempty"`
	Description string `json:"description"`
}

// HintInput is everything the title hint depends on.
type HintInput struct {
	Title       string   // current MR title (draft prefixes allowed)
	Found       []Found  // result of Find for the MR
	Types       []string // allowed Conventional Commits types
	ProjectKeys []string // configured key prefixes (for the placeholder)
	RequireKey  bool     // require the issue key in the title
	// Suggestion from Claude; used when the title is not in the expected
	// format. May be nil.
	Suggestion *Suggestion
}

// Hint is the soft title advice shown in the summary note.
type Hint struct {
	// Messages are Russian sentences; empty means the title is fine.
	Messages []string
	// SuggestedTitle is a ready-to-use title; empty when Messages is empty.
	SuggestedTitle string
}

// Empty reports whether there is nothing to say about the title.
func (h Hint) Empty() bool { return len(h.Messages) == 0 }

// NeedsSuggestion reports whether Claude should be asked for a title
// suggestion: only when the title is not a valid Conventional Commits title
// (a valid title is reused as is, with the key appended by the bot).
func NeedsSuggestion(title string, types []string) bool {
	return !ParseTitle(title, types).Valid
}

// Placeholder is the key part used when no issue key is known:
// "<first project key>-?" or "KEY-?".
func Placeholder(projectKeys []string) string { return keyPrefixFor(projectKeys) + "-?" }

// exampleKey is the key shown in the format description ("PROJ-123").
func exampleKey(projectKeys []string) string { return keyPrefixFor(projectKeys) + "-123" }

// keyPrefixFor is the first configured project key, or "KEY".
func keyPrefixFor(projectKeys []string) string {
	if len(projectKeys) > 0 && projectKeys[0] != "" {
		return projectKeys[0]
	}
	return "KEY"
}

// WhereRU names a source in Russian ("в названии ветки").
func WhereRU(where string) string {
	switch where {
	case WhereTitle:
		return "в заголовке"
	case WhereBranch:
		return "в названии ветки"
	case WhereDescription:
		return "в описании MR"
	case WhereCommits:
		return "в сообщениях коммитов"
	default:
		return where
	}
}

// BuildHint computes the title hint:
//   - valid title with the key at the end → no hint;
//   - key missing from the title but found elsewhere → ask to put it into
//     the title;
//   - no key anywhere → ask to add the task key (placeholder in the
//     suggested title);
//   - title not in Conventional Commits format → explain the format.
//
// The suggested title is built from the current title when it is valid,
// otherwise from in.Suggestion, otherwise from the parsable part of the
// title, otherwise a generic "type: описание (KEY)" template.
func BuildHint(in HintInput) Hint {
	t := ParseTitle(in.Title, in.Types)
	var titleKeys []string
	for _, k := range t.Keys {
		if Allowed(k, in.ProjectKeys) {
			titleKeys = append(titleKeys, k)
		}
	}
	var msgs []string
	keyPart := strings.Join(titleKeys, ", ")

	if in.RequireKey && len(titleKeys) == 0 {
		var inTitle, elsewhere *Found
		for i := range in.Found {
			f := &in.Found[i]
			if f.Where == WhereTitle && inTitle == nil {
				inTitle = f
			} else if f.Where != WhereTitle && elsewhere == nil {
				elsewhere = f
			}
		}
		switch {
		case inTitle != nil:
			keyPart = inTitle.Key
			msgs = append(msgs, fmt.Sprintf("Ключ задачи **%s** нужно указать в конце заголовка в скобках.", inTitle.Key))
		case elsewhere != nil:
			keyPart = elsewhere.Key
			msgs = append(msgs, fmt.Sprintf("Ключ задачи **%s** найден %s, но не в заголовке. Добавьте его в конец заголовка в скобках.", elsewhere.Key, WhereRU(elsewhere.Where)))
		default:
			keyPart = Placeholder(in.ProjectKeys)
			msgs = append(msgs, "В MR не найден ключ задачи Jira (ни в заголовке, ни в ветке, описании или коммитах). Добавьте ключ задачи в конец заголовка в скобках.")
		}
	}
	if keyPart == "" && len(in.Found) > 0 {
		keyPart = in.Found[0].Key
	}
	if !t.Valid {
		msgs = append([]string{fmt.Sprintf("Заголовок не соответствует формату Conventional Commits `тип(область): описание (%s)`. Допустимые типы: %s.",
			exampleKey(in.ProjectKeys), strings.Join(in.Types, ", "))}, msgs...)
	}
	if len(msgs) == 0 {
		return Hint{}
	}
	return Hint{Messages: msgs, SuggestedTitle: suggest(t, in, keyPart)}
}

// typeAliases maps common non-standard types to Conventional Commits ones.
var typeAliases = map[string]string{
	"feature": "feat", "features": "feat", "add": "feat",
	"bug": "fix", "bugfix": "fix", "hotfix": "fix", "fixes": "fix",
	"doc": "docs", "tests": "test", "refactoring": "refactor",
	"performance": "perf", "styles": "style",
}

func suggest(t Title, in HintInput, keyPart string) string {
	switch {
	case t.Valid:
		return Format(t.Type, t.Scope, t.Breaking, t.Description, keyPart)
	case in.Suggestion != nil && validSuggestion(*in.Suggestion, in.Types):
		s := in.Suggestion
		return Format(strings.ToLower(strings.TrimSpace(s.Type)), cleanText(s.Scope), false, cleanText(s.Description), keyPart)
	case t.Type != "" && t.Description != "":
		typ := strings.ToLower(t.Type)
		if a, ok := typeAliases[typ]; ok {
			typ = a
		}
		if !slices.Contains(in.Types, typ) {
			typ = "type"
		}
		return Format(typ, t.Scope, t.Breaking, t.Description, keyPart)
	default:
		return Format("type", "", false, "описание", keyPart)
	}
}

func validSuggestion(s Suggestion, types []string) bool {
	return slices.Contains(types, strings.ToLower(strings.TrimSpace(s.Type))) && cleanText(s.Description) != ""
}

// cleanText makes model-provided text safe for a one-line title in a code
// span: one line, no backticks or parentheses that would break parsing.
func cleanText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer("`", "'", "(", "", ")", "").Replace(s)
	if r := []rune(s); len(r) > 120 {
		s = strings.TrimSpace(string(r[:120]))
	}
	return s
}

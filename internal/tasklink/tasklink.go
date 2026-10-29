// Package tasklink links merge requests to Jira issues by keys found in the
// MR title, source branch, description and commit messages, and checks the
// MR title format: Conventional Commits with the issue key in parentheses at
// the end, e.g. "feat(auth): вход по SSO (PROJ-123)".
//
// Everything here is a pure function.
package tasklink

import (
	"regexp"
	"slices"
	"strings"
)

// MaxIssues is how many linked issues the bot uses at most.
const MaxIssues = 3

// Where a key was found.
const (
	WhereTitle       = "title"
	WhereBranch      = "branch"
	WhereDescription = "description"
	WhereCommits     = "commits"
)

// candidateRe finds key-like tokens. Boundaries are checked by hand so that
// "_" counts as a separator around a key ("PROJ-123_fix", "feature_PROJ-1"),
// which a plain \b would not allow.
var candidateRe = regexp.MustCompile(`[A-Z][A-Z0-9_]*[A-Z0-9]-[0-9]+`)

// keyRe validates a whole key.
var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]+-[0-9]+$`)

// ValidKey reports whether s is a well-formed issue key such as "PROJ-123".
func ValidKey(s string) bool { return keyRe.MatchString(s) }

// KeyPrefix returns the project part of a key ("PROJ" for "PROJ-123").
func KeyPrefix(key string) string {
	if i := strings.LastIndexByte(key, '-'); i > 0 {
		return key[:i]
	}
	return ""
}

// Allowed reports whether key's prefix is in prefixes (any prefix when
// prefixes is empty).
func Allowed(key string, prefixes []string) bool {
	if !ValidKey(key) {
		return false
	}
	return len(prefixes) == 0 || slices.Contains(prefixes, KeyPrefix(key))
}

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// ExtractKeys returns the distinct issue keys in text, in order of first
// appearance. Keys have the form PREFIX-123 where PREFIX starts with an
// uppercase letter and may contain uppercase letters, digits and "_". Only
// prefixes from prefixes are accepted; an empty list accepts any.
func ExtractKeys(text string, prefixes []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, loc := range candidateRe.FindAllStringIndex(text, -1) {
		s, e := loc[0], loc[1]
		if s > 0 && isAlnum(text[s-1]) {
			continue
		}
		if e < len(text) && isAlnum(text[e]) {
			continue
		}
		key := text[s:e]
		if !Allowed(key, prefixes) {
			if key = allowedAfterUnderscore(key, prefixes); key == "" {
				continue
			}
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// allowedAfterUnderscore returns the first part of cand after an "_" that
// is a key of the configured prefixes ("OLD_PROJ-1" -> "PROJ-1"), "" when
// there is none or no prefixes are configured.
func allowedAfterUnderscore(cand string, prefixes []string) string {
	if len(prefixes) == 0 {
		return ""
	}
	for {
		i := strings.IndexByte(cand, '_')
		if i < 0 {
			return ""
		}
		cand = cand[i+1:]
		if Allowed(cand, prefixes) {
			return cand
		}
	}
}

// Sources are the MR texts searched for keys.
type Sources struct {
	Title       string
	Branch      string
	Description string
	Commits     []string // full commit messages
}

// Found is one key and the first place it was found.
type Found struct {
	Key   string
	Where string // WhereTitle, WhereBranch, WhereDescription, WhereCommits
}

// Find returns the distinct keys from all sources in priority order:
// title > branch > description > commits. Branch names are also matched
// case-insensitively ("feature/proj-123-cache") when prefixes are
// configured, since lowercase branch names are common.
func Find(src Sources, prefixes []string) []Found {
	var out []Found
	seen := map[string]bool{}
	add := func(where string, keys []string) {
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				out = append(out, Found{Key: k, Where: where})
			}
		}
	}
	add(WhereTitle, ExtractKeys(src.Title, prefixes))
	branch := ExtractKeys(src.Branch, prefixes)
	if len(prefixes) > 0 {
		for _, k := range ExtractKeys(strings.ToUpper(src.Branch), prefixes) {
			if !slices.Contains(branch, k) {
				branch = append(branch, k)
			}
		}
	}
	add(WhereBranch, branch)
	add(WhereDescription, ExtractKeys(src.Description, prefixes))
	for _, c := range src.Commits {
		add(WhereCommits, ExtractKeys(c, prefixes))
	}
	return out
}

// Keys returns the keys of found, at most n (all when n <= 0).
func Keys(found []Found, n int) []string {
	var out []string
	for _, f := range found {
		if n > 0 && len(out) == n {
			break
		}
		out = append(out, f.Key)
	}
	return out
}

// ---------------------------------------------------------------------------
// Title
// ---------------------------------------------------------------------------

var draftPrefixRe = regexp.MustCompile(`(?i)^\s*(?:draft\s*:|\[draft\]|\(draft\)|wip\s*:|\[wip\])\s*`)

// StripDraft removes GitLab draft prefixes ("Draft:", "[Draft]", "(Draft)",
// "WIP:") from the start of a title, repeatedly.
func StripDraft(title string) string {
	for {
		loc := draftPrefixRe.FindStringIndex(title)
		if loc == nil {
			return strings.TrimSpace(title)
		}
		title = title[loc[1]:]
	}
}

// Title is a parsed MR title.
type Title struct {
	// Valid: "<type>[(scope)][!]: <description>" with an allowed type and a
	// non-empty description. The key suffix is not required for Valid.
	Valid       bool
	Type        string
	Scope       string
	Breaking    bool
	Description string // without the key suffix
	// Key is the first key from the trailing "(KEY)" / "(KEY1, KEY2)".
	Key  string
	Keys []string
}

var (
	titleRe     = regexp.MustCompile(`^([A-Za-z]+)(?:\(([^()]*)\))?(!)?:\s+(.*)$`)
	keySuffixRe = regexp.MustCompile(`\s*\(\s*([A-Z][A-Z0-9_]+-[0-9]+(?:\s*,\s*[A-Z][A-Z0-9_]+-[0-9]+)*)\s*\)\s*$`)
)

// ParseTitle parses title (draft prefixes are stripped first). types are
// the allowed Conventional Commits types (lowercase).
func ParseTitle(title string, types []string) Title {
	var t Title
	s := StripDraft(title)
	if m := keySuffixRe.FindStringSubmatchIndex(s); m != nil {
		for _, k := range strings.Split(s[m[2]:m[3]], ",") {
			t.Keys = append(t.Keys, strings.TrimSpace(k))
		}
		t.Key = t.Keys[0]
		s = strings.TrimSpace(s[:m[0]])
	}
	m := titleRe.FindStringSubmatch(s)
	if m == nil {
		t.Description = s
		return t
	}
	t.Type, t.Scope, t.Breaking = m[1], strings.TrimSpace(m[2]), m[3] == "!"
	t.Description = strings.TrimSpace(m[4])
	emptyScope := strings.HasPrefix(s[len(m[1]):], "(") && t.Scope == ""
	t.Valid = t.Description != "" && slices.Contains(types, t.Type) && !emptyScope
	return t
}

// Format renders a Conventional Commits title: "type(scope)!: description (KEY)".
// keyPart is put in parentheses when non-empty.
func Format(typ, scope string, breaking bool, description, keyPart string) string {
	var b strings.Builder
	b.WriteString(typ)
	if scope != "" {
		b.WriteString("(" + scope + ")")
	}
	if breaking {
		b.WriteString("!")
	}
	b.WriteString(": ")
	b.WriteString(description)
	if keyPart != "" {
		b.WriteString(" (" + keyPart + ")")
	}
	return b.String()
}

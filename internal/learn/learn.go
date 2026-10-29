// Package learn holds the pure parts of the "team rules" feature: the rule
// model, the storage interface (implemented by *store.SQLite), command
// parsing, text normalization and secret scrubbing, and the selection of
// rules for a prompt. It depends only on the standard library.
//
// The bot learns a rule when a team member (Developer+) explains in a bot
// thread why a remark is wrong or states a team convention, or explicitly
// with "@bot запомни <текст>". Rules are either project-scoped (apply to one
// repository) or global (apply to every repository of the configured groups)
// and are added to every later review and chat prompt.
package learn

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Scopes of a rule.
const (
	ScopeProject = "project" // only the repository it was learned in
	ScopeGlobal  = "global"  // every repository
)

// MaxRuleRunes caps the stored rule text.
const MaxRuleRunes = 400

// Rule is one learned team rule.
type Rule struct {
	ID    int64
	Scope string // ScopeProject | ScopeGlobal
	// ProjectPath is the repository the rule applies to (project scope) or
	// was learned in (global scope).
	ProjectPath  string
	Text         string
	SourceURL    string // GitLab note URL: <mr web url>#note_<id>
	SourceNoteID int64
	Author       string // GitLab username
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// DeletedAt is set on a soft-deleted rule (lists of deleted rules).
	DeletedAt time.Time
}

// Errors of the bulk rule changes of the /knowledge page (*store.SQLite):
// each change is all or nothing, these name why nothing was changed.
var (
	// ErrRuleNotFound: an id is not a live rule (delete, scope) or not a
	// deleted one (restore).
	ErrRuleNotFound = errors.New("learn: rule not found")
	// ErrRuleNoProject: a rule cannot be limited to its project because it
	// does not remember one.
	ErrRuleNoProject = errors.New("learn: rule has no project")
	// ErrRuleExists: a deleted rule cannot be restored because an equal live
	// rule exists (same normalized text and scope, same project for
	// project scope: the de-duplication of AddLearning).
	ErrRuleExists = errors.New("learn: an equal live rule exists")
)

// AppliesTo reports whether the rule is applied in projectPath.
func (r Rule) AppliesTo(projectPath string) bool {
	return r.Scope == ScopeGlobal || strings.EqualFold(r.ProjectPath, projectPath)
}

// Store keeps learned rules. *store.SQLite implements it.
type Store interface {
	// AddLearning inserts r, or, when a live rule with the same normalized
	// text and scope (and project, for project scope) exists, refreshes that
	// rule's source/author/updated_at and returns it with created=false.
	AddLearning(ctx context.Context, r Rule) (saved Rule, created bool, err error)
	// GetLearning returns a live (not deleted) rule, or nil, nil.
	GetLearning(ctx context.Context, id int64) (*Rule, error)
	// ListLearnings returns live rules applicable to projectPath (global +
	// that project's), newest first, at most limit (<= 0: all).
	ListLearnings(ctx context.Context, projectPath string, limit int) ([]Rule, error)
	// SetLearningScope changes the scope (and project path) of a live rule.
	SetLearningScope(ctx context.Context, id int64, scope, projectPath string) error
	// DeleteLearning soft-deletes a rule.
	DeleteLearning(ctx context.Context, id int64) error
	// MarkNoteProcessed remembers that a note was used for extraction. It
	// returns false when the note had already been marked.
	MarkNoteProcessed(ctx context.Context, noteID int64) (bool, error)
}

// NormalizeScope maps Claude's / users' scope words to a scope; unknown
// values become ScopeProject (the safer choice).
func NormalizeScope(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), ScopeGlobal) {
		return ScopeGlobal
	}
	return ScopeProject
}

// NormalizeKey is the dedupe key of a rule text: lowercased, whitespace
// collapsed, surrounding punctuation dropped.
func NormalizeKey(text string) string {
	s := strings.ToLower(strings.Join(strings.Fields(text), " "))
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) })
}

// ---------------------------------------------------------------------------
// Secret scrubbing
// ---------------------------------------------------------------------------

// Redacted replaces every secret Scrub finds.
const Redacted = "[скрыто]"

var secretPatterns = []*regexp.Regexp{
	// URLs with credentials: scheme://user:pass@host
	regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s/@:]+:[^\s/@]+@\S+`),
	// Known token formats.
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{10,}`),
	regexp.MustCompile(`\bgl(?:dt|ft|oas|rt|cbt|ptt|soat|imt|agent)-[A-Za-z0-9_\-]{10,}`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{10,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9\-]{10,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`), // JWT
	regexp.MustCompile(`(?i)-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z ]*PRIVATE KEY-----|$)`),
	// Bearer / key=value secrets.
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/\-]{16,}=*`),
	regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key)\s*[:=]\s*\S{6,}`),
	// Long opaque strings (hex / base64-ish) that look like keys.
	regexp.MustCompile(`\b[A-Fa-f0-9]{40,}\b`),
	regexp.MustCompile(`\b[A-Za-z0-9+_\-]{48,}={0,2}`), // no "/": keep file paths
}

// Scrub replaces secrets (tokens, URLs with credentials, private keys, long
// opaque strings) with a placeholder.
func Scrub(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Redacted)
	}
	return s
}

// CleanRule turns Claude's / a user's rule into the stored form: secrets
// scrubbed, one line, HTML comments and markers removed, capped. Empty when
// nothing useful is left.
func CleanRule(s string) string {
	s = StripHTMLComments(s)
	s = Scrub(s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > MaxRuleRunes {
		s = strings.TrimSpace(string(r[:MaxRuleRunes])) + "…"
	}
	if NormalizeKey(strings.ReplaceAll(s, Redacted, "")) == "" {
		return ""
	}
	return s
}

var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// StripHTMLComments removes HTML comments (<!-- … -->) from s.
func StripHTMLComments(s string) string { return htmlCommentRe.ReplaceAllString(s, "") }

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// CommandKind is a rule management command.
type CommandKind int

const (
	CmdRemember CommandKind = iota + 1 // запомни <текст> / remember <text>
	CmdForget                          // забудь #12 / forget #12
	CmdList                            // правила / rules
	CmdScope                           // правило #12 для всех|только здесь / rule #12 global|local
)

// Command is a parsed rule command.
type Command struct {
	Kind CommandKind
	ID   int64  // CmdForget, CmdScope
	Text string // CmdRemember: the rule as the user wrote it (scope words removed)
	// Scope: CmdScope target; CmdRemember an explicit hint ("" = let Claude
	// decide).
	Scope string
}

var (
	listRe     = regexp.MustCompile(`(?i)^(?:rules|правила|список правил)$`)
	forgetRe   = regexp.MustCompile(`(?i)^(?:forget|забудь)(?:\s+(?:rule|правило))?\s*#?(\d+)$`)
	scopeRe    = regexp.MustCompile(`(?i)^(?:rule|правило)\s*#?(\d+)\s*(?:[-—:,]\s*)?(.+)$`)
	rememberRe = regexp.MustCompile(`(?is)^(?:remember|запомни)(?:\s*[:,—-])?\s+(.+)$`)
	// Explicit scope hints at the start or end of a "запомни" text.
	globalHintRe = regexp.MustCompile(`(?i)^(?:для всех(?: репозиториев| проектов)?|везде|глобально|global(?:ly)?|for all(?: repos(?:itories)?| projects)?|everywhere)$`)
	localHintRe  = regexp.MustCompile(`(?i)^(?:только здесь|только для (?:этого )?(?:проекта|репозитория)|здесь|локально|для проекта|local(?:ly)?|here|only here|this (?:project|repo(?:sitory)?) only|project)$`)
)

// ParseCommand parses the text after the bot mention. ok is false when it is
// not a rule command (then it is a chat question).
func ParseCommand(rest string) (Command, bool) {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return Command{}, false
	}
	single := !strings.Contains(rest, "\n")
	line := strings.TrimRight(strings.Join(strings.Fields(rest), " "), ".!?")
	if single {
		if listRe.MatchString(line) {
			return Command{Kind: CmdList}, true
		}
		if m := forgetRe.FindStringSubmatch(line); m != nil {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && id > 0 {
				return Command{Kind: CmdForget, ID: id}, true
			}
		}
		if m := scopeRe.FindStringSubmatch(line); m != nil {
			id, err := strconv.ParseInt(m[1], 10, 64)
			if sc := scopeWord(m[2]); err == nil && id > 0 && sc != "" {
				return Command{Kind: CmdScope, ID: id, Scope: sc}, true
			}
		}
	}
	if m := rememberRe.FindStringSubmatch(rest); m != nil {
		text, scope := splitScopeHint(strings.TrimSpace(m[1]))
		if NormalizeKey(text) != "" {
			return Command{Kind: CmdRemember, Text: text, Scope: scope}, true
		}
	}
	return Command{}, false
}

// IsCommand reports whether rest (the text after the mention) is a rule
// command. Used by the webhook to route the note.
func IsCommand(rest string) bool {
	_, ok := ParseCommand(rest)
	return ok
}

func scopeWord(s string) string {
	s = strings.TrimSpace(strings.TrimRight(strings.TrimSpace(s), ".!?"))
	switch {
	case globalHintRe.MatchString(s):
		return ScopeGlobal
	case localHintRe.MatchString(s):
		return ScopeProject
	}
	return ""
}

// splitScopeHint removes a leading "для всех:" / trailing "(только здесь)"
// style hint from a remember text.
func splitScopeHint(text string) (string, string) {
	trim := func(s string) string {
		return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), ",:;—-()[]"))
	}
	// Leading hint followed by ':' or ','.
	if i := strings.IndexAny(text, ":,"); i > 0 {
		if sc := scopeWord(trim(text[:i])); sc != "" {
			return trim(text[i+1:]), sc
		}
	}
	// Trailing hint: last parenthesized group, or after the last ',' / '—' / '.'.
	t := strings.TrimRight(text, ".!? ")
	if strings.HasSuffix(t, ")") {
		if i := strings.LastIndex(t, "("); i > 0 {
			if sc := scopeWord(t[i+1 : len(t)-1]); sc != "" {
				return trim(t[:i]), sc
			}
		}
	}
	for _, sep := range []string{",", " — ", " - ", ".", ";"} {
		if i := strings.LastIndex(t, sep); i > 0 {
			if sc := scopeWord(t[i+len(sep):]); sc != "" {
				return trim(t[:i]), sc
			}
		}
	}
	return text, ""
}

// ---------------------------------------------------------------------------
// Prompt selection
// ---------------------------------------------------------------------------

// DefaultMaxPromptChars caps the total rule text in one prompt.
const DefaultMaxPromptChars = 8000

// Select keeps rules in order until maxCount rules or maxChars characters of
// rule text are reached (<= 0 = no cap for that dimension).
func Select(rules []Rule, maxCount, maxChars int) []Rule {
	var out []Rule
	total := 0
	for _, r := range rules {
		if maxCount > 0 && len(out) >= maxCount {
			break
		}
		n := len([]rune(r.Text))
		if maxChars > 0 && total+n > maxChars {
			break
		}
		total += n
		out = append(out, r)
	}
	return out
}

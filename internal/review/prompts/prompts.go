// Package prompts holds what the bot's prompt templates share: the template
// functions, rendering, the language name, the tools and merge-blocking rules
// and the invalid-JSON retry nudge. It is a leaf: it must not depend on
// internal/review or any of its feature packages.
package prompts

import (
	"strings"
	"text/template"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// InvalidJSONNudge is appended to the prompt for the single retry after an
// unparsable answer.
const InvalidJSONNudge = `

IMPORTANT: your previous output was invalid JSON or did not match the schema (%s).
Answer again with ONLY one JSON object that matches the JSON schema exactly. No prose, no code fences.`

// Funcs are the template functions of every prompt template: untrusted
// (mdtext.Untrusted), inc, toolsRule (ToolsRule) and section (trims
// newlines around a pre-rendered section).
var Funcs = template.FuncMap{
	"untrusted": mdtext.Untrusted,
	"inc":       func(i int) int { return i + 1 },
	"toolsRule": ToolsRule,
	"section":   func(s string) string { return strings.Trim(s, "\n") },
}

// ToolsRule is the rule on what the model can do, shared by every run with
// tools: read and search files, nothing else (no Bash, no git). base is the
// path of the read-only merge base export as Claude sees it ("" = none).
func ToolsRule(base string) string {
	s := "You can only read and search files (Read, Grep, Glob): you cannot run commands, builds, tests or git. The working directory is the merge request head"
	if base != "" {
		s += "; " + base + " is a read-only copy of the code before the merge request (the merge base): " + base + "/<path> is <path> before the change"
	}
	return s + ". Do not try to modify files, access the network or bypass these limits."
}

// BlockRule tells Claude which severities block the merge under the
// configured review.block_min_severity, with the "only when confident"
// guidance for the lowest blocking level and the level to use instead.
func BlockRule(minSeverity string) string {
	switch core.Severity(strings.ToLower(strings.TrimSpace(minSeverity))) {
	case core.SeverityMajor:
		return `Findings of severity "major" or higher ("critical", "major") BLOCK THE MERGE until a human resolves the thread. Use "major" only when you are confident the change, as written, causes a real bug or a serious risk, and you can point to the exact code path; "critical" only for data loss, a security vulnerability, a crash or broken core behaviour. If you are unsure, or it depends on assumptions you could not verify in the repository, use "minor" instead.`
	case core.SeverityMinor:
		return `Findings of severity "minor" or higher ("critical", "major", "minor") BLOCK THE MERGE until a human resolves the thread. Use "minor" only when you are confident it is a real defect or edge case in the change, as written, and you can point to the exact code path. If you are unsure, or it is only cosmetic, use "nit" instead.`
	case core.SeverityNit:
		return `EVERY finding, of any severity including "nit", BLOCKS THE MERGE until a human resolves the thread. Report a finding only when you are confident it is worth fixing and you can point to the exact code; if you are unsure, leave it out.`
	default: // critical
		return `"critical" BLOCKS THE MERGE until a human resolves the thread. Use it only when you are confident the change, as written, causes data loss, a security vulnerability, a crash, or breaks core behaviour, and you can point to the exact code path. If you are unsure, or it depends on assumptions you could not verify in the repository, use "major" instead.`
	}
}

// Render executes a prompt template.
func Render(t *template.Template, data any) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// LanguageName turns a config language code into a name for the prompt.
func LanguageName(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "", "ru", "rus", "russian", "русский":
		return "Russian"
	case "en", "eng", "english":
		return "English"
	case "de", "german":
		return "German"
	case "uk", "ukrainian":
		return "Ukrainian"
	case "es", "spanish":
		return "Spanish"
	case "fr", "french":
		return "French"
	default:
		return code
	}
}

// System is the data of the system prompt templates.
type System struct {
	Language string
	// BaseDir is the merge base export as Claude sees it ("" = none).
	BaseDir string
	// BlockRule is the sentence on which severities block the merge, from
	// review.block_min_severity (see prompts.BlockRule).
	BlockRule string
}

// OpenFinding is an open bot thread as the prompts list it.
type OpenFinding struct {
	ID    string
	Path  string
	Line  int
	Title string
	Body  string
}

// ChatNote is one note of a thread as the prompts quote it.
type ChatNote struct {
	Author string
	Text   string
}

// Package descsum keeps a short summary of the changes in the merge
// request's own description ("Саммари от ревью-бота",
// review.description_summary): the review run fills
// ReviewOutput.DescriptionSummary, the bot renders it between markers and
// writes the description back, changing nothing outside its own block.
//
// The text comes from the model, which reads untrusted code: it is made
// one line per item, Markdown/HTML metacharacters are escaped, mentions,
// references and links are broken, quick actions neutralized. The author's
// text is never changed: a description with an opt-out marker, with broken
// markers, without the block the bot wrote before (the author removed it)
// or with a quick action at the start of a line is left alone.
package descsum

import (
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
)

// Markers of the block in the description, the author's opt-out and the
// summary note's record that the bot wrote the block.
const (
	Start   = "<!-- reviewbot:description-summary -->"
	End     = "<!-- /reviewbot:description-summary -->"
	Off     = "<!-- reviewbot:description-summary-off -->"
	Written = "<!-- reviewbot:description-summary-written -->"
)

// Caps of the model's text and of the block.
const (
	maxOverviewRunes = 400
	maxItemRunes     = 200
	maxItems         = 8
	// MaxBlockBytes caps the rendered block.
	MaxBlockBytes = 6000
)

var (
	blockRe = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(Start) + `.*?` + regexp.QuoteMeta(End))
	// quickActionRe: a line GitLab could run as a quick action ("/close",
	// "/assign @x"); "/api/v1/x" is a path, not a command.
	quickActionRe = regexp.MustCompile(`(?m)^[ \t]*/[a-z_]+(?:[ \t]|$)`)
	// sandboxRe: the sandbox mount roots of the review run ("/work/a.go" ->
	// "a.go", "/repos/g/p/x" -> "g/p/x"): never shown in the description.
	sandboxRe = regexp.MustCompile("(^|[\\s(`'\"])/(?:work|base|repos|skills)/")
)

// kinds are the groups in display order with their Russian and English
// titles.
var kinds = []struct{ kind, ru, en string }{
	{"new", "Новое", "New"},
	{"fix", "Исправления", "Fixes"},
	{"refactor", "Рефакторинг", "Refactoring"},
	{"test", "Тесты", "Tests"},
	{"docs", "Документация", "Documentation"},
	{"other", "Прочее", "Other"},
}

const schema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["overview", "groups"],
  "description": "Summary of the whole merge request for its description.",
  "properties": {
    "overview": {"type": "string", "description": "1-2 sentences: what the merge request as a whole does and why."},
    "groups": {
      "type": "array",
      "description": "Only non-empty groups, each at most once.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["kind", "items"],
        "properties": {
          "kind": {"type": "string", "enum": ["new", "fix", "refactor", "test", "docs", "other"]},
          "items": {"type": "array", "items": {"type": "string"}, "description": "Short phrases about the change, at most 8."}
        }
      }
    }
  }
}`

// WithSchema adds the required description_summary property when on.
func WithSchema(s string, on bool) string {
	if !on {
		return s
	}
	return jsonschema.Edit(s, func(m map[string]any) {
		m["properties"].(map[string]any)["description_summary"] = jsonschema.Must("schema for description_summary", schema)
		req, _ := m["required"].([]any)
		m["required"] = append(req, "description_summary")
	})
}

// Rules is the system prompt section ("" when off).
func Rules(on bool, lang string) string {
	if !on {
		return ""
	}
	return `

Description summary ("description_summary" field):
- It goes into the merge request's description for its readers: describe the merge request AS A WHOLE, in ` + lang + `.
- overview: 1-2 sentences about what the merge request does and why; not a list of files.
- groups: only non-empty ones, each kind at most once: "new" (new behavior or features), "fix", "refactor", "test", "docs", "other". items: short phrases about behavior and intent (at most 8 per group, each under 160 characters), not file names.
- Plain text only: no emoji, headings, links, code blocks, @mentions or issue references.
- In an INCREMENTAL review the description may already hold the bot's previous summary between <!-- reviewbot:description-summary --> markers: return the summary of the whole merge request, updated with the new commits.`
}

// Normalize drops the summary when it was not asked for.
func Normalize(out *output.ReviewOutput, on bool) {
	if !on {
		out.DescriptionSummary = nil
	}
}

// text makes one model phrase safe for the description: one line, capped,
// Markdown/HTML escaped (no links, mentions or references), no URL
// autolinks, no leading slash.
func text(s string, n int) string {
	t := mdtext.OneLine(sandboxRe.ReplaceAllString(s, "$1"))
	t = mdtext.Escape(mdtext.Shorten(t, n))
	t = strings.ReplaceAll(t, "://", ":​//")
	if strings.HasPrefix(t, "/") {
		t = "\\" + t
	}
	return t
}

// Render renders the block with its markers ("" when the summary is empty).
func Render(sum *output.DescriptionSummary, lang string) string {
	if sum == nil {
		return ""
	}
	ru := !strings.HasPrefix(strings.ToLower(lang), "english")
	items := map[string][]string{}
	for _, g := range sum.Groups {
		k := g.Kind
		if !validKind(k) {
			k = "other"
		}
		for _, it := range g.Items {
			if t := text(it, maxItemRunes); t != "" && len(items[k]) < maxItems {
				items[k] = append(items[k], t)
			}
		}
	}
	overview := text(sum.Overview, maxOverviewRunes)
	if overview == "" && len(items) == 0 {
		return ""
	}
	var b strings.Builder
	title, note := "Саммари от ревью-бота", "Блок обновляется при каждом ревью; текст описания вне блока бот не меняет."
	if !ru {
		title, note = "Summary by the review bot", "Updated on every review; the bot never changes the description outside this block."
	}
	b.WriteString("**" + title + "**\n")
	if overview != "" {
		b.WriteString("\n" + overview + "\n")
	}
	for _, k := range kinds {
		list := items[k.kind]
		if len(list) == 0 {
			continue
		}
		name := k.ru
		if !ru {
			name = k.en
		}
		b.WriteString("\n**" + name + "**\n")
		for _, it := range list {
			b.WriteString("- " + it + "\n")
		}
	}
	b.WriteString("\n<sub>" + note + "</sub>\n")
	body := b.String()
	// Over the size cap (long items in many groups): drop the last items
	// (the overview alone always fits).
	for len(body) > MaxBlockBytes-len(Start)-len(End)-2 && strings.Contains(body, "\n- ") {
		i := strings.LastIndex(body, "\n- ")
		j := strings.Index(body[i+1:], "\n")
		body = body[:i] + body[i+1+j:]
	}
	return mdtext.NeutralizeQuickActions(summary.Block(Start+"\n", body, End))
}

func validKind(k string) bool {
	for _, x := range kinds {
		if x.kind == k {
			return true
		}
	}
	return false
}

// Why a description is left alone (Merge).
const (
	WhySame         = "unchanged"
	WhyOff          = "opted out"
	WhyRemoved      = "the author removed the block"
	WhyBroken       = "broken markers"
	WhyQuickActions = "quick action in the description"
)

// Merge puts block into the description: in place of the bot's block, or
// at the end after a blank line. It returns the new description, or "" and
// why the description is left alone. written: the bot wrote the block
// before (the summary note says so).
func Merge(desc, block string, written bool) (string, string) {
	author := blockRe.ReplaceAllString(desc, "")
	switch {
	case strings.Contains(author, Off):
		return "", WhyOff
	case strings.Contains(author, Start) || strings.Contains(author, End):
		return "", WhyBroken
	case len(quickActionRe.FindAllString(author, -1)) > len(quickActionRe.FindAllString(mdtext.NeutralizeQuickActions(author), -1)):
		// The author's text has a line GitLab could run as a quick action
		// on this update: never resend it.
		return "", WhyQuickActions
	}
	var out string
	if loc := blockRe.FindStringIndex(desc); loc != nil {
		out = desc[:loc[0]] + block + desc[loc[1]:]
	} else {
		if written {
			return "", WhyRemoved
		}
		out = strings.TrimRight(desc, " \t\r\n")
		if out != "" {
			out += "\n\n"
		}
		out += block
	}
	if out == desc {
		return "", WhySame
	}
	return out, ""
}

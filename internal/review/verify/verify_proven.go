package verify

// Verification only where needed (review.verify): findings the bot already
// proved deterministically are not sent to the verification run, which is
// for the model's opinion findings. Proven:
//
//   - migration: a blocking finding on a migration file whose
//     deployed_usage passed every evidence check of migcheck.Feature.Enforce (the
//     merge base really uses what the migration drops or renames);
//   - gitleaks: the finding is about a secret on a line gitleaks reported
//     (same file, the result inside the finding's lines ±1, the text speaks
//     of a secret/token/password/key) — the gitleaks thread is the proof;
//   - compat: the finding is about a breaking change a deterministic checker
//     (oasdiff, buf, graphql-inspector, custom) reported: same file, lines
//     within ±2, and the same topic (compatTopic: the finding speaks of a
//     compatibility break, names what the tool names, or is about the same
//     kind of change) — the compat thread is the proof. Another finding on
//     those lines (a missing auth check on the spec line, say) is verified.
//
// Proven findings are published as they are; the summary's "🧪 Перепроверка"
// block counts them and reviewbot_verify_verdicts_total{verdict="proven"}
// counts them by severity.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/fixcik/gitlab-review-bot/internal/lint"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// What proved a finding (log, tests).
const (
	provenMigration = "migration"
	provenGitleaks  = "gitleaks"
	provenCompat    = "compat"
)

// provenFinding is an eligible finding the verification skipped.
type provenFinding struct {
	F  output.Finding
	By string // provenMigration | provenGitleaks | provenCompat
}

// provenBy returns the proof check of a review (nil when nothing can prove).
// migProven are the ids of the migration findings whose deployed_usage
// passed the evidence check (migcheck.Run.ProvenIDs).
func provenBy(migProven map[string]bool, lr *linters.Run, cr *compatcheck.Run) func(output.Finding) string {
	var secrets []lint.Finding
	if lr != nil && lr.Res != nil {
		for _, f := range lr.Res.Findings {
			if f.Secret() {
				secrets = append(secrets, f)
			}
		}
	}
	var changes []compatcheck.Item
	if cr != nil && cr.Res != nil {
		for _, c := range cr.Res.Changes {
			changes = append(changes, compatcheck.DeterministicItem(c))
		}
	}
	if len(migProven) == 0 && len(secrets) == 0 && len(changes) == 0 {
		return nil
	}
	return func(f output.Finding) string {
		if migProven[finding.ID(f.Path, f.Title)] {
			return provenMigration
		}
		end := max(f.EndLine, f.Line)
		if len(secrets) > 0 && linters.SecretWordsRe.MatchString(f.Title+"\n"+f.Body) {
			for _, s := range secrets {
				if s.Path == f.Path && s.Line >= f.Line-1 && s.Line <= end+1 {
					return provenGitleaks
				}
			}
		}
		for _, c := range changes {
			if c.Path == f.Path && c.Line > 0 && f.Line > 0 && c.Line >= f.Line-2 && c.Line <= end+2 && compatTopic(f, c) {
				return provenCompat
			}
		}
		return ""
	}
}

// compatWordsRe: the finding speaks of a compatibility break.
var compatWordsRe = regexp.MustCompile(`(?i)break|compat|совместим|контракт|contract|ломает|сломает|сломаются|сломают`)

// quotedRe: the names a tool quotes in its message ('email', "User",
// `field`).
var quotedRe = regexp.MustCompile("'([^'\n]{2,80})'|\"([^\"\n]{2,80})\"|`([^`\n]{2,80})`")

// compatKinds: a kind of change in the tool's rule or message (tool) and
// how a finding about that kind speaks of it (finding).
var compatKinds = []struct{ tool, finding *regexp.Regexp }{
	{regexp.MustCompile(`(?i)remov|delet`), regexp.MustCompile(`(?i)удал|убра|исчез|пропа|remov|delet|dropp?ed`)},
	{regexp.MustCompile(`(?i)type|format`), regexp.MustCompile(`(?i)тип|type|формат|format`)},
	{regexp.MustCompile(`(?i)required|optional`), regexp.MustCompile(`(?i)обязател|required|optional`)},
	{regexp.MustCompile(`(?i)renam`), regexp.MustCompile(`(?i)переимен|renam`)},
	{regexp.MustCompile(`(?i)enum`), regexp.MustCompile(`(?i)enum|перечисл`)},
}

// compatTopic reports whether finding f is about the deterministic breaking
// change c (see "compat" in the comment at the top of this file).
func compatTopic(f output.Finding, c compatcheck.Item) bool {
	text := f.Title + "\n" + f.Body
	if compatWordsRe.MatchString(text) {
		return true
	}
	lower := strings.ToLower(text)
	for _, m := range quotedRe.FindAllStringSubmatch(c.Message, -1) {
		if mentions(lower, strings.ToLower(m[1]+m[2]+m[3])) {
			return true
		}
	}
	tool := c.Rule + "\n" + c.Message
	for _, k := range compatKinds {
		if k.tool.MatchString(tool) && k.finding.MatchString(text) {
			return true
		}
	}
	return false
}

// mentions reports whether text contains name as a whole word (no letter,
// digit or "_" right before or after it).
func mentions(text, name string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], name)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(name)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if !wordRune(before) && !wordRune(after) {
			return true
		}
		i = start + 1
	}
}

func wordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// provenLine is the summary line about proven findings ("" = none).
func (vr *Run) provenLine() string {
	n := len(vr.proven)
	if n == 0 {
		return ""
	}
	by := map[string]bool{}
	var names []string
	for _, p := range vr.proven {
		if by[p.By] {
			continue
		}
		by[p.By] = true
		switch p.By {
		case provenMigration:
			names = append(names, "использование в задеплоенном коде")
		case provenGitleaks:
			names = append(names, "gitleaks")
		case provenCompat:
			names = append(names, "проверка совместимости API")
		}
	}
	return fmt.Sprintf("🧪 Без перепроверки: %d %s проверками бота (%s).\n", n,
		mdtext.Plural(n, "замечание доказано", "замечания доказаны", "замечаний доказано"), strings.Join(names, ", "))
}

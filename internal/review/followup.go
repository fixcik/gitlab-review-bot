package review

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// Second look (review.followup). A second review in a NEW conversation, told
// what the first one found, that reads the code on its own: in the ReviewBench
// measurements it finds defects the first one never reached (+6 points of F1,
// more in high findings) at about the cost of one more review run. Its answer
// adds findings only; the summary, labels and the rest stay those of the
// first run. It runs without MCP tools (the full prompt sections), so a
// failed second look never touches the first review's session.

// followUpSimilarity is the Jaccard of the words from which two findings on
// near lines are the same problem (the threshold of RevEngine's merge).
const followUpSimilarity = 0.45

// followUp adds the findings of a second look to rv.out. Any failure (the
// limit, the run, the answer) keeps the first review as it is.
func (r *Reviewer) followUp(ctx context.Context, rv *reviewRun) {
	if !r.env.Cfg.Review.FollowUp || rv.rt.light() || rv.out == nil {
		return
	}
	dec, err := r.env.Limiter.CheckGlobal(ctx)
	if err != nil || !dec.Allowed {
		rv.log.Warn("review: second look skipped by the global limit", "err", err, "reason", dec.Reason)
		return
	}
	req, _, err := r.reviewRequest(ctx, rv, nil)
	if err != nil {
		rv.log.Warn("review: second look request", "err", err)
		return
	}
	req.Prompt += followUpPrompt(rv.out.Findings)
	var second *output.ReviewOutput
	err = r.env.RunStructured(ctx, rv.job, req, func(s string) error {
		o, err := parseReviewOutput(s, r.env.Cfg.Review.MaxFindings)
		second = o
		return err
	})
	if err != nil || second == nil {
		rv.log.Warn("review: second look failed, the first review is kept", "err", err)
		return
	}
	rv.rc.LinkReview(second)
	before := len(rv.out.Findings)
	rv.out.Findings = finding.Cap(mergeFollowUp(rv.out.Findings, second.Findings), r.env.Cfg.Review.MaxFindings)
	rv.log.Info("review: second look", "first", before, "second", len(second.Findings), "merged", len(rv.out.Findings))
}

// followUpPrompt is the end of the second look's user prompt: the findings of
// the first review, which are not to be repeated.
func followUpPrompt(first []output.Finding) string {
	var b strings.Builder
	b.WriteString("\n\n## Second review\nA first reviewer has already reported these findings. Do not repeat them:\n")
	for _, f := range first {
		fmt.Fprintf(&b, "- %s:%d (%s) %s\n", f.Path, f.Line, f.Severity, f.Title)
	}
	if len(first) == 0 {
		b.WriteString("- (none)\n")
	}
	b.WriteString("\nRead the changed files, the callers of the changed code and the tests yourself, in a fresh and independent review. Report only additional real problems the first review missed, including ones it may have left out because it could not fully prove them. Do not repeat its findings.")
	return b.String()
}

// mergeFollowUp appends the findings of the second look that do not repeat a
// finding of the first review: the same file, lines within 3 of each other
// and a similar text. Findings of one run are never merged with each other.
func mergeFollowUp(first, second []output.Finding) []output.Finding {
	out := first
next:
	for _, f := range second {
		for _, k := range first {
			if k.Path == f.Path && f.Line <= endLine(k)+3 && k.Line <= endLine(f)+3 &&
				jaccard(wordSet(k.Title+" "+k.Body), wordSet(f.Title+" "+f.Body)) >= followUpSimilarity {
				continue next
			}
		}
		out = append(out, f)
	}
	return out
}

func endLine(f output.Finding) int {
	if f.EndLine > f.Line {
		return f.EndLine
	}
	return f.Line
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]{3,}`)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		m[w] = true
	}
	return m
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

package review

// Verification problems (internal/problems, review.problems): what this
// review could not check. Bot-side steps record them with one-line hooks
// (lint tool states, merge base export, compat, coverage, related repositories, Jira,
// advisor, system map, cut diff, unparsable answer, migrations evidence,
// MCP); the model reports what it could not verify through the
// report_problem MCP tool or, without MCP, the optional "problems" array
// of its answer. The summary gets a compact "⚠️ Не удалось проверить"
// block with this run's problems and, for new ones, a cc line mentioning
// review.problems.notify (or a separate note, notify_mode: note).
// Recording never fails a review. The bot-side hooks (linters.NoteProblems,
// noteCompatIssues, noteCoverageProblems, crossrepo.RunContext.NoteProblems,
// noteTaskProblems) live with their features; they record through
// jobctx.BotProblem.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mcp"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// ---------------------------------------------------------------------------
// Model-reported problems
// ---------------------------------------------------------------------------

// problemsOn: review.problems.enabled (the model is asked to report).
func (r *Reviewer) problemsOn() bool { return r.env.Cfg.Review.Problems.Enabled }

// problemsSystemNote is the system prompt paragraph asking the model to
// report what it could not verify ("" when problems are off).
func (r *Reviewer) problemsSystemNote(withMCP bool) string {
	if !r.problemsOn() {
		return ""
	}
	how := "put it into the optional `problems` array of your answer (kind, a short subject, a detail)"
	if withMCP {
		how = "report it with the `" + mcp.ToolReportProblem + "` tool (kind, a short subject, a detail)"
	}
	return fmt.Sprintf("\n\n## Problems of the review itself\nIf you could not verify something you should have checked (a tool failed, code the change depends on is not in the repository, such as vendor/, node_modules or generated code, a file too large to read, no access to a related repository, unclear project configuration), %s, at most %d per review. This tells the bot's operators what to fix in the setup; it is NOT for problems of the code (those are findings), and nothing is needed when everything could be checked.\n",
		how, problems.MaxModelPerRun)
}

// withProblemsSchema adds the optional "problems" array to a review schema
// (only without MCP; with MCP the model uses report_problem).
func withProblemsSchema(schema string) string {
	return jsonschema.MustSetProp(schema, "problems", map[string]any{
		"type": "array", "maxItems": problems.MaxModelPerRun,
		"description": "Optional: what you could not verify during this review (not problems of the code). Omit or leave empty when everything could be checked.",
		"items":       mcp.ModelProblemSchema(),
	})
}

// addModelProblems records the "problems" array of the review answer.
// Invalid entries are skipped; at most MaxModelPerRun are kept.
func addModelProblems(ctx context.Context, raw string) {
	var v struct {
		Problems []mcp.ModelProblemArgs `json:"problems"`
	}
	if err := jsonschema.UnmarshalStrict(raw, &v); err != nil {
		return
	}
	for i, p := range v.Problems {
		if i == problems.MaxModelPerRun {
			break
		}
		_, _ = problems.AddModel(ctx, p.Kind, prompts.CleanModelText(p.Subject), prompts.CleanModelText(p.Detail))
	}
}

// ---------------------------------------------------------------------------
// Summary block and notification
// ---------------------------------------------------------------------------

const (
	problemsMaxLines = 5
	problemsCollapse = 3
)

// problemsRun is this run's problems for the summary.
type problemsRun struct {
	block  string
	notify []problems.Outcome // to mark notified once the mention is out
	mode   string
	cc     string // "cc @a @b — …"
	svc    *problems.Service
}

// problemsSummary persists the run's problems so far and renders the
// summary block (nil when there is nothing to show or the block is off).
func (r *Reviewer) problemsSummary(ctx context.Context, eff config.Effective) *problemsRun {
	ctx, tsp := tracing.Start(ctx, "review.problems")
	defer tsp.End()
	outs := problems.Commit(ctx)
	if !eff.ProblemsSummary || len(outs) == 0 {
		return nil
	}
	pr := &problemsRun{svc: problems.ServiceOf(ctx)}
	var users []string
	for _, u := range pr.svc.Notify() {
		if mdtext.UsernameRe.MatchString(u) {
			users = append(users, "@"+u)
		}
	}
	for _, o := range outs {
		if o.Notify {
			pr.notify = append(pr.notify, o)
		}
	}
	if len(pr.notify) > 0 && len(users) > 0 {
		pr.mode = pr.svc.NotifyMode()
		pr.cc = "cc " + strings.Join(users, " ") + " — новая проблема проверки"
		if link := pr.svc.PageLink(); link != "" {
			pr.cc += ", подробнее: " + link
		} else {
			pr.cc += "."
		}
	} else {
		pr.notify = nil
	}
	cc := ""
	if pr.mode == problems.NotifySummary {
		cc = pr.cc
	}
	pr.block = renderProblemsBlock(outs, cc)
	return pr
}

// renderProblemsBlock: one line per problem (kind title + subject), at
// most problemsMaxLines, collapsed above problemsCollapse; cc is the
// mention line ("" = none).
func renderProblemsBlock(outs []problems.Outcome, cc string) string {
	if len(outs) == 0 {
		return ""
	}
	rank := map[problems.Severity]int{problems.SevError: 0, problems.SevWarn: 1, problems.SevInfo: 2}
	sorted := append([]problems.Outcome(nil), outs...)
	sort.SliceStable(sorted, func(i, j int) bool { return rank[sorted[i].Info.Severity] < rank[sorted[j].Info.Severity] })
	var lines strings.Builder
	for i, o := range sorted {
		if i == problemsMaxLines {
			fmt.Fprintf(&lines, "- и ещё %d\n", len(sorted)-problemsMaxLines)
			break
		}
		fmt.Fprintf(&lines, "- %s", mdtext.Inline(o.Info.Title, 120))
		if s := strings.TrimSpace(o.Subject); s != "" {
			fmt.Fprintf(&lines, ": %s", mdtext.Inline(s, 160))
		}
		lines.WriteString("\n")
	}
	var b strings.Builder
	if len(sorted) > problemsCollapse {
		fmt.Fprintf(&b, "<details>\n<summary>⚠️ Не удалось проверить: %d</summary>\n\n%s\n</details>\n", len(sorted), lines.String())
	} else {
		b.WriteString("⚠️ **Не удалось проверить:**\n" + lines.String())
	}
	if cc != "" {
		b.WriteString("\n" + cc + "\n")
	}
	return summary.Block(marker.ProblemsStart+"\n", b.String(), marker.ProblemsEnd)
}

// problemsNotified sends the separate note (notify_mode: note) and marks
// the mentioned problems notified once the mention is out (summaryOK: the
// summary with the cc line was written).
func (r *Reviewer) problemsNotified(ctx context.Context, job core.Job, pr *problemsRun, summaryOK bool) {
	if pr == nil || len(pr.notify) == 0 {
		return
	}
	switch pr.mode {
	case problems.NotifyNote:
		var titles []string
		seen := map[string]bool{}
		for _, o := range pr.notify {
			if t := o.Info.Title; !seen[t] {
				seen[t] = true
				titles = append(titles, mdtext.Inline(t, 120))
			}
		}
		body := marker.ProblemsNotify + "\n⚠️ " + pr.cc + "\n\n" + strings.Join(mdtext.CapList(titles, problemsMaxLines), "; ") + "."
		if _, err := r.env.GitLab.CreateNote(ctx, job.ProjectID, job.MRIID, mdtext.NeutralizeQuickActions(body)); err != nil {
			r.env.Log.Warn("review: problems notify note", "mr", job.MRKey(), "err", err)
			return
		}
	default:
		if !summaryOK {
			return
		}
	}
	pr.svc.MarkNotified(ctx, pr.notify)
}

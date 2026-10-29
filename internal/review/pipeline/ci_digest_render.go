package pipeline

// Rendering the digest of a failed pipeline: the Markdown of the summary
// block, the thread and the main branch note (renderCIDigest), and the
// plain text of the explanation prompt (renderCIDigestPrompt). Job and test
// names, failure reasons, messages and logs are untrusted: escaped or
// fenced, every "<!--" defused.

import (
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
)

// ciReasonRu names GitLab's failure_reason.
func ciReasonRu(s string) string {
	switch s {
	case "", "unknown_failure":
		return ""
	case "script_failure":
		return "ошибка скрипта"
	case "runner_system_failure":
		return "сбой раннера"
	case "stuck_or_timeout_failure":
		return "зависла или таймаут"
	case "job_execution_timeout":
		return "таймаут job"
	case "api_failure":
		return "сбой API GitLab"
	case "missing_dependency_failure":
		return "нет артефактов зависимостей"
	case "runner_unsupported":
		return "раннер не поддерживается"
	case "scheduler_failure":
		return "сбой планировщика"
	case "data_integrity_failure":
		return "нарушение целостности данных"
	case "unmet_prerequisites":
		return "не выполнены предусловия"
	case "out_of_memory":
		return "не хватило памяти"
	}
	return mdtext.InlineCode(s)
}

// ciJobLink is the job name as a link to the job (inline code otherwise).
func ciJobLink(j core.CIJob) string {
	name := "**" + mdtext.InlineCode(j.Name) + "**"
	if mdtext.IsHTTPURL(j.WebURL) {
		return name + " ([job](" + j.WebURL + "))"
	}
	return name
}

// renderCIDigest renders the failed jobs (no header, no markers).
// withLogs adds the collapsed log tails. Every "<!--" of the untrusted text
// (log lines, job and test names, messages, failure reasons) is defused.
func renderCIDigest(d *ciDigest, withLogs bool) string {
	var b strings.Builder
	for _, jd := range d.Failed {
		b.WriteString("- ❌ " + ciJobLink(jd.Job) + ciJobMeta(jd.Job))
		switch {
		case jd.Kind == ciKindLint:
			b.WriteString(" — линтер " + mdtext.InlineCode(jd.LintTool) + ": его замечания публикуются тредами (блок «" + linters.CITitle + "»)\n")
		case jd.Kind == ciKindTests:
			fmt.Fprintf(&b, " — упало тестов: %d", jd.TestsFailed)
			if jd.TestsTotal > 0 {
				fmt.Fprintf(&b, " из %d", jd.TestsTotal)
			}
			b.WriteString("\n")
			for _, t := range jd.Tests {
				b.WriteString("  - " + ciTestLine(t) + "\n")
			}
		case jd.LogNote != "":
			b.WriteString(" — " + jd.LogNote + "\n")
		case !withLogs:
			b.WriteString(" — конец лога в треде «" + ciThreadTitle + "»\n")
		default:
			b.WriteString(" — конец лога ниже\n")
		}
	}
	if d.MoreTests > 0 {
		fmt.Fprintf(&b, "- … и ещё %d %s\n", d.MoreTests, mdtext.Plural(d.MoreTests, "упавший тест", "упавших теста", "упавших тестов"))
	}
	if withLogs {
		for _, jd := range d.Failed {
			if jd.Kind != ciKindLog || len(jd.Log) == 0 {
				continue
			}
			body := strings.Join(jd.Log, "\n")
			fence := mdtext.CodeFence(body)
			fmt.Fprintf(&b, "\n<details>\n<summary>%s: %d %s лога</summary>\n\n%stext\n%s\n%s\n\n</details>\n",
				html1(jd.Job.Name), len(jd.Log), mdtext.Plural(len(jd.Log), "последняя строка", "последние строки", "последних строк"), fence, body, fence)
		}
	}
	if len(d.Warnings) > 0 {
		var names []string
		for _, j := range d.Warnings {
			names = append(names, j.Name)
		}
		b.WriteString("\n⚠️ Упали, но им разрешено (`allow_failure`): " + mdtext.InlineCodeList(names, 10) + ".\n")
	}
	if len(d.Ignored) > 0 {
		b.WriteString("\nНе разбираются (`ignore_jobs`): " + mdtext.InlineCodeList(d.Ignored, 10) + ".\n")
	}
	return mdtext.DefuseComments(b.String())
}

// ciJobMeta is " · стадия `x` · reason" of a failed job ("" without either).
func ciJobMeta(j core.CIJob) string {
	var meta []string
	if j.Stage != "" {
		meta = append(meta, "стадия "+mdtext.InlineCode(j.Stage))
	}
	if r := ciReasonRu(j.FailureReason); r != "" {
		meta = append(meta, r)
	}
	if len(meta) == 0 {
		return ""
	}
	return " · " + strings.Join(meta, " · ")
}

// html1 makes a job name safe inside <summary> (HTML, one line).
func html1(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "'", "@", "@​").Replace(mdtext.Shorten(s, 120))
}

// ciTestLine: `file` · `class.name` — first line of the message.
func ciTestLine(t ciTestCase) string {
	var b strings.Builder
	if t.File != "" {
		b.WriteString(mdtext.CodeSpan(t.File) + " · ")
	}
	name := t.Name
	if t.Classname != "" && !strings.Contains(t.Name, t.Classname) {
		name = t.Classname + "." + t.Name
	}
	b.WriteString(mdtext.CodeSpan(name))
	if t.Status == "error" {
		b.WriteString(" (ошибка)")
	}
	if first, _, _ := strings.Cut(t.Message, "\n"); strings.TrimSpace(first) != "" {
		b.WriteString(" — " + mdtext.Inline(first, ciMsgRunes))
	}
	return b.String()
}

// renderCIDigestPrompt renders the digest for a prompt (inside an
// <untrusted_ci_failures> block written by the caller).
func renderCIDigestPrompt(d *ciDigest) string {
	var b strings.Builder
	for _, jd := range d.Failed {
		fmt.Fprintf(&b, "--- job: %s\n", mdtext.Untrusted(mdtext.OneLine(jd.Job.Name)))
		if jd.Job.Stage != "" {
			fmt.Fprintf(&b, "stage: %s\n", mdtext.Untrusted(mdtext.OneLine(jd.Job.Stage)))
		}
		if jd.Job.FailureReason != "" {
			fmt.Fprintf(&b, "failure_reason: %s\n", mdtext.Untrusted(mdtext.OneLine(jd.Job.FailureReason)))
		}
		switch jd.Kind {
		case ciKindLint:
			fmt.Fprintf(&b, "this is the repository's linter job %s (its findings are already published as review threads)\n", mdtext.Untrusted(jd.LintTool))
		case ciKindTests:
			fmt.Fprintf(&b, "failed tests: %d of %d\n", jd.TestsFailed, jd.TestsTotal)
			for _, t := range jd.Tests {
				fmt.Fprintf(&b, "* test %s", mdtext.Untrusted(mdtext.OneLine(strings.TrimPrefix(t.Classname+"."+t.Name, "."))))
				if t.File != "" {
					fmt.Fprintf(&b, " (file %s)", mdtext.Untrusted(mdtext.OneLine(t.File)))
				}
				fmt.Fprintf(&b, " [%s]\n", t.Status)
				if t.Message != "" {
					b.WriteString(mdtext.Indent(mdtext.Untrusted(t.Message), "    ") + "\n")
				}
			}
		default:
			if jd.LogNote != "" {
				fmt.Fprintf(&b, "log: not available (%s)\n", jd.LogNote)
				continue
			}
			fmt.Fprintf(&b, "last %d meaningful lines of the job log:\n%s\n", len(jd.Log), mdtext.Untrusted(strings.Join(jd.Log, "\n")))
		}
	}
	return b.String()
}

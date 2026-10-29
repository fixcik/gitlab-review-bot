package crossrepo

// System map, phase B: the summary block and the system prompt section.
// Everything from repositories, merge requests and the map is escaped or
// validated before it is shown.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/tasklink"
)

// safeProjectRe: project paths safe in a URL and in the system prompt.
var safeProjectRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.\-]*(/[A-Za-z0-9_][A-Za-z0-9_.\-]*)+$`)

// safeBranchRe: branch names shown verbatim.
var safeBranchRe = regexp.MustCompile(`^[A-Za-z0-9._/\-]{1,60}$`)

// safeProject: a project path safe in a URL and in the system prompt.
func safeProject(repo string) bool {
	return safeProjectRe.MatchString(repo) && !strings.Contains(repo, "..")
}

// mrText is "group/project!12" (unescaped).
func mrText(m *openMR) string {
	return m.repo + "!" + strconv.FormatInt(m.mr.IID, 10)
}

// mrRef renders "[group/project!12](url)": the URL is built from
// gitlab.url and the project path (never taken from the API).
func (xr *Feature) mrRef(m *openMR) string {
	if !safeProject(m.repo) || !mdtext.IsHTTPURL(xr.env.Cfg.GitLab.URL) {
		return mdtext.CodeSpan(mrText(m))
	}
	u := strings.TrimRight(xr.env.Cfg.GitLab.URL, "/") + "/" + m.repo + "/-/merge_requests/" + strconv.FormatInt(m.mr.IID, 10)
	return "[" + mdtext.Escape(mrText(m)) + "](" + u + ")"
}

// mrRefPlain is the reference for the prompt ("" when unsafe).
func mrRefPlain(m *openMR) string {
	if !safeProject(m.repo) {
		return ""
	}
	return mrText(m)
}

func branchText(b string) string {
	if safeBranchRe.MatchString(b) {
		return b
	}
	return "ветке по умолчанию"
}

// renderDepsBlock renders the summary block ("" when there is nothing to
// say). Everything from repositories, merge requests and the map is
// escaped: code spans, mdtext.Inline, links built from gitlab.url.
func (xr *Feature) renderDepsBlock(d *DepsRun, self string) string {
	var missing, linked []string
	for _, m := range d.missing {
		c := m.check.contract
		line := "- " + mdtext.CodeSpan(c.Name())
		if c.File != "" {
			line += " (" + citation(self, c.File, c.Line) + ")"
		}
		line += " нет в " + branchText(c.ProviderBranch) + " " + mdtext.CodeSpan(c.Provider) + " — "
		switch {
		case len(m.adding) > 0:
			var refs []string
			for _, o := range m.adding {
				ref := xr.mrRef(o)
				var notes []string
				notes = append(notes, "не смержен")
				if o.mr.Draft {
					notes = append(notes, "черновик")
				}
				if o.share != "" {
					notes = append(notes, o.share)
				}
				refs = append(refs, ref+" ("+strings.Join(notes, "; ")+")")
			}
			pron := "него"
			if len(refs) > 1 {
				pron = "них"
			}
			line += "добавляется в " + strings.Join(refs, ", ") + ": мержить после " + pron
		case m.checked:
			line += "открытого MR, который его добавляет, не найдено"
		case d.budgetHit:
			line += "открытые MR провайдера не проверены (лимит запросов к GitLab)"
		default:
			line += "открытые MR провайдера не проверены"
		}
		missing = append(missing, line)
	}
	for _, m := range d.linked {
		ref := xr.mrRef(m)
		head := "- " + ref
		if t := strings.TrimSpace(tasklink.StripDraft(m.mr.Title)); t != "" {
			head += " «" + mdtext.Inline(mdtext.OneLine(t), 80) + "»"
		}
		if m.mr.Draft {
			head += " (черновик)"
		}
		var rels []string
		for _, l := range d.links[m] {
			rel, _ := relation(l.check.contract.Role, l.act, mdtext.CodeSpan(l.check.contract.Name()))
			rels = append(rels, rel)
		}
		line := head + " — " + strings.Join(rels, "; ")
		if m.share != "" {
			line += "; " + m.share
		}
		line += "."
		if o := orderText(combineOrder(d.links[m]), mdtext.Escape(mrText(m))); o != "" {
			line += " " + o
		}
		linked = append(linked, line)
	}
	off := offTopologyLine(d.offTopology)
	if len(missing) == 0 && len(linked) == 0 && off == "" {
		return ""
	}
	var body strings.Builder
	if len(missing) > 0 {
		body.WriteString("**⏳ Зависит от неразвёрнутого:**\n\n" + strings.Join(missing, "\n") + "\n\n")
	}
	if len(linked) > 0 {
		body.WriteString("**🔗 Связанные MR:**\n\n" + strings.Join(linked, "\n") + "\n\n")
	}
	if off != "" {
		body.WriteString(off + "\n\n")
	}
	body.WriteString("<sub>По карте системы (ветки по умолчанию = задеплоено) и открытым MR соседних репозиториев. Подсказка: ничего не блокирует.</sub>")
	var b strings.Builder
	if len(missing)+len(linked) <= depsCollapseLines {
		b.WriteString(body.String() + "\n")
	} else {
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, fmt.Sprintf("⏳ %d %s", len(missing), mdtext.Plural(len(missing), "зависимость", "зависимости", "зависимостей")))
		}
		if len(linked) > 0 {
			parts = append(parts, fmt.Sprintf("🔗 %d %s", len(linked), mdtext.Plural(len(linked), "связанный MR", "связанных MR", "связанных MR")))
		}
		b.WriteString("<details>\n<summary>Зависимости между репозиториями: " + strings.Join(parts, ", ") + "</summary>\n\n")
		b.WriteString(body.String() + "\n\n</details>\n")
	}
	return mdtext.NeutralizeQuickActions(summary.Block(marker.SysmapDepsStart+"\n", b.String(), marker.SysmapDepsEnd))
}

// maxOffTopology caps the calls named in the off-topology line.
const maxOffTopology = 5

// protoName labels a protocol in the summary.
var protoName = map[sysmap.Protocol]string{sysmap.HTTP: "HTTP", sysmap.Webhook: "вебхук", sysmap.WS: "WebSocket", sysmap.SSE: "SSE"}

// offTopologyLine is the one summary line about new calls outside the
// declared topology ("" when none): informative, never a finding.
func offTopologyLine(cs []sysmap.DepContract) string {
	if len(cs) == 0 {
		return ""
	}
	var items []string
	for i, c := range cs {
		if i == maxOffTopology {
			items = append(items, fmt.Sprintf("и ещё %d", len(cs)-i))
			break
		}
		items = append(items, mdtext.CodeSpan(c.Name())+" → "+mdtext.CodeSpan(c.Provider)+" ("+protoName[c.Protocol]+")")
	}
	return "🧭 Вне заявленной архитектуры (`review.sysmap.topology`): " + strings.Join(items, ", ") + "."
}

// renderDepsPrompt is the system prompt section: the same facts with only
// normalized names, validated paths, merge request references and Jira
// keys (titles and raw code stay out), plus the severity rule.
func renderDepsPrompt(d *DepsRun, self string, rc *RunContext, docker, search bool) string {
	if len(d.missing) == 0 && len(d.linked) == 0 {
		return ""
	}
	mounts := rc.Mounts(docker)
	repoText := func(repo string) string {
		if !safeProject(repo) {
			return "another repository"
		}
		if m, ok := mounts[strings.ToLower(repo)]; ok {
			return repo + " (mounted at " + m + ")"
		}
		return repo
	}
	var b strings.Builder
	b.WriteString("\n\n## System map: cross-repository dependencies of this merge request\n")
	b.WriteString("Deterministic facts from the system map (each repository's default branch = what is deployed; every repository deploys on its own) and from the open merge requests of the other repositories. The map may miss a route, so treat them as leads, not proof.\n")
	if len(d.missing) > 0 {
		b.WriteString("Not deployed yet:\n")
		for _, m := range d.missing {
			c := m.check.contract
			fmt.Fprintf(&b, "- this merge request starts to use %s %s", c.Protocol, c.Name())
			if promptPathRe.MatchString(c.File) {
				fmt.Fprintf(&b, " (%s:%d)", c.File, c.Line)
			}
			branch := "default branch"
			if safeBranchRe.MatchString(c.ProviderBranch) {
				branch += " " + c.ProviderBranch
			}
			fmt.Fprintf(&b, "; the provider %s does not have it on its %s", repoText(c.Provider), branch)
			var refs []string
			for _, o := range m.adding {
				if ref := mrRefPlain(o); ref != "" {
					refs = append(refs, ref)
				}
			}
			switch {
			case len(refs) > 0:
				fmt.Fprintf(&b, "; open merge request %s adds it (not merged), so this one must be merged after it", strings.Join(refs, ", "))
			case m.checked:
				b.WriteString("; no open merge request of the provider adds it")
			}
			b.WriteString(".\n")
		}
	}
	if len(d.linked) > 0 {
		b.WriteString("Related open merge requests in other repositories (they touch the same contracts):\n")
		for _, m := range d.linked {
			ref := mrRefPlain(m)
			if ref == "" {
				continue
			}
			var rels []string
			for _, l := range d.links[m] {
				rels = append(rels, relationEN(l.check.contract.Role, l.act, string(l.check.contract.Protocol)+" "+l.check.contract.Name()))
			}
			fmt.Fprintf(&b, "- %s: %s", ref, strings.Join(rels, "; "))
			if m.sharePrompt != "" {
				b.WriteString("; " + m.sharePrompt)
			}
			b.WriteString("." + orderTextEN(combineOrder(d.links[m]), ref) + "\n")
		}
	}
	b.WriteString("These facts are already shown to the team in the summary note; do not repeat them as findings. Report a finding only for a problem you verified in the code (for example a call of this merge request (" + self + ") to an endpoint or event that does not exist in the provider, or a contract mismatch with the related merge request): check the provider's code in the context repositories")
	if search {
		b.WriteString(" or delegate the search to the `code-search` subagent")
	}
	b.WriteString(". Deployment order and not-yet-deployed dependencies are a process matter, not a defect of this code: a finding based on this information must have severity at most \"major\" and must NEVER be \"critical\".\n")
	return b.String()
}

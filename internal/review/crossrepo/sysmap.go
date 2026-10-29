package crossrepo

// System map (internal/sysmap) in reviews: which contracts of the reviewed
// repository the merge request touches (handlers whose lines the diff
// changes, endpoints of the OpenAPI changelog) and who uses them in other
// repositories. Shown as a summary block and given to Claude as a list of
// usages to verify first. Never fails the review: without the map, for a
// repository outside it, or on any error there is simply no block.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// SystemMap answers which contracts a change touches (implemented by
// *sysmap.Map).
type SystemMap interface {
	Impact(repo string, changed map[string][]sysmap.LineRange, endpoints []sysmap.EndpointRef) *sysmap.Impact
}

const (
	// sysmapCollapseUsages: more usages than this -> collapsed <details>.
	sysmapCollapseUsages = 6
	// sysmapCitesPerRepo: citations listed per repository and contract.
	sysmapCitesPerRepo = 3
	// sysmapPromptUsages caps the usages listed in the system prompt.
	sysmapPromptUsages = 40
)

// SysmapRun is the map's answer for one review.
type SysmapRun struct {
	Impact *sysmap.Impact
	Block  string // summary block with markers
	System string // system prompt section
}

// SysmapDiff is the diff the map works on: the full MR diff (merge base ..
// head), because the map's line numbers refer to the default branch. The
// reviewed diff is reused unless the review is incremental. "" when the
// map is off or the full diff cannot be computed (logged).
func (xr *Feature) SysmapDiff(ctx context.Context, job core.Job, co core.Checkout, mr *core.MergeRequest, eff config.Effective,
	diff string, incremental bool) string {
	if xr.sysmap == nil || !xr.env.Cfg.SysmapEnabled() {
		return ""
	}
	if !incremental {
		return diff
	}
	full, err := co.Diff(ctx, diffcap.FullDiffBase(mr), eff.Ignore)
	if err != nil {
		xr.env.Log.Warn("review: system map: full diff", "mr", job.MRKey(), "err", err)
		return ""
	}
	return full
}

// CollectSysmap asks the map what the MR touches. diff is the full MR diff
// (SysmapDiff).
func (xr *Feature) CollectSysmap(ctx context.Context, job core.Job, diff string, api *compatcheck.APIRun, rc *RunContext) (out *SysmapRun) {
	ctx, tsp := tracing.Start(ctx, "review.sysmap")
	defer tsp.End()
	if xr.sysmap == nil || !xr.env.Cfg.SysmapEnabled() || diff == "" {
		return nil
	}
	log := xr.env.Log.With("mr", job.MRKey())
	defer func() {
		if p := recover(); p != nil {
			log.Error("review: system map panicked, continuing without it", "panic", p)
			jobctx.BotProblem(ctx, problems.SysmapFailed, "карта системы: внутренняя ошибка", fmt.Sprint(p))
			out = nil
		}
	}()
	imp := xr.sysmap.Impact(job.ProjectPath, oldSideRanges(diff), apiEndpoints(api))
	if imp == nil || len(imp.Contracts) == 0 {
		return nil
	}
	sm := &SysmapRun{Impact: imp}
	sm.Block = rc.Link(renderSysmapBlock(imp, job.ProjectPath))
	sm.System = renderSysmapPrompt(imp, job.ProjectPath, rc, xr.env.DockerSandbox(), xr.env.Cfg.Claude.SearchModel != "")
	n := 0
	for _, c := range imp.Contracts {
		n += len(c.Usages)
	}
	log.Info("review: system map", "contracts", len(imp.Contracts), "usages", n)
	return sm
}

// apiEndpoints are the changed or removed endpoints of the OpenAPI
// changelog (added ones have no consumers yet).
func apiEndpoints(api *compatcheck.APIRun) []sysmap.EndpointRef {
	if api == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []sysmap.EndpointRef
	for _, s := range api.Specs {
		for _, it := range s.Items {
			if it.Endpoint == "" || it.Operation == "" || compatcheck.EndpointAdded(it.Rule) {
				continue
			}
			k := it.Operation + " " + it.Endpoint
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, sysmap.EndpointRef{Method: it.Operation, Path: it.Endpoint})
		}
	}
	return out
}

// contractTitle names a touched contract for humans.
func contractTitle(c sysmap.Touched) string {
	name := mdtext.CodeSpan(c.Name())
	switch {
	case c.Sender:
		return "вебхук " + name + " (отправляет этот репозиторий)"
	case c.Protocol == sysmap.Webhook:
		return "вебхук " + name
	case c.Protocol == sysmap.SSE:
		return "SSE " + name
	case c.Protocol == sysmap.WS:
		return "WebSocket " + name
	}
	return name
}

// usageWord counts usages: "3 вызова", "1 подписка", "2 получателя".
func usageWord(c sysmap.Touched, n int) string {
	switch {
	case c.Sender:
		return fmt.Sprintf("%d %s", n, mdtext.Plural(n, "получатель", "получателя", "получателей"))
	case c.Protocol == sysmap.Webhook:
		return fmt.Sprintf("%d %s", n, mdtext.Plural(n, "отправка", "отправки", "отправок"))
	case c.Protocol == sysmap.SSE || c.Protocol == sysmap.WS:
		return fmt.Sprintf("%d %s", n, mdtext.Plural(n, "подписка", "подписки", "подписок"))
	}
	return fmt.Sprintf("%d %s", n, mdtext.Plural(n, "вызов", "вызова", "вызовов"))
}

type repoUsages struct {
	repo   string
	usages []sysmap.Usage
}

func groupByRepo(us []sysmap.Usage) []repoUsages {
	var out []repoUsages
	idx := map[string]int{}
	for _, u := range us {
		k := strings.ToLower(u.Repo)
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, repoUsages{repo: u.Repo})
		}
		out[i].usages = append(out[i].usages, u)
	}
	return out
}

// citation is `group/project:path:line` as inline code (turned into a link
// by the citation linker).
func citation(repo, file string, line int) string {
	s := repo + ":" + file
	if line > 0 {
		s += ":" + strconv.Itoa(line)
	}
	return mdtext.CodeSpan(s)
}

func repoUsageText(c sysmap.Touched, g repoUsages, possible bool) string {
	var cites []string
	for i, u := range g.usages {
		if i == sysmapCitesPerRepo {
			cites = append(cites, fmt.Sprintf("и ещё %d", len(g.usages)-sysmapCitesPerRepo))
			break
		}
		cites = append(cites, citation(u.Repo, u.File, u.Line))
	}
	what := usageWord(c, len(g.usages))
	if possible {
		what = "возможно, " + what
	}
	return mdtext.CodeSpan(g.repo) + " — " + what + " (" + strings.Join(cites, ", ") + ")"
}

// renderSysmapBlock renders the summary block with markers ("" when
// nothing touched is used elsewhere).
func renderSysmapBlock(imp *sysmap.Impact, self string) string {
	if imp == nil || len(imp.Contracts) == 0 {
		return ""
	}
	var lines []string
	usages, repos := 0, map[string]bool{}
	for _, c := range imp.Contracts {
		var parts []string
		for _, g := range groupByRepo(c.Usages) {
			parts = append(parts, repoUsageText(c, g, false))
			repos[strings.ToLower(g.repo)] = true
		}
		for _, g := range groupByRepo(c.Possible) {
			parts = append(parts, repoUsageText(c, g, true))
			repos[strings.ToLower(g.repo)] = true
		}
		usages += len(c.Usages) + len(c.Possible) + c.More
		where := ""
		if c.File != "" {
			where = " (" + citation(self, c.File, c.Line) + ")"
		}
		more := ""
		if c.More > 0 {
			more = fmt.Sprintf("; и ещё %d", c.More)
		}
		lines = append(lines, "- "+contractTitle(c)+where+": "+strings.Join(parts, "; ")+more)
	}
	foot := "<sub>По карте системы: ветки по умолчанию репозиториев"
	if imp.SHA != "" {
		foot += ", этот репозиторий @ " + mdtext.CodeSpan(mdtext.ShortSHA(imp.SHA))
	}
	foot += ". «Возможно» — вызов с тем же путём, но адрес сервиса не определён; подсказки и неразрешённые вызовы — на странице `/map` бота.</sub>"
	var b strings.Builder
	if usages <= sysmapCollapseUsages && len(lines) <= 3 {
		b.WriteString("**🔗 Затронуто в других репозиториях:**\n\n")
		b.WriteString(strings.Join(lines, "\n") + "\n\n" + foot + "\n")
	} else {
		fmt.Fprintf(&b, "<details>\n<summary>🔗 Затронуто в других репозиториях: %d %s, %d %s</summary>\n\n",
			len(repos), mdtext.Plural(len(repos), "репозиторий", "репозитория", "репозиториев"),
			usages, mdtext.Plural(usages, "использование", "использования", "использований"))
		b.WriteString(strings.Join(lines, "\n") + "\n\n" + foot + "\n\n</details>\n")
	}
	return summary.Block(marker.SysmapStart+"\n", b.String(), marker.SysmapEnd) // names from other repositories' code
}

// PromptKind names the kind of a touched contract in prompts and tool
// answers.
func PromptKind(c sysmap.Touched) string {
	if c.Sender {
		return "webhook sent by this repository"
	}
	return string(c.Protocol)
}

// promptPathRe: file paths safe to put into the system prompt verbatim.
var promptPathRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./@+\-]*$`)

// SafePromptPath reports whether a file path of another repository is safe
// to put into a prompt or a tool answer verbatim: restricted characters, no
// "..".
func SafePromptPath(p string) bool {
	return promptPathRe.MatchString(p) && !strings.Contains(p, "..")
}

// renderSysmapPrompt is the system prompt section: the touched contracts
// and their usages with mount paths, to verify first. Only normalized
// names (restricted character sets), validated paths and line numbers go
// in; raw expressions from repositories stay out.
func renderSysmapPrompt(imp *sysmap.Impact, self string, rc *RunContext, docker, search bool) string {
	if imp == nil || len(imp.Contracts) == 0 {
		return ""
	}
	mounts := rc.Mounts(docker)
	loc := func(u sysmap.Usage) string {
		if !SafePromptPath(u.File) {
			return ""
		}
		where := u.File
		if m, ok := mounts[strings.ToLower(u.Repo)]; ok {
			where = m + "/" + u.File
		}
		if u.Line > 0 {
			where += ":" + strconv.Itoa(u.Line)
		}
		return where
	}
	var b strings.Builder
	b.WriteString("\n\n## System map: other repositories use what this merge request touches\n")
	b.WriteString("The bot keeps a map of the contracts between the team's repositories (HTTP endpoints, webhooks, WebSocket and SSE), built from their main (deployed) branches; consumers are matched to providers by method and normalized path or event name. It may be incomplete or slightly stale. According to it, this merge request changes contracts that other repositories use:\n")
	listed := 0
	for _, c := range imp.Contracts {
		fmt.Fprintf(&b, "- %s %s", PromptKind(c), c.Name())
		if c.File != "" && promptPathRe.MatchString(c.File) {
			fmt.Fprintf(&b, " (defined in this repository at %s:%d)", c.File, c.Line)
		} else if c.File == "" {
			b.WriteString(" (changed in the OpenAPI spec)")
		}
		b.WriteString(":\n")
		for _, grp := range []struct {
			label string
			us    []sysmap.Usage
		}{{"used by", c.Usages}, {"possibly used by (unresolved call: the same path, the service address is unknown)", c.Possible}} {
			for _, g := range groupByRepo(grp.us) {
				var ls []string
				for _, u := range g.usages {
					if listed >= sysmapPromptUsages {
						break
					}
					if l := loc(u); l != "" {
						ls = append(ls, l)
						listed++
					}
				}
				if len(ls) == 0 {
					continue
				}
				note := ""
				if _, ok := mounts[strings.ToLower(g.repo)]; !ok {
					note = " (not mounted in this run)"
				}
				fmt.Fprintf(&b, "  - %s %s%s: %s\n", grp.label, g.repo, note, strings.Join(ls, ", "))
			}
		}
	}
	b.WriteString("Verify these usages FIRST, before searching the repositories yourself: read each one and check that it still works with the changed contract (paths and methods, request and response shape, required fields, enum values, status codes, event names and payloads, semantics). Report a break as a finding on the changed line of THIS repository (" + self + ") and cite the affected usage as `group/project:path:line`.")
	if search {
		b.WriteString(" Delegate reading many usages to the `code-search` subagent.")
	}
	b.WriteString(" The list is a starting point, not the complete set of consumers.\n")
	return b.String()
}

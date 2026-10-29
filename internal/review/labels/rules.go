package labels

import (
	"regexp"
	"sort"

	"github.com/fixcik/gitlab-review-bot/internal/compat"
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
)

// tri is a three-valued answer; the zero value is "unknown".
type tri int8

const (
	triUnknown tri = iota
	triNo
	triYes
)

// inputs is what the rules are evaluated on.
type inputs struct {
	// files: every file the MR changes (merge base .. head, ignore globs
	// applied); filesKnown false when they could not be listed.
	files      []string
	filesKnown bool
	// facts: deterministic facts (config.LabelFacts); absent = unknown.
	facts map[string]tri
	// model: the instruction labels the model chose; modelAnswered: the
	// answer had the field; incremental: the model saw only new commits
	// (its "no" does not count then).
	model         map[string]bool
	modelAnswered bool
	incremental   bool
}

// evalRule: yes when any condition says yes; no when every condition
// definitely says no; unknown otherwise.
func evalRule(r config.LabelRule, in inputs) tri {
	res := triNo
	merge := func(t tri) {
		switch {
		case t == triYes:
			res = triYes
		case t == triUnknown && res != triYes:
			res = triUnknown
		}
	}
	if len(r.Paths) > 0 {
		if !in.filesKnown {
			merge(triUnknown)
		} else {
			m := triNo
			for _, f := range in.files {
				if r.MatchFile(f) {
					m = triYes
					break
				}
			}
			merge(m)
		}
	}
	for _, f := range r.WhenFact {
		merge(in.facts[f])
	}
	if r.Instruction != "" {
		switch {
		case in.model[r.Name]:
			merge(triYes)
		case !in.modelAnswered || in.incremental:
			merge(triUnknown)
		default:
			merge(triNo)
		}
	}
	return res
}

var (
	labelTestRe = regexp.MustCompile(`(?i)(^|/)(tests?|spec|specs|__tests__|testdata|fixtures|e2e)/|_test\.go$|(^|/)test_[^/]*\.py$|_test\.py$|tests?\.(php|java|kt|cs|swift|scala)$|\.(spec|test)\.[cm]?[jt]sx?$|_spec\.rb$`)
	labelCodeRe = regexp.MustCompile(`(?i)\.(go|py|js|jsx|mjs|cjs|ts|tsx|vue|svelte|php|java|kt|kts|scala|rb|cs|rs|swift|c|cc|cpp|h|hpp|m)$`)
	labelVendRe = regexp.MustCompile(`(^|/)(vendor|node_modules|third_party)/`)
)

// isTestFile / isCodeFile: the no_tests_changed heuristics.
func isTestFile(p string) bool { return labelTestRe.MatchString(p) }

func isCodeFile(p string) bool {
	return labelCodeRe.MatchString(p) && !labelVendRe.MatchString(p) && !isTestFile(p)
}

// mrFacts computes the deterministic facts of the whole MR from the
// review's analyzers. A fact whose step is off or failed stays unknown.
func mrFacts(eff config.Effective, in *inputs, run Run) map[string]tri {
	f := map[string]tri{}
	yesNo := func(b bool) tri {
		if b {
			return triYes
		}
		return triNo
	}
	// migrations: the analysis runs on the full MR diff.
	if eff.Migrations.Enabled {
		switch {
		case run.Migrations != nil && run.Migrations.Res != nil && len(run.Migrations.Res.Files) > 0:
			f[config.FactMigrations] = triYes
		case in.filesKnown:
			f[config.FactMigrations] = triNo
		}
	}
	// api_changed / api_breaking: compat discovery + deterministic checkers
	// (full MR), or the OpenAPI changelog when the compat check is off.
	breaking3 := func(specs []compat.APIChanges) bool {
		for _, s := range specs {
			for _, it := range s.Items {
				if it.Level >= 3 {
					return true
				}
			}
		}
		return false
	}
	switch {
	case run.Compat != nil:
		if res := run.Compat.Res; res != nil {
			f[config.FactAPIChanged] = yesNo(len(res.Specs)+len(res.Deleted) > 0 || len(res.API) > 0)
			failed := false
			for _, is := range res.Issues {
				if is.Kind == compat.IssueUnavailable || is.Kind == compat.IssueFailed {
					failed = true
				}
			}
			switch {
			case len(res.Changes) > 0 || breaking3(res.API):
				f[config.FactAPIBreaking] = triYes
			case !failed:
				f[config.FactAPIBreaking] = triNo
			}
		}
	case run.API != nil:
		switch {
		case len(run.API.Specs) > 0:
			f[config.FactAPIChanged] = triYes
		case len(run.API.Issues) == 0:
			f[config.FactAPIChanged] = triNo
		}
		switch {
		case breaking3(run.API.Specs):
			f[config.FactAPIBreaking] = triYes
		case len(run.API.Issues) == 0:
			f[config.FactAPIBreaking] = triNo
		}
	}
	// big_mr: the size warning of the trunk-based block.
	if ts := run.TBD; ts != nil && ts.SizeKnown && eff.TBD.MaxLines > 0 {
		switch total := ts.Added + ts.Removed; {
		case total > eff.TBD.MaxLines:
			f[config.FactBigMR] = triYes
		case !ts.SizePartial:
			f[config.FactBigMR] = triNo
		}
	}
	// lint_secret: gitleaks over every commit of the MR.
	if l := run.Lint; l != nil && l.Res != nil {
		found := false
		for _, x := range l.Res.Findings {
			if x.Secret() {
				found = true
				break
			}
		}
		switch {
		case found:
			f[config.FactLintSecret] = triYes
		case l.Res.ToolOK(lint.ToolGitleaks):
			f[config.FactLintSecret] = triNo
		}
	}
	// no_tests_changed: code files changed, no test file changed.
	if in.filesKnown {
		code, tests := false, false
		for _, p := range in.files {
			code = code || isCodeFile(p)
			tests = tests || isTestFile(p)
		}
		f[config.FactNoTestsChanged] = yesNo(code && !tests)
	}
	return f
}

// labelPlan is what to change on the MR, and the bot's label state after
// reconciling it with the MR's current labels.
type labelPlan struct {
	add, remove []string
	// added / suppressed: the bot's state after reconciliation (before the
	// changes are applied).
	added, suppressed map[string]bool
	// held: labels whose rule applies but which are not set because a
	// human removed them earlier (suppressed) or another label of the
	// same scope was set by a human.
	held []string
}

func toSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, l := range list {
		m[l] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// planLabels computes the changes. Rules are in priority order: of several
// applying labels of one scope ("type::bug", "type::feature") only the
// first is set. Bot labels missing from the MR were removed by a human:
// they become suppressed and are never set again on this MR; a suppressed
// label a human put back is the human's (no longer the bot's).
func planLabels(l config.EffLabels, in inputs, current, added, suppressed []string) labelPlan {
	cur := toSet(current)
	p := labelPlan{added: map[string]bool{}, suppressed: toSet(suppressed)}
	for _, a := range added {
		if cur[a] {
			p.added[a] = true
		} else {
			p.suppressed[a] = true
		}
	}
	for s := range p.suppressed {
		if cur[s] {
			delete(p.suppressed, s)
		}
	}

	status := map[string]tri{}
	desired := map[string]bool{}
	scopeWinner := map[string]string{}
	for _, r := range l.Rules {
		st := evalRule(r, in)
		status[r.Name] = st
		if st != triYes {
			continue
		}
		if sc := config.LabelScope(r.Name); sc != "" {
			if _, taken := scopeWinner[sc]; taken {
				continue
			}
			scopeWinner[sc] = r.Name
		}
		desired[r.Name] = true
	}
	onMR := map[string]bool{} // scopes whose winner ends up on the MR
	for _, r := range l.Rules {
		name := r.Name
		if !desired[name] {
			continue
		}
		sc := config.LabelScope(name)
		switch {
		case cur[name]:
			onMR[sc] = true
			continue
		case p.suppressed[name]:
			p.held = append(p.held, name)
			continue
		}
		if sc != "" {
			human := false
			for c := range cur {
				if c != name && config.LabelScope(c) == sc && !p.added[c] {
					human = true
					break
				}
			}
			if human {
				p.held = append(p.held, name)
				continue
			}
		}
		onMR[sc] = true
		p.add = append(p.add, name)
	}
	if l.RemoveStale {
		for _, a := range sortedKeys(p.added) {
			if desired[a] {
				continue
			}
			st, known := status[a]
			if !known {
				st = triNo // the rule was removed from the configuration
			}
			if st == triUnknown {
				continue
			}
			if st == triYes {
				// Lost to another label of its scope: removed only when the
				// winner is (or will be) on the MR.
				if sc := config.LabelScope(a); sc == "" || !onMR[sc] {
					continue
				}
			}
			p.remove = append(p.remove, a)
		}
	}
	return p
}

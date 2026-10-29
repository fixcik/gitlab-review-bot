package core

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// Every JobKind (TestAllJobKindsComplete checks AllJobKinds against the
// constants) has a complete entry in the kind table.
func TestKindTableComplete(t *testing.T) {
	if len(jobKinds) != len(kindIndex) {
		t.Fatalf("kind table has %d entries for %d kinds (duplicate kind?)", len(jobKinds), len(kindIndex))
	}
	usage := map[UsageKind]bool{UsageAuto: true, UsageChat: true, UsageRerun: true}
	runKinds := map[string]bool{runKindReview: true, runKindChat: true, runKindVerification: true, runKindLearning: true, runKindOther: true}
	words := map[string]JobKind{}
	for _, k := range AllJobKinds() {
		info, ok := k.Info()
		if !ok {
			t.Errorf("%s: no entry in the kind table", k)
			continue
		}
		if info.Label == "" {
			t.Errorf("%s: no Label", k)
		}
		if !usage[info.Usage] {
			t.Errorf("%s: Usage %q", k, info.Usage)
		}
		if !runKinds[info.RunKind] {
			t.Errorf("%s: RunKind %q", k, info.RunKind)
		}
		if (info.Reacts || len(info.Commands) > 0) && info.Help == "" {
			t.Errorf("%s: a command without Help", k)
		}
		if (info.MRCommand || info.CommandReason) && len(info.Commands) == 0 {
			t.Errorf("%s: MRCommand/CommandReason without Commands", k)
		}
		for _, w := range info.Commands {
			if w != strings.ToLower(strings.Join(strings.Fields(w), " ")) || strings.TrimRight(w, ".!?") != w {
				t.Errorf("%s: command %q is not normalized", k, w)
			}
			if other, dup := words[w]; dup {
				t.Errorf("command %q requests both %s and %s", w, other, k)
			}
			words[w] = k
		}
	}
	if _, ok := JobKind("no_such_kind").Info(); ok {
		t.Error("unknown kind has an entry")
	}
}

// The help line of a fixed-phrase command names phrases it accepts.
func TestKindHelpNamesCommands(t *testing.T) {
	quoted := regexp.MustCompile("`\\{mention\\} ([^`<#\\[]+)`")
	for _, k := range AllJobKinds() {
		info, _ := k.Info()
		if len(info.Commands) == 0 {
			continue
		}
		m := quoted.FindAllStringSubmatch(info.Help, -1)
		if len(m) == 0 {
			t.Errorf("%s: help names no command", k)
		}
		for _, s := range m {
			found := false
			for _, w := range info.Commands {
				found = found || w == s[1]
			}
			if !found {
				t.Errorf("%s: help names %q, not one of %q", k, s[1], info.Commands)
			}
		}
	}
	if got := JobPause.Help("@rb"); !strings.HasPrefix(got, "- `@rb pause` (`@rb пауза`) — ") || strings.Contains(got, "{mention}") {
		t.Errorf("Help(@rb) = %q", got)
	}
	if JobAutoReview.Help("@rb") != "" {
		t.Error("auto review has help")
	}
}

// The lists derived from the kind table are the ones the packages kept
// before the table existed (meter.KindForJob, env usageKind, the reaction
// lifecycle, diagweb labels, Manual).
func TestKindTableDerivedLists(t *testing.T) {
	collect := func(pred func(KindInfo) bool) []JobKind {
		var out []JobKind
		for _, i := range jobKinds {
			if pred(i) {
				out = append(out, i.Kind)
			}
		}
		return out
	}
	check := func(name string, got, want []JobKind) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %v\nwant %v", name, got, want)
		}
	}
	if got := AllJobKinds(); !reflect.DeepEqual(got, []JobKind{
		JobAutoReview, JobReview, JobFullReview, JobChat, JobHelp, JobRefreshTitle, JobRefreshStatus, JobThreadReply,
		JobRules, JobPause, JobResume, JobIgnore, JobResolveAll, JobConfig, JobMemory, JobCheck, JobForceReview, JobReact,
		JobPipeline,
	}) {
		t.Errorf("AllJobKinds order changed: %v", got)
	}
	check("not Manual", collect(func(i KindInfo) bool { return !i.Kind.Manual() }), []JobKind{JobAutoReview})
	if !JobKind("unknown").Manual() || !JobRefreshTitle.Manual() || !JobPipeline.Manual() {
		t.Error("Manual must be true for every kind but auto_review")
	}
	check("UsageAuto", collect(func(i KindInfo) bool { return i.Usage == UsageAuto }), []JobKind{JobAutoReview, JobPipeline})
	check("UsageChat", collect(func(i KindInfo) bool { return i.Usage == UsageChat }), []JobKind{JobChat})
	check("run kind review", collect(func(i KindInfo) bool { return i.RunKind == runKindReview }),
		[]JobKind{JobAutoReview, JobReview, JobFullReview, JobForceReview})
	check("run kind chat", collect(func(i KindInfo) bool { return i.RunKind == runKindChat }), []JobKind{JobChat})
	check("run kind learning", collect(func(i KindInfo) bool { return i.RunKind == runKindLearning }), []JobKind{JobThreadReply, JobRules})
	check("run kind verification", collect(func(i KindInfo) bool { return i.RunKind == runKindVerification }), []JobKind{JobCheck})
	check("Reacts", collect(func(i KindInfo) bool { return i.Reacts }), []JobKind{
		JobReview, JobFullReview, JobChat, JobHelp, JobRules, JobPause, JobResume,
		JobIgnore, JobResolveAll, JobConfig, JobMemory, JobCheck, JobForceReview,
	})
	check("MRCommand", collect(func(i KindInfo) bool { return i.MRCommand }), []JobKind{
		JobPause, JobResume, JobIgnore, JobResolveAll, JobConfig, JobCheck,
	})
	check("CommandReason", collect(func(i KindInfo) bool { return i.CommandReason }), []JobKind{JobPause, JobResume})
	check("Help", collect(func(i KindInfo) bool { return i.Help != "" }), []JobKind{
		JobReview, JobFullReview, JobChat, JobHelp, JobThreadReply, JobRules, JobPause, JobResume,
		JobIgnore, JobResolveAll, JobConfig, JobMemory, JobCheck, JobForceReview,
	})
	labels := map[JobKind]string{}
	for _, i := range jobKinds {
		labels[i.Kind] = i.Label
	}
	if want := map[JobKind]string{
		JobAutoReview: "авторевью", JobReview: "ревью", JobFullReview: "полное ревью", JobChat: "вопрос",
		JobHelp: "помощь", JobRefreshTitle: "заголовок", JobRefreshStatus: "статус TBD", JobThreadReply: "обучение",
		JobRules: "правила", JobPause: "пауза", JobResume: "продолжить", JobIgnore: "ignore",
		JobResolveAll: "закрыть треды", JobConfig: "конфиг", JobMemory: "память", JobReact: "реакция",
		JobCheck: "перепроверка", JobForceReview: "ревью принудительно", JobPipeline: "пайплайн CI",
	}; !reflect.DeepEqual(labels, want) {
		t.Errorf("labels:\n got %v\nwant %v", labels, want)
	}
}

// The service commands close the thread they started once answered; questions,
// rule and memory talk and the automatic kinds leave it open for a follow-up.
func TestKindsClosingTheirThread(t *testing.T) {
	closing := map[JobKind]bool{
		JobReview: true, JobFullReview: true, JobForceReview: true, JobHelp: true, JobPause: true,
		JobResume: true, JobIgnore: true, JobResolveAll: true, JobConfig: true, JobCheck: true,
	}
	for _, k := range AllJobKinds() {
		info, _ := k.Info()
		if info.ClosesThread != closing[k] {
			t.Errorf("%s: ClosesThread = %v, want %v", k, info.ClosesThread, closing[k])
		}
	}
}

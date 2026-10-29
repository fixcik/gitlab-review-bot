package events

import (
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Pipeline events without an identity or (outside an MR) without a ref are
// dropped with a reason; an MR pipeline needs no ref (the MR names it).
func TestRouterPipelineRejectsIncompleteEvents(t *testing.T) {
	r := NewRouter(Options{BotUsername: "reviewbot", Pipelines: true, Now: func() time.Time { return fixedNow }})
	ok := PipelineEvent{ProjectID: 42, ProjectPath: "group/api", DefaultBranch: "main", PipelineID: 5, Ref: "feature",
		SHA: "abc", Status: "failed"}
	for _, tc := range []struct {
		name   string
		mutate func(e *PipelineEvent)
		why    string
	}{
		{"no project id", func(e *PipelineEvent) { e.ProjectID = 0 }, "missing project or pipeline id"},
		{"no pipeline id", func(e *PipelineEvent) { e.PipelineID = 0 }, "missing project or pipeline id"},
		{"branch pipeline without ref", func(e *PipelineEvent) { e.Ref = "" }, "pipeline without ref"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := ok
			tc.mutate(&e)
			j, why := r.Pipeline(e)
			if j != nil || why != tc.why {
				t.Fatalf("got %+v %q, want nil %q", j, why, tc.why)
			}
		})
	}
	// The unmodified event is accepted (the cases above fail for their own
	// reason only): a branch pipeline, not the main branch.
	if j, why := r.Pipeline(ok); j == nil || j.Kind != core.JobPipeline || j.Pipeline.Main || j.MRIID != 0 {
		t.Fatalf("baseline event: %+v %q", j, why)
	}
	// An MR pipeline is routed by its MR even without a ref.
	mr := ok
	mr.Ref, mr.MRIID = "", 7
	j, why := r.Pipeline(mr)
	if j == nil || j.MRIID != 7 || j.Pipeline.Main || j.Pipeline.Ref != "" {
		t.Fatalf("MR pipeline without ref: %+v %q", j, why)
	}
}

// ParseCommand with no mention configured never matches: an empty mention
// would otherwise "occur" at offset 0 of every note and turn it into help.
func TestParseCommandEmptyMention(t *testing.T) {
	for _, text := range []string{"", "review", "@reviewbot review", " "} {
		if k, q, ok := ParseCommand(text, ""); ok || k != "" || q != "" {
			t.Errorf("ParseCommand(%q, \"\") = %q %q %v", text, k, q, ok)
		}
	}
	// The same text with the mention configured does match.
	if k, _, ok := ParseCommand("@reviewbot review", "@reviewbot"); !ok || k != core.JobReview {
		t.Fatalf("control: %q %v", k, ok)
	}
}

// findMention matches the mention as a whole word only, case-insensitively:
// not inside a longer username, an e-mail or another mention, and returns
// the byte range of the first real occurrence.
func TestFindMention(t *testing.T) {
	const m = "@reviewbot"
	for _, tc := range []struct {
		text       string
		start, end int
		ok         bool
	}{
		{"@reviewbot", 0, 10, true},
		{"@ReviewBot review", 0, 10, true},
		{"hi @reviewbot", 3, 13, true},
		{"@reviewbot, why?", 0, 10, true},
		{"@reviewbot.", 0, 10, true},                // the end of a sentence
		{"ask @reviewbot. Now", 4, 14, true},        // a sentence ends, a new one starts
		{"(@reviewbot)", 1, 11, true},               // punctuation around
		{"привет @reviewbot", 13, 23, true},         // byte offsets after Cyrillic
		{"@reviewbot2 @reviewbot ok", 12, 22, true}, // the first whole-word one
		{"x@reviewbot @reviewbot", 12, 22, true},
		{"@reviewbot.foo, @reviewbot hi", 16, 26, true},
		{"@reviewbot2", 0, 0, false}, // a longer username
		{"@reviewbot_x", 0, 0, false},
		{"@reviewbot-ci", 0, 0, false},
		{"@reviewbotБ", 0, 0, false},
		{"@reviewbot.foo", 0, 0, false}, // "@reviewbot.foo" is another username
		{"bob@reviewbot", 0, 0, false},  // an e-mail-like word
		{"ё@reviewbot", 0, 0, false},
		{"@@reviewbot", 0, 0, false}, // "@@" is not this mention
		{"@reviewbo", 0, 0, false},
		{"", 0, 0, false},
	} {
		start, end, ok := findMention(tc.text, m)
		if start != tc.start || end != tc.end || ok != tc.ok {
			t.Errorf("findMention(%q) = %d %d %v, want %d %d %v", tc.text, start, end, ok, tc.start, tc.end, tc.ok)
		}
	}
	if s, e, ok := findMention("anything", ""); s != 0 || e != 0 || ok {
		t.Errorf("empty mention: %d %d %v", s, e, ok)
	}
}

// The pipeline filters: off unless enabled, tags, statuses other than
// failed/success, and the main branch (configured or the default one).
func TestRouterPipelineFilters(t *testing.T) {
	base := PipelineEvent{ProjectID: 42, ProjectPath: "group/api", DefaultBranch: "main", PipelineID: 5, Ref: "main",
		SHA: "abc", Status: "failed"}
	on := NewRouter(Options{Pipelines: true})
	if j, why := NewRouter(Options{}).Pipeline(base); j != nil || why != "pipeline events off (review.ci.enabled: false)" {
		t.Fatalf("off: %+v %q", j, why)
	}
	tag := base
	tag.Tag = true
	if j, why := on.Pipeline(tag); j != nil || why != "tag pipeline" {
		t.Fatalf("tag: %+v %q", j, why)
	}
	for _, st := range []string{"running", "pending", "canceled", "skipped", "manual", "created", ""} {
		e := base
		e.Status = st
		if j, why := on.Pipeline(e); j != nil || why != "pipeline "+st {
			t.Errorf("status %q: %+v %q", st, j, why)
		}
	}
	for _, tc := range []struct {
		name string
		main func(string, string) string
		ref  string
		want bool
	}{
		{"default branch", nil, "main", true},
		{"default branch, padded ref", nil, " main ", true},
		{"other branch", nil, "feature", false},
		{"configured main", func(p, d string) string { return "release" }, "release", true},
		{"configured main, default branch ref", func(p, d string) string { return "release" }, "main", false},
		{"no main branch", func(p, d string) string { return "" }, "main", false},
	} {
		e := base
		e.Ref = tc.ref
		r := NewRouter(Options{Pipelines: true, MainBranch: tc.main})
		j, why := r.Pipeline(e)
		if j == nil || j.Pipeline.Main != tc.want {
			t.Errorf("%s: %+v %q, want main=%v", tc.name, j, why, tc.want)
		}
	}
	// MainBranch receives the project path and the event's default branch.
	var gotPath, gotDefault string
	NewRouter(Options{Pipelines: true, MainBranch: func(p, d string) string { gotPath, gotDefault = p, d; return d }}).Pipeline(base)
	if gotPath != "group/api" || gotDefault != "main" {
		t.Fatalf("MainBranch(%q, %q)", gotPath, gotDefault)
	}
}

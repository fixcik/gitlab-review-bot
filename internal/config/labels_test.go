package config

import (
	"strings"
	"testing"
)

func TestParseRepoLabels(t *testing.T) {
	r, err := ParseRepo([]byte(`labels:
  remove_stale: false
  rules:
    - name: " backend "
      paths: ["./api/**", "/src/**"]
    - name: db-migration
      when_fact: Migrations
    - name: big
      when_fact: [big_mr, no_tests_changed]
    - name: type::bug
      instruction: "  fixes a bug  "
    - name: legacy
      enabled: false
`))
	if err != nil {
		t.Fatal(err)
	}
	rules := r.Labels.Rules
	if len(rules) != 5 || rules[0].Name != "backend" || strings.Join(rules[0].Paths, ",") != "api/**,src/**" ||
		strings.Join(rules[1].WhenFact, ",") != "migrations" || strings.Join(rules[2].WhenFact, ",") != "big_mr,no_tests_changed" ||
		rules[3].Instruction != "fixes a bug" || rules[4].On() {
		t.Fatalf("rules %+v", rules)
	}
	if r.Labels.RemoveStale == nil || *r.Labels.RemoveStale {
		t.Fatal("remove_stale")
	}

	bad := map[string]string{
		"comma":        "labels:\n  rules:\n    - name: \"a,b\"\n      paths: [x]\n",
		"empty name":   "labels:\n  rules:\n    - name: \"\"\n      paths: [x]\n",
		"control":      "labels:\n  rules:\n    - name: \"a\\tb\"\n      paths: [x]\n",
		"too long":     "labels:\n  rules:\n    - name: " + strings.Repeat("a", 256) + "\n      paths: [x]\n",
		"no condition": "labels:\n  rules:\n    - name: a\n",
		"bad fact":     "labels:\n  rules:\n    - name: a\n      when_fact: feature_flag_needed\n",
		"bad glob":     "labels:\n  rules:\n    - name: a\n      paths: [\"[\"]\n",
		"duplicate":    "labels:\n  rules:\n    - name: a\n      paths: [x]\n    - name: a\n      paths: [y]\n",
		"long instr":   "labels:\n  rules:\n    - name: a\n      instruction: " + strings.Repeat("x", 1001) + "\n",
	}
	for name, y := range bad {
		if _, err := ParseRepo([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	var many strings.Builder
	many.WriteString("labels:\n  rules:\n")
	for i := 0; i <= MaxLabelRules; i++ {
		many.WriteString("    - name: l" + string(rune('a'+i%26)) + strings.Repeat("x", i) + "\n      paths: [x]\n")
	}
	if _, err := ParseRepo([]byte(many.String())); err == nil || !strings.Contains(err.Error(), "at most 30") {
		t.Fatalf("31 rules: %v", err)
	}
}

func TestMergeLabels(t *testing.T) {
	svc := Defaults()
	if e := Merge(&svc, nil); !e.Labels.Enabled || !e.Labels.RemoveStale || e.Labels.CreateMissing || e.Labels.On() {
		t.Fatalf("defaults %+v", e.Labels)
	}
	svc.Review.Labels.Rules = []LabelRule{
		{Name: "backend", Paths: []string{"src/**"}},
		{Name: "legacy", Paths: []string{"old/**"}},
		{Name: "type::bug", Instruction: "bug"},
	}
	if err := svc.Validate(); err != nil && strings.Contains(err.Error(), "labels") {
		t.Fatal(err)
	}
	repo, err := ParseRepo([]byte(`labels:
  create_missing: true
  rules:
    - name: backend
      paths: ["api/**"]
    - name: legacy
      enabled: false
    - name: frontend
      paths: ["web/**"]
`))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&svc, repo).Labels
	var names []string
	for _, r := range e.Rules {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "backend,type::bug,frontend" || e.Rules[0].Paths[0] != "api/**" || !e.CreateMissing || !e.On() {
		t.Fatalf("merged %+v", e)
	}
	if ir := e.InstructionRules(); len(ir) != 1 || ir[0].Name != "type::bug" {
		t.Fatalf("instruction rules %+v", ir)
	}
	// The repo may turn labels off; it cannot turn them on against the service.
	off, _ := ParseRepo([]byte("labels:\n  enabled: false\n"))
	if Merge(&svc, off).Labels.On() {
		t.Fatal("repo switch off ignored")
	}
	on, _ := ParseRepo([]byte("labels:\n  enabled: true\n"))
	svc.Review.Labels.Enabled = false
	if Merge(&svc, on).Labels.On() {
		t.Fatal("repo turned labels on against the service")
	}
	// Total cap: extra repo rules are dropped and counted.
	svc.Review.Labels.Enabled = true
	svc.Review.Labels.Rules = nil
	for i := 0; i < MaxLabelRules; i++ {
		svc.Review.Labels.Rules = append(svc.Review.Labels.Rules, LabelRule{Name: "s" + strings.Repeat("x", i), Paths: []string{"x"}})
	}
	if e := Merge(&svc, repo).Labels; len(e.Rules) != MaxLabelRules || e.Dropped != 2 {
		t.Fatalf("cap: %d dropped %d", len(e.Rules), e.Dropped)
	}
	// Invalid service rules fail Validate.
	svc.Review.Labels.Rules = []LabelRule{{Name: "a,b", Paths: []string{"x"}}}
	if err := svc.Validate(); err == nil || !strings.Contains(err.Error(), "review.labels.rules") {
		t.Fatalf("validate: %v", err)
	}
}

func TestLabelHelpers(t *testing.T) {
	for in, want := range map[string]string{"type::bug": "type", "a::b::c": "a::b", "plain": "", "::x": "", "x::": ""} {
		if got := LabelScope(in); got != want {
			t.Errorf("LabelScope(%q) = %q", in, got)
		}
	}
	if ValidLabelName("ok label") != true || ValidLabelName("a\x00b") || ValidLabelName("a,b") || ValidLabelName("  ") {
		t.Error("ValidLabelName")
	}
	r := LabelRule{Paths: []string{"api/**", "*.md"}}
	if !r.MatchFile("api/v1/x.go") || !r.MatchFile("README.md") || r.MatchFile("web/a.md") {
		t.Error("MatchFile")
	}
	if len(LabelFacts()) != 6 {
		t.Error("facts")
	}
}

// The size-override label is never a label rule: refused at startup for
// the service config, ignored at merge time for a repository.
func TestLabelRuleForOverrideLabelRefused(t *testing.T) {
	c := Defaults()
	c.Review.Labels.Rules = []LabelRule{{Name: "Big-MR-OK", Paths: []string{"**"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "review.big_mr.override_label") {
		t.Fatalf("service rule for the override label: %v", err)
	}
	c.Review.Labels.Rules[0].Enabled = new(bool) // disabled: harmless
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "override_label") {
		t.Fatalf("disabled rule: %v", err)
	}
	c.Review.Labels.Rules[0].Enabled = nil
	c.Review.BigMR.OverrideLabel = ""
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "override_label") {
		t.Fatalf("no override label: %v", err)
	}

	svc := Defaults()
	repo, err := ParseRepo([]byte("labels:\n  rules:\n    - name: big-mr-ok\n      paths: [\"**\"]\n    - name: backend\n      paths: [\"**\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	e := Merge(&svc, repo)
	if len(e.Labels.Rules) != 1 || e.Labels.Rules[0].Name != "backend" || strings.Join(e.Labels.Refused, ",") != "big-mr-ok" {
		t.Fatalf("rules %+v refused %v", e.Labels.Rules, e.Labels.Refused)
	}
}

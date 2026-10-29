package labels

import (
	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"strings"
	"testing"
)

func TestEvalRule(t *testing.T) {
	in := inputs{files: []string{"api/x.go", "README.md"}, filesKnown: true,
		facts: map[string]tri{config.FactMigrations: triYes, config.FactBigMR: triNo},
		model: map[string]bool{"m": true}, modelAnswered: true}
	cases := []struct {
		r    config.LabelRule
		in   inputs
		want tri
	}{
		{config.LabelRule{Name: "a", Paths: []string{"api/**"}}, in, triYes},
		{config.LabelRule{Name: "a", Paths: []string{"web/**"}}, in, triNo},
		{config.LabelRule{Name: "a", Paths: []string{"web/**"}}, inputs{}, triUnknown},
		{config.LabelRule{Name: "a", WhenFact: config.FactList{"migrations"}}, in, triYes},
		{config.LabelRule{Name: "a", WhenFact: config.FactList{"big_mr"}}, in, triNo},
		{config.LabelRule{Name: "a", WhenFact: config.FactList{"lint_secret"}}, in, triUnknown},
		{config.LabelRule{Name: "a", WhenFact: config.FactList{"big_mr", "migrations"}}, in, triYes},
		{config.LabelRule{Name: "m", Instruction: "x"}, in, triYes},
		{config.LabelRule{Name: "n", Instruction: "x"}, in, triNo},
		{config.LabelRule{Name: "n", Instruction: "x"}, inputs{modelAnswered: false}, triUnknown},
		{config.LabelRule{Name: "n", Instruction: "x"}, inputs{modelAnswered: true, incremental: true}, triUnknown},
		// Deterministic OR model.
		{config.LabelRule{Name: "n", Paths: []string{"api/**"}, Instruction: "x"}, in, triYes},
		{config.LabelRule{Name: "m", Paths: []string{"web/**"}, Instruction: "x"}, in, triYes},
		{config.LabelRule{Name: "n", Paths: []string{"web/**"}, Instruction: "x"}, in, triNo},
		{config.LabelRule{Name: "n", WhenFact: config.FactList{"lint_secret"}, Instruction: "x"}, in, triUnknown},
	}
	for i, c := range cases {
		if got := evalRule(c.r, c.in); got != c.want {
			t.Errorf("%d %+v: %v, want %v", i, c.r, got, c.want)
		}
	}
}

func TestPlanLabels(t *testing.T) {
	rules := []config.LabelRule{
		{Name: "type::bug", Instruction: "bug"},
		{Name: "type::feature", Paths: []string{"**"}},
		{Name: "backend", Paths: []string{"**/*.go"}},
		{Name: "secret", WhenFact: config.FactList{"lint_secret"}},
	}
	l := config.EffLabels{Enabled: true, RemoveStale: true, Rules: rules}
	in := inputs{files: []string{"a.go"}, filesKnown: true, model: map[string]bool{"type::bug": true}, modelAnswered: true}

	// Scoped: of two applying labels of one scope only the first is set.
	p := planLabels(l, in, nil, nil, nil)
	if got := strings.Join(p.add, ","); got != "type::bug,backend" {
		t.Fatalf("add %s", got)
	}
	// The bot's own type::feature is replaced (removed) by the winner.
	p = planLabels(l, in, []string{"type::feature", "backend"}, []string{"type::feature", "backend"}, nil)
	if strings.Join(p.add, ",") != "type::bug" || strings.Join(p.remove, ",") != "type::feature" {
		t.Fatalf("replace: %v %v", p.add, p.remove)
	}
	// A human's label of the same scope wins: nothing set in that scope.
	p = planLabels(l, in, []string{"type::refactoring"}, nil, nil)
	if strings.Join(p.add, ",") != "backend" || strings.Join(p.held, ",") != "type::bug" {
		t.Fatalf("human scope: %v held %v", p.add, p.held)
	}
	// A bot label a human removed is suppressed and never set again; a
	// suppressed label a human put back belongs to the human.
	p = planLabels(l, in, []string{"type::bug"}, []string{"backend"}, []string{"type::bug"})
	if len(p.add) != 0 || !p.suppressed["backend"] || p.suppressed["type::bug"] || p.added["backend"] {
		t.Fatalf("suppress: %+v", p)
	}
	// Unknown rules keep the label; removed rules (not in config) remove it.
	p = planLabels(l, inputs{files: []string{"README.md"}, filesKnown: true, model: map[string]bool{}, modelAnswered: true},
		[]string{"secret", "old", "backend", "human"},
		[]string{"secret", "old", "backend"}, nil)
	if got := strings.Join(p.remove, ","); got != "backend,old" {
		t.Fatalf("remove %s", got)
	}
}

func TestLabelSummaryLine(t *testing.T) {
	cases := map[string]labelOutcome{
		"":                           {},
		"🏷 Метки бота: `a`, `b`\n\n": {current: []string{"a", "b"}},
		"🏷 Метки: +`a` −`b`\n\n":     {added: []string{"a"}, removed: []string{"b"}, current: []string{"a"}},
		"🏷 Метки: +`a` не удалось: `x`\n\n": {added: []string{"a"}, failed: []string{"x"}},
		"🏷 Метки: не удалось: `x`\n\n":      {failed: []string{"x"}, current: []string{"a"}},
		"🏷 Метки бота: ``a`b``\n\n":         {current: []string{"a`b"}},
	}
	for want, o := range cases {
		if got := o.summaryLine(); got != want {
			t.Errorf("%+v: %q, want %q", o, got, want)
		}
	}
}

func TestLabelSchemaAndParse(t *testing.T) {
	lp := NewPrompt(config.EffLabels{Enabled: true, Rules: []config.LabelRule{{Name: "x", Paths: []string{"a"}}}}, false)
	if lp.on() || lp.WithSchema(jsonschema.Review) != jsonschema.Review || lp.Block != "" || lp.System != "" {
		t.Fatal("no instruction rules: request unchanged")
	}
	lp = NewPrompt(config.EffLabels{Enabled: true, Rules: []config.LabelRule{{Name: "a", Instruction: "i"}, {Name: "b", Instruction: "j"}}}, false)
	lp.Parse(`{"summary":"s"}`)
	if lp.answered {
		t.Fatal("missing field is not an answer")
	}
	lp.Parse(`{"labels":["b","zzz"," a "]}`)
	if !lp.answered || !lp.chosen["a"] || !lp.chosen["b"] || lp.chosen["zzz"] {
		t.Fatalf("parse %+v", lp.chosen)
	}
	var nilLP *Prompt
	nilLP.Parse("{}")
	if nilLP.on() {
		t.Fatal("nil")
	}
}

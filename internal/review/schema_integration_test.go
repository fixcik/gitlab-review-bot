package review

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/memory"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/learning"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/task"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// CheckSchemaNode validates the subset of JSON Schema the bot uses: every
// object closes additionalProperties, declares every required property and
// gives each property a known type.
func CheckSchemaNode(t *testing.T, path string, n map[string]any) {
	t.Helper()
	typ, _ := n["type"].(string)
	switch typ {
	case "object":
		if ap, ok := n["additionalProperties"].(bool); !ok || ap {
			t.Errorf("%s: additionalProperties must be false", path)
		}
		props, ok := n["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: object without properties", path)
			return
		}
		req, _ := n["required"].([]any)
		seen := map[string]bool{}
		for _, r := range req {
			name, ok := r.(string)
			if !ok {
				t.Errorf("%s: required entry %v is not a string", path, r)
				continue
			}
			if seen[name] {
				t.Errorf("%s: %q required twice", path, name)
			}
			seen[name] = true
			if _, ok := props[name]; !ok {
				t.Errorf("%s: required %q is not declared", path, name)
			}
		}
		for name, p := range props {
			pm, ok := p.(map[string]any)
			if !ok {
				t.Errorf("%s.%s: not an object schema", path, name)
				continue
			}
			CheckSchemaNode(t, path+"."+name, pm)
		}
	case "array":
		items, ok := n["items"].(map[string]any)
		if !ok {
			t.Errorf("%s: array without items", path)
			return
		}
		CheckSchemaNode(t, path+"[]", items)
	case "string", "integer", "boolean", "number":
		if e, ok := n["enum"]; ok {
			if l, ok := e.([]any); !ok || len(l) == 0 {
				t.Errorf("%s: empty or invalid enum", path)
			}
		}
	default:
		t.Errorf("%s: unknown type %q", path, typ)
	}
}

func ParseSchema(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("schema is not valid JSON: %v\n%s", err, s)
	}
	return m
}

// The review schema with every feature on: task alignment, title, compat,
// diagram, memory and migrations (findings[].deployed_usage). It must stay
// a closed, consistent schema, and an answer that uses every property must
// parse into output.ReviewOutput.
func TestReviewSchemaAllFeatures(t *testing.T) {
	types := config.Defaults().Jira.TitleTypes
	if len(types) == 0 {
		types = []string{"feat", "fix"}
	}
	schema := task.ReviewSchemaFor(true, true, types)
	schema = compatcheck.WithSchema(schema)
	schema = crossrepo.DiagramSchema(schema, true)
	mem := &learning.MemoryRun{On: true}
	schema = mem.WithSchema(schema)
	schema = migcheck.WithSchema(schema)

	m := ParseSchema(t, schema)
	CheckSchemaNode(t, "$", m)
	props := m["properties"].(map[string]any)
	want := []string{"summary", "diagram", "walkthrough", "findings", "resolved", "task_alignment", "suggested_title", "compatibility", "memory_updates"}
	for _, p := range want {
		if _, ok := props[p]; !ok {
			t.Errorf("property %q missing", p)
		}
	}
	if len(props) != len(want) {
		var got []string
		for k := range props {
			got = append(got, k)
		}
		sort.Strings(got)
		t.Errorf("unexpected properties: %v", got)
	}
	reqd := map[string]bool{}
	for _, r := range m["required"].([]any) {
		reqd[r.(string)] = true
	}
	for _, r := range []string{"summary", "walkthrough", "findings", "resolved", "task_alignment", "suggested_title", "compatibility"} {
		if !reqd[r] {
			t.Errorf("%q should be required", r)
		}
	}
	if reqd["memory_updates"] || reqd["diagram"] {
		t.Errorf("optional properties must not be required: %v", reqd)
	}

	// The memory runner recognizes the run by the property name.
	if !jsonHasKey(schema, learning.MemoryUpdatesKey) {
		t.Fatalf("memory capture would not recognize the schema")
	}

	// Disabled diagram: removed, rest intact.
	off := ParseSchema(t, mem.WithSchema(crossrepo.DiagramSchema(compatcheck.WithSchema(task.ReviewSchemaFor(true, true, types)), false)))
	CheckSchemaNode(t, "$", off)
	if _, ok := off["properties"].(map[string]any)["diagram"]; ok {
		t.Errorf("diagram must be removed when off")
	}

	// A full answer parses (unknown fields are ignored by encoding/json, so
	// check the typed fields explicitly).
	answer := fmt.Sprintf(`{"summary":"ok","diagram":"flowchart TD\nA-->B","walkthrough":[{"path":"a.go","summary":"x"}],
"findings":[{"path":"a.go","line":1,"end_line":1,"severity":"major","title":"t","body":"b","deployed_usage":{"path":"src/a.go","line":2,"name":"col"}}],
"resolved":[{"id":"abc","comment":"c"}],
"task_alignment":{"verdict":"match","implemented":[],"missing":[],"extra":[],"notes":""},
"suggested_title":{"type":%q,"scope":"","description":"d"},
"compatibility":{"breaking":[{"path":"a.go","line":1,"kind":"api","title":"t","explanation":"e","previous_behavior":"p","new_behavior":"n","safe_alternative":"s"}],"resolved":[]},
"memory_updates":[{"section":"test","op":"add","text":"go test ./...","command":"go test ./..."}]}`, types[0])
	out, err := parseReviewOutput(answer, 30)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if out.TaskAlignment == nil || out.SuggestedTitle == nil || out.Compatibility == nil || out.Diagram == "" || len(out.Findings) != 1 ||
		out.Findings[0].DeployedUsage == nil {
		t.Fatalf("fields lost: %+v", out)
	}
	var w struct {
		Updates []memory.Update `json:"memory_updates"`
	}
	if err := jsonschema.UnmarshalStrict(answer, &w); err != nil || len(w.Updates) != 1 {
		t.Fatalf("memory updates: %v %+v", err, w.Updates)
	}

	// Chat: learning + memory.
	chat := ParseSchema(t, mem.WithSchema(learning.ChatSchemaFor(true)))
	CheckSchemaNode(t, "$", chat)
	for _, p := range []string{"reply", "resolve", "learning", learning.MemoryUpdatesKey} {
		if _, ok := chat["properties"].(map[string]any)[p]; !ok {
			t.Errorf("chat property %q missing", p)
		}
	}
	// Every fixed schema of the package.
	for name, s := range map[string]string{
		"review": jsonschema.Review, "chat": jsonschema.Chat, "learn": learning.ExtractSchema, "synth": learning.SynthSchema,
		"compact": learning.MemoryCompactSchema, "verify": verify.ClaimsSchema,
	} {
		CheckSchemaNode(t, name, ParseSchema(t, s))
	}
	_ = output.ReviewOutput{}
}

func jsonHasKey(schema, key string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(schema), &m) != nil {
		return false
	}
	p, _ := m["properties"].(map[string]any)
	_, ok := p[key]
	return ok
}

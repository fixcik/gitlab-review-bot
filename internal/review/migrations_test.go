package review_test

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/jsonschema"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
)

func TestWithMigrationsSchema(t *testing.T) {
	s := migcheck.WithSchema(jsonschema.Review)
	m := review.ParseSchema(t, s)
	review.CheckSchemaNode(t, "$", m)
	props, required := reviewtest.FindingsItemProps(t, s)
	u, ok := props["deployed_usage"].(map[string]any)
	if !ok {
		t.Fatalf("no deployed_usage:\n%s", s)
	}
	for _, r := range required {
		if r == "deployed_usage" {
			t.Error("deployed_usage must not be required")
		}
	}
	up := u["properties"].(map[string]any)
	if up["line"].(map[string]any)["type"] != "integer" || up["line"].(map[string]any)["minimum"] != float64(1) ||
		up["path"] == nil || up["name"] == nil || len(u["required"].([]any)) != 3 {
		t.Errorf("deployed_usage schema: %v", u)
	}
	// The rest of the schema is intact.
	if top, _ := reviewtest.SchemaProps(t, s); len(top) != len(review.ParseSchema(t, jsonschema.Review)["properties"].(map[string]any)) {
		t.Errorf("top-level properties changed: %v", top)
	}
	// An answer with deployed_usage keeps it through parsing.
	o, err := review.ParseReviewOutput(`{"summary":"s","walkthrough":[],"resolved":[],"findings":[{"path":"./db/m.sql","line":1,"end_line":1,
"severity":"critical","title":"t","body":"b","deployed_usage":{"path":" ./src/a.py ","line":3,"name":" created_by "}}]}`, 30)
	if err != nil {
		t.Fatal(err)
	}
	if u := o.Findings[0].DeployedUsage; u == nil || *u != (output.UsageRef{Path: "src/a.py", Line: 3, Name: "created_by"}) {
		t.Errorf("parsed deployed_usage %+v", u)
	}
}

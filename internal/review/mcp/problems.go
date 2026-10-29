package mcp

// report_problem: what the model could not verify (review.problems).

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
)

// ToolReportProblem is the tool through which the model reports what it
// could not verify (review.problems; the final JSON answer's "problems"
// without MCP).
const ToolReportProblem = "report_problem"

// ModelProblemSchema is the item schema shared by the JSON answer and the
// MCP tool.
func ModelProblemSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"kind", "subject"},
		"properties": map[string]any{
			"kind": map[string]any{"type": "string", "enum": problems.ModelNames(),
				"description": "tool_failed: a tool (file reading or a review bot tool) failed; missing_dependency: code the change depends on is not in the repository (vendor/, node_modules, generated code); no_access: a repository or file you needed is not available; too_large: a file was too large to read; config_unclear: the project configuration is unclear; other."},
			"subject": map[string]any{"type": "string", "maxLength": problems.MaxModelSubjectRunes,
				"description": "What could not be checked, short and stable (e.g. \"generated protobuf code\", \"node_modules\", \"group/payments repository\")."},
			"detail": map[string]any{"type": "string", "maxLength": problems.MaxDetailRunes,
				"description": "What happened (the error, what is missing)."},
		},
	}
}

// ModelProblemArgs is one reported problem (JSON answer or MCP call).
type ModelProblemArgs struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Detail  string `json:"detail"`
}

// reportProblemTool is the MCP tool (registered when problems are on).
func (m *Run) reportProblemTool() mcpserver.Tool {
	schema, _ := json.Marshal(ModelProblemSchema())
	return mcpserver.Tool{Name: ToolReportProblem, InputSchema: schema, Handler: m.reportProblem,
		Description: fmt.Sprintf("Report something you could not verify during this review (a command that could not run, missing dependencies, a file too large, no access to a related repository, unclear configuration). Not for problems of the code: those are findings. At most %d per review; shown to the bot's operators.", problems.MaxModelPerRun)}
}

// reportProblem is report_problem: it records a model problem of the job.
func (m *Run) reportProblem(ctx context.Context, raw json.RawMessage) (string, error) {
	var a ModelProblemArgs
	if err := mcpserver.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	k, err := problems.AddModel(ctx, a.Kind, prompts.CleanModelText(a.Subject), prompts.CleanModelText(a.Detail))
	if err != nil {
		return "", err
	}
	m.log.Info("review: mcp report_problem", "kind", k, "subject", problems.CleanText(a.Subject, 120))
	return fmt.Sprintf(`{"status":"recorded","kind":%q}`, k), nil
}

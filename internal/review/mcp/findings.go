package mcp

// The findings the model submitted (submit_finding keeps them, see
// mcp/validate.go): withdraw_finding, list_findings and the merge with the
// final answer.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/mcpserver"
	"github.com/fixcik/gitlab-review-bot/internal/review/finding"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
)

// draft is an accepted finding of the session.
type draft struct {
	id string
	f  output.Finding
}

// findings are the accepted findings in submission order.
func (m *Run) findings() []output.Finding {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]output.Finding, 0, len(m.drafts))
	for _, d := range m.drafts {
		out = append(out, d.f)
	}
	return out
}

// Merge adds the submitted findings to the final answer's findings.
func (m *Run) Merge(final []output.Finding) []output.Finding {
	sub := m.findings()
	if len(sub) == 0 {
		return final
	}
	return mergeFindings(sub, final, m.f.env.Cfg.Review.MaxFindings)
}

// mergeFindings puts submitted findings first and adds the final answer's
// findings that are not the same (path, line, title); then orders by
// severity and caps like finding.NormalizeReview.
func mergeFindings(submitted, final []output.Finding, max int) []output.Finding {
	key := func(f output.Finding) string {
		return f.Path + "\n" + strconv.Itoa(f.Line) + "\n" + strings.ToLower(strings.TrimSpace(f.Title))
	}
	out := make([]output.Finding, 0, len(submitted)+len(final))
	seen := map[string]bool{}
	for _, list := range [][]output.Finding{submitted, final} {
		for _, f := range list {
			if k := key(f); !seen[k] {
				seen[k] = true
				out = append(out, f)
			}
		}
	}
	return finding.Cap(out, max)
}

type withdrawArgs struct {
	ID string `json:"id"`
}

// withdraw is withdraw_finding: it forgets a submitted finding by its id.
func (m *Run) withdraw(_ context.Context, raw json.RawMessage) (string, error) {
	var a withdrawArgs
	if err := mcpserver.DecodeArgs(raw, &a); err != nil {
		return "", err
	}
	id := strings.ToUpper(strings.TrimSpace(a.ID))
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, d := range m.drafts {
		if d.id == id {
			m.drafts = append(m.drafts[:i], m.drafts[i+1:]...)
			m.log.Info("review: mcp withdraw_finding", "id", id, "path", d.f.Path, "line", d.f.Line)
			return fmt.Sprintf(`{"status":"withdrawn","id":%q}`, id), nil
		}
	}
	return "", fmt.Errorf("no submitted finding with id %q (see list_findings)", mdtext.Shorten(a.ID, 20))
}

// list is list_findings: the submitted findings and the cap.
func (m *Run) list(context.Context, json.RawMessage) (string, error) {
	type item struct {
		ID       string        `json:"id"`
		Path     string        `json:"path"`
		Line     int           `json:"line"`
		Severity core.Severity `json:"severity"`
		Title    string        `json:"title"`
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []item{}
	for _, d := range m.drafts {
		out = append(out, item{ID: d.id, Path: d.f.Path, Line: d.f.Line, Severity: d.f.Severity, Title: d.f.Title})
	}
	b, _ := json.Marshal(map[string]any{"findings": out, "max": m.f.env.Cfg.Review.MaxFindings})
	return string(b), nil
}

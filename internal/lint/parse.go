package lint

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// ---------------------------------------------------------------------------
// gitleaks
// ---------------------------------------------------------------------------

// gitleaksFinding is the part of a gitleaks JSON report entry (report.Finding,
// v8.30) the bot reads. Secret, Match, Line, Fragment, Fingerprint, Author and
// Email are deliberately NOT decoded: the secret value (even redacted) must
// never reach any struct, log line or note.
type gitleaksFinding struct {
	RuleID      string `json:"RuleID"`
	Description string `json:"Description"`
	StartLine   int    `json:"StartLine"`
	EndLine     int    `json:"EndLine"`
	File        string `json:"File"`
	Commit      string `json:"Commit"`
}

// ParseGitleaks parses `gitleaks ... --report-format json` output (a JSON
// array of findings).
func ParseGitleaks(out []byte) ([]Finding, error) {
	t := bytes.TrimSpace(out)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil, nil
	}
	var items []gitleaksFinding
	if err := json.Unmarshal(t, &items); err != nil {
		return nil, fmt.Errorf("gitleaks json: %w", err)
	}
	var fs []Finding
	for _, it := range items {
		rule := strings.TrimSpace(it.RuleID)
		if rule == "" {
			rule = "secret"
		}
		msg := strings.TrimSpace(it.Description)
		if msg == "" {
			msg = "секрет (" + rule + ")"
		}
		commit := strings.TrimSpace(it.Commit)
		if len(commit) > 12 {
			commit = commit[:12]
		}
		fs = append(fs, Finding{
			Tool: ToolGitleaks, Rule: rule, Path: it.File, Line: it.StartLine, EndLine: it.EndLine,
			Severity: core.SeverityCritical, Message: shortText(msg, 200), Commit: commit,
		})
	}
	return fs, nil
}

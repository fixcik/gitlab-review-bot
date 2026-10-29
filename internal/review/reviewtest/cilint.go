package reviewtest

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
)

// The harness MR adds lines 2 and 3 of cache.go.

// sarifReport: semgrep --sarif with one result on an added line and one on
// old code (the runner's absolute path).
const sarifReport = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"Semgrep","rules":[{"id":"go.lang.security.audit.dangerous-exec-command","helpUri":"https://sg.run/ydO7"}]}},
 "results":[
  {"ruleId":"go.lang.security.audit.dangerous-exec-command","level":"error","message":{"text":"Detected non-static command inside Command"},
   "locations":[{"physicalLocation":{"artifactLocation":{"uri":"/builds/group/api/cache.go"},"region":{"startLine":3}}}]},
  {"ruleId":"go.lang.old","level":"warning","message":{"text":"old code"},
   "locations":[{"physicalLocation":{"artifactLocation":{"uri":"cache.go"},"region":{"startLine":5}}}]}]}]}`

// codeQualityReport: ruff --output-format=gitlab.
const codeQualityReport = `[{"check_name":"F401","description":"` + "`os`" + ` imported but unused","severity":"major","fingerprint":"f1",
 "location":{"path":"cache.go","positions":{"begin":{"line":2,"column":8},"end":{"line":2,"column":10}}}}]`

// CIHarness: a review with gitleaks (fake checker) and two CI linters.
func CIHarness(t *testing.T) (*Harness, *FakeLint, *FakeModerator) {
	t.Helper()
	h, fl := LintHarness(t)
	m := WithModerator(h)
	fl.Res = &lint.Result{Tools: Ran(lint.ToolGitleaks)}
	h.GL.RepoConfig = `lint:
  ci:
    - job: semgrep
      artifact: gl-sast.sarif
    - job: "ruff*"
      report: codequality
      tool: ruff
`
	h.GL.MR.Pipeline = &core.Pipeline{ID: 77, Status: "success", SHA: "head111111111111", WebURL: "https://gitlab.example.com/group/api/-/pipelines/77"}
	h.GL.Jobs = []core.CIJob{
		{ID: 1, Name: "semgrep", Status: "success", WebURL: "https://gitlab.example.com/group/api/-/jobs/1"},
		{ID: 2, Name: "ruff 1/2", Status: "failed"}, // lint found something; allow_failure + artifacts:when: always
		{ID: 3, Name: "ruff 2/2", Status: "success"},
		{ID: 4, Name: "tests", Status: "success"},
	}
	h.GL.Artifacts = map[string][]byte{"1:gl-sast.sarif": []byte(sarifReport)}
	h.GL.Reports = map[string][]byte{"2:codequality": []byte(codeQualityReport), "3:codequality": []byte("[]")}
	return h, fl, m
}

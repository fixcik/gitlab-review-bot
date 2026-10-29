package reviewtest

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

const CovXML = `<?xml version="1.0" ?>
<coverage><sources><source>/builds/group/api</source></sources><packages><package><classes>
<class filename="cache.go"><lines>
<line number="2" hits="1"/><line number="3" hits="0"/>
</lines></class>
</classes></package></packages></coverage>`

func CoverageHarness(t *testing.T) *Harness {
	t.Helper()
	h := New(t)
	h.Repos.Dir = t.TempDir()
	h.GL.RepoConfig = "coverage:\n  job: tests\n  path: build/coverage.xml\n  min: 80\n"
	h.GL.MR.Pipeline = &core.Pipeline{ID: 77, Status: "success", SHA: "head111111111111", WebURL: "https://gitlab.example.com/group/api/-/pipelines/77"}
	h.GL.Jobs = []core.CIJob{
		{ID: 1, Name: "tests 1/2", Status: "success", WebURL: "https://gitlab.example.com/group/api/-/jobs/1"},
		{ID: 2, Name: "tests 2/2", Status: "success"},
		{ID: 3, Name: "tests-slow", Status: "success"},
		{ID: 4, Name: "tests", Status: "failed"},
	}
	h.GL.Artifacts = map[string][]byte{"1:build/coverage.xml": []byte(CovXML), "2:build/coverage.xml": []byte(CovXML)}
	h.GL.Diffs = append(h.GL.Diffs, core.FileDiff{OldPath: "svc.go", NewPath: "svc.go", Diff: "@@ -0,0 +1,1 @@\n+x\n"})
	return h
}

package main

import (
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/store"
)

// The diagnostics wrapper (on by default) must not hide the real client's
// optional capabilities: coverage, CI linters and phase B look them up and
// silently skip their work when they are missing.
func TestDiagWrappedGitLabKeepsCapabilities(t *testing.T) {
	c, err := gitlab.New("https://gitlab.example.com", "x")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := new(diag.Recorder).WrapGitLab(c)
	if wrapped == core.GitLab(c) {
		t.Fatal("not wrapped")
	}
	if _, ok := core.GitLabAs[cijob.Artifacts](wrapped); !ok {
		t.Error("CIArtifacts hidden by the diag wrapper")
	}
	if _, ok := core.GitLabAs[crossrepo.OpenMRs](wrapped); !ok {
		t.Error("OpenMRs hidden by the diag wrapper")
	}
	// Reactions get the raw client in main (newReactions); it stays
	// reachable through the wrapper as well.
	if _, ok := core.GitLabAs[reactions.API](wrapped); !ok {
		t.Error("reactions.API hidden by the diag wrapper")
	}
	if _, ok := core.GitLabAs[linters.CIReports](wrapped); !ok {
		t.Error("CIReports (lint.ci report: codequality) hidden by the diag wrapper")
	}
	// review.ci: the failure digest, the main branch scope and the comment
	// on a commit pushed without an MR.
	if _, ok := core.GitLabAs[pipeline.CIFailures](wrapped); !ok {
		t.Error("CIFailures hidden by the diag wrapper")
	}
	if _, ok := core.GitLabAs[pipeline.CIMain](wrapped); !ok {
		t.Error("CIMain hidden by the diag wrapper")
	}
	if _, ok := core.GitLabAs[pipeline.CommitComments](wrapped); !ok {
		t.Error("CommitComments hidden by the diag wrapper")
	}
}

// The store implements every optional record store of review.Deps (serve.go
// wires them as typed fields, so this also fails to compile when one is
// missing): main branch state (review.ci), MR flags, bot labels, finding
// outcomes.
var (
	_ pipeline.CIMainStore = (*store.SQLite)(nil)
	_ review.MRFlags       = (*store.SQLite)(nil)
	_ labels.Store         = (*store.SQLite)(nil)
	_ learn.OutcomeStore   = (*store.SQLite)(nil)
)

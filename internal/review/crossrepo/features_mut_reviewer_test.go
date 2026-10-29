package crossrepo_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

// diffErrGitLab fails the diffs of the listed merge requests ("pid:iid")
// and the open merge request lists of the listed projects.
type diffErrGitLab struct {
	*reviewtest.FakeGitLab
	fail     map[string]bool
	failList map[string]bool
}

func (g diffErrGitLab) ListOpenMRs(ctx context.Context, project string, limit int) ([]core.MergeRequest, error) {
	if g.failList[project] {
		return nil, context.DeadlineExceeded
	}
	return g.FakeGitLab.ListOpenMRs(ctx, project, limit)
}

func (g diffErrGitLab) GetMRDiffs(ctx context.Context, p, i int64) ([]core.FileDiff, error) {
	if g.fail[strconv.FormatInt(p, 10)+":"+strconv.FormatInt(i, 10)] {
		return nil, context.DeadlineExceeded
	}
	return g.FakeGitLab.GetMRDiffs(ctx, p, i)
}

// A provider merge request whose diff could not be read was not checked:
// phase B must not claim that no open merge request adds the contract.
func TestDepsProviderDiffErrorNotChecked(t *testing.T) {
	h, _ := depsHarness(t, depsResult(missingHistory()))
	h.GL.OpenMRs["backend/orders"] = []core.MergeRequest{
		{ProjectID: 50, IID: 11, TargetBranch: "main", State: "opened"},
		{ProjectID: 50, IID: 14, TargetBranch: "main", State: "opened"},
	}
	h.GL.MRDiffs["50:14"] = []core.FileDiff{{NewPath: "app/other.py", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	h.Env().GitLab = diffErrGitLab{FakeGitLab: h.GL, fail: map[string]bool{"50:11": true}}
	sum, sys := runDeps(t, h)
	blk := depsBlock(t, sum)
	if strings.Contains(blk, "не найдено") || !strings.Contains(blk, "открытые MR провайдера не проверены") {
		t.Errorf("block:\n%s", blk)
	}
	if strings.Contains(sys, "no open merge request of the provider adds it") {
		t.Errorf("prompt:\n%s", sys)
	}
}

// The provider's open merge request removes the route this one starts to
// use: a conflict link, not "adds it"; a second contract linked through the
// same merge request lists it once.
func TestDepsConflictAndOneLinePerMRMut(t *testing.T) {
	dropped := sysmap.DepContract{Role: sysmap.RoleConsumesDropped, Protocol: sysmap.HTTP, Method: "DELETE", Path: "/orders/{id}",
		File: "src/api.ts", Line: 20, Provider: "backend/orders", ProviderComplete: true, ProviderBranch: "main"}
	h, _ := depsHarness(t, depsResult(missingHistory(), dropped))
	h.GL.OpenMRs["backend/orders"] = []core.MergeRequest{ordersMR()}
	h.GL.MRDiffs["50:12"] = []core.FileDiff{{NewPath: "app/routes.py",
		Diff: "@@ -1,3 +1,1 @@\n-@app.get(\"/orders/{order_id}/history\")\n-@app.delete(\"/orders/{order_id}\")\n b\n"}}
	sum, sys := runDeps(t, h)
	blk := depsBlock(t, sum)
	if !strings.Contains(blk, "открытого MR, который его добавляет, не найдено") || strings.Contains(blk, "мержить после") {
		t.Errorf("a removing MR does not add the contract:\n%s", blk)
	}
	if strings.Count(blk, "merge_requests/12)") != 1 || !strings.Contains(blk, "⚠️ Конфликт") ||
		!strings.Contains(blk, "убирает `GET /orders/{id}/history`, который начинает использовать этот MR") ||
		!strings.Contains(blk, "убирает `DELETE /orders/{id}`, который перестаёт использовать этот MR") {
		t.Errorf("one conflict line for the MR:\n%s", blk)
	}
	if !strings.Contains(sys, "Conflict: this merge request starts to use what backend/orders!12 removes.") {
		t.Errorf("prompt:\n%s", sys)
	}
	if len(h.GL.ListCalls) != 1 {
		t.Errorf("the provider's list is read once: %v", h.GL.ListCalls)
	}
}

// Two missing contracts of one provider: the open merge requests are listed
// once and serve both.
func TestDepsProviderListSharedMut(t *testing.T) {
	items := missingHistory()
	items.Path, items.Line = "/orders/{id}/items", 12
	h, _ := depsHarness(t, depsResult(missingHistory(), items))
	h.GL.OpenMRs["backend/orders"] = []core.MergeRequest{ordersMR()}
	h.GL.MRDiffs["50:12"] = []core.FileDiff{{NewPath: "app/routes.py",
		Diff: "@@ -1 +1,3 @@\n a\n+@app.get(\"/orders/{order_id}/history\")\n+@app.get(\"/orders/{order_id}/items\")\n"}}
	sum, _ := runDeps(t, h)
	blk := depsBlock(t, sum)
	if strings.Count(blk, "мержить после него") != 2 {
		t.Errorf("both contracts are added by !12:\n%s", blk)
	}
	if len(h.GL.ListCalls) != 1 || len(h.GL.DiffCalls) != 1 {
		t.Errorf("lists %v diffs %v", h.GL.ListCalls, h.GL.DiffCalls)
	}
}

// At most depsMaxRepos counterpart repositories are looked at; a list
// error leaves that provider unchecked.
func TestDepsRepoCapAndListErrorMut(t *testing.T) {
	var cs []sysmap.DepContract
	for i := 0; i < crossrepo.DepsMaxRepos+1; i++ {
		c := missingHistory()
		c.Provider = "svc/p" + strconv.Itoa(i)
		c.Path = "/things" + strconv.Itoa(i) + "/{id}"
		cs = append(cs, c)
	}
	h, _ := depsHarness(t, depsResult(cs...))
	h.Cfg.Review.Sysmap.DepsMaxAPICalls = 100
	h.Env().GitLab = diffErrGitLab{FakeGitLab: h.GL, failList: map[string]bool{"svc/p0": true}}
	sum, _ := runDeps(t, h)
	blk := depsBlock(t, sum)
	if len(h.GL.ListCalls) != crossrepo.DepsMaxRepos-1 {
		t.Errorf("listed %v (svc/p0 failed)", h.GL.ListCalls)
	}
	if n := strings.Count(blk, "открытого MR, который его добавляет, не найдено"); n != crossrepo.DepsMaxRepos-1 {
		t.Errorf("%d checked providers:\n%s", n, blk)
	}
	unchecked := 0
	for _, l := range strings.Split(blk, "\n") {
		if strings.HasSuffix(l, "открытые MR провайдера не проверены") {
			unchecked++
			if !strings.Contains(l, "svc/p0") && !strings.Contains(l, "svc/p"+strconv.Itoa(crossrepo.DepsMaxRepos)) {
				t.Errorf("unexpected unchecked line %q", l)
			}
		}
	}
	if unchecked != 2 {
		t.Errorf("the failed and the capped provider are unchecked:\n%s", blk)
	}
}

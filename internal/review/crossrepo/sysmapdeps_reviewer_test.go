package crossrepo_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/jira"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/crossrepo"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/reviewtest"
	"github.com/fixcik/gitlab-review-bot/internal/review/summary"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
)

// fakeDeps is a system map that also answers phase B.
type fakeDeps struct {
	reviewtest.FakeSysmap
	res   *sysmap.DepsResult
	err   error
	reqs  []sysmap.DepsRequest
	attrs []meter.Attribution
}

func (f *fakeDeps) Dependencies(ctx context.Context, req sysmap.DepsRequest) (*sysmap.DepsResult, error) {
	f.reqs = append(f.reqs, req)
	if req.Attribute != nil {
		if a, ok := meter.From(req.Attribute(ctx)); ok {
			f.attrs = append(f.attrs, a)
		}
	}
	return f.res, f.err
}

const depsDiff = "diff --git a/src/api.ts b/src/api.ts\n--- a/src/api.ts\n+++ b/src/api.ts\n@@ -10,2 +10,3 @@\n a\n+fetch(`${ORDERS_URL}/orders/${id}/history`)\n b\n"

// missingHistory: the MR starts to call GET /orders/{id}/history, which
// backend/orders' main does not have.
func missingHistory() sysmap.DepContract {
	return sysmap.DepContract{Role: sysmap.RoleConsumesNew, Protocol: sysmap.HTTP, Method: "GET", Path: "/orders/{id}/history",
		File: "src/api.ts", Line: 11, Provider: "backend/orders", Via: sysmap.ViaBase, ProviderComplete: true, ProviderBranch: "main"}
}

func depsResult(cs ...sysmap.DepContract) *sysmap.DepsResult {
	return &sysmap.DepsResult{Contracts: cs, Files: 1, Runs: 1,
		Branches: map[string]string{"backend/orders": "main", "frontend/web": "main", "group/api": "main"}}
}

// ordersMR adds the history route in backend/orders.
func ordersMR() core.MergeRequest {
	return core.MergeRequest{ProjectID: 50, IID: 12, Title: "feat: история заказа (PROJ-11)", SourceBranch: "PROJ-11-history",
		TargetBranch: "main", State: "opened"}
}

const ordersMRDiff = "@@ -1,2 +1,4 @@\n a\n+@app.get(\"/orders/{order_id}/history\")\n+def history(order_id): ...\n b\n"

func depsHarness(t *testing.T, res *sysmap.DepsResult) (*reviewtest.Harness, *fakeDeps) {
	t.Helper()
	h := reviewtest.New(t)
	h.Cfg.GitLab.URL = "https://gitlab.example.com"
	h.Cfg.GitLab.Groups = []string{"group"}
	f := &fakeDeps{res: res}
	h.Deps.SystemMap = f
	h.Repos.Diff = depsDiff
	h.Repos.Files = []string{"src/api.ts"}
	h.Repos.Show = map[string]string{"src/api.ts": "a\nfetch(`${ORDERS_URL}/orders/${id}/history`)\nb\n"}
	h.GL.OpenMRs = map[string][]core.MergeRequest{}
	h.GL.MRDiffs = map[string][]core.FileDiff{}
	h.Runner.Resps = []reviewtest.RunResp{{Out: reviewtest.ReviewOut(t, nil, nil)}}
	return h, f
}

// runDeps runs the review and returns the summary note and the user prompt,
// where the phase B facts are (per-MR facts never go into the system
// prompt: prompt caching).
func runDeps(t *testing.T, h *reviewtest.Harness) (summary, prompt string) {
	t.Helper()
	if err := h.R().Handle(context.Background(), reviewtest.AutoJob()); err != nil {
		t.Fatal(err)
	}
	req := h.Runner.Reqs[0]
	if strings.Contains(req.SystemPrompt, "## System map") {
		t.Errorf("system map facts in the system prompt:\n%s", req.SystemPrompt)
	}
	return reviewtest.SummaryBody(t, h), req.Prompt
}

func depsBlock(t *testing.T, sum string) string {
	t.Helper()
	i, j := strings.Index(sum, marker.SysmapDepsStart), strings.Index(sum, marker.SysmapDepsEnd)
	if i < 0 || j < i {
		t.Fatalf("no deps block:\n%s", sum)
	}
	return sum[i : j+len(marker.SysmapDepsEnd)]
}

// noThreads asserts that the review posted nothing but the summary note.
func noThreads(t *testing.T, h *reviewtest.Harness) {
	t.Helper()
	if len(h.GL.Drafts) != 0 || len(h.GL.Published) != 0 || len(h.GL.Replies) != 0 || len(h.GL.Discussions) != 0 ||
		len(h.GL.CreatedNotes) != 1 || len(h.GL.Resolved) != 0 {
		t.Errorf("phase B must not post threads: drafts %d published %d replies %d discussions %d notes %v",
			len(h.GL.Drafts), len(h.GL.Published), len(h.GL.Replies), len(h.GL.Discussions), h.GL.CreatedNotes)
	}
	if sum := h.GL.Notes[h.GL.CreatedNotes[0]]; strings.Contains(sum, marker.Blocking) {
		t.Error("blocking marker in the summary")
	}
}

func TestDepsMissingAddedByProviderMR(t *testing.T) {
	h, f := depsHarness(t, depsResult(missingHistory()))
	h.GL.OpenMRs["backend/orders"] = []core.MergeRequest{
		{ProjectID: 50, IID: 11, Title: "chore: deps", TargetBranch: "main", State: "opened"},
		ordersMR(),
		{ProjectID: 50, IID: 13, Title: "feat: other branch", TargetBranch: "release", State: "opened"},
	}
	h.GL.MRDiffs["50:11"] = []core.FileDiff{{NewPath: "requirements.txt", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	h.GL.MRDiffs["50:12"] = []core.FileDiff{{NewPath: "app/routes.py", Diff: ordersMRDiff}}
	h.GL.MRDiffs["50:13"] = []core.FileDiff{{NewPath: "app/routes.py", Diff: ordersMRDiff}}
	sum, sys := runDeps(t, h)
	blk := depsBlock(t, sum)
	want := "- `GET /orders/{id}/history` (`group/api:src/api.ts:11`) нет в main `backend/orders` — добавляется в " +
		"[backend/orders\\!12](https://gitlab.example.com/backend/orders/-/merge_requests/12) (не смержен): мержить после него"
	if !strings.Contains(blk, "**⏳ Зависит от неразвёрнутого:**") || !strings.Contains(blk, want) || !strings.Contains(blk, "ничего не блокирует") {
		t.Errorf("block:\n%s", blk)
	}
	if strings.Contains(blk, "!13") || strings.Contains(blk, "Связанные MR") {
		t.Errorf("an MR for another branch or a duplicate link:\n%s", blk)
	}
	for _, w := range []string{"## System map: cross-repository dependencies",
		"starts to use http GET /orders/{id}/history (src/api.ts:11); the provider backend/orders does not have it on its default branch main",
		"open merge request backend/orders!12 adds it (not merged), so this one must be merged after it",
		`severity at most "major"`, `must NEVER be "critical"`} {
		if !strings.Contains(sys, w) {
			t.Errorf("prompt lacks %q:\n%s", w, sys)
		}
	}
	if strings.Contains(sys, "история заказа") {
		t.Error("an MR title in the prompt")
	}
	// The request: changed lines and head contents, attribution as a map run.
	if len(f.reqs) != 1 {
		t.Fatalf("requests %d", len(f.reqs))
	}
	req := f.reqs[0]
	if req.Repo != "group/api" || !reflect.DeepEqual(req.Added, map[string][]sysmap.LineRange{"src/api.ts": {{From: 11, To: 11}}}) ||
		len(req.Files) != 1 || req.Files[0].Path != "src/api.ts" || req.MaxFiles != 1000 || req.MaxRuns != 30 {
		t.Errorf("request %+v", req)
	}
	if len(f.attrs) != 1 || f.attrs[0].Kind != meter.KindMap || f.attrs[0].MRIID != reviewtest.IID || f.attrs[0].Username != "" {
		t.Errorf("attribution %+v", f.attrs)
	}
	if !reflect.DeepEqual(h.GL.ListCalls, []string{"backend/orders"}) || len(h.GL.DiffCalls) != 2 {
		t.Errorf("api calls: lists %v diffs %v", h.GL.ListCalls, h.GL.DiffCalls)
	}
	noThreads(t, h)
}

func TestDepsMissingNoProviderMR(t *testing.T) {
	h, _ := depsHarness(t, depsResult(missingHistory()))
	h.GL.OpenMRs["backend/orders"] = []core.MergeRequest{{ProjectID: 50, IID: 11, TargetBranch: "main", State: "opened"}}
	h.GL.MRDiffs["50:11"] = []core.FileDiff{{NewPath: "app/routes.py", Diff: "@@ -1 +1 @@\n-@app.get(\"/orders/{id}\")\n+@app.get(\"/orders/{id}/historyx\")\n"}}
	sum, sys := runDeps(t, h)
	if blk := depsBlock(t, sum); !strings.Contains(blk, "нет в main `backend/orders` — открытого MR, который его добавляет, не найдено") {
		t.Errorf("block:\n%s", blk)
	}
	if !strings.Contains(sys, "no open merge request of the provider adds it") {
		t.Errorf("prompt:\n%s", sys)
	}
	noThreads(t, h)
}

func TestDepsAPICallCap(t *testing.T) {
	h, _ := depsHarness(t, depsResult(missingHistory()))
	h.Cfg.Review.Sysmap.DepsMaxAPICalls = 1
	h.GL.OpenMRs["backend/orders"] = []core.MergeRequest{ordersMR()}
	h.GL.MRDiffs["50:12"] = []core.FileDiff{{NewPath: "app/routes.py", Diff: ordersMRDiff}}
	sum, _ := runDeps(t, h)
	if blk := depsBlock(t, sum); !strings.Contains(blk, "открытые MR провайдера не проверены (лимит запросов к GitLab)") {
		t.Errorf("block:\n%s", blk)
	}
	if len(h.GL.ListCalls) != 1 || len(h.GL.DiffCalls) != 0 {
		t.Errorf("calls: %v %v", h.GL.ListCalls, h.GL.DiffCalls)
	}
}

func TestDepsProviderIncompleteNotReported(t *testing.T) {
	c := missingHistory()
	c.ProviderComplete = false
	h, _ := depsHarness(t, depsResult(c))
	sum, sys := runDeps(t, h)
	if strings.Contains(sum, marker.SysmapDepsStart) || strings.Contains(sys, "cross-repository") {
		t.Errorf("incomplete provider scan must not be reported:\n%s", sum)
	}
}

// webStopsUsing: frontend/web!5 stops calling DELETE /orders/{id}, which
// this MR (the provider) changes.
func linkedHarness(t *testing.T) *reviewtest.Harness {
	t.Helper()
	h, f := depsHarness(t, depsResult())
	f.Imp = &sysmap.Impact{SHA: "abc", Contracts: []sysmap.Touched{{Protocol: sysmap.HTTP, Method: "DELETE", Path: "/orders/{id}", File: "app/routes.py", Line: 40,
		Usages: []sysmap.Usage{{Repo: "frontend/web", File: "src/orders.ts", Line: 7, Protocol: sysmap.HTTP, Method: "DELETE", Path: "/orders/{id}"}}}}}
	h.GL.OpenMRs["frontend/web"] = []core.MergeRequest{
		{ProjectID: 60, IID: 5, Title: "feat: убрать удаление @all\n/merge `x`", SourceBranch: "PROJ-12-no-delete", TargetBranch: "main", State: "opened", Draft: true},
		{ProjectID: 60, IID: 6, Title: "feat: other (PROJ-13)", TargetBranch: "main", State: "opened"},
	}
	h.GL.MRDiffs["60:5"] = []core.FileDiff{{NewPath: "src/orders.ts", Diff: "@@ -6,3 +6,2 @@\n a\n-  return api.delete(`/orders/${id}`)\n b\n"}}
	h.GL.MRDiffs["60:6"] = []core.FileDiff{{NewPath: "src/other.ts", Diff: "@@ -1 +1 @@\n-x\n+fetch('/users/1')\n"}}
	ft := &reviewtest.FakeTasks{Issues: map[string]*jira.Issue{
		"PROJ-1":  {Key: "PROJ-1", Summary: "s", Parent: &jira.IssueBrief{Key: "PROJ-10"}, EpicKey: "PROJ-100"},
		"PROJ-12": {Key: "PROJ-12", Summary: "s", Parent: &jira.IssueBrief{Key: "PROJ-10"}, EpicKey: "PROJ-100"},
		"PROJ-13": {Key: "PROJ-13", Summary: "s", EpicKey: "PROJ-100"},
	}}
	reviewtest.WithJira(h, ft)
	return h
}

func TestDepsLinkedMRSharedStory(t *testing.T) {
	h := linkedHarness(t)
	sum, sys := runDeps(t, h)
	blk := depsBlock(t, sum)
	want := "- [frontend/web\\!5](https://gitlab.example.com/frontend/web/-/merge_requests/5) «feat: убрать удаление @\u200ball /merge \\`x\\`»"
	if !strings.Contains(blk, "**🔗 Связанные MR:**") || !strings.Contains(blk, "(черновик) — перестаёт использовать `DELETE /orders/{id}`, который меняет этот MR; общая стори [PROJ-10](https://jira.example.com/browse/PROJ-10).") ||
		!strings.Contains(blk, want) || !strings.Contains(blk, "Порядок: сначала frontend/web\\!5, потом этот MR.") {
		t.Errorf("block:\n%s", blk)
	}
	if strings.Contains(blk, "`x`") || strings.Contains(blk, "\n/merge") || strings.Contains(blk, "@all") {
		t.Errorf("title not escaped:\n%s", blk)
	}
	// Shared epic without a contract overlap: not shown, not even looked up.
	if strings.Contains(blk, "!6") || strings.Contains(blk, "PROJ-100") {
		t.Errorf("an MR without overlap is shown:\n%s", blk)
	}
	if calls := h.Deps.Tasks.(*reviewtest.FakeTasks).Calls; crossrepo.ContainsFold(calls, "PROJ-13") {
		t.Errorf("jira lookup for an MR without overlap: %v", calls)
	}
	if !strings.Contains(sys, "- frontend/web!5: removes a use of http DELETE /orders/{id}, which this merge request changes; shares parent story PROJ-10. Merge order: frontend/web!5 first, then this merge request.") {
		t.Errorf("prompt:\n%s", sys)
	}
	if strings.Contains(sys, "убрать удаление") {
		t.Error("title in the prompt")
	}
	// Placement: after TBD and the sysmap block, before the compat section.
	si, di := strings.Index(sum, marker.SysmapEnd), strings.Index(sum, marker.SysmapDepsStart)
	if si < 0 || di < si || strings.Index(sum, marker.TBDEnd) > di {
		t.Errorf("placement:\n%s", sum)
	}
	if ci := strings.Index(sum, summary.CompatSectionTitle); ci >= 0 && ci < di {
		t.Error("the block must come before the compat section")
	}
	noThreads(t, h)
}

func TestDepsSharedEpicWithoutOverlapNotShown(t *testing.T) {
	h := linkedHarness(t)
	h.GL.MRDiffs["60:5"] = []core.FileDiff{{NewPath: "src/orders.ts", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	sum, sys := runDeps(t, h)
	if strings.Contains(sum, marker.SysmapDepsStart) || strings.Contains(sys, "cross-repository") {
		t.Errorf("nothing to say, yet a block:\n%s", sum)
	}
}

func TestDepsNothingWhenOffOrEmpty(t *testing.T) {
	for name, setup := range map[string]func(h *reviewtest.Harness, f *fakeDeps){
		"deps off":        func(h *reviewtest.Harness, f *fakeDeps) { h.Cfg.Review.Sysmap.Deps = false },
		"map off":         func(h *reviewtest.Harness, f *fakeDeps) { h.Cfg.Review.Sysmap.Enabled = false },
		"repo not in map": func(h *reviewtest.Harness, f *fakeDeps) { f.res = nil },
		"budget exhausted": func(h *reviewtest.Harness, f *fakeDeps) {
			f.res = &sysmap.DepsResult{Skipped: 1, SkipReason: "budget exhausted"}
		},
		"error": func(h *reviewtest.Harness, f *fakeDeps) { f.res, f.err = nil, context.DeadlineExceeded },
		"internal contract": func(h *reviewtest.Harness, f *fakeDeps) {
			c := missingHistory()
			c.InProviderMain = true
			f.res = depsResult(c)
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, f := depsHarness(t, depsResult(missingHistory()))
			setup(h, f)
			sum, sys := runDeps(t, h)
			if strings.Contains(sum, marker.SysmapDepsStart) || strings.Contains(sys, "cross-repository") {
				t.Errorf("unexpected phase B output:\n%s", sum)
			}
			noThreads(t, h)
		})
	}
}

func TestDepsIncrementalKeepsBlock(t *testing.T) {
	h, f := depsHarness(t, depsResult(missingHistory()))
	h.GL.OpenMRs["backend/orders"] = []core.MergeRequest{ordersMR()}
	h.GL.MRDiffs["50:12"] = []core.FileDiff{{NewPath: "app/routes.py", Diff: ordersMRDiff}}
	h.St.States[core.MRKey(reviewtest.PID, reviewtest.IID)] = core.MRState{ProjectID: reviewtest.PID, MRIID: reviewtest.IID, ReviewedSHA: "prev0000000000"}
	h.Repos.Ancestors["prev0000000000"] = true
	sum, _ := runDeps(t, h)
	if !strings.Contains(depsBlock(t, sum), "мержить после него") {
		t.Errorf("block:\n%s", sum)
	}
	if got := h.Repos.DiffFrom; len(got) < 2 || got[0] != "prev0000000000" || !crossrepo.ContainsFold(got[1:], "base000000000000") {
		t.Errorf("phase B must use the full MR diff: %v", got)
	}
	if len(f.reqs) != 1 || len(f.reqs[0].Added["src/api.ts"]) != 1 {
		t.Errorf("request: %+v", f.reqs)
	}
}

func TestRenderDepsBlockCollapsesAndEscapes(t *testing.T) {
	r := review.New(review.Deps{Config: nil})
	r.Internals().Env.Cfg.GitLab.URL = "https://gitlab.example.com"
	d := &crossrepo.DepsRun{}
	for i := 0; i < 5; i++ {
		c := missingHistory()
		c.Provider = "evil`repo @all"
		d.AddMissing(c, true)
	}
	blk := r.Internals().Crossrepo.RenderDepsBlock(d, "group/api")
	if !strings.Contains(blk, "<details>\n<summary>Зависимости между репозиториями: ⏳ 5 зависимостей</summary>") {
		t.Errorf("collapsed:\n%s", blk)
	}
	if strings.Contains(blk, "evil`repo") {
		t.Errorf("not escaped:\n%s", blk)
	}
	// An unsafe project path is not turned into a link.
	m := crossrepo.NewOpenMR("../x](javascript:alert(1))", core.MergeRequest{IID: 3})
	if ref := r.Internals().Crossrepo.MRRef(m); strings.HasPrefix(ref, "[") || !strings.HasPrefix(ref, "`") {
		t.Errorf("ref %q", ref)
	}
	if crossrepo.RenderDepsPrompt(&crossrepo.DepsRun{}, "g/p", nil, true, true) != "" || r.Internals().Crossrepo.RenderDepsBlock(&crossrepo.DepsRun{}, "g/p") != "" {
		t.Error("nothing to say -> nothing")
	}
}

// A new call outside the declared topology: one short line in the deps
// block, not a finding and not blocking; in the main of its provider, so
// no other phase B fact.
func TestDepsOffTopologyLine(t *testing.T) {
	c := missingHistory()
	c.InProviderMain, c.OffTopology = true, true
	h, _ := depsHarness(t, depsResult(c))
	sum, _ := runDeps(t, h)
	blk := depsBlock(t, sum)
	if !strings.Contains(blk, "🧭 Вне заявленной архитектуры (`review.sysmap.topology`): `GET /orders/{id}/history` → `backend/orders` (HTTP)") ||
		strings.Contains(blk, "Зависит от неразвёрнутого") {
		t.Errorf("block:\n%s", blk)
	}
	noThreads(t, h)
}

func TestDepsOffTopologyCapped(t *testing.T) {
	var cs []sysmap.DepContract
	for i := 0; i < 7; i++ {
		c := missingHistory()
		c.Path, c.Line, c.InProviderMain, c.OffTopology = "/p"+strings.Repeat("x", i), 11+i, true, true
		cs = append(cs, c)
	}
	h, _ := depsHarness(t, depsResult(cs...))
	sum, _ := runDeps(t, h)
	if blk := depsBlock(t, sum); !strings.Contains(blk, "`GET /pxxxx` → `backend/orders` (HTTP), и ещё 2.") || strings.Contains(blk, "/pxxxxx`") {
		t.Errorf("block:\n%s", blk)
	}
}

package repo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/lint"
)

// newCachedManager is a Manager with the checkout cache on (no janitor:
// tests sweep by hand). The read-only trees are removed at the end (a
// non-root TempDir cleanup could not).
func newCachedManager(t *testing.T, gitlabURL string, ttl time.Duration, maxBytes int64) *Manager {
	t.Helper()
	m, err := New(Options{BaseDir: t.TempDir(), GitLabURL: gitlabURL, Token: testToken,
		CacheTTL: ttl, CacheMaxBytes: maxBytes, NoJanitor: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		m.Close()
		removeTree(m.trees.dir, nil)
	})
	return m
}

func readOnly(t *testing.T, p string) {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("%s is writable: %v", p, fi.Mode())
	}
}

func TestCachedCheckout(t *testing.T) {
	r, mr := setupMR(t)
	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	ctx := context.Background()

	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	dir := co.Dir()
	if filepath.Dir(dir) != m.trees.dir || !strings.Contains(filepath.Base(dir), "@"+mr.DiffRefs.HeadSHA) {
		t.Fatalf("tree dir %s", dir)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "main.go")); err != nil || !strings.Contains(string(b), "func main") {
		t.Fatalf("head files: %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("cached tree has .git: %v", err)
	}
	readOnly(t, filepath.Join(dir, "main.go"))
	readOnly(t, dir)
	readOnly(t, filepath.Join(dir, "vendor", "x"))
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("x"), 0o644); err == nil && os.Geteuid() != 0 {
		t.Fatal("wrote into the cached tree")
	}

	// Git runs against the mirror with the head SHA.
	files, err := co.ChangedFiles(ctx, mr.DiffRefs.StartSHA, []string{"**/go.sum", "vendor/**"})
	if err != nil || strings.Join(files, ",") != "docs/[weird].md,main.go" {
		t.Fatalf("changed files %v %v", files, err)
	}
	diff, err := co.Diff(ctx, mr.DiffRefs.StartSHA, []string{"**/go.sum", "vendor/**"})
	if err != nil || !strings.Contains(diff, "+func main() {}") || strings.Contains(diff, "go.sum") {
		t.Fatalf("diff %v:\n%s", err, diff)
	}
	if ok, err := co.IsAncestor(ctx, mr.DiffRefs.BaseSHA); err != nil || !ok {
		t.Fatalf("is-ancestor %v %v", ok, err)
	}
	if b, err := co.ShowFile(ctx, mr.DiffRefs.BaseSHA, "go.sum"); err != nil || string(b) != "a v1\n" {
		t.Fatalf("show %q %v", b, err)
	}
	if fs, err := co.ListFiles(ctx, mr.DiffRefs.HeadSHA); err != nil || len(fs) != 5 {
		t.Fatalf("list %v %v", fs, err)
	}
	if ch, err := co.(*Checkout).LinesChanged(ctx, mr.DiffRefs.StartSHA, "", "main.go", 1, 1); err != nil || !ch {
		t.Fatalf("lines changed %v %v", ch, err)
	}

	// The bot's own files go to the job's scratch dir, never into the tree.
	sd, err := co.(*Checkout).ScratchDir(".reviewbot-compat")
	if err != nil || strings.HasPrefix(sd, dir) || filepath.Base(sd) != ".reviewbot-compat" {
		t.Fatalf("scratch %s %v", sd, err)
	}
	hist, err := co.(*Checkout).HistoryDir(ctx)
	if err != nil || strings.HasPrefix(hist, dir) {
		t.Fatalf("history %s %v", hist, err)
	}
	if fi, err := os.Stat(filepath.Join(hist, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("history has no .git dir: %v", err)
	}
	if got := gitT(t, hist, "log", "--format=%H", mr.DiffRefs.BaseSHA+".."+mr.DiffRefs.HeadSHA); !strings.Contains(got, mr.DiffRefs.HeadSHA) {
		t.Fatalf("history lacks the MR commits: %q", got)
	}
	if _, err := os.Stat(filepath.Join(hist, ".git", "objects", "info", "alternates")); err == nil {
		t.Fatal("history clone has alternates")
	}
	assertNoToken(t, filepath.Join(hist, ".git"))
	if h2, _ := co.(*Checkout).HistoryDir(ctx); h2 != hist {
		t.Fatal("history cloned twice")
	}

	// A second checkout of the same head reuses the tree.
	co2, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	if co2.Dir() != dir {
		t.Fatalf("second checkout %s, want the cached %s", co2.Dir(), dir)
	}
	if n, b := m.CacheStats(); n != 1 || b <= 0 {
		t.Fatalf("cache stats %d %d", n, b)
	}
	if err := co.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sd); !os.IsNotExist(err) {
		t.Fatalf("scratch dir left after Close: %v", err)
	}
	if _, err := os.Stat(hist); !os.IsNotExist(err) {
		t.Fatalf("history clone left after Close: %v", err)
	}
	co2.Close()
	// Kept after use (TTL).
	m.trees.sweep()
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("tree removed before its TTL: %v", err)
	}
	if err := co.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
}

func TestCachedExportTreeSharedMergeBase(t *testing.T) {
	r, mr := setupMR(t)
	gitT(t, r.dir, "checkout", "-q", "main")
	r.write(".claude/settings.json", `{}`)
	r.write(".mcp.json", "{}")
	// The merge base itself carries Claude config: dropped from /base.
	gitT(t, r.dir, "checkout", "-q", "feature")
	r.write(".claude/settings.json", `{"permissions":{"allow":["Bash"]}}`)
	r.write(".mcp.json", "{}")
	head := r.commit("claude config")
	r.setMRHead(1, head)
	gitT(t, r.dir, "checkout", "-q", "main")
	mr.DiffRefs.HeadSHA = head
	// A second MR on the same merge base.
	gitT(t, r.dir, "checkout", "-q", "-b", "other", mr.DiffRefs.BaseSHA)
	r.write("other.go", "package other\n")
	head2 := r.commit("other")
	r.setMRHead(2, head2)
	gitT(t, r.dir, "checkout", "-q", "main")
	mr2 := &core.MergeRequest{IID: 2, TargetBranch: "main", DiffRefs: core.DiffRefs{BaseSHA: mr.DiffRefs.BaseSHA, StartSHA: mr.DiffRefs.StartSHA, HeadSHA: head2}}

	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	// The head tree keeps the MR's files as they are (Claude ignores the
	// config: --setting-sources user).
	if _, err := os.Stat(filepath.Join(co.Dir(), ".mcp.json")); err != nil {
		t.Fatalf("head tree lost .mcp.json: %v", err)
	}
	ex, _ := core.CheckoutAs[core.TreeExporter](co)
	base, rel, err := ex.ExportTree(ctx, mr.DiffRefs.HeadSHA, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".claude", ".mcp.json", ".git"} {
		if _, err := os.Lstat(filepath.Join(base, p)); !os.IsNotExist(err) {
			t.Errorf("%s in the base tree: %v", p, err)
		}
	}
	if base == co.Dir() {
		t.Fatal("the base variant must not be the head tree")
	}
	rel()
	rel() // idempotent

	b1, rel1, err := ex.ExportTree(ctx, mr.DiffRefs.BaseSHA, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Join(b1, "README.md"))
	co2, err := m.Checkout(ctx, "group/proj", mr2)
	if err != nil {
		t.Fatal(err)
	}
	ex2, _ := core.CheckoutAs[core.TreeExporter](co2)
	b2, rel2, err := ex2.ExportTree(ctx, mr.DiffRefs.BaseSHA, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if b1 != b2 {
		t.Fatalf("merge base not shared: %s vs %s", b1, b2)
	}
	rel1()
	rel2()
	co2.Close()
	if _, err := os.Stat(b1); err != nil {
		t.Fatalf("released base removed before its TTL: %v", err)
	}
	// The size cap (review.context.max_repo_mb) holds for a cached tree too.
	if _, _, err := ex.ExportTree(ctx, mr.DiffRefs.BaseSHA, 3); !errors.Is(err, core.ErrTreeTooLarge) {
		t.Fatalf("size cap on the cached base: %v", err)
	}
	if _, _, err := ex.ExportTree(ctx, mr.DiffRefs.StartSHA, 3); !errors.Is(err, core.ErrTreeTooLarge) {
		t.Fatalf("size cap before checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.trees.dir, treeKey("group/proj", mr.DiffRefs.StartSHA, true))); !os.IsNotExist(err) {
		t.Fatalf("a too large tree was written: %v", err)
	}
	if err := co.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ex.ExportTree(ctx, mr.DiffRefs.BaseSHA, 0); err == nil {
		t.Fatal("export after Close")
	}
}

func TestCachedTreeSurvivesMirrorFetch(t *testing.T) {
	r, mr := setupMR(t)
	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	ctx := context.Background()
	co1, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co1.Close()
	// A new push to the MR while the first review still runs.
	gitT(t, r.dir, "checkout", "-q", "feature")
	r.write("main.go", "package main\n\nfunc main() { println(1) }\n")
	head2 := r.commit("more")
	r.setMRHead(1, head2)
	gitT(t, r.dir, "checkout", "-q", "main")
	mr2 := *mr
	mr2.DiffRefs.HeadSHA = head2
	co2, err := m.Checkout(ctx, "group/proj", &mr2)
	if err != nil {
		t.Fatal(err)
	}
	defer co2.Close()
	if co1.Dir() == co2.Dir() {
		t.Fatal("two heads share a tree")
	}
	b, _ := os.ReadFile(filepath.Join(co1.Dir(), "main.go"))
	if strings.Contains(string(b), "println") {
		t.Fatal("the live tree changed under the first review")
	}
	d1, err := co1.Diff(ctx, mr.DiffRefs.BaseSHA, nil)
	if err != nil || strings.Contains(d1, "println") || !strings.Contains(d1, "func main() {}") {
		t.Fatalf("first review's diff after the fetch: %v\n%s", err, d1)
	}
	if anc, _ := co2.IsAncestor(ctx, mr.DiffRefs.HeadSHA); !anc {
		t.Fatal("previous head is an ancestor of the new one")
	}
}

func TestCachedTreeNeutralizesEscapingSymlink(t *testing.T) {
	r := newRemote(t)
	r.write("README.md", "hi\n")
	base := r.commit("init")
	gitT(t, r.dir, "checkout", "-q", "-b", "feature")
	r.write("safe.txt", "ok\n")
	if err := os.Symlink("../../../../etc/passwd", filepath.Join(r.dir, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("safe.txt", filepath.Join(r.dir, "good")); err != nil {
		t.Fatal(err)
	}
	head := r.commit("add symlinks")
	r.setMRHead(1, head)
	gitT(t, r.dir, "checkout", "-q", "main")
	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	co, err := m.Checkout(context.Background(), "group/proj", &core.MergeRequest{IID: 1, TargetBranch: "main",
		DiffRefs: core.DiffRefs{BaseSHA: base, StartSHA: base, HeadSHA: head}})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if _, err := os.Lstat(filepath.Join(co.Dir(), "evil")); !os.IsNotExist(err) {
		t.Errorf("escaping symlink kept: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(co.Dir(), "good")); err != nil {
		t.Errorf("in-tree symlink removed: %v", err)
	}
}

func TestCachedConcurrentReviewsOfTwoMRs(t *testing.T) {
	r, mr := setupMR(t)
	gitT(t, r.dir, "checkout", "-q", "-b", "two", mr.DiffRefs.BaseSHA)
	r.write("two.go", "package two\n")
	head2 := r.commit("two")
	r.setMRHead(2, head2)
	gitT(t, r.dir, "checkout", "-q", "main")
	mr2 := &core.MergeRequest{IID: 2, TargetBranch: "main", DiffRefs: core.DiffRefs{BaseSHA: mr.DiffRefs.BaseSHA, StartSHA: mr.DiffRefs.StartSHA, HeadSHA: head2}}
	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		for _, x := range []*core.MergeRequest{mr, mr2} {
			wg.Add(1)
			go func(x *core.MergeRequest) {
				defer wg.Done()
				co, err := m.Checkout(ctx, "group/proj", x)
				if err != nil {
					errs <- err
					return
				}
				defer co.Close()
				files, err := co.ChangedFiles(ctx, x.DiffRefs.BaseSHA, nil)
				want := "two.go"
				if x == mr {
					want = "docs/[weird].md,go.sum,main.go,vendor/x/x.go"
				}
				if err != nil || strings.Join(files, ",") != want {
					errs <- errors.New("wrong files: " + strings.Join(files, ","))
					return
				}
				ex, _ := core.CheckoutAs[core.TreeExporter](co)
				_, rel, err := ex.ExportTree(ctx, x.DiffRefs.BaseSHA, 0)
				if err != nil {
					errs <- err
					return
				}
				rel()
			}(x)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Two heads and one shared merge base.
	if n, _ := m.CacheStats(); n != 3 {
		t.Fatalf("trees: %d", n)
	}
}

func TestTreeCacheTTLAndBudget(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := newTreeCache(filepath.Join(t.TempDir(), "trees"), time.Hour, 250, log, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeTree(c.dir, nil) })
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	var builds atomic.Int32
	build := func(size int64) func(context.Context, string) (int64, error) {
		return func(_ context.Context, dir string) (int64, error) {
			builds.Add(1)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return 0, err
			}
			return size, os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
		}
	}
	ctx := context.Background()
	dA, relA, err := c.acquire(ctx, "a", build(100))
	if err != nil {
		t.Fatal(err)
	}
	readOnly(t, filepath.Join(dA, "f"))
	_, relB, _ := c.acquire(ctx, "b", build(100))
	now = now.Add(time.Minute)
	relA()
	now = now.Add(time.Minute)
	relB()
	// Over the budget (300 > 250) with "c" in use: the least recently used
	// idle tree ("a") goes, "b" stays.
	dC, relC, _ := c.acquire(ctx, "c", build(100))
	if _, err := os.Stat(dA); !os.IsNotExist(err) {
		t.Fatalf("LRU tree not evicted over the budget: %v", err)
	}
	if n, b := c.stats(); n != 2 || b != 200 {
		t.Fatalf("stats %d %d", n, b)
	}
	// In use: never evicted, even past the TTL.
	now = now.Add(2 * time.Hour)
	c.sweep()
	if _, err := os.Stat(dC); err != nil {
		t.Fatalf("tree in use evicted: %v", err)
	}
	if n, _ := c.stats(); n != 1 {
		t.Fatalf("idle tree past its TTL kept: %d", n)
	}
	// Reuse within the TTL does not rebuild.
	relC()
	now = now.Add(30 * time.Minute)
	c.sweep()
	if _, rel, _ := c.acquire(ctx, "c", build(100)); rel != nil {
		rel()
	}
	if builds.Load() != 3 {
		t.Fatalf("builds: %d", builds.Load())
	}
	now = now.Add(61 * time.Minute)
	c.sweep()
	if n, _ := c.stats(); n != 0 {
		t.Fatalf("trees after the TTL: %d", n)
	}
	if _, err := os.Stat(dC); !os.IsNotExist(err) {
		t.Fatalf("evicted tree still on disk: %v", err)
	}
}

func TestTreeCacheConcurrentAcquireBuildsOnce(t *testing.T) {
	c, err := newTreeCache(filepath.Join(t.TempDir(), "trees"), time.Hour, 0, slog.New(slog.DiscardHandler), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeTree(c.dir, nil) })
	var builds atomic.Int32
	fail := atomic.Bool{}
	fail.Store(true)
	build := func(_ context.Context, dir string) (int64, error) {
		builds.Add(1)
		time.Sleep(20 * time.Millisecond)
		if fail.Swap(false) {
			return 0, errors.New("first build fails")
		}
		return 1, os.MkdirAll(dir, 0o755)
	}
	var wg sync.WaitGroup
	var ok atomic.Int32
	dirs := sync.Map{}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, rel, err := c.acquire(context.Background(), "k", build)
			if err != nil {
				return // the failed first build
			}
			ok.Add(1)
			dirs.Store(d, true)
			rel()
		}()
	}
	wg.Wait()
	if ok.Load() < 5 || builds.Load() != 2 {
		t.Fatalf("ok %d, builds %d (want one failed and one good build)", ok.Load(), builds.Load())
	}
	n := 0
	dirs.Range(func(any, any) bool { n++; return true })
	if n != 1 {
		t.Fatalf("%d different dirs", n)
	}
	// A cancelled waiter gives up.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.acquire(ctx, "slow", func(ctx context.Context, dir string) (int64, error) {
		return 0, ctx.Err()
	}); err == nil {
		t.Fatal("cancelled build succeeded")
	}
}

func TestTreeCacheEmptiedAtStartup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "trees")
	mustMkdir(t, filepath.Join(dir, "old@abc", "sub"))
	if err := os.WriteFile(filepath.Join(dir, "old@abc", "sub", "f"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	makeReadOnly(filepath.Join(dir, "old@abc"))
	c, err := newTreeCache(dir, time.Hour, 0, slog.New(slog.DiscardHandler), false)
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(c.dir)
	if len(ents) != 0 {
		t.Fatalf("left at startup: %v", ents)
	}
}

// execRecorder is a core.Runner that records Exec requests and the files
// the checker's directory holds at that moment.
type execRecorder struct {
	mu    sync.Mutex
	reqs  []core.ExecRequest
	files [][]string
}

func (e *execRecorder) Run(context.Context, core.RunRequest) (*core.RunResult, error) {
	return nil, errors.New("no runs")
}

func (e *execRecorder) Exec(_ context.Context, req core.ExecRequest) (*core.ExecResult, error) {
	var fs []string
	_ = filepath.WalkDir(req.Dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.Contains(p, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			rel, _ := filepath.Rel(req.Dir, p)
			fs = append(fs, rel)
		}
		return nil
	})
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	e.files = append(e.files, fs)
	e.mu.Unlock()
	return &core.ExecResult{ExitCode: 0, Stdout: "[]"}, nil
}

// gitleaks of a cached checkout runs in the job's history clone: a real
// .git directory with the MR's commits, its config next to it, the shared
// tree untouched.
func TestCachedCheckoutGitleaksOutsideTree(t *testing.T) {
	r, mr := setupMR(t)
	m := newCachedManager(t, filepath.Dir(filepath.Dir(r.dir)), time.Hour, 0)
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	before, _ := os.ReadDir(co.Dir())
	rec := &execRecorder{}
	res, err := (&lint.Checker{Runner: rec}).Check(ctx, lint.Request{Checkout: co, Base: mr.DiffRefs.BaseSHA, Head: mr.DiffRefs.HeadSHA,
		Config: lint.Config{Tools: []string{lint.ToolGitleaks}}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.ToolOK(lint.ToolGitleaks) || len(rec.reqs) != 1 {
		t.Fatalf("gitleaks: %+v, execs %d", res.Tools, len(rec.reqs))
	}
	dir := rec.reqs[0].Dir
	if strings.HasPrefix(dir, co.Dir()) {
		t.Fatalf("gitleaks ran in the shared tree %s", dir)
	}
	if fi, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !fi.IsDir() {
		t.Fatalf("no .git dir for gitleaks: %v", err)
	}
	if got := strings.Join(rec.files[0], ","); !strings.Contains(got, ".reviewbot-lint/gitleaks.toml") {
		t.Fatalf("gitleaks config not next to the history: %s", got)
	}
	after, _ := os.ReadDir(co.Dir())
	if len(after) != len(before) {
		t.Fatalf("the shared tree changed: %v -> %v", before, after)
	}
}

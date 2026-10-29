package repo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/migrations"
)

const testToken = "glpat-SUPERSECRET-token-123"

type remote struct {
	t   *testing.T
	dir string // working repo acting as the GitLab project
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_NOSYSTEM=1", "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRemote(t *testing.T) *remote {
	dir := filepath.Join(t.TempDir(), "group", "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "init", "-q", "-b", "main")
	return &remote{t: t, dir: dir}
}

func (r *remote) write(path, content string) {
	r.t.Helper()
	p := filepath.Join(r.dir, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *remote) commit(msg string) string {
	r.t.Helper()
	gitT(r.t, r.dir, "add", "-A")
	gitT(r.t, r.dir, "commit", "-q", "-m", msg)
	return gitT(r.t, r.dir, "rev-parse", "HEAD")
}

func (r *remote) setMRHead(iid int, sha string) {
	r.t.Helper()
	gitT(r.t, r.dir, "update-ref", "refs/merge-requests/"+strconv.Itoa(iid)+"/head", sha)
}

func newManager(t *testing.T, gitlabURL string) *Manager {
	t.Helper()
	m, err := New(Options{BaseDir: t.TempDir(), GitLabURL: gitlabURL, Token: testToken})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// setupMR builds main with two commits and an MR branch with changes.
func setupMR(t *testing.T) (*remote, *core.MergeRequest) {
	r := newRemote(t)
	r.write("README.md", "hello\n")
	r.write("go.sum", "a v1\n")
	base := r.commit("init")
	r.write("main.go", "package main\n")
	start := r.commit("main 2")

	gitT(t, r.dir, "checkout", "-q", "-b", "feature")
	r.write("main.go", "package main\n\nfunc main() {}\n")
	r.write("go.sum", "a v1\nb v2\n")
	r.write("vendor/x/x.go", "package x\n")
	r.write("docs/[weird].md", "glob chars\n")
	head := r.commit("feature work")
	r.setMRHead(1, head)
	gitT(t, r.dir, "checkout", "-q", "main")

	return r, &core.MergeRequest{
		IID: 1, TargetBranch: "main", SourceBranch: "feature",
		DiffRefs: core.DiffRefs{BaseSHA: base, StartSHA: start, HeadSHA: head},
	}
}

func TestCheckoutDiffIgnore(t *testing.T) {
	r, mr := setupMR(t)
	m := newManager(t, filepath.Dir(filepath.Dir(r.dir))) // base dir; project resolved as group/proj
	ctx := context.Background()

	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	if got := gitT(t, co.Dir(), "rev-parse", "HEAD"); got != mr.DiffRefs.HeadSHA {
		t.Fatalf("HEAD = %s, want %s", got, mr.DiffRefs.HeadSHA)
	}
	if b, err := os.ReadFile(filepath.Join(co.Dir(), "main.go")); err != nil || !strings.Contains(string(b), "func main") {
		t.Fatalf("working tree not checked out: %v %q", err, b)
	}

	// Self-contained: no alternates, origin is a placeholder.
	if _, err := os.Stat(filepath.Join(co.Dir(), ".git", "objects", "info", "alternates")); err == nil {
		t.Fatal("clone has alternates")
	}
	if fi, err := os.Stat(filepath.Join(co.Dir(), ".git")); err != nil || !fi.IsDir() {
		t.Fatal(".git is not a directory")
	}
	cfg, _ := os.ReadFile(filepath.Join(co.Dir(), ".git", "config"))
	if strings.Contains(string(cfg), m.mirrorDir) {
		t.Fatalf("clone config references mirror:\n%s", cfg)
	}

	files, err := co.ChangedFiles(ctx, mr.DiffRefs.StartSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "docs/[weird].md,go.sum,main.go,vendor/x/x.go" {
		t.Fatalf("changed files = %v", files)
	}

	ignore := []string{"**/go.sum", "vendor/**"}
	files, err = co.ChangedFiles(ctx, mr.DiffRefs.StartSHA, ignore)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(files, ",") != "docs/[weird].md,main.go" {
		t.Fatalf("changed files with ignore = %v", files)
	}

	diff, err := co.Diff(ctx, mr.DiffRefs.StartSHA, ignore)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "+func main() {}") || !strings.Contains(diff, "glob chars") {
		t.Fatalf("diff missing main.go/weird changes:\n%s", diff)
	}
	if strings.Contains(diff, "go.sum") || strings.Contains(diff, "vendor/") {
		t.Fatalf("diff contains ignored files:\n%s", diff)
	}

	full, err := co.Diff(ctx, mr.DiffRefs.StartSHA, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full, "go.sum") {
		t.Fatal("unfiltered diff should include go.sum")
	}

	none, err := co.Diff(ctx, mr.DiffRefs.StartSHA, []string{"**"})
	if err != nil || none != "" {
		t.Fatalf("all-ignored diff = %q, %v", none, err)
	}

	// Base SHA is available for diffing too.
	if _, err := co.Diff(ctx, mr.DiffRefs.BaseSHA, nil); err != nil {
		t.Fatal(err)
	}

	assertNoToken(t, filepath.Join(co.Dir(), ".git"))
	assertNoToken(t, m.mirrorDir)

	dir := co.Dir()
	if err := co.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("work dir not removed")
	}
	if err := co.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
}

func TestCheckoutNeutralizesEscapingSymlink(t *testing.T) {
	r := newRemote(t)
	r.write("README.md", "hi\n")
	base := r.commit("init")
	start := base
	gitT(t, r.dir, "checkout", "-q", "-b", "feature")
	r.write("safe.txt", "ok\n")
	// Commit symlinks: one escaping (../../../etc/passwd), one in-tree.
	if err := os.Symlink("../../../../etc/passwd", filepath.Join(r.dir, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("safe.txt", filepath.Join(r.dir, "good")); err != nil {
		t.Fatal(err)
	}
	head := r.commit("add symlinks")
	r.setMRHead(1, head)
	gitT(t, r.dir, "checkout", "-q", "main")

	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	mr := &core.MergeRequest{IID: 1, TargetBranch: "main", SourceBranch: "feature",
		DiffRefs: core.DiffRefs{BaseSHA: base, StartSHA: start, HeadSHA: head}}
	co, err := m.Checkout(context.Background(), "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if _, err := os.Lstat(filepath.Join(co.Dir(), "evil")); !os.IsNotExist(err) {
		t.Errorf("escaping symlink not neutralized (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(co.Dir(), "good")); err != nil {
		t.Errorf("in-tree symlink should survive: %v", err)
	}
}

func TestDiffRenameKept(t *testing.T) {
	r := newRemote(t)
	r.write("a/old.go", strings.Repeat("line\n", 50))
	base := r.commit("init")
	gitT(t, r.dir, "mv", "a/old.go", "a/new.go")
	head := r.commit("rename")
	r.setMRHead(2, head)

	m := newManager(t, r.dir) // the base path is the repository itself
	co, err := m.Checkout(context.Background(), "x/y", &core.MergeRequest{
		IID: 2, TargetBranch: "main", DiffRefs: core.DiffRefs{BaseSHA: base, StartSHA: base, HeadSHA: head},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	diff, err := co.Diff(context.Background(), base, []string{"**/*.md"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "rename from a/old.go") || !strings.Contains(diff, "rename to a/new.go") {
		t.Fatalf("rename not detected with pathspecs:\n%s", diff)
	}
}

func TestIsAncestorAfterForcePush(t *testing.T) {
	r, mr := setupMR(t)
	m := newManager(t, "file://"+r.dir)
	ctx := context.Background()

	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := co.IsAncestor(ctx, mr.DiffRefs.StartSHA)
	if err != nil || !ok {
		t.Fatalf("start should be ancestor: %v %v", ok, err)
	}
	oldHead := mr.DiffRefs.HeadSHA
	co.Close()

	// Force push: rewrite the feature branch from start with a different commit.
	gitT(t, r.dir, "checkout", "-q", "-B", "feature", mr.DiffRefs.StartSHA)
	r.write("other.go", "package main\n")
	newHead := r.commit("rewritten")
	r.setMRHead(1, newHead)
	gitT(t, r.dir, "checkout", "-q", "main")

	mr2 := *mr
	mr2.DiffRefs.HeadSHA = newHead
	co2, err := m.Checkout(ctx, "group/proj", &mr2)
	if err != nil {
		t.Fatal(err)
	}
	defer co2.Close()

	ok, err = co2.IsAncestor(ctx, oldHead)
	if err != nil || ok {
		t.Fatalf("old head must not be ancestor after force push: %v %v", ok, err)
	}
	ok, err = co2.IsAncestor(ctx, "0123456789abcdef0123456789abcdef01234567")
	if err != nil || ok {
		t.Fatalf("unknown commit: %v %v", ok, err)
	}
	ok, err = co2.IsAncestor(ctx, "not-a-sha")
	if err != nil || ok {
		t.Fatalf("garbage commit: %v %v", ok, err)
	}
	ok, err = co2.IsAncestor(ctx, mr.DiffRefs.BaseSHA)
	if err != nil || !ok {
		t.Fatalf("base should be ancestor: %v %v", ok, err)
	}
}

func TestFetchUnreferencedSHA(t *testing.T) {
	r, mr := setupMR(t)
	// A commit that is not reachable from any fetched ref.
	gitT(t, r.dir, "checkout", "-q", "-b", "side", mr.DiffRefs.BaseSHA)
	r.write("side.txt", "x\n")
	side := r.commit("side")
	gitT(t, r.dir, "checkout", "-q", "main")
	gitT(t, r.dir, "config", "uploadpack.allowAnySHA1InWant", "true")

	m := newManager(t, r.dir)
	mr.DiffRefs.BaseSHA = side
	co, err := m.Checkout(context.Background(), "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	if _, err := co.Diff(context.Background(), side, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRemoteURLAndSanitize(t *testing.T) {
	m := newManager(t, "https://gitlab.example.com/")
	if got := m.remoteURL("group/sub/proj"); got != "https://gitlab.example.com/group/sub/proj.git" {
		t.Fatalf("remoteURL = %s", got)
	}
	a, b := sanitize("group/sub/proj"), sanitize("group/sub_proj")
	if a == b || strings.ContainsAny(a, "/\\") {
		t.Fatalf("sanitize: %s %s", a, b)
	}
	if strings.Contains(sanitize("../../etc"), "..") {
		t.Fatal("sanitize allows ..")
	}
}

func TestAuthHeaderEnvOnly(t *testing.T) {
	g := &gitRunner{bin: "git", token: testToken}
	env := g.env(gitCmd{auth: true})
	joined := strings.Join(env, "\n")
	// Forced safe config is prepended, so the auth header is the last pair.
	n := len(hostSafeGitConfig)
	if !hasConfigPair(env, n, "http.extraHeader", g.authHeader()) ||
		!strings.HasPrefix(g.authHeader(), "Authorization: Basic b2F1dGgyOm") {
		t.Fatalf("auth header missing from env:\n%s", joined)
	}
	if strings.Contains(strings.Join(g.env(gitCmd{}), "\n"), "extraHeader") {
		t.Fatal("auth header set for non-network command")
	}
	if strings.Contains(g.redact("x "+testToken+" "+g.authHeader()), testToken) {
		t.Fatal("redact failed")
	}
}

// hasConfigPair reports whether GIT_CONFIG_KEY_<i>/VALUE_<i> hold key/value.
func hasConfigPair(env []string, i int, key, value string) bool {
	wantK := "GIT_CONFIG_KEY_" + strconv.Itoa(i) + "=" + key
	wantV := "GIT_CONFIG_VALUE_" + strconv.Itoa(i) + "=" + value
	var k, v bool
	for _, kv := range env {
		if kv == wantK {
			k = true
		}
		if kv == wantV {
			v = true
		}
	}
	return k && v
}

func TestHostSafeGitEnv(t *testing.T) {
	g := &gitRunner{bin: "git", token: testToken}
	env := strings.Join(g.env(gitCmd{}), "\n")
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_KEY_0=core.fsmonitor",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.hooksPath",
		"GIT_CONFIG_VALUE_1=/dev/null",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("host git env missing %q:\n%s", want, env)
		}
	}
	for _, k := range []string{"core.pager", "diff.external", "core.sshCommand"} {
		if !strings.Contains(env, k) {
			t.Errorf("host git env missing key %q", k)
		}
	}
	// safe.directory=* must never be forced host-side.
	if strings.Contains(env, "safe.directory") {
		t.Error("host git must not set safe.directory itself")
	}
	// An explicit c.config entry is appended after the forced config.
	env2 := g.env(gitCmd{config: [][2]string{{"core.quotePath", "false"}}})
	if !hasConfigPair(env2, len(hostSafeGitConfig), "core.quotePath", "false") {
		t.Errorf("explicit config not appended after forced config:\n%s", strings.Join(env2, "\n"))
	}
}

func TestNeutralizeSymlinks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var is itself a symlink
	if err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(root, "clone")
	mustMkdir(t, filepath.Join(clone, "sub"))
	mustMkdir(t, filepath.Join(clone, ".git"))
	// In-tree regular file and a safe in-tree symlink to it.
	if err := os.WriteFile(filepath.Join(clone, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "real.txt", filepath.Join(clone, "sub", "inlink"))
	mustSymlink(t, "../real.txt", filepath.Join(clone, "sub", "uplink")) // still inside clone
	// Escaping symlinks: absolute, relative traversal, and dangling escape.
	outside := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, filepath.Join(clone, "abs"))
	mustSymlink(t, "../secret.txt", filepath.Join(clone, "rel"))
	mustSymlink(t, "/etc/shadow", filepath.Join(clone, "sub", "etc"))
	mustSymlink(t, "../nonexistent-escape", filepath.Join(clone, "dangling"))
	// A symlink inside .git must be left alone (skipped).
	mustSymlink(t, outside, filepath.Join(clone, ".git", "gitlink"))

	n := neutralizeSymlinks(clone, nil)
	if n != 4 {
		t.Fatalf("removed %d, want 4", n)
	}
	for _, kept := range []string{"real.txt", "sub/inlink", "sub/uplink", ".git/gitlink"} {
		if _, err := os.Lstat(filepath.Join(clone, kept)); err != nil {
			t.Errorf("%s should be kept: %v", kept, err)
		}
	}
	for _, gone := range []string{"abs", "rel", "sub/etc", "dangling"} {
		if _, err := os.Lstat(filepath.Join(clone, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should be removed (err=%v)", gone, err)
		}
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestValidators(t *testing.T) {
	for _, s := range []string{"main", "release/1.0", "feat-x_y"} {
		if !validRefName(s) {
			t.Errorf("validRefName(%q) = false", s)
		}
	}
	for _, s := range []string{"", "-x", "a..b", "a:b", "a b", "a*", "x.lock"} {
		if validRefName(s) {
			t.Errorf("validRefName(%q) = true", s)
		}
	}
	if validRev("--upload-pack=x") || !validRev("abcdef12") {
		t.Error("validRev")
	}
}

func assertNoToken(t *testing.T, dir string) {
	t.Helper()
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Size() > 1<<20 {
			return err
		}
		name := filepath.Base(p)
		if name != "config" && !strings.HasSuffix(name, ".log") && name != "FETCH_HEAD" && !strings.HasPrefix(filepath.Base(filepath.Dir(p)), "log") {
			return nil
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), testToken) || strings.Contains(string(b), "extraHeader") || strings.Contains(string(b), "Authorization") {
			t.Errorf("token material found in %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestKeptPaths(t *testing.T) {
	out := []byte("M\x00a.go\x00R100\x00old/x.go\x00new/x.go\x00A\x00vendor/v.go\x00D\x00gone.go\x00")
	got := keptPaths(out, []string{"vendor/**"})
	want := [][]string{{"a.go"}, {"old/x.go", "new/x.go"}, {"gone.go"}}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if strings.Join(got[i], "|") != strings.Join(want[i], "|") {
			t.Fatalf("got %q", got)
		}
	}
}

func TestRedactShortTokenKeepsText(t *testing.T) {
	g := &gitRunner{bin: "git", token: "x"}
	if got := g.redact("fatal: https://g.example/x.git"); got != "fatal: https://g.example/x.git" {
		t.Fatalf("redact corrupted text: %q", got)
	}
}

func TestShowFileAndListFiles(t *testing.T) {
	r, mr := setupMR(t)
	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()

	b, err := co.ShowFile(ctx, mr.DiffRefs.BaseSHA, "go.sum")
	if err != nil || string(b) != "a v1\n" {
		t.Fatalf("base go.sum = %q, %v", b, err)
	}
	b, err = co.ShowFile(ctx, mr.DiffRefs.HeadSHA, "go.sum")
	if err != nil || string(b) != "a v1\nb v2\n" {
		t.Fatalf("head go.sum = %q, %v", b, err)
	}
	if _, err := co.ShowFile(ctx, mr.DiffRefs.BaseSHA, "main.go"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := co.ShowFile(ctx, "not-a-rev;", "x"); err == nil {
		t.Fatal("invalid rev accepted")
	}
	files, err := co.ListFiles(ctx, mr.DiffRefs.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(files, ",")
	for _, want := range []string{"README.md", "main.go", "vendor/x/x.go", "docs/[weird].md"} {
		if !strings.Contains(got, want) {
			t.Errorf("ListFiles lacks %s: %s", want, got)
		}
	}
}

// gitlab.ca_file reaches every network git call through the per-process
// config (http.sslCAInfo) and never a local one or argv.
func TestCAFileEnvOnNetworkCalls(t *testing.T) {
	g := &gitRunner{bin: "git", token: testToken, caFile: "/etc/ssl/internal-ca.pem"}
	env := g.env(gitCmd{auth: true, args: []string{"fetch"}})
	if !hasConfigPair(env, len(hostSafeGitConfig), "http.sslCAInfo", "/etc/ssl/internal-ca.pem") {
		t.Fatalf("sslCAInfo missing from network env:\n%s", strings.Join(env, "\n"))
	}
	if !hasConfigPair(env, len(hostSafeGitConfig)+1, "http.extraHeader", g.authHeader()) {
		t.Fatal("auth header missing next to the CA file")
	}
	if strings.Contains(strings.Join(g.env(gitCmd{}), "\n"), "sslCAInfo") {
		t.Fatal("sslCAInfo set for a local command")
	}
	// Without a token (e.g. anonymous fetch) the CA file still applies.
	g2 := &gitRunner{bin: "git", caFile: "/ca.pem"}
	if !hasConfigPair(g2.env(gitCmd{auth: true}), len(hostSafeGitConfig), "http.sslCAInfo", "/ca.pem") {
		t.Fatal("sslCAInfo missing without a token")
	}

	// New resolves a relative path (git runs inside the mirror dir).
	dir := t.TempDir()
	t.Chdir(dir)
	m, err := New(Options{BaseDir: filepath.Join(dir, "repos"), GitLabURL: "https://gitlab.example.com", CAFile: "ca.pem"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "ca.pem"); m.git.caFile != want {
		t.Fatalf("caFile = %q, want %q", m.git.caFile, want)
	}
}

// Host-side git never inherits a service secret or a Claude credential.
func TestGitEnvScrubsAllSecrets(t *testing.T) {
	for _, name := range core.SecretEnv() {
		t.Setenv(name, "secret-value-"+name)
	}
	t.Setenv("REVIEWBOT_HARMLESS", "1")
	g := &gitRunner{bin: "git", token: testToken}
	env := strings.Join(g.env(gitCmd{auth: true}), "\n")
	for _, name := range []string{"GITLAB_TOKEN", "GITLAB_WEBHOOK_SECRET", "GITLAB_SERVICE_TOKEN", "GITLAB_HOOKS_TOKEN",
		"JIRA_TOKEN", "USAGE_TOKEN", "DIAG_TOKEN", "METRICS_TOKEN", "MAP_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		if strings.Contains(env, "secret-value-"+name) {
			t.Errorf("%s leaked into git env", name)
		}
	}
	if !strings.Contains(env, "REVIEWBOT_HARMLESS=1") {
		t.Error("ordinary variables must be kept")
	}
}

func TestGrepWords(t *testing.T) {
	r, mr := setupMR(t)
	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	g := co.(core.WordGrepper)

	// main.go at head: "func main() {}" on line 3; base has no main.go.
	ms, trunc, err := g.GrepWords(ctx, mr.DiffRefs.HeadSHA, []string{"main", "nothing_here"}, []string{"*.go"}, 0)
	if err != nil || trunc {
		t.Fatalf("grep: %v %v", err, trunc)
	}
	var got []string
	for _, x := range ms {
		got = append(got, x.Path+":"+strconv.Itoa(x.Line)+":"+x.Text)
	}
	if strings.Join(got, "|") != "main.go:1:package main|main.go:3:func main() {}" {
		t.Errorf("matches %q", got)
	}
	// Whole words only, no match is not an error.
	if ms, _, err := g.GrepWords(ctx, mr.DiffRefs.BaseSHA, []string{"mai"}, nil, 0); err != nil || len(ms) != 0 {
		t.Errorf("partial word matched: %v %v", ms, err)
	}
	// Output cap.
	if ms, trunc, err := g.GrepWords(ctx, mr.DiffRefs.HeadSHA, []string{"main"}, []string{"*.go"}, 60); err != nil || !trunc || len(ms) != 0 {
		t.Errorf("cap: %v %v %v", ms, trunc, err)
	}
	// Words and revisions are validated.
	for _, w := range []string{"-e", "a b", "--open-files-in-pager=x", ""} {
		if _, _, err := g.GrepWords(ctx, mr.DiffRefs.HeadSHA, []string{w}, nil, 0); err == nil {
			t.Errorf("word %q accepted", w)
		}
	}
	if _, _, err := g.GrepWords(ctx, "--output=x", []string{"main"}, nil, 0); err == nil {
		t.Error("bad revision accepted")
	}
}

// The migration usage check end to end over a real clone: git grep on the
// merge base and the head, tests and migrations not counted.
func TestGrepWordsMigrationUsageCheck(t *testing.T) {
	r := newRemote(t)
	r.write("src/Entity/Order.php", "<?php\nclass Order {\n    private ?int $createdBy;\n}\n")
	r.write("tests/OrderTest.php", "<?php $o->createdBy = 1;\n")
	r.write("migrations/Version1.php", "<?php $this->addSql('ALTER TABLE orders ADD created_by INT');\n")
	base := r.commit("init")
	gitT(t, r.dir, "checkout", "-q", "-b", "feature")
	r.write("src/Entity/Order.php", "<?php\nclass Order {\n}\n")
	r.write("migrations/Version2.php", "<?php\nuse Doctrine\\Migrations\\AbstractMigration;\nclass Version2 extends AbstractMigration {\n    public function up(Schema $schema): void\n    {\n        $this->addSql('ALTER TABLE orders DROP COLUMN created_by');\n    }\n}\n")
	head := r.commit("drop")
	r.setMRHead(1, head)
	gitT(t, r.dir, "checkout", "-q", "main")
	mr := &core.MergeRequest{IID: 1, TargetBranch: "main", SourceBranch: "feature", DiffRefs: core.DiffRefs{BaseSHA: base, StartSHA: base, HeadSHA: head}}
	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	diff, err := co.Diff(ctx, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []migrations.Input
	for _, f := range migrations.FilesFromDiff(diff) {
		b, err := co.ShowFile(ctx, head, f.NewPath)
		if err != nil {
			continue
		}
		inputs = append(inputs, migrations.Input{Path: f.NewPath, Content: b, New: f.New, Existed: f.Existed() && f.Changed(), Added: f.Added})
	}
	cfg := migrations.Config{}
	res := migrations.Analyze(inputs, cfg)
	migrations.CheckUsages(ctx, migrations.GrepSearcher{G: co.(core.WordGrepper)}, res, migrations.UsageOptions{Base: base, Head: head,
		Keep: func(p string) bool { return !cfg.IsMigrationPath(p) }})
	if len(res.Findings) != 1 {
		t.Fatalf("findings %+v", res.Findings)
	}
	f := res.Findings[0]
	if f.Level != migrations.LevelCritical || f.Usage.BaseTotal != 1 || f.Usage.Base[0].Path != "src/Entity/Order.php" ||
		f.Usage.Base[0].Line != 3 || f.Usage.HeadTotal != 0 || f.Line != 6 {
		t.Errorf("finding %+v usage %+v", f, f.Usage)
	}
	assertNoToken(t, co.Dir())
}

// ExportTree writes the tree of a commit (the merge base for /base) as a
// read-only directory next to the clone, removed by release or by Close.
func TestExportTree(t *testing.T) {
	r, mr := setupMR(t)
	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	ex, ok := co.(core.TreeExporter)
	if !ok {
		t.Fatal("checkout does not export trees")
	}

	dir, release, err := ex.ExportTree(ctx, mr.DiffRefs.BaseSHA, 0)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != filepath.Dir(co.Dir()) || strings.HasPrefix(dir, co.Dir()+string(filepath.Separator)) {
		t.Fatalf("export %s is not a sibling of the clone %s", dir, co.Dir())
	}
	b, err := os.ReadFile(filepath.Join(dir, "go.sum"))
	if err != nil || string(b) != "a v1\n" {
		t.Fatalf("base go.sum = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); !os.IsNotExist(err) {
		t.Fatalf("main.go is not in the merge base: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("export has .git: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "README.md"))
	if err != nil || fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("exported file must be read-only: %v %v", fi.Mode(), err)
	}
	release()
	release() // idempotent
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("release left %s: %v", dir, err)
	}

	// Size cap: the whole tree is a few bytes over 3.
	if _, _, err := ex.ExportTree(ctx, mr.DiffRefs.BaseSHA, 3); !errors.Is(err, core.ErrTreeTooLarge) {
		t.Fatalf("size cap: %v", err)
	}
	if _, _, err := ex.ExportTree(ctx, "HEAD;rm", 0); err == nil {
		t.Fatal("invalid rev accepted")
	}
	entries, _ := os.ReadDir(filepath.Dir(co.Dir()))
	if len(entries) != 1 {
		t.Fatalf("failed exports left directories: %v", entries)
	}

	// Close removes exports nobody released.
	dir2, _, err := ex.ExportTree(ctx, mr.DiffRefs.HeadSHA, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := co.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir2); !os.IsNotExist(err) {
		t.Fatalf("Close left %s: %v", dir2, err)
	}
	if _, _, err := ex.ExportTree(ctx, mr.DiffRefs.HeadSHA, 0); err == nil {
		t.Fatal("export after Close")
	}
}

// Claude Code project config of the exported commit is dropped.
func TestExportTreeDropsClaudeConfig(t *testing.T) {
	r, mr := setupMR(t)
	gitT(t, r.dir, "checkout", "-q", "feature")
	r.write(".claude/settings.json", `{"permissions":{"allow":["Bash"]}}`)
	r.write(".mcp.json", "{}")
	head := r.commit("claude config")
	r.setMRHead(1, head)
	gitT(t, r.dir, "checkout", "-q", "main")
	mr.DiffRefs.HeadSHA = head
	m := newManager(t, filepath.Dir(filepath.Dir(r.dir)))
	ctx := context.Background()
	co, err := m.Checkout(ctx, "group/proj", mr)
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	dir, release, err := co.(core.TreeExporter).ExportTree(ctx, head, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, p := range []string{".claude", ".mcp.json"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s exported: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		t.Errorf("main.go missing: %v", err)
	}
}

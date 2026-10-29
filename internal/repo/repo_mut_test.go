package repo

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

// Host git never inherits a GIT_* variable from the service environment
// (GIT_DIR, GIT_EXTERNAL_DIFF... could redirect or run programs), and the
// filter skips only those: variables set after them are kept.
func TestGitEnvStripsInheritedGitVars(t *testing.T) {
	t.Setenv("GIT_DIR", "/evil/repo")
	t.Setenv("GIT_EXTERNAL_DIFF", "/evil/diff.sh")
	t.Setenv("REVIEWBOT_AFTER_GIT_VARS", "kept")
	g := &gitRunner{bin: "git", token: testToken}
	env := g.env(gitCmd{})
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"GIT_DIR=", "GIT_EXTERNAL_DIFF="} {
		if strings.Contains(joined, bad) {
			t.Errorf("%s inherited by host git:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, "REVIEWBOT_AFTER_GIT_VARS=kept") {
		t.Errorf("ordinary variable after a GIT_* one dropped:\n%s", joined)
	}
}

// fakeGit writes an executable shell script used as the git binary.
func fakeGit(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-git")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A failing git command never leaks the token (raw or as the base64 auth
// header) through its stderr or its argv in the error, keeps the tail of a
// long stderr, and exposes the exit code. Same for stream().
func TestGitErrorRedactsTokenAndKeepsStderrTail(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("oauth2:" + testToken))
	junk := strings.Repeat("j", 5000)
	bin := fakeGit(t, "printf '%s' '"+junk+" fatal: token="+testToken+" header="+b64+" END' >&2\nexit 3\n")
	g := &gitRunner{bin: bin, token: testToken}
	args := []string{"fetch", "--", "https://oauth2:" + testToken + "@gitlab.example/x.git"}

	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		msg := err.Error()
		if strings.Contains(msg, testToken) || strings.Contains(msg, b64) {
			t.Errorf("%s: token leaked into error: %s", name, msg)
		}
		if !strings.Contains(msg, "[REDACTED]") {
			t.Errorf("%s: no redaction marker: %s", name, msg)
		}
		if exitCode(err) != 3 {
			t.Errorf("%s: exit code %d, want 3", name, exitCode(err))
		}
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Errorf("%s: exec.ExitError not reachable: %v", name, err)
		}
		var ge *gitError
		if !errors.As(err, &ge) {
			t.Fatalf("%s: not a gitError", name)
		}
		if !strings.HasPrefix(ge.stderr, "...") || !strings.HasSuffix(ge.stderr, "END") {
			t.Errorf("%s: stderr must keep its tail: %.40q...%q", name, ge.stderr, ge.stderr[max(0, len(ge.stderr)-20):])
		}
		if len(ge.stderr) > maxErrOutput+3 {
			t.Errorf("%s: stderr not capped: %d bytes", name, len(ge.stderr))
		}
	}
	_, err := g.run(context.Background(), gitCmd{dir: t.TempDir(), args: args})
	check("run", err)
	err = g.stream(context.Background(), gitCmd{dir: t.TempDir(), args: args}, func(r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	check("stream", err)
}

// A git command killed by its timeout reports the deadline (callers can tell
// a hung GitLab from a git failure).
func TestGitTimeoutReportsDeadline(t *testing.T) {
	bin := fakeGit(t, "exec sleep 5\n")
	g := &gitRunner{bin: bin}
	start := time.Now()
	_, err := g.run(context.Background(), gitCmd{dir: t.TempDir(), args: []string{"fetch"}, timeout: 200 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Error("timeout not enforced")
	}
}

// cappedBuffer keeps exactly the first max bytes across several writes and
// reports truncation only when bytes were actually dropped.
func TestCappedBuffer(t *testing.T) {
	b := &cappedBuffer{max: 10}
	for _, s := range []string{"abcd", "efgh", "ijkl", "mn"} {
		if n, err := b.Write([]byte(s)); err != nil || n != len(s) {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := b.buf.String(); got != "abcdefghij" || !b.over {
		t.Errorf("got %q over=%v, want abcdefghij over=true", got, b.over)
	}

	exact := &cappedBuffer{max: 8}
	_, _ = exact.Write([]byte("abcd"))
	_, _ = exact.Write([]byte("efgh"))
	if got := exact.buf.String(); got != "abcdefgh" || exact.over {
		t.Errorf("exactly max bytes: got %q over=%v, want no truncation", got, exact.over)
	}
	_, _ = exact.Write([]byte("i"))
	if !exact.over || exact.buf.String() != "abcdefgh" {
		t.Errorf("one byte past max: %q over=%v", exact.buf.String(), exact.over)
	}
}

func TestNormalizeRemote(t *testing.T) {
	for _, c := range []struct {
		in    string
		want  string
		local bool
	}{
		{"file:///srv/git/a%20b", "/srv/git/a b", true},
		{"https://gitlab.example.com/", "https://gitlab.example.com", false},
		{"/srv/git/", "/srv/git", true},
	} {
		got, local := normalizeRemote(c.in)
		if got != c.want || local != c.local {
			t.Errorf("normalizeRemote(%q) = %q,%v want %q,%v", c.in, got, local, c.want, c.local)
		}
	}
}

// Mirror directory names are stable (a change would orphan every existing
// mirror on upgrade): readable prefix of safe characters + 8 hex of sha1.
func TestSanitizePinned(t *testing.T) {
	re := regexp.MustCompile(`^(.*)-[0-9a-f]{8}$`)
	for in, wantName := range map[string]string{
		"azAZ09/my-proj_x.y":            "azAZ09_my-proj_x.y",
		"../../etc":                     "etc",
		"g/" + strings.Repeat("p", 100): "g_" + strings.Repeat("p", 78),
	} {
		m := re.FindStringSubmatch(sanitize(in))
		if m == nil || m[1] != wantName {
			t.Errorf("sanitize(%q) = %q, want %q-<hash>", in, sanitize(in), wantName)
		}
	}
}

func TestUniqueNonEmpty(t *testing.T) {
	got := uniqueNonEmpty("", "a", "b", "a", "", "c", "b")
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("got %q", got)
	}
}

func TestValidRevAndRefNameBoundaries(t *testing.T) {
	for s, ok := range map[string]bool{
		"abcd": true, "abc": false, strings.Repeat("a", 64): true, strings.Repeat("a", 65): false,
		"aaaa": true, "ffff": true, "AAAA": true, "FFFF": true, "0000": true, "9999": true,
		"gggg": false, "GGGG": false, "abc-": false, "HEAD": false,
	} {
		if validRev(s) != ok {
			t.Errorf("validRev(%q) != %v", s, ok)
		}
	}
	for _, s := range []string{"/main", "main/", "a@{1}", "a~1", "a^", "a?", "a[x", `a\b`, "a\tb", "a\x7fb", "-main"} {
		if validRefName(s) {
			t.Errorf("validRefName(%q) = true", s)
		}
	}
	for _, s := range []string{"main", "a/b", "v1.2", "a@b"} {
		if !validRefName(s) {
			t.Errorf("validRefName(%q) = false", s)
		}
	}
}

// checkoutWithShortNames: a checkout whose head has one-character paths.
func checkoutWithShortNames(t *testing.T) (core.Checkout, string) {
	t.Helper()
	r := newRemote(t)
	r.write("x", "main x\n")
	r.write("dir/y.go", "package y // main\n")
	head := r.commit("init")
	r.setMRHead(1, head)
	m := newManager(t, r.dir)
	co, err := m.Checkout(context.Background(), "g/p", &core.MergeRequest{IID: 1, TargetBranch: "main",
		DiffRefs: core.DiffRefs{BaseSHA: head, StartSHA: head, HeadSHA: head}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { co.Close() })
	return co, head
}

const unknownSHA = "0123456789abcdef0123456789abcdef01234567"

func TestListFilesShowFileValidation(t *testing.T) {
	co, head := checkoutWithShortNames(t)
	ctx := context.Background()

	files, err := co.ListFiles(ctx, head)
	if err != nil || strings.Join(files, ",") != "dir/y.go,x" {
		t.Fatalf("ListFiles = %q, %v", files, err)
	}
	// Only hex object names: no options, no symbolic refs.
	for _, rev := range []string{"--output=/tmp/x", "HEAD"} {
		if _, err := co.ListFiles(ctx, rev); err == nil {
			t.Errorf("ListFiles accepted revision %q", rev)
		}
	}
	if files, err := co.ListFiles(ctx, unknownSHA); err == nil {
		t.Errorf("ListFiles of an unknown commit = %q, want error", files)
	}

	// Invalid paths are rejected up front, not reported as "not found".
	for _, p := range []string{"", "a\nb", "a\x00b"} {
		if _, err := co.ShowFile(ctx, head, p); err == nil || errors.Is(err, core.ErrNotFound) {
			t.Errorf("ShowFile(%q) err = %v, want a validation error", p, err)
		}
	}
	if b, err := co.ShowFile(ctx, head, "x"); err != nil || string(b) != "main x\n" {
		t.Errorf("ShowFile(x) = %q, %v", b, err)
	}
}

func TestGrepWordsEdgeCases(t *testing.T) {
	co, head := checkoutWithShortNames(t)
	g := co.(core.WordGrepper)
	ctx := context.Background()

	ms, trunc, err := g.GrepWords(ctx, head, nil, nil, 0)
	if err != nil || trunc || len(ms) != 0 {
		t.Errorf("no words: %v %v %v", ms, trunc, err)
	}
	// One-character paths are real matches.
	ms, _, err = g.GrepWords(ctx, head, []string{"main"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range ms {
		got = append(got, m.Path)
	}
	if strings.Join(got, ",") != "dir/y.go,x" {
		t.Errorf("matches %q", got)
	}
	for _, p := range []string{"", "a\nb"} {
		if _, _, err := g.GrepWords(ctx, head, []string{"main"}, []string{p}, 0); err == nil {
			t.Errorf("pathspec %q accepted", p)
		}
	}
	// A git failure (unknown commit) is an error, never "no usages": the
	// migration check would otherwise call a still-used column unused.
	if ms, _, err := g.GrepWords(ctx, unknownSHA, []string{"main"}, nil, 0); err == nil {
		t.Errorf("grep of an unknown commit = %v, want error", ms)
	}
}

// Invalid project paths and branches are rejected before any git work: no
// mirror is created and nothing outside the GitLab base is fetched.
func TestSnapshotRejectsInvalidSpecBeforeGit(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "gitlab")
	newGroupRemote(t, base, "g/p", map[string]string{"a.txt": "a"})
	newGroupRemote(t, root, "evil", map[string]string{"secret.txt": "s"}) // outside base
	m, _ := newSnapManager(t, base)
	ctx := context.Background()
	for _, sp := range []SnapshotSpec{
		{Path: "../evil", DefaultBranch: "main"},
		{Path: "g/p", DefaultBranch: "-main"},
		{Path: "g/p", DefaultBranch: "a..b"},
	} {
		if s, err := m.Snapshot(ctx, sp, SnapshotOptions{}); err == nil {
			s.Release()
			t.Errorf("%+v accepted", sp)
		}
	}
	if entries, _ := os.ReadDir(m.mirrorDir); len(entries) != 0 {
		t.Errorf("mirror created for an invalid spec: %v", entries)
	}
}

// A failed refresh is remembered for the refresh interval (GitLab is not
// hammered), but only for the same branch.
func TestSnapshotCachedErrorPerBranch(t *testing.T) {
	base := t.TempDir()
	m, clk := newSnapManager(t, base)
	ctx := context.Background()
	opt := SnapshotOptions{RefreshInterval: 10 * time.Minute}
	spec := SnapshotSpec{Path: "g/late", DefaultBranch: "main"}
	if _, err := m.Snapshot(ctx, spec, opt); err == nil {
		t.Fatal("missing repository must fail")
	}
	newGroupRemote(t, base, "g/late", map[string]string{"a.txt": "a"})
	clk.Add(time.Minute)
	if s, err := m.Snapshot(ctx, spec, opt); err == nil {
		s.Release()
		t.Fatal("error not cached within the refresh interval")
	}
	// Another branch is a different request: fetched now.
	s, err := m.Snapshot(ctx, SnapshotSpec{Path: "g/late"}, opt)
	if err != nil || s == nil {
		t.Fatalf("other branch must be fetched: %v", err)
	}
	s.Release()
}

// Within the refresh interval a different branch of the same project is
// fetched, and a snapshot whose directory vanished is rebuilt.
func TestSnapshotCacheBranchSwitchAndVanishedDir(t *testing.T) {
	base := t.TempDir()
	r := newGroupRemote(t, base, "g/p", map[string]string{"a.txt": "main\n"})
	gitT(t, r.dir, "checkout", "-q", "-b", "dev")
	r.write("a.txt", "dev\n")
	devSHA := r.commit("dev")
	gitT(t, r.dir, "checkout", "-q", "main")
	m, _ := newSnapManager(t, base)
	ctx := context.Background()
	opt := SnapshotOptions{RefreshInterval: time.Hour}

	s1, err := m.Snapshot(ctx, SnapshotSpec{Path: "g/p", DefaultBranch: "main"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	s1.Release()
	s2, err := m.Snapshot(ctx, SnapshotSpec{Path: "g/p", DefaultBranch: "dev"}, opt)
	if err != nil {
		t.Fatal(err)
	}
	s2.Release()
	if s2.SHA != devSHA || s2.Branch != "dev" {
		t.Fatalf("branch switch served the cached snapshot: %+v", s2)
	}

	if err := os.RemoveAll(s2.Dir); err != nil {
		t.Fatal(err)
	}
	s3, err := m.Snapshot(ctx, SnapshotSpec{Path: "g/p", DefaultBranch: "dev"}, opt)
	if err != nil || s3 == nil {
		t.Fatalf("vanished snapshot: %v", err)
	}
	defer s3.Release()
	if b, err := os.ReadFile(filepath.Join(s3.Dir, "a.txt")); err != nil || string(b) != "dev\n" {
		t.Errorf("rebuilt snapshot content %q, %v", b, err)
	}
}

// max_repo_mb: a tree of exactly MaxBytes fits; an existing snapshot is
// checked against the current limit too.
func TestSnapshotSizeLimitBoundary(t *testing.T) {
	base := t.TempDir()
	newGroupRemote(t, base, "g/p", map[string]string{"a.txt": strings.Repeat("x", 4096)})
	m, _ := newSnapManager(t, base)
	ctx := context.Background()
	spec := SnapshotSpec{Path: "g/p", DefaultBranch: "main"}

	s, err := m.Snapshot(ctx, spec, SnapshotOptions{MaxBytes: 4096})
	if err != nil {
		t.Fatalf("tree of exactly MaxBytes rejected: %v", err)
	}
	s.Release()
	// Existing snapshot directory (refresh with the same SHA).
	s, err = m.Snapshot(ctx, spec, SnapshotOptions{MaxBytes: 4096})
	if err != nil {
		t.Fatalf("existing snapshot of exactly MaxBytes rejected: %v", err)
	}
	s.Release()
	if _, err := m.Snapshot(ctx, spec, SnapshotOptions{MaxBytes: 4095}); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Fatalf("existing snapshot over the limit: err = %v", err)
	}
}

type tarEntry struct {
	name, link string
	typ        byte
	mode       int64
	body       string
}

func buildTar(t *testing.T, entries []tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: e.mode, Size: int64(len(e.body))}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestExtractTarDetails(t *testing.T) {
	// A leading "./" entry does not stop extraction; modes are read-only,
	// executables keep their x bit; a symlink clashing with a file is skipped
	// without dropping the links after it.
	root := t.TempDir()
	size, err := extractTar(buildTar(t, []tarEntry{
		{name: "./", typ: tar.TypeDir, mode: 0o755},
		{name: "run.sh", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\n"},
		{name: "a.txt", typ: tar.TypeReg, mode: 0o644, body: "a"},
		{name: "a.txt", typ: tar.TypeSymlink, link: "run.sh"},
		{name: "b", typ: tar.TypeSymlink, link: "a.txt"},
	}), root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len("#!/bin/sh\n")+1) {
		t.Errorf("size %d", size)
	}
	for name, want := range map[string]os.FileMode{"run.sh": 0o555, "a.txt": 0o444} {
		fi, err := os.Lstat(filepath.Join(root, name))
		if err != nil || fi.Mode().Perm() != want || !fi.Mode().IsRegular() {
			t.Errorf("%s: %v %v, want regular %v", name, fi.Mode(), err, want)
		}
	}
	if target, err := os.Readlink(filepath.Join(root, "b")); err != nil || target != "a.txt" {
		t.Errorf("symlink after a clashing one not created: %q %v", target, err)
	}

	// A duplicate regular file is an error (never overwritten).
	if _, err := extractTar(buildTar(t, []tarEntry{
		{name: "f", typ: tar.TypeReg, mode: 0o644, body: "1"},
		{name: "f", typ: tar.TypeReg, mode: 0o644, body: "2"},
	}), t.TempDir(), 0); err == nil {
		t.Error("duplicate entry accepted")
	}

	// A corrupt archive is an error.
	bad := buildTar(t, []tarEntry{{name: "f", typ: tar.TypeReg, mode: 0o644, body: "hello"}})
	b := bad.Bytes()
	copy(b[148:156], "xxxxxxxx") // break the header checksum
	if _, err := extractTar(bytes.NewReader(b), t.TempDir(), 0); err == nil {
		t.Error("corrupt archive accepted")
	}

	// The size limit counts regular files: exactly the limit fits.
	if _, err := extractTar(buildTar(t, []tarEntry{{name: "f", typ: tar.TypeReg, mode: 0o644, body: "12345"}}), t.TempDir(), 5); err != nil {
		t.Errorf("exactly the limit: %v", err)
	}
	if _, err := extractTar(buildTar(t, []tarEntry{{name: "f", typ: tar.TypeReg, mode: 0o644, body: "123456"}}), t.TempDir(), 5); !errors.Is(err, ErrSnapshotTooLarge) {
		t.Errorf("over the limit: %v", err)
	}
}

// Variables named by tracing.headers (${NAME}: exporter credentials) never
// reach host git, matched case-insensitively like the Claude runner does.
func TestGitEnvHidesConfiguredVars(t *testing.T) {
	t.Setenv("OTLP_SECRET_FOR_TEST", "otlp-secret-value")
	t.Setenv("REVIEWBOT_HARMLESS", "1")
	m, err := New(Options{BaseDir: t.TempDir(), GitLabURL: t.TempDir(), HideEnv: []string{"otlp_secret_for_test"}})
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(m.git.env(gitCmd{auth: true}), "\n")
	if strings.Contains(env, "otlp-secret-value") {
		t.Error("tracing header variable leaked into git env")
	}
	if !strings.Contains(env, "REVIEWBOT_HARMLESS=1") {
		t.Error("ordinary variables must be kept")
	}
}

package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func ctxDirs(t *testing.T) []core.ContextDir {
	t.Helper()
	a := filepath.Join(t.TempDir(), "billing_api@abc")
	b := filepath.Join(t.TempDir(), "platform_libs@def")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return []core.ContextDir{
		{HostPath: a, Name: "billing/api", SHA: "abc1234", Branch: "main"},
		{HostPath: b, Name: "platform/sub/libs", SHA: "def5678", Branch: "master"},
	}
}

func TestUsableContextDirs(t *testing.T) {
	good := ctxDirs(t)
	in := append([]core.ContextDir{}, good...)
	in = append(in,
		core.ContextDir{HostPath: good[0].HostPath, Name: "../etc"},
		core.ContextDir{HostPath: good[0].HostPath, Name: "a/b:/work"},
		core.ContextDir{HostPath: "relative/path", Name: "x/y"},
		core.ContextDir{HostPath: "/no/such/dir/at/all", Name: "x/z"},
		core.ContextDir{HostPath: good[0].HostPath + ":/work", Name: "x/w"},
		core.ContextDir{HostPath: good[1].HostPath, Name: "Billing/API"}, // duplicate
	)
	got := usableContextDirs(in, nil)
	if len(got) != 2 || got[0].Name != "billing/api" || got[1].Name != "platform/sub/libs" {
		t.Fatalf("got %+v", got)
	}
}

func TestDockerContextMountsReadOnly(t *testing.T) {
	dirs := ctxDirs(t)
	o := dockerOpts()
	spec := reviewSpec(o, "n", "/data/work/abc", "1:1", nil, []string{"-p"})
	spec.ROMounts = contextMounts(dirs)
	args := buildDockerArgs(o, spec)
	for _, d := range dirs {
		want := d.HostPath + ":/repos/" + d.Name + ":ro"
		if !hasPair(args, "-v", want) {
			t.Errorf("missing -v %s in %q", want, args)
		}
	}
	if !hasPair(args, "-v", "/data/work/abc:/work:ro") {
		t.Error("work mount missing")
	}
	// Every mount of a Claude run is read-only, /work included.
	for i, a := range args {
		if a == "-v" && !strings.HasSuffix(args[i+1], ":ro") {
			t.Errorf("writable extra mount %q", args[i+1])
		}
	}
	// Mounts come before the image.
	img := indexOf(args, o.Image)
	for i, a := range args {
		if a == "-v" && i > img {
			t.Errorf("mount after image")
		}
	}
	// Exec containers never get context repos.
	ex := buildDockerArgs(o, execSpec("s", "/w", "1:1", nil, []string{"true"}))
	if strings.Contains(strings.Join(ex, " "), "/repos") {
		t.Error("exec container must not mount context repos")
	}
}

func TestAddDirArgs(t *testing.T) {
	dirs := ctxDirs(t)
	if got := addDirArgs(readOnlyDirs{}, true); got != nil {
		t.Errorf("no dirs -> no flag: %q", got)
	}
	if got := strings.Join(addDirArgs(readOnlyDirs{ctx: dirs}, true), " "); got != "--add-dir /repos" {
		t.Errorf("docker: %q", got)
	}
	if got := strings.Join(addDirArgs(readOnlyDirs{ctx: dirs}, false), " "); got != "--add-dir "+dirs[0].HostPath+" "+dirs[1].HostPath {
		t.Errorf("local: %q", got)
	}
	sk, base := t.TempDir(), t.TempDir()
	if got := strings.Join(addDirArgs(readOnlyDirs{ctx: dirs, skills: sk}, true), " "); got != "--add-dir /repos /skills" {
		t.Errorf("docker + skills: %q", got)
	}
	if got := strings.Join(addDirArgs(readOnlyDirs{skills: sk}, false), " "); got != "--add-dir "+sk {
		t.Errorf("local skills only: %q", got)
	}
	if got := strings.Join(addDirArgs(readOnlyDirs{base: base, ctx: dirs, skills: sk}, true), " "); got != "--add-dir /base /repos /skills" {
		t.Errorf("docker + base: %q", got)
	}
	if got := strings.Join(addDirArgs(readOnlyDirs{base: base}, false), " "); got != "--add-dir "+base {
		t.Errorf("local base: %q", got)
	}
}

func TestSkillsMount(t *testing.T) {
	sk := t.TempDir()
	if usableSkillsDir("", nil) != "" || usableSkillsDir("rel/dir", nil) != "" || usableSkillsDir(sk+"/missing", nil) != "" {
		t.Error("unusable dirs must be dropped")
	}
	if usableSkillsDir(sk, nil) != sk {
		t.Error("usable dir dropped")
	}
	o := Options{Mode: "docker", Image: "img", DockerBin: "docker"}
	spec := reviewSpec(o, "r", "/w", "1:1", nil, []string{"-p"})
	spec.ROMounts = skillsMounts(sk)
	args := strings.Join(buildDockerArgs(o, spec), " ")
	if !strings.Contains(args, sk+":/skills:ro") {
		t.Errorf("skills mount missing or writable: %s", args)
	}
}

// The merge base export: only an absolute, existing directory outside the
// clone is mounted, read-only at /base.
func TestUsableBaseDir(t *testing.T) {
	parent := t.TempDir()
	work := filepath.Join(parent, "clone")
	base := filepath.Join(parent, "clone-tree-x")
	inside := filepath.Join(work, "sub")
	for _, d := range []string{work, base, inside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := usableBaseDir(base, work, nil); got != base {
		t.Errorf("usable dir dropped: %q", got)
	}
	for _, bad := range []string{"", "rel/dir", base + "/missing", work, inside} {
		if got := usableBaseDir(bad, work, nil); got != "" {
			t.Errorf("%q accepted", bad)
		}
	}
	m := readOnlyDirs{base: base}.mounts()
	if len(m) != 1 || m[0].Host != base || m[0].Container != "/base" {
		t.Errorf("mounts %+v", m)
	}
}

func TestRunDockerFakeContextDirs(t *testing.T) {
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	docker := writeScript(t, bin, "docker", `
for a in "$@"; do echo "ARG: $a" >> "`+logf+`"; done
cat >/dev/null
echo '`+fakeResult+`'
`)
	o := dockerOpts()
	o.DockerBin = docker
	r, err := newRunner(o, func() []string { return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"} })
	if err != nil {
		t.Fatal(err)
	}
	dirs := ctxDirs(t)
	if _, err := r.Run(context.Background(), core.RunRequest{Dir: t.TempDir(), Prompt: "x", JSONSchema: "{}", ContextDirs: dirs}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(logf)
	var args []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		args = append(args, strings.TrimPrefix(l, "ARG: "))
	}
	for _, d := range dirs {
		if !hasPair(args, "-v", d.HostPath+":/repos/"+d.Name+":ro") {
			t.Errorf("mount for %s missing", d.Name)
		}
	}
	if n := len(args); args[n-2] != "--add-dir" || args[n-1] != "/repos" {
		t.Errorf("--add-dir must be last: %q", args[n-4:])
	}
	if indexOf(args, "--settings") >= 0 {
		t.Errorf("no Claude Code sandbox settings expected: %q", args)
	}
	// Without context dirs: no mounts under /repos, no --add-dir.
	_ = os.Remove(logf)
	if _, err := r.Run(context.Background(), core.RunRequest{Dir: t.TempDir(), Prompt: "x", JSONSchema: "{}"}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(logf)
	if strings.Contains(string(b), "/repos") || strings.Contains(string(b), "--add-dir") {
		t.Errorf("unexpected context args:\n%s", b)
	}
}

func TestLocalModeContextDirs(t *testing.T) {
	bin := t.TempDir()
	claudeBin := writeScript(t, bin, "claude", `
for a in "$@"; do echo "$a"; done > args.txt
cat >/dev/null
echo '`+fakeResult+`'
`)
	work := t.TempDir()
	r, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"}
	})
	if err != nil {
		t.Fatal(err)
	}
	dirs := ctxDirs(t)
	if _, err := r.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "x", JSONSchema: "{}", ContextDirs: dirs}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(work, "args.txt"))
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	n := len(args)
	if args[n-3] != "--add-dir" || args[n-2] != dirs[0].HostPath || args[n-1] != dirs[1].HostPath {
		t.Errorf("local --add-dir: %q", args[n-4:])
	}
	if strings.Contains(string(b), "/repos") {
		t.Error("local mode must use host paths")
	}
}

// The merge base is mounted read-only at /base next to the clone, and
// passed with --add-dir (docker: the mount root; local: the host path).
func TestRunMergeBaseMount(t *testing.T) {
	bin := t.TempDir()
	logf := filepath.Join(bin, "log")
	docker := writeScript(t, bin, "docker", `
for a in "$@"; do echo "ARG: $a" >> "`+logf+`"; done
cat >/dev/null
echo '`+fakeResult+`'
`)
	o := dockerOpts()
	o.DockerBin = docker
	r, err := newRunner(o, func() []string { return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"} })
	if err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	work, base := filepath.Join(parent, "clone"), filepath.Join(parent, "clone-tree-1")
	for _, d := range []string{work, base} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dirs := ctxDirs(t)
	if _, err := r.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "x", JSONSchema: "{}", BaseDir: base, ContextDirs: dirs}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(logf)
	var args []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		args = append(args, strings.TrimPrefix(l, "ARG: "))
	}
	if !hasPair(args, "-v", base+":/base:ro") || !hasPair(args, "-v", work+":/work:ro") {
		t.Errorf("mounts: %q", args)
	}
	if n := len(args); args[n-3] != "--add-dir" || args[n-2] != "/base" || args[n-1] != "/repos" {
		t.Errorf("--add-dir: %q", args[n-4:])
	}

	// A base dir inside the clone is never mounted.
	_ = os.Remove(logf)
	inside := filepath.Join(work, "base")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "x", JSONSchema: "{}", BaseDir: inside}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(logf); strings.Contains(string(b), "/base") {
		t.Errorf("base inside the clone mounted:\n%s", b)
	}

	// Local mode: the host path.
	claudeBin := writeScript(t, bin, "claude", `
for a in "$@"; do echo "$a"; done > args.txt
cat >/dev/null
echo '`+fakeResult+`'
`)
	lr, err := newRunner(Options{Mode: "local", ClaudeBin: claudeBin}, func() []string {
		return []string{"PATH=" + os.Getenv("PATH"), "ANTHROPIC_API_KEY=k"}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lr.Run(context.Background(), core.RunRequest{Dir: work, Prompt: "x", JSONSchema: "{}", BaseDir: base}); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(work, "args.txt"))
	la := strings.Split(strings.TrimSpace(string(b)), "\n")
	if n := len(la); la[n-2] != "--add-dir" || la[n-1] != base {
		t.Errorf("local --add-dir: %q", la[n-3:])
	}
}

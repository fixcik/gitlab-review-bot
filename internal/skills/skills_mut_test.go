package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadMaxSkills(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i <= MaxSkills; i++ {
		name := fmt.Sprintf("s%03d", i)
		write(t, filepath.Join(dir, name, "SKILL.md"), skillMD(name, "d"))
	}
	list, warns, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != MaxSkills || list[MaxSkills-1].Name != fmt.Sprintf("s%03d", MaxSkills-1) {
		t.Errorf("loaded %d skills", len(list))
	}
	if !strings.Contains(strings.Join(warns, "\n"), fmt.Sprintf("more than %d skills", MaxSkills)) {
		t.Errorf("warnings %q", warns)
	}
}

// Hidden files (.env, .DS_Store) are never copied, and skipping one does not
// skip the rest of the directory.
func TestLoadSkipsHiddenFilesOnly(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "sql", ".env"), "TOKEN=secret")
	write(t, filepath.Join(dir, "sql", "SKILL.md"), skillMD("sql", "SQL"))
	write(t, filepath.Join(dir, "sql", "zz.md"), "z")
	write(t, filepath.Join(dir, "sql", "ref", ".hidden.md"), "h")
	write(t, filepath.Join(dir, "sql", "ref", "x.md"), "x")
	list, warns, err := Load(dir)
	if err != nil || len(list) != 1 {
		t.Fatalf("%+v %q %v", list, warns, err)
	}
	got := keys(list[0].Files)
	want := map[string]bool{"SKILL.md": true, "zz.md": true, "ref/x.md": true}
	if len(got) != len(want) {
		t.Errorf("files %v", got)
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected file %q", k)
		}
	}
}

func TestLoadDepthLimit(t *testing.T) {
	dir := t.TempDir()
	ok := filepath.Join("a", "b", "c", "d", "e", "f") // 6 levels
	write(t, filepath.Join(dir, "deep-ok", "SKILL.md"), skillMD("deep-ok", "d"))
	write(t, filepath.Join(dir, "deep-ok", ok, "x.md"), "x")
	write(t, filepath.Join(dir, "too-deep", "SKILL.md"), skillMD("too-deep", "d"))
	write(t, filepath.Join(dir, "too-deep", ok, "g", "x.md"), "x")
	list, warns, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "deep-ok" {
		t.Fatalf("skills %+v, warnings %q", list, warns)
	}
	if _, ok := list[0].Files["a/b/c/d/e/f/x.md"]; !ok {
		t.Errorf("files %v", keys(list[0].Files))
	}
	if !strings.Contains(strings.Join(warns, "\n"), "too-deep: nested deeper than") {
		t.Errorf("warnings %q", warns)
	}
}

func TestLooseFrontmatterContinuation(t *testing.T) {
	m, err := frontmatter([]byte("---\nname: sql\ndescription: Use when: reviewing SQL\n migrations and\n\tindexes\nother: x\n---\nbody"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "sql" || m.Description != "Use when: reviewing SQL migrations and indexes" {
		t.Errorf("%+v", m)
	}
	if _, err := frontmatter([]byte("---\nfoo: : :\n---\n")); err == nil {
		t.Error("a broken block without known keys must fail")
	}
}

func TestStageRefusesEscapingPaths(t *testing.T) {
	base := t.TempDir()
	dst := filepath.Join(base, "stage")
	for _, p := range []string{"../escape.md", "a/../../escape.md", "../../escape.md"} {
		err := Stage(dst, []Skill{{Name: "ok", Files: map[string][]byte{"SKILL.md": []byte("x"), p: []byte("evil")}}})
		if err == nil {
			t.Errorf("%q: staging must fail", p)
		}
		_ = Unstage(dst)
	}
	for _, p := range []string{filepath.Join(base, "escape.md"), filepath.Join(base, "stage", "escape.md")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("wrote %s", p)
		}
	}
}

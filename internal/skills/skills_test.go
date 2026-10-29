package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skillMD(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n# " + name + "\nBody.\n"
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go-style", "SKILL.md"), skillMD("go-style", "Стиль Go:\n  ошибки и контексты"))
	write(t, filepath.Join(dir, "go-style", "reference", "errors.md"), "wrap errors")
	write(t, filepath.Join(dir, "go-style", ".git", "HEAD"), "ignored")
	write(t, filepath.Join(dir, "migrations", "SKILL.md"), "\xef\xbb\xbf---\r\ndescription: \"Миграции Postgres\"\r\n---\r\nx")
	write(t, filepath.Join(dir, "no-desc", "SKILL.md"), "---\nname: no-desc\n---\n")
	write(t, filepath.Join(dir, "no-md", "README.md"), "x")
	write(t, filepath.Join(dir, "Bad_Name", "SKILL.md"), skillMD("Bad_Name", "x"))
	write(t, filepath.Join(dir, "other-name", "SKILL.md"), skillMD("something-else", "x"))
	write(t, filepath.Join(dir, "no-front", "SKILL.md"), "# just markdown")
	write(t, filepath.Join(dir, "README.md"), "top-level file is ignored")
	write(t, filepath.Join(dir, ".hidden", "SKILL.md"), skillMD("hidden", "x"))
	big := filepath.Join(dir, "huge", "SKILL.md")
	write(t, big, skillMD("huge", "big"))
	write(t, filepath.Join(dir, "huge", "blob.bin"), strings.Repeat("x", MaxSkillBytes))
	if err := os.Symlink("/etc", filepath.Join(dir, "go-style", "etc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "go-style"), filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}

	list, warns, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "go-style" || list[1].Name != "migrations" {
		t.Fatalf("skills %+v\nwarnings %q", list, warns)
	}
	if list[0].Description != "Стиль Go: ошибки и контексты" || list[1].Description != "Миграции Postgres" {
		t.Errorf("descriptions %q / %q", list[0].Description, list[1].Description)
	}
	if _, ok := list[0].Files["reference/errors.md"]; !ok || len(list[0].Files) != 2 {
		t.Errorf("files %v", keys(list[0].Files))
	}
	joined := strings.Join(warns, "\n")
	for _, w := range []string{"no-desc: SKILL.md frontmatter has no description", "no-md: no SKILL.md", "Bad_Name: directory name",
		"other-name: frontmatter name", "no-front: SKILL.md must start", "huge: larger than", "linked: symlinks"} {
		if !strings.Contains(joined, w) {
			t.Errorf("missing warning %q in:\n%s", w, joined)
		}
	}
	if _, _, err := Load(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing dir must fail")
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSelectStageAndPrompt(t *testing.T) {
	all := []Skill{
		{Name: "go-style", Description: "Go", Files: map[string][]byte{"SKILL.md": []byte("a"), "ref/x.md": []byte("b")}},
		{Name: "migrations", Description: "SQL", Files: map[string][]byte{"SKILL.md": []byte("c")}},
	}
	sel := Select(all, []string{" Migrations "})
	if len(sel) != 1 || sel[0].Name != "go-style" || len(Select(all, nil)) != 2 {
		t.Fatalf("select %+v", sel)
	}
	dst := filepath.Join(t.TempDir(), "stage")
	if err := Stage(dst, all); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "go-style", "ref", "x.md")); err != nil || string(b) != "b" {
		t.Fatalf("staged file: %q %v", b, err)
	}
	fi, _ := os.Stat(filepath.Join(dst, "go-style", "SKILL.md"))
	di, _ := os.Stat(filepath.Join(dst, "go-style", "ref"))
	if fi.Mode().Perm() != 0o444 || di.Mode().Perm() != 0o555 {
		t.Errorf("modes %v %v", fi.Mode(), di.Mode())
	}
	if os.Geteuid() != 0 {
		if err := os.WriteFile(filepath.Join(dst, "go-style", "new.md"), nil, 0o644); err == nil {
			t.Error("staged dir must be read-only")
		}
	}
	if err := Unstage(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("unstage left %v", err)
	}

	sec := SystemSection(sel, "/skills")
	if !strings.Contains(sec, "- `go-style` — Go (/skills/go-style/SKILL.md)") || strings.Contains(sec, "migrations") {
		t.Errorf("section:\n%s", sec)
	}
	if SystemSection(nil, "/skills") != "" {
		t.Error("empty list -> empty section")
	}
	used := Used([]string{"/skills/migrations/SKILL.md", "/work/main.go", "/skills/go-style/ref/x.md", "/skills/go-style/SKILL.md", "/skillsx/a/SKILL.md"}, "/skills/")
	if strings.Join(used, ",") != "go-style,migrations" {
		t.Errorf("used %v", used)
	}
}

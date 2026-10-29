package dotenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	got := Parse([]byte(`# comment
A=1
export B = two
C='lit $X # not a comment'
D="q \"x\" \\"
E=plain # inline comment
bad line
1X=no
F=
A=later
`))
	want := map[string]string{"A": "later", "B": "two", "C": "lit $X # not a comment", "D": `q "x" \`, "E": "plain", "F": ""}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestUpdateKeepsForeignLinesAndKeys(t *testing.T) {
	in := "# head\nFOREIGN=keep\nGITLAB_TOKEN=old\n\nGITLAB_TOKEN=dup\nexport X=1"
	got, err := Update([]byte(in), map[string]string{"GITLAB_TOKEN": "new", "Z": "a b", "B": "$HOME", "E": ""})
	if err != nil {
		t.Fatal(err)
	}
	want := "# head\nFOREIGN=keep\nGITLAB_TOKEN=new\n\nexport X=1\nB='$HOME'\nE=\nZ='a b'\n"
	if string(got) != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if p := Parse(got); p["B"] != "$HOME" || p["Z"] != "a b" || p["FOREIGN"] != "keep" || p["X"] != "1" {
		t.Fatalf("round trip: %v", p)
	}
	// An empty file and a file with a trailing newline.
	if got, _ := Update(nil, map[string]string{"A": "1"}); string(got) != "A=1\n" {
		t.Fatalf("empty: %q", got)
	}
	if got, _ := Update([]byte("A=0\n"), map[string]string{"A": "1"}); string(got) != "A=1\n" {
		t.Fatalf("newline: %q", got)
	}
}

func TestUpdateRefuses(t *testing.T) {
	for _, set := range []map[string]string{
		{"A": "x'y"},
		{"A": "x\ny"},
		{"bad-key": "1"},
	} {
		if _, err := Update(nil, set); err == nil {
			t.Errorf("%v: want an error", set)
		}
	}
	// Refused on an existing line too.
	if _, err := Update([]byte("A=1\n"), map[string]string{"A": "it's"}); err == nil {
		t.Error("existing line: want an error")
	}
}

func TestReadWriteFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".env")
	if m, err := Read(p); err != nil || len(m) != 0 {
		t.Fatalf("missing: %v %v", m, err)
	}
	if err := os.WriteFile(p, []byte("A=old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(p, []byte("A=1\n")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != Mode {
		t.Fatalf("mode %v %v", st.Mode(), err)
	}
	if m, err := Read(p); err != nil || m["A"] != "1" {
		t.Fatalf("read: %v %v", m, err)
	}
	// A directory is not readable as a file.
	if _, err := Read(dir); err == nil {
		t.Fatal("dir: want an error")
	}
	// No such directory.
	if err := WriteFile(filepath.Join(dir, "no", ".env"), nil); err == nil {
		t.Fatal("want an error")
	}
	// The target is a directory: the rename fails, the temp file is gone.
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(filepath.Join(sub, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(sub, []byte("A=1\n")); err == nil {
		t.Fatal("rename onto a non-empty dir: want an error")
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".env-") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

package migcheck

import (
	"testing"
)

func TestCleanRepoPathMut(t *testing.T) {
	for p, want := range map[string]bool{
		"src/a.py":       true,
		"a.py":           true,
		"":               false,
		"/src/a.py":      false,
		"src/../a.py":    false,
		"../a.py":        false,
		"./src/a.py":     false,
		"src//a.py":      false,
		"src/a.py/":      false,
		`src\a.py`:       false,
		"src/a\x00.py":   false,
		"src/..":         false,
		"src/..hidden/a": true,
	} {
		if got := CleanRepoPath(p); got != want {
			t.Errorf("cleanRepoPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestSnakeNameMut(t *testing.T) {
	for in, want := range map[string]string{"createdBy": "created_by", "CreatedBy": "created_by", "created_by": "created_by",
		"order2Items": "order2_items", "HTTPCode": "httpcode", "aB": "a_b", "x": "x"} {
		if got := snakeName(in); got != want {
			t.Errorf("snakeName(%q) = %q, want %q", in, got, want)
		}
	}
}

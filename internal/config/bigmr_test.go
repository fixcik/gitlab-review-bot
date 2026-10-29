package config

import (
	"strings"
	"testing"
)

func TestBigMRDefaultsAndValidation(t *testing.T) {
	d := Defaults()
	if d.Review.MaxMRLines != 2000 || !d.Review.BigMR.Deterministic || d.Review.BigMR.OverrideLabel != "big-mr-ok" || d.Review.BigMR.Block {
		t.Fatalf("defaults: %d %+v", d.Review.MaxMRLines, d.Review.BigMR)
	}
	for _, c := range []struct {
		lines int
		label string
		bad   bool
	}{
		{0, "", false}, {2000, "big-mr-ok", false}, {-1, "x", true}, {10, "a,b", true}, {10, " x", true}, {10, strings.Repeat("x", 256), true},
	} {
		r := d.Review
		r.MaxMRLines, r.BigMR.OverrideLabel = c.lines, c.label
		if got := len(r.validateBigMR()) > 0; got != c.bad {
			t.Errorf("%d %q: invalid=%v", c.lines, c.label, got)
		}
	}
}

func TestBigMRRepoOnlyStricter(t *testing.T) {
	svc := Defaults()
	for _, c := range []struct {
		svc   int
		yaml  string
		want  int
		block bool
	}{
		{2000, "", 2000, false},
		{2000, "max_mr_lines: 800\n", 800, false},
		{2000, "max_mr_lines: 5000\n", 2000, false},
		{2000, "max_mr_lines: 0\n", 2000, false},
		{0, "max_mr_lines: 1500\n", 1500, false},
		{2000, "big_mr:\n  block: true\n", 2000, true},
		{2000, "big_mr:\n  block: false\n", 2000, false},
	} {
		svc.Review.MaxMRLines = c.svc
		repo, err := ParseRepo([]byte(c.yaml))
		if err != nil {
			t.Fatal(err)
		}
		e := Merge(&svc, repo).BigMR
		if e.MaxLines != c.want || e.Block != c.block || e.On() != (c.want > 0) {
			t.Errorf("svc %d repo %q: %+v", c.svc, c.yaml, e)
		}
	}
	// The repository cannot turn off a blocking service policy.
	svc.Review.BigMR.Block = true
	repo, _ := ParseRepo([]byte("big_mr:\n  block: false\n"))
	if !Merge(&svc, repo).BigMR.Block {
		t.Error("repo turned block off")
	}
	if _, err := ParseRepo([]byte("max_mr_lines: -5\n")); err == nil {
		t.Error("negative repo max_mr_lines accepted")
	}
}

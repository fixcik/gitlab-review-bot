package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/core"
)

func TestTodoDefaults(t *testing.T) {
	d := Defaults()
	c := d.Review.Todo
	if !c.Enabled || !c.RequireTask || c.Blocking || c.VerifyKeys || c.Severity != "minor" ||
		!reflect.DeepEqual(c.Keywords, []string{"TODO", "FIXME", "XXX", "HACK"}) ||
		!reflect.DeepEqual(c.Ignore, []string{"vendor/**", "node_modules/**", "**/*.min.js", "**/*.min.css"}) {
		t.Fatalf("defaults %+v", c)
	}
	if errs := c.validate(); len(errs) != 0 {
		t.Fatalf("defaults invalid: %v", errs)
	}
	e := Merge(&d, nil)
	if !e.Todo.Enabled || e.Todo.Severity != core.SeverityMinor || e.Todo.Blocking || len(e.Todo.Keywords) != 4 {
		t.Fatalf("effective %+v", e.Todo)
	}
}

func TestTodoValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*ReviewTodo)
		want string
	}{
		{"lowercase keyword", func(c *ReviewTodo) { c.Keywords = []string{"todo"} }, "review.todo.keywords"},
		{"one letter", func(c *ReviewTodo) { c.Keywords = []string{"T"} }, "review.todo.keywords"},
		{"empty when on", func(c *ReviewTodo) { c.Keywords = nil }, "must not be empty"},
		{"severity", func(c *ReviewTodo) { c.Severity = "high" }, "review.todo.severity"},
		{"glob", func(c *ReviewTodo) { c.Ignore = []string{"a/[b"} }, "review.todo.ignore"},
		{"absolute glob", func(c *ReviewTodo) { c.Ignore = []string{"/vendor/**"} }, "review.todo.ignore"},
	} {
		c := defaultReviewTodo()
		tc.edit(&c)
		if errs := strings.Join(c.validate(), "; "); !strings.Contains(errs, tc.want) {
			t.Errorf("%s: %q", tc.name, errs)
		}
	}
	c := defaultReviewTodo()
	c.Enabled, c.Keywords = false, nil
	if errs := c.validate(); len(errs) != 0 {
		t.Errorf("off without keywords is fine: %v", errs)
	}
	svc := Defaults()
	svc.Review.Todo.Severity = "urgent"
	if err := svc.Validate(); err == nil || !strings.Contains(err.Error(), "review.todo.severity") {
		t.Errorf("Validate: %v", err)
	}
}

func TestTodoRepoParse(t *testing.T) {
	r, err := ParseRepo([]byte("todo:\n  enabled: true\n  keywords: [' TODO ', NOCOMMIT]\n  severity: ' Major '\n  blocking: true\n  ignore: ['./legacy/**', '']\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Todo; !reflect.DeepEqual(got.Keywords, []string{"TODO", "NOCOMMIT"}) || got.Severity != "major" ||
		!reflect.DeepEqual(got.Ignore, []string{"legacy/**"}) || got.Blocking == nil || !*got.Blocking {
		t.Fatalf("normalized %+v", got)
	}
	for _, bad := range []string{
		"todo:\n  keywords: [todo]\n",
		"todo:\n  severity: high\n",
		"todo:\n  ignore: ['/abs/**']\n",
	} {
		if _, err := ParseRepo([]byte(bad)); err == nil || !strings.Contains(err.Error(), "todo.") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestTodoMerge(t *testing.T) {
	yes, no := true, false
	svc := Defaults()
	for _, tc := range []struct {
		name  string
		svc   func(*ReviewTodo)
		repo  *RepoTodo
		check func(Todo) bool
	}{
		{"repo disables", nil, &RepoTodo{Enabled: &no}, func(t Todo) bool { return !t.Enabled }},
		{"repo cannot enable", func(c *ReviewTodo) { c.Enabled = false }, &RepoTodo{Enabled: &yes}, func(t Todo) bool { return !t.Enabled }},
		{"keywords replaced", nil, &RepoTodo{Keywords: []string{"NOCOMMIT"}}, func(t Todo) bool {
			return t.Enabled && reflect.DeepEqual(t.Keywords, []string{"NOCOMMIT"})
		}},
		{"severity", nil, &RepoTodo{Severity: "nit"}, func(t Todo) bool { return t.Severity == core.SeverityNit }},
		{"repo makes it blocking", nil, &RepoTodo{Blocking: &yes}, func(t Todo) bool { return t.Blocking }},
		{"repo cannot relax blocking", func(c *ReviewTodo) { c.Blocking = true }, &RepoTodo{Blocking: &no}, func(t Todo) bool { return t.Blocking }},
		{"ignore added", nil, &RepoTodo{Ignore: []string{"legacy/**"}}, func(t Todo) bool {
			return len(t.Ignore) == len(DefaultTodoIgnore)+1 && t.Ignore[0] == "vendor/**" && t.Ignore[len(t.Ignore)-1] == "legacy/**"
		}},
		{"nothing to check", func(c *ReviewTodo) { c.RequireTask = false }, nil, func(t Todo) bool { return !t.Enabled }},
		{"verify only", func(c *ReviewTodo) { c.RequireTask, c.VerifyKeys = false, true }, nil, func(t Todo) bool {
			return t.Enabled && t.VerifyKeys && !t.RequireTask
		}},
	} {
		s := svc
		s.Review.Todo = defaultReviewTodo()
		if tc.svc != nil {
			tc.svc(&s.Review.Todo)
		}
		var repo *Repo
		if tc.repo != nil {
			repo = &Repo{Todo: tc.repo}
		}
		if got := Merge(&s, repo).Todo; !tc.check(got) {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}

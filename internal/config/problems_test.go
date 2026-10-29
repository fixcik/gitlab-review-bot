package config

import (
	"strings"
	"testing"
	"time"
)

func TestProblemsDefaults(t *testing.T) {
	d := Defaults()
	p := d.Review.Problems
	if !p.Enabled || p.Mode() != "summary" || p.RenotifyD() != 168*time.Hour || p.RetentionD() != 90*24*time.Hour ||
		len(p.Notify) != 0 || p.PageURL != "" {
		t.Errorf("defaults %+v", p)
	}
	if errs := p.validate(); len(errs) > 0 {
		t.Errorf("defaults invalid: %v", errs)
	}
	var zero Problems
	if zero.Mode() != "summary" || zero.RenotifyD() != DefaultProblemsRenotify || zero.RetentionD() != DefaultProblemsRetention {
		t.Errorf("zero value fallbacks: %q %v %v", zero.Mode(), zero.RenotifyD(), zero.RetentionD())
	}
}

func TestProblemsValidate(t *testing.T) {
	ok := defaultProblems()
	ok.Notify = []string{"@alice", "bob.smith", "dev_ops-1", " "}
	ok.NotifyMode, ok.PageURL = "note", "https://reviewbot.example.com"
	if errs := ok.validate(); len(errs) > 0 {
		t.Fatalf("valid config rejected: %v", errs)
	}
	if got := strings.Join(ok.NotifyUsers(), ","); got != "alice,bob.smith,dev_ops-1" {
		t.Errorf("users %q", got)
	}
	for _, tc := range []struct {
		name string
		edit func(*Problems)
		want string
	}{
		{"space", func(p *Problems) { p.Notify = []string{"alice bob"} }, "not a GitLab username"},
		{"markup", func(p *Problems) { p.Notify = []string{"<b>x</b>"} }, "not a GitLab username"},
		{"mention injection", func(p *Problems) { p.Notify = []string{"all\n/merge"} }, "not a GitLab username"},
		{"trailing dot", func(p *Problems) { p.Notify = []string{"bob."} }, "not a GitLab username"},
		{"too many", func(p *Problems) {
			for i := 0; i < 21; i++ {
				p.Notify = append(p.Notify, "u"+strings.Repeat("x", i))
			}
		}, "at most 20"},
		{"mode", func(p *Problems) { p.NotifyMode = "email" }, "notify_mode"},
		{"renotify", func(p *Problems) { p.RenotifyAfter = Duration(time.Minute) }, "renotify_after"},
		{"retention", func(p *Problems) { p.Retention = Duration(time.Hour) }, "retention"},
		{"page url scheme", func(p *Problems) { p.PageURL = "javascript:alert(1)" }, "page_url"},
		{"page url creds", func(p *Problems) { p.PageURL = "https://u:p@bot" }, "page_url"},
		{"page url query", func(p *Problems) { p.PageURL = "https://bot/?x=1" }, "page_url"},
	} {
		p := defaultProblems()
		tc.edit(&p)
		errs := strings.Join(p.validate(), "; ")
		if !strings.Contains(errs, tc.want) {
			t.Errorf("%s: errors %q, want %q", tc.name, errs, tc.want)
		}
	}
	// Wired into Service.Validate.
	c := Defaults()
	c.GitLab.URL, c.GitLab.Token, c.GitLab.WebhookSecret = "https://gl", "t", "s"
	c.Review.Problems.NotifyMode = "pager"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "notify_mode") {
		t.Errorf("Validate: %v", err)
	}
}

func TestProblemsRepoSummarySwitch(t *testing.T) {
	svc := Defaults()
	if e := Merge(&svc, nil); !e.ProblemsSummary {
		t.Error("default: block on")
	}
	r, err := ParseRepo([]byte("problems:\n  summary: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if e := Merge(&svc, r); e.ProblemsSummary {
		t.Error("repo summary: false must hide the block")
	}
	on, _ := ParseRepo([]byte("problems:\n  summary: true\n"))
	svc.Review.Problems.Enabled = false
	if e := Merge(&svc, on); e.ProblemsSummary {
		t.Error("a repository cannot turn the block on when the service is off")
	}
}

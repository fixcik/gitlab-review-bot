package config

import (
	"strings"
	"testing"
)

// The MR title suggestion: English by default whatever
// review.default_language is; jira.title_prompt / title_language of the
// service, overridden by the repository's jira section.
func TestTitlePrompt(t *testing.T) {
	svc := Defaults()
	svc.Review.DefaultLang = "ru"
	e := Merge(&svc, nil)
	if e.TitleLanguage != "en" || e.TitlePrompt != "" {
		t.Fatalf("defaults: %q %q", e.TitleLanguage, e.TitlePrompt)
	}
	got := TitleInstructions(e.TitlePrompt, e.TitleLanguage, []string{"feat", "fix"})
	for _, s := range []string{"type(scope): description", "one of: feat, fix", "in English", "imperative", "72 characters", "trailing period",
		"lowercase", "module or subsystem", "not a list of the changed files", "Jira issue key", "emoji"} {
		if !strings.Contains(got, s) {
			t.Errorf("default prompt lacks %q:\n%s", s, got)
		}
	}
	if strings.Contains(got, "{types}") || strings.Contains(got, "{language}") {
		t.Errorf("placeholders left:\n%s", got)
	}

	svc.Jira.TitlePrompt = "Use {types}. Language: {language}. Braces {{types}} keep one pair."
	svc.Jira.TitleLanguage = "ru"
	e = Merge(&svc, nil)
	if got := TitleInstructions(e.TitlePrompt, e.TitleLanguage, []string{"feat"}); got != "Use feat. Language: Russian. Braces {feat} keep one pair." {
		t.Errorf("service prompt: %q", got)
	}
	// The repository overrides the service.
	repo := mustRepo(t, "jira:\n  title_prompt: \"Короткий заголовок: {types}\"\n  title_language: de\n")
	e = Merge(&svc, repo)
	if e.TitlePrompt != "Короткий заголовок: {types}" || e.TitleLanguage != "de" {
		t.Errorf("repo override: %q %q", e.TitlePrompt, e.TitleLanguage)
	}
	if got := TitleInstructions(e.TitlePrompt, e.TitleLanguage, []string{"feat"}); got != "Короткий заголовок: feat" {
		t.Errorf("repo prompt: %q", got)
	}
	// An empty repository value keeps the service one.
	if e = Merge(&svc, mustRepo(t, "jira:\n  project_keys: [PROJ]\n")); e.TitlePrompt != svc.Jira.TitlePrompt || e.TitleLanguage != "ru" {
		t.Errorf("repo without title: %q %q", e.TitlePrompt, e.TitleLanguage)
	}
	if TitleInstructions("", "fr-CA", []string{"feat"}) == "" || !strings.Contains(TitleInstructions("", "fr-CA", nil), "in fr-CA") {
		t.Error("an unknown language code is used as written")
	}

	// Validation: service and repository.
	for _, c := range []struct {
		prompt, lang, want string
	}{
		{strings.Repeat("x", MaxTitlePromptBytes+1), "en", "jira.title_prompt: at most 4000 bytes"},
		{"Use {type}", "en", `jira.title_prompt: unknown placeholder "{type}" (use {types}, {language})`},
		{"bad \x00 byte", "en", "jira.title_prompt: control characters"},
		{"ok", "english!", `jira.title_language: "english!"`},
	} {
		s := mutSvc()
		s.Jira.TitlePrompt, s.Jira.TitleLanguage = c.prompt, c.lang
		wantInvalid(t, c.want, s, c.want)
		if _, err := ParseRepo([]byte("jira:\n  title_prompt: " + yamlQuote(c.prompt) + "\n  title_language: " + yamlQuote(c.lang) + "\n")); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("repo %q: %v", c.want, err)
		}
	}
	s := mutSvc()
	s.Jira.TitleLanguage = ""
	wantValid(t, "empty language = en", s)
	if e := Merge(&s, nil); e.TitleLanguage != "en" {
		t.Errorf("empty language: %q", e.TitleLanguage)
	}
}

func yamlQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\x00", `\0`)
	return `"` + r.Replace(s) + `"`
}

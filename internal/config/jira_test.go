package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GITLAB_URL", "https://gitlab.example.com")
	t.Setenv("GITLAB_TOKEN", "tok")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "sec")
	t.Setenv("JIRA_URL", "")
	t.Setenv("JIRA_TOKEN", "")
}

func TestJiraDefaults(t *testing.T) {
	setBaseEnv(t)
	cfg, err := LoadService("")
	if err != nil {
		t.Fatal(err)
	}
	j := cfg.Jira
	if j.Enabled() || j.URL != "" {
		t.Fatalf("jira must be disabled by default: %+v", j)
	}
	if !j.TitleCheck || !j.RequireKeyInTitle || j.CommentsLimit != 10 || j.EpicLinkField != "customfield_10008" {
		t.Fatalf("defaults: %+v", j)
	}
	if strings.Join(j.TitleTypes, ",") != strings.Join(DefaultTitleTypes, ",") {
		t.Fatalf("types: %v", j.TitleTypes)
	}
}

func TestJiraFileAndEnv(t *testing.T) {
	setBaseEnv(t)
	p := filepath.Join(t.TempDir(), "c.yaml")
	os.WriteFile(p, []byte(`
jira:
  url: https://jira.example.com/
  token: from-file
  project_keys: [proj, " OPS "]
  comments_limit: 3
  title_check: false
  title_types: [Feat, fix]
`), 0o600)
	cfg, err := LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	j := cfg.Jira
	if j.URL != "https://jira.example.com" || j.Token != "from-file" || !j.Enabled() {
		t.Fatalf("url/token: %+v", j)
	}
	if strings.Join(j.ProjectKeys, ",") != "PROJ,OPS" || j.CommentsLimit != 3 || j.TitleCheck {
		t.Fatalf("values: %+v", j)
	}
	if !j.RequireKeyInTitle || j.EpicLinkField != "customfield_10008" {
		t.Fatalf("unset fields lost defaults: %+v", j)
	}
	if strings.Join(j.TitleTypes, ",") != "feat,fix" {
		t.Fatalf("types: %v", j.TitleTypes)
	}

	t.Setenv("JIRA_TOKEN", "from-env")
	t.Setenv("JIRA_URL", "https://jira2.example.com")
	cfg, err = LoadService(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Jira.Token != "from-env" || cfg.Jira.URL != "https://jira2.example.com" {
		t.Fatalf("env override: %+v", cfg.Jira)
	}
}

func TestJiraValidation(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{"url without token", "jira:\n  url: https://jira.example.com\n", "JIRA_TOKEN"},
		{"bad url", "jira:\n  url: jira.example.com\n  token: x\n", "jira.url"},
		{"bad key", "jira:\n  project_keys: [\"1AB\"]\n", "project_keys"},
		{"negative comments", "jira:\n  comments_limit: -1\n", "comments_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			p := filepath.Join(t.TempDir(), "c.yaml")
			os.WriteFile(p, []byte(tc.yaml), 0o600)
			_, err := LoadService(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

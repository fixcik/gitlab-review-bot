package config

import (
	"errors"
	"strings"
	"testing"
)

// The keys of the command-running model (setup, tools, the Bash sandbox
// switch) are refused with a pointer to the reason, in the service config
// and in .reviewbot.yaml, whatever their value.
func TestRemovedServiceKeys(t *testing.T) {
	for _, k := range []string{"GITLAB_URL", "GITLAB_HOOKS_TOKEN", "GITLAB_SERVICE_TOKEN", "JIRA_URL", "JIRA_TOKEN"} {
		t.Setenv(k, "")
	}
	t.Setenv("GITLAB_TOKEN", "glpat-test")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "s")
	for body, key := range map[string]string{
		"gitlab:\n  url: https://g.example.com\nsandbox:\n  setup_timeout: 10m\n":            "sandbox.setup_timeout",
		"gitlab:\n  url: https://g.example.com\nsandbox:\n  allow_unsandboxed_bash: false\n": "sandbox.allow_unsandboxed_bash",
		"gitlab:\n  url: https://g.example.com\nreview:\n  default_tools: []\n":              "review.default_tools",
	} {
		p := writeConfig(t, body)
		_, err := LoadService(p)
		if err == nil || !errors.Is(err, ErrRemovedKey) || !strings.Contains(err.Error(), key) ||
			!strings.Contains(err.Error(), "no longer runs commands") || !strings.Contains(err.Error(), "docs/security.md") {
			t.Errorf("%s: err = %v", key, err)
		}
		if _, err := LoadSandbox(p); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("LoadSandbox %s: err = %v", key, err)
		}
		if _, err := LoadSyncHooks(p); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("LoadSyncHooks %s: err = %v", key, err)
		}
	}
	// Similar names elsewhere are not affected: lint has its own `tools`.
	p := writeConfig(t, "gitlab:\n  url: https://g.example.com\nreview:\n  lint:\n    tools: [gitleaks]\n")
	if _, err := LoadService(p); err != nil && errors.Is(err, ErrRemovedKey) {
		t.Errorf("review.lint.tools refused: %v", err)
	}
}

func TestRemovedRepoKeys(t *testing.T) {
	for body, keys := range map[string][]string{
		"setup:\n  - go mod download\n":          {"line 1: setup"},
		"tools: [\"go test ./...\"]\n":           {"line 1: tools"},
		"language: en\ntools: []\nsetup: null\n": {"line 2: tools", "line 3: setup"},
	} {
		_, err := ParseRepo([]byte(body))
		if err == nil || !errors.Is(err, ErrRemovedKey) ||
			!strings.Contains(err.Error(), ".reviewbot.yaml") || !strings.Contains(err.Error(), RemovedKeyReason) {
			t.Errorf("%q: err = %v", body, err)
			continue
		}
		for _, k := range keys {
			if !strings.Contains(err.Error(), k+": "+RemovedKeyReason) {
				t.Errorf("%q: no %q in %v", body, k, err)
			}
		}
	}
	// compat.commands: the bot no longer runs repository commands.
	for _, body := range []string{"compat:\n  commands: [make compat]\n", "compat:\n  enabled: true\n  commands: []\n"} {
		_, err := ParseRepo([]byte(body))
		if err == nil || !errors.Is(err, ErrRemovedKey) || !strings.Contains(err.Error(), "compat.commands: "+RemovedRepoCommandsReason) {
			t.Errorf("%q: err = %v", body, err)
		}
	}
	// Every removed key is reported, each with its line and own reason,
	// next to the other problems of the file.
	_, err := ParseRepo([]byte("tools: []\ncompat:\n  commands: [x]\nlint:\n  custom: []\n  semgrep_configs: [p/go]\nlanguag: en\n"))
	want := ".reviewbot.yaml: line 1: tools: " + RemovedKeyReason +
		"; line 3: compat.commands: " + RemovedRepoCommandsReason +
		"; line 5: lint.custom: " + LintRemovedHint +
		"; line 6: lint.semgrep_configs: " + LintRemovedHint +
		"; line 7: languag: unknown key (did you mean \"language\"?)"
	if err == nil || err.Error() != want || !errors.Is(err, ErrRemovedKey) {
		t.Errorf("mixed: err = %v\nwant %s", err, want)
	}
	// An unknown key alone is not a removed one.
	if _, err := ParseRepo([]byte("commands: [x]\n")); err == nil || errors.Is(err, ErrRemovedKey) {
		t.Errorf("unknown key: err = %v", err)
	}
	for _, body := range []string{"lint:\n  disable: [gitleaks]\n", "compat:\n  openapi: [a.yml]\n", "instructions: \"setup: x\"\n"} {
		if _, err := ParseRepo([]byte(body)); err != nil {
			t.Errorf("%q refused: %v", body, err)
		}
	}
}

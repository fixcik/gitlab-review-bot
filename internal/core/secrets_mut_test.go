package core

import (
	"slices"
	"testing"
)

func TestSecretEnvClassification(t *testing.T) {
	for _, tc := range []struct {
		name            string
		secret, service bool
	}{
		{"GITLAB_TOKEN", true, true},
		{"gitlab_token", true, true}, // case-insensitive
		{"Gitlab_Webhook_Secret", true, true},
		{"MAP_TOKEN", true, true},
		{"CLAUDE_CODE_OAUTH_TOKEN", true, false}, // Claude gets its own credential
		{"anthropic_api_key", true, false},
		{"PATH", false, false},
		{"GITLAB_TOKEN_X", false, false}, // exact names only
		{"", false, false},
	} {
		if got := IsSecretEnv(tc.name); got != tc.secret {
			t.Errorf("IsSecretEnv(%q) = %v", tc.name, got)
		}
		if got := IsServiceSecretEnv(tc.name); got != tc.service {
			t.Errorf("IsServiceSecretEnv(%q) = %v", tc.name, got)
		}
	}
}

func TestSecretEnvListsAreCopies(t *testing.T) {
	all := SecretEnv()
	for _, n := range append(ClaudeAuthEnv(), ServiceSecretEnv()...) {
		if !slices.Contains(all, n) {
			t.Errorf("SecretEnv() misses %s", n)
		}
	}
	if len(all) != len(ClaudeAuthEnv())+len(ServiceSecretEnv()) {
		t.Errorf("SecretEnv() = %v", all)
	}
	// Callers mutating the returned slices must not change the shared lists.
	s := ServiceSecretEnv()
	s[0] = "PATH"
	c := ClaudeAuthEnv()
	c[0] = "HOME"
	all[0], all[len(all)-1] = "X", "Y"
	if !IsServiceSecretEnv("GITLAB_TOKEN") || IsSecretEnv("PATH") || IsSecretEnv("HOME") ||
		!IsSecretEnv("CLAUDE_CODE_OAUTH_TOKEN") || !IsSecretEnv("MAP_TOKEN") {
		t.Fatal("shared secret lists were modified through a returned slice")
	}
}

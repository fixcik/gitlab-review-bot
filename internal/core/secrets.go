package core

import "strings"

// Secret environment variables. This is the single list every package uses
// to keep secrets out of child processes (git on the host, setup/exec
// containers, Claude), so the lists cannot drift apart.

// serviceSecretEnv are the service's own secrets (GitLab, Jira, page
// tokens): they never reach any child process, Claude included.
var serviceSecretEnv = []string{
	"GITLAB_TOKEN", "GITLAB_WEBHOOK_SECRET", "GITLAB_SERVICE_TOKEN", "GITLAB_HOOKS_TOKEN",
	"JIRA_TOKEN", "USAGE_TOKEN", "DIAG_TOKEN", "METRICS_TOKEN", "MAP_TOKEN",
	// Trace exporter credentials (tracing.headers, OTel convention).
	"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
}

// claudeAuthEnv are the Claude credentials: only the Claude process itself
// gets them (by name), never setup/exec commands or host-side git.
var claudeAuthEnv = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"}

// ServiceSecretEnv returns the names of the service's own secret variables.
func ServiceSecretEnv() []string { return append([]string(nil), serviceSecretEnv...) }

// ClaudeAuthEnv returns the names of the Claude credential variables.
func ClaudeAuthEnv() []string { return append([]string(nil), claudeAuthEnv...) }

// SecretEnv returns every secret variable name (Claude credentials first).
func SecretEnv() []string {
	return append(ClaudeAuthEnv(), serviceSecretEnv...)
}

// IsServiceSecretEnv reports whether name (case-insensitive) is one of the
// service's own secrets.
func IsServiceSecretEnv(name string) bool { return inFold(serviceSecretEnv, name) }

// IsSecretEnv reports whether name (case-insensitive) is any secret: a
// service secret or a Claude credential.
func IsSecretEnv(name string) bool {
	return inFold(claudeAuthEnv, name) || inFold(serviceSecretEnv, name)
}

func inFold(list []string, name string) bool {
	for _, s := range list {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

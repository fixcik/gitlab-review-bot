package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/hooksync"
)

func TestRunSyncHooksEndToEnd(t *testing.T) {
	var (
		mu     sync.Mutex
		writes []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "glpat-hooks-token" {
			t.Errorf("sync-hooks must use the service token, got %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v4/groups/team%2Fsub/projects":
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "path_with_namespace": "team/sub/a"},
				{"id": 2, "path_with_namespace": "team/sub/b"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/1/hooks":
			json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/2/hooks":
			http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/projects/1/hooks":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			writes = append(writes, string(b))
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 5})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			http.Error(w, "nope", http.StatusTeapot)
		}
	}))
	defer srv.Close()

	t.Setenv("GITLAB_URL", srv.URL)
	// sync-hooks needs only the service token and the webhook secret: no
	// bot token, and the rest of the config (sandbox, ...) is not validated.
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("GITLAB_SERVICE_TOKEN", "")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "hook-secret-123")
	t.Setenv("GITLAB_HOOKS_TOKEN", "glpat-hooks-token")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfgPath, []byte("gitlab:\n  groups: [\"team/sub\"]\n  webhook_url: https://bot.example/webhook\nsandbox:\n  mode: bogus\n"), 0o600)

	// Dry run: no writes, skipped project is not a failure.
	if err := runSyncHooks(context.Background(), []string{"-config", cfgPath, "-dry-run"}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	mu.Lock()
	n := len(writes)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("dry run wrote %d hooks", n)
	}

	if err := runSyncHooks(context.Background(), []string{"-config", cfgPath}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(writes) != 1 || !strings.Contains(writes[0], `"token":"hook-secret-123"`) || !strings.Contains(writes[0], `"push_events":false`) {
		t.Fatalf("writes %q", writes)
	}
}

func TestRunSyncHooksRequiresConfig(t *testing.T) {
	t.Setenv("GITLAB_URL", "https://g.example")
	t.Setenv("GITLAB_TOKEN", "glpat-test-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "s")
	t.Setenv("GITLAB_SERVICE_TOKEN", "glpat-svc")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfgPath, []byte("listen: \":1\"\n"), 0o600)
	err := runSyncHooks(context.Background(), []string{"-config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "gitlab.webhook_url") || !strings.Contains(err.Error(), "gitlab.groups") {
		t.Fatalf("got %v", err)
	}
}

// The service token and the webhook secret (set on every hook) are required.
func TestRunSyncHooksRequiresServiceTokenAndSecret(t *testing.T) {
	t.Setenv("GITLAB_URL", "https://g.example")
	t.Setenv("GITLAB_TOKEN", "glpat-test-token")
	t.Setenv("GITLAB_WEBHOOK_SECRET", "")
	t.Setenv("GITLAB_SERVICE_TOKEN", "")
	t.Setenv("GITLAB_HOOKS_TOKEN", "")
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(cfgPath, []byte("gitlab:\n  groups: [team]\n  webhook_url: https://bot.example/webhook\n"), 0o600)
	err := runSyncHooks(context.Background(), []string{"-config", cfgPath})
	if err == nil || !strings.Contains(err.Error(), "GITLAB_SERVICE_TOKEN") || !strings.Contains(err.Error(), "GITLAB_WEBHOOK_SECRET") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "GITLAB_TOKEN is required") {
		t.Fatalf("sync-hooks must not require the bot token: %v", err)
	}
}

func TestPrintSyncReport(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitLab.WebhookURL = "https://bot/webhook"
	cfg.GitLab.Groups = []string{"a", "b/c"}
	var buf bytes.Buffer
	printSyncReport(&buf, &cfg, true, hooksync.Report{Projects: 3, Created: 1, Skipped: 1, Unchanged: 1, Errors: []string{"b/c/x: пропущен"}})
	out := buf.String()
	for _, want := range []string{"dry-run", "будет создано", "a, b/c", "b/c/x: пропущен",
		"пользователю служебного токена (GITLAB_SERVICE_TOKEN) роль Maintainer или Owner", "Бот-аккаунту (GITLAB_TOKEN) достаточно Reporter"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "выдайте боту") {
		t.Errorf("the bot account must stay Reporter:\n%s", out)
	}
}

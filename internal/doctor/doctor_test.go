package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// fakeGitLab answers the read-only endpoints doctor uses; tokens map to
// users, the bot is Reporter in "backend", the service user Maintainer.
func fakeGitLab(t *testing.T, mut func(path string, w http.ResponseWriter) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("doctor wrote to GitLab: %s %s", r.Method, r.URL.Path)
		}
		if mut != nil && mut(r.URL.Path, w) {
			return
		}
		tok := r.Header.Get("PRIVATE-TOKEN")
		user := map[string]any{"bot-tok": map[string]any{"id": 7, "username": "reviewbot"}, "svc-tok": map[string]any{"id": 8, "username": "maint"}}[tok]
		if user == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.EscapedPath() {
		case "/api/v4/version":
			json.NewEncoder(w).Encode(map[string]string{"version": "17.5.1-ee"})
		case "/api/v4/user":
			json.NewEncoder(w).Encode(user)
		case "/api/v4/personal_access_tokens/self":
			exp := "2027-01-01"
			if tok == "svc-tok" {
				exp = "2026-11-02" // expires in 4 days
			}
			json.NewEncoder(w).Encode(map[string]any{"scopes": []string{"api"}, "active": true, "expires_at": exp})
		case "/api/v4/groups/backend":
			json.NewEncoder(w).Encode(map[string]any{"id": 5, "full_path": "backend"})
		case "/api/v4/groups/5/members/all/7":
			json.NewEncoder(w).Encode(map[string]any{"access_level": 20})
		case "/api/v4/groups/5/members/all/8":
			json.NewEncoder(w).Encode(map[string]any{"access_level": 40})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func byID(r Report) map[string]Result {
	m := map[string]Result{}
	for _, x := range r.Results {
		m[x.ID] = x
	}
	return m
}

func TestGitLabHealthy(t *testing.T) {
	srv := fakeGitLab(t, nil)
	in := GitLabInput{URL: srv.URL + "/", Groups: []string{"backend"}, BotToken: "bot-tok", ServiceToken: "svc-tok", Events: "webhook",
		WebhookURL: "https://bot.example.com/webhook", WebhookSecret: "s", Now: func() time.Time { return t0 }}
	r := Run(context.Background(), t0, GitLab(in))
	m := byID(r)
	for id, want := range map[string]Status{"gitlab.version": OK, "gitlab.bot_token": OK, "gitlab.bot_role.backend": OK,
		"gitlab.service_token": Warn, "gitlab.service_role.backend": OK, "gitlab.events": OK} {
		if m[id].Status != want {
			t.Errorf("%s: %+v, want %s", id, m[id], want)
		}
	}
	if !strings.Contains(m["gitlab.version"].Detail, "17.5.1") || !strings.Contains(m["gitlab.bot_token"].Detail, "reviewbot") ||
		!strings.Contains(m["gitlab.service_token"].Detail, "истекает") {
		t.Errorf("details: %+v", m)
	}
	out, _ := json.Marshal(r)
	if strings.Contains(string(out), "bot-tok") || strings.Contains(string(out), "svc-tok") {
		t.Fatal("a token leaked into the report")
	}
	if r.Fails != 0 || r.ExitCode() != 0 {
		t.Errorf("fails %d", r.Fails)
	}
}

func TestGitLabProblems(t *testing.T) {
	// The bot is Developer (above Reporter), its token lacks scope api, the
	// group does not exist for the service token, no service token at all
	// in the second run.
	srv := fakeGitLab(t, func(p string, w http.ResponseWriter) bool {
		switch p {
		case "/api/v4/groups/5/members/all/7":
			json.NewEncoder(w).Encode(map[string]any{"access_level": 30})
			return true
		case "/api/v4/personal_access_tokens/self":
			json.NewEncoder(w).Encode(map[string]any{"scopes": []string{"read_api"}, "active": true})
			return true
		}
		return false
	})
	in := GitLabInput{URL: srv.URL, Groups: []string{"backend", "ghost"}, BotToken: "bot-tok", Events: "webhook", Now: func() time.Time { return t0 }}
	m := byID(Run(context.Background(), t0, GitLab(in)))
	for id, want := range map[string]Status{"gitlab.bot_token": Fail, "gitlab.bot_role.backend": Warn, "gitlab.bot_role.ghost": Fail,
		"gitlab.service_token": Warn, "gitlab.events": Fail} {
		if m[id].Status != want || m[id].Action == "" && want != OK {
			t.Errorf("%s: %+v, want %s with an action", id, m[id], want)
		}
	}
	if !strings.Contains(m["gitlab.bot_role.backend"].Detail, "выше Reporter") || !strings.Contains(m["gitlab.events"].Detail, "GITLAB_WEBHOOK_SECRET") {
		t.Errorf("details: %+v", m)
	}

	// A wrong token, an unreachable GitLab, no token at all.
	m = byID(Run(context.Background(), t0, GitLab(GitLabInput{URL: srv.URL, BotToken: "nope", Groups: []string{"backend"}, Events: "poll"})))
	if m["gitlab.bot_token"].Status != Fail || !strings.Contains(m["gitlab.bot_token"].Detail, "не принят") || m["gitlab.events"].Status != OK {
		t.Errorf("wrong token: %+v", m)
	}
	m = byID(Run(context.Background(), t0, GitLab(GitLabInput{URL: "http://127.0.0.1:1", BotToken: "x", Events: "poll"})))
	if m["gitlab.version"].Status != Fail || !strings.Contains(m["gitlab.version"].Action, "адрес") || m["gitlab.bot_token"].Status != Skip {
		t.Errorf("unreachable: %+v", m)
	}
	m = byID(Run(context.Background(), t0, GitLab(GitLabInput{URL: srv.URL, Events: "poll"})))
	if m["gitlab.bot_token"].Status != Fail || !strings.Contains(m["gitlab.bot_token"].Detail, "GITLAB_TOKEN") {
		t.Errorf("no token: %+v", m)
	}
	m = byID(Run(context.Background(), t0, GitLab(GitLabInput{Events: "poll"})))
	if m["gitlab.version"].Status != Fail {
		t.Errorf("no url: %+v", m)
	}
}

type fakeExec map[string]struct {
	out string
	err error
}

func (f fakeExec) run(_ context.Context, name string, args ...string) ([]byte, error) {
	k := name + " " + strings.Join(args, " ")
	for prefix, r := range f {
		if strings.HasPrefix(k, prefix) {
			return []byte(r.out), r.err
		}
	}
	return nil, errors.New("unexpected " + k)
}

func TestDocker(t *testing.T) {
	ok := fakeExec{"docker version": {out: "27.3.1\n"}, "docker image inspect": {out: "sha256:abc\n"}, "docker network inspect": {out: "[]"}}
	m := byID(Run(context.Background(), t0, Docker(DockerInput{Image: "ghcr.io/fixcik/gitlab-review-bot-sandbox:latest", Network: "reviewbot-sandbox", Exec: ok.run, GOOS: "darwin"})))
	if m["docker.access"].Status != OK || m["docker.sandbox_image"].Status != OK || m["docker.sandbox_network"].Status != OK {
		t.Errorf("healthy: %+v", m)
	}
	// No daemon access on Linux: the hint names DOCKER_GID; then nothing else runs.
	no := fakeExec{"docker version": {out: "permission denied while trying to connect", err: errors.New("exit 1")}}
	m = byID(Run(context.Background(), t0, Docker(DockerInput{Image: "x", Network: "n", Exec: no.run, GOOS: "linux"})))
	if m["docker.access"].Status != Fail || !strings.Contains(m["docker.access"].Action, "DOCKER_GID") || m["docker.sandbox_image"].Status != Skip {
		t.Errorf("no access: %+v", m)
	}
	// The image is missing: pull is offered; an unauthorized registry says
	// how to log in; a missing network is only a warning.
	missing := fakeExec{"docker version": {out: "27"}, "docker image inspect": {err: errors.New("no such image")}, "docker manifest inspect": {out: "{}"},
		"docker network inspect": {err: errors.New("no such network")}}
	m = byID(Run(context.Background(), t0, Docker(DockerInput{Image: "ghcr.io/fixcik/gitlab-review-bot-sandbox:latest", Network: "n", Exec: missing.run, GOOS: "linux"})))
	if m["docker.sandbox_image"].Status != Fail || !strings.Contains(m["docker.sandbox_image"].Action, "docker compose pull") || m["docker.sandbox_network"].Status != Warn {
		t.Errorf("missing: %+v", m)
	}
	denied := fakeExec{"docker version": {out: "27"}, "docker image inspect": {err: errors.New("no such image")},
		"docker manifest inspect": {out: "unauthorized: authentication required", err: errors.New("exit 1")}, "docker network inspect": {out: "[]"}}
	m = byID(Run(context.Background(), t0, Docker(DockerInput{Image: "ghcr.io/fixcik/gitlab-review-bot-sandbox:latest", Network: "n", Exec: denied.run, GOOS: "linux"})))
	if a := m["docker.sandbox_image"].Action; !strings.Contains(a, "docker login ghcr.io") || !strings.Contains(a, "read:packages") {
		t.Errorf("unauthorized: %+v", m["docker.sandbox_image"])
	}
}

func TestClaude(t *testing.T) {
	for _, c := range []struct {
		in   ClaudeInput
		want Status
	}{
		{ClaudeInput{OAuthToken: "sk-ant-oat01-xyz"}, OK},
		{ClaudeInput{APIKey: "sk-ant-api03-xyz"}, OK},
		{ClaudeInput{APIKey: "hello"}, Warn},
		{ClaudeInput{}, Fail},
	} {
		r := Run(context.Background(), t0, Claude(c.in))
		if got := r.Results[0]; got.Status != c.want || strings.Contains(got.Detail, "xyz") || strings.Contains(got.Detail, "hello") {
			t.Errorf("%+v: %+v", c.in, got)
		}
	}
}

func TestEgress(t *testing.T) {
	m := byID(Run(context.Background(), t0, Egress(EgressInput{Proxy: "http://egress:3128", Allow: []string{"statsig.anthropic.com"},
		Dial: func(context.Context, string) (string, error) { return "HTTP/1.1 200 Connection established", nil }})))
	if m["egress.allow"].Status != Fail || m["egress.proxy"].Status != OK {
		t.Errorf("allow: %+v", m)
	}
	m = byID(Run(context.Background(), t0, Egress(EgressInput{Proxy: "http://egress:3128", Allow: []string{"api.anthropic.com"},
		Dial: func(context.Context, string) (string, error) { return "", errors.New("no such host") }})))
	if m["egress.proxy"].Status != Warn || !strings.Contains(m["egress.proxy"].Action, "docker compose exec reviewbot reviewbot doctor") {
		t.Errorf("unreachable: %+v", m)
	}
	m = byID(Run(context.Background(), t0, Egress(EgressInput{Proxy: "http://egress:3128", Allow: []string{"api.anthropic.com"},
		Dial: func(context.Context, string) (string, error) { return "HTTP/1.1 403 Forbidden", nil }})))
	if m["egress.proxy"].Status != Fail {
		t.Errorf("refused: %+v", m)
	}
	if r := Run(context.Background(), t0, Egress(EgressInput{})); r.Results[0].Status != Skip {
		t.Errorf("no proxy: %+v", r.Results)
	}
}

func TestReportText(t *testing.T) {
	r := Run(context.Background(), t0, Claude(ClaudeInput{}), Claude(ClaudeInput{OAuthToken: "sk-ant-oat01-x"}))
	var b bytes.Buffer
	r.WriteText(&b)
	s := b.String()
	for _, want := range []string{"✗ Claude: токен", "→ ", "✓ Claude: токен", "Итог: 1 ошибка"} {
		if !strings.Contains(s, want) {
			t.Errorf("text lacks %q:\n%s", want, s)
		}
	}
	if r.ExitCode() != 1 {
		t.Error("a failure exits 1")
	}
}

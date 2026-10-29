package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoleNamesAndLevels(t *testing.T) {
	for lvl, want := range map[int]string{50: "Owner", 40: "Maintainer", 30: "Developer", 20: "Reporter", 10: "Guest", 0: "нет доступа"} {
		if roleName(lvl) != want {
			t.Errorf("%d: %s", lvl, roleName(lvl))
		}
	}
	for _, c := range []struct {
		level   int
		service bool
		want    Status
	}{{10, false, Fail}, {50, false, Warn}, {30, true, Warn}, {50, true, OK}} {
		srv := fakeGitLab(t, func(p string, w http.ResponseWriter) bool {
			if strings.HasPrefix(p, "/api/v4/groups/5/members/all/") {
				json.NewEncoder(w).Encode(map[string]any{"access_level": c.level})
				return true
			}
			return false
		})
		r := roleCheck(context.Background(), api{base: srv.URL, http: http.DefaultClient}, "bot-tok", 7, "backend", "x", "t", c.service)
		if r.Status != c.want {
			t.Errorf("%+v: %+v", c, r)
		}
	}
}

func TestNotAMember(t *testing.T) {
	srv := fakeGitLab(t, func(p string, w http.ResponseWriter) bool {
		if strings.HasPrefix(p, "/api/v4/groups/5/members/all/") {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		return false
	})
	a := api{base: srv.URL, http: http.DefaultClient}
	if r := roleCheck(context.Background(), a, "bot-tok", 7, "backend", "x", "t", false); r.Status != Fail || !strings.Contains(r.Action, "Reporter") {
		t.Errorf("bot: %+v", r)
	}
	if r := roleCheck(context.Background(), a, "svc-tok", 8, "backend", "x", "t", true); r.Status != Fail || !strings.Contains(r.Action, "Maintainer") {
		t.Errorf("service: %+v", r)
	}
}

func TestGitLabEdgeCases(t *testing.T) {
	// /user fails with 500, the token endpoint is missing (old GitLab).
	srv := fakeGitLab(t, func(p string, w http.ResponseWriter) bool {
		switch p {
		case "/api/v4/personal_access_tokens/self":
			w.WriteHeader(http.StatusNotFound)
			return true
		case "/api/v4/version":
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		return false
	})
	m := byID(Run(context.Background(), t0, GitLab(GitLabInput{URL: srv.URL, BotToken: "bot-tok", Events: "both", WebhookSecret: "s"})))
	if m["gitlab.version"].Status != OK || !strings.Contains(m["gitlab.bot_token"].Detail, "scope не проверен") || m["gitlab.events"].Status != Warn {
		t.Errorf("old gitlab: %+v", m)
	}
	srv2 := fakeGitLab(t, func(p string, w http.ResponseWriter) bool {
		if p == "/api/v4/user" {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		return false
	})
	m = byID(Run(context.Background(), t0, GitLab(GitLabInput{URL: srv2.URL, BotToken: "bot-tok", ServiceToken: "svc-tok", Events: "carrier-pigeon"})))
	if m["gitlab.bot_token"].Status != Fail || !strings.Contains(m["gitlab.bot_token"].Detail, "не удалось прочитать") || m["gitlab.events"].Status != Fail {
		t.Errorf("errors: %+v", m)
	}
	if _, err := (api{base: "http://x\n", http: http.DefaultClient}).get(context.Background(), "", "/version", nil); err == nil {
		t.Error("a bad URL must fail")
	}
	if eventsCheck(GitLabInput{Events: "", WebhookSecret: "s", WebhookURL: "https://x/webhook"}).Status != OK {
		t.Error("the default events mode is webhook")
	}
}

func TestDockerMore(t *testing.T) {
	no := fakeExec{"docker version": {out: "Cannot connect", err: errors.New("exit 1")}}
	m := byID(Run(context.Background(), t0, Docker(DockerInput{Image: "x", Network: "n", Exec: no.run, GOOS: "darwin"})))
	if !strings.Contains(m["docker.access"].Action, "Docker Desktop") {
		t.Errorf("darwin: %+v", m["docker.access"])
	}
	gone := fakeExec{"docker version": {out: "27"}, "docker image inspect": {err: errors.New("no")}, "docker manifest inspect": {out: "manifest unknown", err: errors.New("exit 1")},
		"docker network inspect": {out: "[]"}}
	m = byID(Run(context.Background(), t0, Docker(DockerInput{Image: "local/sandbox:dev", Network: "n", Exec: gone.run})))
	if !strings.Contains(m["docker.sandbox_image"].Action, "docker compose build") {
		t.Errorf("unknown image: %+v", m["docker.sandbox_image"])
	}
	if out, err := osExec(context.Background(), "echo", "hi"); err != nil || strings.TrimSpace(string(out)) != "hi" {
		t.Errorf("osExec: %q %v", out, err)
	}
	// The default runner is os/exec (a cancelled context: nothing runs).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := Docker(DockerInput{Image: "x", Network: "n"}).Check(ctx); r[0].Status != Fail {
		t.Errorf("default exec: %+v", r[0])
	}
}

func TestDialConnect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		buf := make([]byte, 512)
		n, _ := c.Read(buf)
		if strings.HasPrefix(string(buf[:n]), "CONNECT api.anthropic.com:443 ") {
			c.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		}
		c.Close()
	}()
	if line, err := dialConnect(context.Background(), ln.Addr().String()); err != nil || !strings.Contains(line, " 200") {
		t.Fatalf("connect: %q %v", line, err)
	}
	if _, err := dialConnect(context.Background(), "127.0.0.1:1"); err == nil {
		t.Error("a closed port must fail")
	}
	// The default dialer is used when none is given (proxy unreachable).
	m := byID(Run(context.Background(), t0, Egress(EgressInput{Proxy: "127.0.0.1:1", Allow: []string{"api.anthropic.com"}})))
	if m["egress.proxy"].Status != Warn {
		t.Errorf("default dial: %+v", m["egress.proxy"])
	}
}

func TestTextHelpers(t *testing.T) {
	r := Report{Fails: 2, Warns: 5}
	var b bytes.Buffer
	r.WriteText(&b)
	if !strings.Contains(b.String(), "2 ошибки, 5 предупреждений") {
		t.Errorf("plural: %s", b.String())
	}
	if s := oneLine(strings.Repeat("я ", 300)); len([]rune(s)) != 201 {
		t.Errorf("oneLine: %d", len([]rune(s)))
	}
}

func TestGitLabCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"version":"17.5"}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	check := func(caFile string) Result {
		return byID(Run(context.Background(), t0, GitLab(GitLabInput{URL: srv.URL, CAFile: caFile, Events: "poll"})))["gitlab.version"]
	}
	if r := check(""); r.Status != Fail || !strings.Contains(r.Detail, "недоступен") {
		t.Fatalf("no CA: %+v", r)
	}
	if r := check(ca); r.Status != OK || !strings.Contains(r.Detail, "17.5") {
		t.Fatalf("with CA: %+v", r)
	}
	bad := filepath.Join(dir, "bad.pem")
	os.WriteFile(bad, []byte("not pem"), 0o600)
	for _, f := range []string{bad, filepath.Join(dir, "missing.pem")} {
		if r := check(f); r.Status != Fail || !strings.Contains(r.Detail, "gitlab.ca_file") {
			t.Fatalf("%s: %+v", f, r)
		}
	}
}

// Pull: only a missing image is pulled; a pulled image is OK; a failed
// pull keeps the reason and the hint. Without Pull nothing changes state
// and the hint names `reviewbot doctor --pull`.
func TestDockerPull(t *testing.T) {
	const img = "ghcr.io/fixcik/gitlab-review-bot-sandbox:latest"
	var pulled []string
	pull := func(err error) func(context.Context, string) error {
		return func(_ context.Context, image string) error { pulled = append(pulled, image); return err }
	}
	present := fakeExec{"docker version": {out: "27"}, "docker image inspect": {out: "sha256:x"}, "docker network inspect": {out: "[]"}}
	m := byID(Run(context.Background(), t0, Docker(DockerInput{Image: img, Network: "n", Exec: present.run, Pull: pull(nil)})))
	if len(pulled) != 0 || m["docker.sandbox_image"].Status != OK {
		t.Fatalf("present: pulled %v, %+v", pulled, m["docker.sandbox_image"])
	}
	missing := fakeExec{"docker version": {out: "27"}, "docker image inspect": {err: errors.New("no such image")}, "docker manifest inspect": {out: "{}"},
		"docker network inspect": {out: "[]"}}
	m = byID(Run(context.Background(), t0, Docker(DockerInput{Image: img, Network: "n", Exec: missing.run})))
	if r := m["docker.sandbox_image"]; r.Status != Fail || !strings.Contains(r.Action, "reviewbot doctor --pull") {
		t.Fatalf("no pull: %+v", r)
	}
	m = byID(Run(context.Background(), t0, Docker(DockerInput{Image: img, Network: "n", Exec: missing.run, Pull: pull(nil)})))
	if r := m["docker.sandbox_image"]; len(pulled) != 1 || pulled[0] != img || r.Status != OK || !strings.Contains(r.Detail, "скачан") || r.Action != "" {
		t.Fatalf("pulled: %v %+v", pulled, r)
	}
	denied := fakeExec{"docker version": {out: "27"}, "docker image inspect": {err: errors.New("no such image")},
		"docker manifest inspect": {out: "unauthorized", err: errors.New("exit 1")}, "docker network inspect": {out: "[]"}}
	m = byID(Run(context.Background(), t0, Docker(DockerInput{Image: img, Network: "n", Exec: denied.run, Pull: pull(errors.New("exit status 1"))})))
	if r := m["docker.sandbox_image"]; r.Status != Fail || !strings.Contains(r.Detail, "docker pull не удался") || !strings.Contains(r.Action, "docker login ghcr.io") {
		t.Fatalf("pull failed: %+v", r)
	}
}

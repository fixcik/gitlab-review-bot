package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/doctor"
	"github.com/fixcik/gitlab-review-bot/internal/initwiz"
)

func clearSecretsEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GITLAB_URL", "GITLAB_TOKEN", "GITLAB_WEBHOOK_SECRET", "GITLAB_SERVICE_TOKEN", "GITLAB_HOOKS_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func fakeDocker(_ context.Context, name string, args ...string) ([]byte, error) {
	k := name + " " + strings.Join(args, " ")
	if strings.HasPrefix(k, "docker version") {
		return []byte("27.3.1"), nil
	}
	return nil, nil
}

func TestRunDoctor(t *testing.T) {
	clearSecretsEnv(t)
	gl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("doctor wrote: %s", r.Method)
		}
		switch {
		case r.Header.Get("PRIVATE-TOKEN") != "glpat-SECRET":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/api/v4/user":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "reviewbot"})
		case r.URL.Path == "/api/v4/personal_access_tokens/self":
			json.NewEncoder(w).Encode(map[string]any{"scopes": []string{"api"}})
		case r.URL.Path == "/api/v4/groups/backend":
			json.NewEncoder(w).Encode(map[string]any{"id": 5})
		case r.URL.Path == "/api/v4/groups/5/members/all/7":
			json.NewEncoder(w).Encode(map[string]any{"access_level": 20})
		default:
			json.NewEncoder(w).Encode(map[string]any{"version": "17.5"})
		}
	}))
	defer gl.Close()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	envPath := filepath.Join(dir, ".env")
	os.WriteFile(cfgPath, []byte("gitlab:\n  url: \""+gl.URL+"\"\n  groups: [\"backend\"]\n  events: \"poll\"\nsandbox:\n  egress_proxy: \"http://egress:3128\"\n"), 0o644)
	os.WriteFile(envPath, []byte("GITLAB_TOKEN=glpat-SECRET\nCLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-SECRET\n"), 0o600)
	deps := doctorDeps{exec: fakeDocker, dial: func(context.Context, string) (string, error) { return "HTTP/1.1 200 Connection established", nil }}

	var out, errb bytes.Buffer
	code := runDoctor(context.Background(), []string{"-config", cfgPath, "-env", envPath}, &out, &errb, deps)
	if code != 0 {
		t.Fatalf("code %d\n%s%s", code, out.String(), errb.String())
	}
	o := out.String()
	if strings.Contains(o, "SECRET") {
		t.Fatalf("a secret reached the output:\n%s", o)
	}
	for _, want := range []string{"✓ Конфигурация: " + cfgPath, "✓ GitLab: токен бота — пользователь reviewbot, scope api", "Reporter (нужна ровно эта)", "✓ Egress: прокси песочницы", "✓ Claude: токен", "Итог: 0 ошибок"} {
		if !strings.Contains(o, want) {
			t.Errorf("output lacks %q:\n%s", want, o)
		}
	}
	// The environment wins over .env: a wrong token fails, exit 1; JSON.
	t.Setenv("GITLAB_TOKEN", "wrong")
	out.Reset()
	if code := runDoctor(context.Background(), []string{"-config", cfgPath, "-env", envPath, "-json"}, &out, &errb, deps); code != 1 {
		t.Fatalf("wrong token: code %d\n%s", code, out.String())
	}
	var rep doctor.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.Fails == 0 {
		t.Fatalf("json: %v %+v", err, rep)
	}
	// Usage errors and a missing config.
	for _, args := range [][]string{{"-nope"}, {"extra"}, {"-config", filepath.Join(dir, "missing.yaml")}, {"-config", cfgPath, "-env", dir}} {
		errb.Reset()
		if code := runDoctor(context.Background(), args, &out, &errb, deps); code != 2 {
			t.Fatalf("%v: code %d", args, code)
		}
	}
	errb.Reset()
	runDoctor(context.Background(), []string{"-config", filepath.Join(dir, "missing.yaml")}, &out, &errb, deps)
	if !strings.Contains(errb.String(), "reviewbot init") {
		t.Fatalf("missing config: %s", errb.String())
	}
	os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("gitlab: [\n"), 0o644)
	errb.Reset()
	if code := runDoctor(context.Background(), []string{"-config", filepath.Join(dir, "bad.yaml")}, &out, &errb, deps); code != 2 || !strings.Contains(errb.String(), "bad.yaml") {
		t.Fatalf("bad yaml: %d %s", code, errb.String())
	}
	if err := loadDotenv(""); err != nil {
		t.Fatal(err)
	}
}

func TestLoadServeConfigNeedsInit(t *testing.T) {
	clearSecretsEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("gitlab:\n  url: \"https://g.example\"\n  groups: [\"backend\"]\n  events: \"poll\"\n"), 0o644)
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if _, err := loadServeConfig(cfgPath, getenv); !errors.Is(err, errNeedsInit) {
		t.Fatalf("no tokens: %v", err)
	}
	t.Setenv("GITLAB_TOKEN", "glpat-x")
	if _, err := loadServeConfig(cfgPath, getenv); !errors.Is(err, errNeedsInit) {
		t.Fatalf("no Claude credential: %v", err)
	}
	env["ANTHROPIC_API_KEY"] = "sk-ant-api03-x"
	cfg, err := loadServeConfig(cfgPath, getenv)
	if err != nil || cfg.GitLab.Token != "glpat-x" {
		t.Fatalf("tokens: %v", err)
	}
	// Other validation errors stay what they are.
	os.WriteFile(cfgPath, []byte("gitlab:\n  url: \"https://g.example\"\n  events: \"sometimes\"\n"), 0o644)
	if _, err := loadServeConfig(cfgPath, getenv); err == nil || errors.Is(err, errNeedsInit) || !strings.Contains(err.Error(), "gitlab.events") {
		t.Fatalf("invalid: %v", err)
	}
	// Exit codes.
	var errb bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&errb, nil))
	if code := fatal(logger, &errb, errNeedsInit); code != 2 || !strings.Contains(errb.String(), "reviewbot init") {
		t.Fatalf("needs init: %d %s", code, errb.String())
	}
	if code := fatal(logger, &errb, errors.New("boom")); code != 1 {
		t.Fatalf("other: %d", code)
	}
	if _, ok := runCLI("serve", nil); ok {
		t.Fatal("serve is not a terminal command")
	}
	if code, ok := runCLI("validate-repo-config", []string{"-nope"}); !ok || code != 2 {
		t.Fatalf("validate-repo-config: %d %v", code, ok)
	}
	if code, ok := runCLI("doctor", []string{"-nope"}); !ok || code != 2 {
		t.Fatalf("doctor: %d %v", code, ok)
	}
	if code, ok := runCLI("init", []string{"-nope"}); !ok || code != 2 {
		t.Fatalf("init: %d %v", code, ok)
	}
}

func TestDoctorInputsFromConfig(t *testing.T) {
	cfg := config.Defaults()
	cfg.GitLab.URL, cfg.GitLab.Groups, cfg.GitLab.Token, cfg.GitLab.CAFile = "https://g", []string{"a"}, "t", "/etc/ca.pem"
	in := doctorInputs("c.yaml", &cfg, nil, func(k string) string { return map[string]string{"ANTHROPIC_API_KEY": "k"}[k] })
	if in.GitLab.URL != "https://g" || in.Docker.Image != cfg.Sandbox.Image || in.Egress.Proxy != cfg.Sandbox.EgressProxy || in.Claude.APIKey != "k" || in.ConfigPath != "c.yaml" || in.GitLab.CAFile != "/etc/ca.pem" {
		t.Fatalf("%+v", in)
	}
}

func TestRunInit(t *testing.T) {
	clearSecretsEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	var out, errb bytes.Buffer
	// --repo-config: the starter is a valid .reviewbot.yaml.
	if code := runInit(context.Background(), []string{"-repo-config"}, initIO{out: &out, errw: &errb}); code != 0 {
		t.Fatalf("repo-config: %d", code)
	}
	if _, issues := config.ValidateRepo(out.Bytes()); len(issues) != 0 {
		t.Fatalf("starter .reviewbot.yaml: %v", issues)
	}
	// Existing files are the defaults; no terminal -> usage error.
	os.WriteFile("config.yaml", []byte("data_dir: \"/srv/rb\"\ngitlab:\n  url: \"https://g.example\"\n  groups: [\"backend\"]\n  ca_file: \"/etc/ca.pem\"\n"), 0o644)
	os.WriteFile(".env", []byte("GITLAB_TOKEN=glpat-SECRET\nANTHROPIC_API_KEY=sk-ant-api03-SECRET\n"), 0o600)
	v, err := initDefaults("config.yaml", ".env")
	if err != nil || v.GitLabURL != "https://g.example" || v.DataDir != "/srv/rb" || v.BotToken != "glpat-SECRET" || v.ClaudeAPIKey == "" || v.Groups[0] != "backend" || v.CAFile != "/etc/ca.pem" {
		t.Fatalf("defaults %+v %v", v, err)
	}
	if v, _ := initDefaults("config.yaml", dir); v.BotToken != "" {
		t.Fatal(".env is a directory: no tokens")
	}
	os.WriteFile("bad.yaml", []byte("gitlab: [\n"), 0o644)
	if code := runInit(context.Background(), []string{"-config", "bad.yaml"}, initIO{out: &out, errw: &errb}); code != 1 {
		t.Fatalf("bad config: %d", code)
	}
	sys := realInitSystem(func() (string, error) { return dir, nil })
	sys.Exec, sys.Stream = fakeDocker, func(context.Context, string, ...string) error { return nil }
	out.Reset()
	noTTY := func(...string) (string, error) { return "", errors.New("not a tty") }
	if code := runInit(context.Background(), nil, initIO{in: os.Stdin, out: &out, errw: &errb, sys: &sys, stty: noTTY}); code != initwiz.ExitUsage || !strings.Contains(out.String(), "--non-interactive") {
		t.Fatalf("no tty: %d %s", code, out.String())
	}
	if sys.Wd != dir || sys.GOOS == "" || sys.SocketGID == nil {
		t.Fatalf("system %+v", sys)
	}
	// The real stty on a non-terminal fails; the real system's SocketGID runs.
	if _, err := realStty(nil)("-g"); err == nil {
		t.Log("stty -g succeeded without a terminal")
	}
	_, _ = realInitSystem(os.Getwd).SocketGID()
	if err := realInitSystem(os.Getwd).Stream(context.Background(), "true"); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterSetupWeb(t *testing.T) {
	clearSecretsEnv(t)
	cfg := config.Defaults()
	cfg.Sandbox.EgressProxy = "" // no network in the test
	cfg.Usage.Token = "page-token-123456"
	mux := http.NewServeMux()
	registerSetupWeb(mux, &cfg, webNav(&cfg, nil, false, false, false, slog.Default()), slog.Default())
	r := httptest.NewRequest("GET", "/setup/data.json", nil)
	r.Header.Set("Authorization", "Bearer page-token-123456")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var rep doctor.Report
	if err := json.Unmarshal(w.Body.Bytes(), &rep); w.Code != http.StatusOK || err != nil || rep.Fails == 0 {
		t.Fatalf("code %d %v %+v", w.Code, err, rep)
	}
	for _, x := range rep.Results {
		if x.ID == "config.valid" {
			t.Fatal("the running config needs no config check")
		}
	}
}

// doctor reads only; -pull downloads a missing sandbox image (progress on
// stderr, the report stays clean JSON).
func TestRunDoctorPull(t *testing.T) {
	clearSecretsEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte("sandbox:\n  image: \"ghcr.io/fixcik/gitlab-review-bot-sandbox:latest\"\n  egress_proxy: \"\"\n"), 0o644)
	missing := func(_ context.Context, name string, args ...string) ([]byte, error) {
		k := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(k, "docker image inspect"):
			return []byte("No such image"), errors.New("exit 1")
		case strings.HasPrefix(k, "docker pull"):
			t.Errorf("pull through the read-only runner: %s", k)
		}
		return fakeDocker(context.Background(), name, args...)
	}
	var pulled []string
	deps := doctorDeps{exec: missing, pull: func(_ context.Context, image string) error { pulled = append(pulled, image); return nil }}
	var out, errb bytes.Buffer
	runDoctor(context.Background(), []string{"-config", cfgPath, "-env", ""}, &out, &errb, deps)
	if len(pulled) != 0 || !strings.Contains(out.String(), "reviewbot doctor --pull") {
		t.Fatalf("without -pull: pulled %v\n%s", pulled, out.String())
	}
	out.Reset()
	runDoctor(context.Background(), []string{"-config", cfgPath, "-env", "", "-pull", "-json"}, &out, &errb, deps)
	var rep doctor.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || len(pulled) != 1 || pulled[0] != "ghcr.io/fixcik/gitlab-review-bot-sandbox:latest" {
		t.Fatalf("-pull: %v %v\n%s", err, pulled, out.String())
	}
	for _, r := range rep.Results {
		if r.ID == "docker.sandbox_image" && (r.Status != doctor.OK || !strings.Contains(r.Detail, "скачан")) {
			t.Fatalf("image %+v", r)
		}
	}
	// The real pull streams to the given writer.
	var progress bytes.Buffer
	if err := dockerPull(&progress, "true")(context.Background(), "img"); err != nil {
		t.Fatal(err)
	}
}

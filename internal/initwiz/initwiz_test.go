package initwiz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/dotenv"
)

// Secrets used by the tests: none of them may ever reach the output.
const (
	botTok = "bot-tok-SECRET"
	svcTok = "svc-tok-SECRET"
	oat    = "sk-ant-oat01-SECRET"
	apiKey = "sk-ant-api03-SECRET"
)

func fakeGitLab(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("init wrote to GitLab: %s %s", r.Method, r.URL.Path)
		}
		tok := r.Header.Get("PRIVATE-TOKEN")
		user := map[string]any{botTok: map[string]any{"id": 7, "username": "reviewbot"}, svcTok: map[string]any{"id": 8, "username": "maint"}}[tok]
		if user == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.EscapedPath() {
		case "/api/v4/version":
			json.NewEncoder(w).Encode(map[string]string{"version": "17.5.1"})
		case "/api/v4/user":
			json.NewEncoder(w).Encode(user)
		case "/api/v4/personal_access_tokens/self":
			json.NewEncoder(w).Encode(map[string]any{"scopes": []string{"api"}, "expires_at": "2027-06-01"})
		case "/api/v4/groups/backend":
			json.NewEncoder(w).Encode(map[string]any{"id": 5})
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

// script is a fake terminal: each Line/Secret takes the next answer.
type script struct {
	t       *testing.T
	answers []string
	prompts []string
	secrets int
}

func (s *script) next(p string) (string, error) {
	s.prompts = append(s.prompts, p)
	if len(s.answers) == 0 {
		s.t.Fatalf("no answer for %q (prompts so far: %q)", p, s.prompts)
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	if a == "^C" {
		return "", ErrInterrupted
	}
	return a, nil
}

func (s *script) Line(p string) (string, error) { return s.next(p) }
func (s *script) Secret(p string) (string, error) {
	s.secrets++
	return s.next(p)
}

// fakeSys: docker works, the image is in the registry but not pulled,
// no git work tree, Linux with the socket in group 998.
type fakeSys struct {
	cmds    []string
	fail    map[string]string // command prefix -> output of a failure
	streams []string
	streamE map[string]error
	// block: streams that run until their context ends (a long pull).
	block map[string]bool
}

func (f *fakeSys) exec(_ context.Context, name string, args ...string) ([]byte, error) {
	k := name + " " + strings.Join(args, " ")
	f.cmds = append(f.cmds, k)
	for p, out := range f.fail {
		if strings.HasPrefix(k, p) {
			return []byte(out), errors.New("exit 1")
		}
	}
	switch {
	case strings.HasPrefix(k, "docker version"):
		return []byte("27.3.1"), nil
	case strings.HasPrefix(k, "git"):
		return []byte("fatal: not a git repository"), errors.New("exit 128")
	}
	return nil, nil
}

func (f *fakeSys) stream(ctx context.Context, name string, args ...string) error {
	k := name + " " + strings.Join(args, " ")
	f.streams = append(f.streams, k)
	if f.block[k] {
		<-ctx.Done()
		return ctx.Err()
	}
	return f.streamE[k]
}

func (f *fakeSys) sys(env map[string]string) System {
	return System{
		Getenv:    func(k string) string { return env[k] },
		Exec:      f.exec,
		Stream:    f.stream,
		HTTP:      http.DefaultClient,
		GOOS:      "linux",
		SocketGID: func() (string, error) { return "998", nil },
		Rand:      bytes.NewReader(bytes.Repeat([]byte{0xab}, 64)),
		Now:       func() time.Time { return time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC) },
	}
}

func opts(dir string) Options {
	return Options{ConfigPath: "config.yaml", EnvPath: ".env", HaveTTY: true}
}

func noSecrets(t *testing.T, out string) {
	t.Helper()
	for _, s := range []string{botTok, svcTok, oat, apiKey, strings.Repeat("ab", 32)} {
		if strings.Contains(out, s) {
			t.Fatalf("a secret reached the output:\n%s", out)
		}
	}
}

func TestInteractiveNewInstall(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gl := fakeGitLab(t)
	term := &script{t: t, answers: []string{
		gl.URL + "/", "backend", // 1: address, groups
		botTok,  // 2
		svcTok,  // 3
		"", oat, // 4: way 1 (default), token
		"", "https://bot.example/webhook", // 5: webhooks, address
		"",  // 6: data dir default
		"n", // compose? (no compose file: not asked)
	}}
	term.answers = term.answers[:len(term.answers)-1]
	fs := &fakeSys{fail: map[string]string{"docker image inspect": "No such image"}}
	var out bytes.Buffer
	code := Run(context.Background(), opts(dir), Values{}, term, &out, fs.sys(nil))
	if code != ExitOK {
		t.Fatalf("code %d\n%s", code, out.String())
	}
	o := out.String()
	noSecrets(t, o)
	for _, want := range []string{"1/6  GitLab", "✓ адрес: " + gl.URL + " отвечает", "пользователь reviewbot, scope api", "Reporter (нужна ровно эта)",
		"роль служебного токена в backend: Maintainer", "CLAUDE_CODE_OAUTH_TOKEN принят (формат)", "секрет вебхука сгенерирован",
		"✓ доступ к Docker", "скачиваю образ " + defaultImage, "✓ образ песочницы: " + defaultImage + " скачан", "Записано: config.yaml (без секретов), .env (права 600)",
		"sudo mkdir -p /var/lib/reviewbot", "reviewbot sync-hooks", "reviewbot init --repo-config"} {
		if !strings.Contains(o, want) {
			t.Errorf("output lacks %q:\n%s", want, o)
		}
	}
	if term.secrets != 3 {
		t.Errorf("secrets read hidden: %d, want 3", term.secrets)
	}
	cfg, _ := os.ReadFile("config.yaml")
	c := string(cfg)
	for _, want := range []string{`url: "` + gl.URL + `"`, `groups: ["backend"]`, `events: "both"`, `webhook_url: "https://bot.example/webhook"`,
		`data_dir: "/var/lib/reviewbot"`, `image: "ghcr.io/fixcik/gitlab-review-bot-sandbox:latest"`, "egress_allow:"} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %q:\n%s", want, c)
		}
	}
	noSecrets(t, c)
	st, _ := os.Stat(".env")
	if st.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode %v", st.Mode())
	}
	env, _ := dotenv.Read(".env")
	want := map[string]string{"GITLAB_TOKEN": botTok, "GITLAB_SERVICE_TOKEN": svcTok, "CLAUDE_CODE_OAUTH_TOKEN": oat,
		"GITLAB_WEBHOOK_SECRET": strings.Repeat("ab", 32), "REVIEWBOT_DATA_DIR": "/var/lib/reviewbot",
		"REVIEWBOT_SANDBOX_IMAGE": defaultImage, "DOCKER_GID": "998"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf(".env %s = %q, want %q", k, env[k], v)
		}
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY written without a value")
	}
}

func TestRerunKeepsForeignKeysAndAsksBeforeOverwrite(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gl := fakeGitLab(t)
	oldCfg := "# mine\ndata_dir: \"/srv/rb\"\ngitlab:\n  url: \"" + gl.URL + "\"\n  groups: [\"backend\"]\n  events: \"webhook\"\njira:\n  token: \"jira-SECRET\"\nlimits:\n  max_concurrent: 7\n"
	os.WriteFile("config.yaml", []byte(oldCfg), 0o644)
	os.WriteFile(".env", []byte("FOREIGN=1\nGITLAB_TOKEN="+botTok+"\nANTHROPIC_API_KEY="+apiKey+"\nGITLAB_WEBHOOK_SECRET=keep\n"), 0o644)
	old := Values{GitLabURL: gl.URL, Groups: []string{"backend"}, Events: "webhook", DataDir: "/srv/rb", BotToken: botTok, ClaudeAPIKey: apiKey, WebhookSecret: "keep"}
	fs := &fakeSys{}
	o := opts(dir)
	o.NonInteractive, o.WebhookURL = true, "https://bot/webhook"
	var out bytes.Buffer
	if code := Run(context.Background(), o, old, nil, &out, fs.sys(nil)); code != ExitFailed || !strings.Contains(out.String(), "--force") {
		t.Fatalf("without --force: code %d\n%s", code, out.String())
	}
	if b, _ := os.ReadFile("config.yaml"); string(b) != oldCfg {
		t.Fatal("config changed without --force")
	}
	if !strings.Contains(out.String(), "+   webhook_url") || strings.Contains(out.String(), "jira-SECRET") {
		t.Fatalf("diff:\n%s", out.String())
	}
	o.Force = true
	out.Reset()
	if code := Run(context.Background(), o, old, nil, &out, fs.sys(nil)); code != ExitOK {
		t.Fatalf("--force: code %d\n%s", code, out.String())
	}
	noSecrets(t, out.String())
	if !strings.Contains(out.String(), "события MR: вебхук https://bot/webhook") {
		t.Fatalf("the events check must see the webhook mode:\n%s", out.String())
	}
	b, _ := os.ReadFile("config.yaml")
	for _, want := range []string{"# mine", "max_concurrent: 7", `token: "jira-SECRET"`, `events: "webhook"`, `webhook_url: "https://bot/webhook"`, `sandbox:`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("config lacks %q:\n%s", want, b)
		}
	}
	env, _ := dotenv.Read(".env")
	if env["FOREIGN"] != "1" || env["ANTHROPIC_API_KEY"] != apiKey || env["GITLAB_WEBHOOK_SECRET"] != "keep" || env["REVIEWBOT_DATA_DIR"] != "/srv/rb" {
		t.Fatalf(".env = %v", env)
	}
	if st, _ := os.Stat(".env"); st.Mode().Perm() != 0o600 {
		t.Fatalf(".env mode %v", st.Mode())
	}
	// Same answers again: nothing to change in config.yaml.
	out.Reset()
	o.Force = false
	if code := Run(context.Background(), o, old, nil, &out, fs.sys(nil)); code != ExitOK || !strings.Contains(out.String(), "config.yaml: без изменений") {
		t.Fatalf("unchanged: code %d\n%s", code, out.String())
	}
}

func TestNonInteractiveMissingAndFailing(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	fs := &fakeSys{}
	o := opts(dir)
	o.NonInteractive = true
	var out bytes.Buffer
	if code := Run(context.Background(), o, Values{}, nil, &out, fs.sys(nil)); code != ExitUsage ||
		!strings.Contains(out.String(), "GITLAB_TOKEN") || !strings.Contains(out.String(), "--groups") || !strings.Contains(out.String(), "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatalf("missing: code %d\n%s", code, out.String())
	}
	o.Events = "sometimes"
	out.Reset()
	if code := Run(context.Background(), o, Values{}, nil, &out, fs.sys(nil)); code != ExitUsage || !strings.Contains(out.String(), "--events") {
		t.Fatalf("events: code %d\n%s", code, out.String())
	}
	gl := fakeGitLab(t)
	o.Events, o.GitLabURL, o.Groups = "poll", gl.URL, "backend, /backend/"
	out.Reset()
	env := map[string]string{"GITLAB_TOKEN": "wrong-SECRET", "CLAUDE_CODE_OAUTH_TOKEN": oat}
	if code := Run(context.Background(), o, Values{}, nil, &out, fs.sys(env)); code != ExitFailed || !strings.Contains(out.String(), "ничего не записано") {
		t.Fatalf("bad token: code %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "wrong-SECRET") {
		t.Fatal("token printed")
	}
	if _, err := os.Stat(".env"); err == nil {
		t.Fatal(".env written although a check failed")
	}
	// Poll mode with good values and -up: no webhook secret; compose runs.
	os.WriteFile(composeFile, []byte("services: {}\n"), 0o644)
	env["GITLAB_TOKEN"] = botTok
	o.Up = true
	out.Reset()
	if code := Run(context.Background(), o, Values{}, nil, &out, fs.sys(env)); code != ExitOK {
		t.Fatalf("poll: code %d\n%s", code, out.String())
	}
	e, _ := dotenv.Read(".env")
	if _, ok := e["GITLAB_WEBHOOK_SECRET"]; ok {
		t.Fatalf("poll: webhook secret written: %v", e)
	}
	if strings.Join(fs.streams, ";") != "docker compose pull;docker compose up -d" {
		t.Fatalf("streams %v", fs.streams)
	}
}

func TestInteractiveRetriesAndChoices(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gl := fakeGitLab(t)
	os.WriteFile(composeFile, []byte("services: {}\n"), 0o644)
	term := &script{t: t, answers: []string{
		"http://127.0.0.1:1", "", "y", // unreachable, no groups -> again
		gl.URL, "backend",
		"bad-SECRET", "", botTok, // wrong token -> again (default yes)
		"",          // no service token
		"2", apiKey, // API key
		"2",            // poll
		"relative/dir", // not absolute
	}}
	fs := &fakeSys{}
	var out bytes.Buffer
	code := Run(context.Background(), opts(dir), Values{}, term, &out, fs.sys(nil))
	if code != ExitUsage || !strings.Contains(out.String(), "абсолютным") {
		t.Fatalf("code %d\n%s", code, out.String())
	}
	o := out.String()
	noSecrets(t, o)
	for _, want := range []string{"GitLab недоступен", "нужна хотя бы одна группа", "токен не принят", "служебный токен: не задан", "ANTHROPIC_API_KEY принят", "опрос API раз в 30 секунд"} {
		if !strings.Contains(o, want) {
			t.Errorf("output lacks %q:\n%s", want, o)
		}
	}

	// Again, to the end: poll, the compose questions; pull fails, build
	// works, up works.
	term.answers = []string{gl.URL, "backend", botTok, "", "2", apiKey, "2", dir + "/data", "y"}
	fs.streamE = map[string]error{"docker compose pull": errors.New("denied")}
	out.Reset()
	if code := Run(context.Background(), opts(dir), Values{}, term, &out, fs.sys(nil)); code != ExitOK {
		t.Fatalf("code %d\n%s", code, out.String())
	}
	if strings.Join(fs.streams, ";") != "docker compose pull;docker compose build;docker compose up -d" || !strings.Contains(out.String(), "docker login ghcr.io") {
		t.Fatalf("streams %v\n%s", fs.streams, out.String())
	}
	// The config changes now (re-run with webhooks): the user says no.
	term.answers = []string{gl.URL, "backend", "", "", "2", "", "1", "", "", "n"}
	out.Reset()
	old := Values{GitLabURL: gl.URL, Groups: []string{"backend"}, Events: "poll", DataDir: dir + "/data", BotToken: botTok, ClaudeAPIKey: apiKey}
	if code := Run(context.Background(), opts(dir), old, term, &out, fs.sys(nil)); code != ExitFailed || !strings.Contains(out.String(), "Ничего не записано") {
		t.Fatalf("declined: code %d\n%s", code, out.String())
	}
	if !strings.Contains(strings.Join(term.prompts, "\n"), "оставить текущий") {
		t.Fatalf("existing tokens are offered as defaults:\n%s", out.String())
	}
}

func TestInteractiveFailedChecksAskBeforeWriting(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gl := fakeGitLab(t)
	answers := []string{gl.URL, "backend", "bad-SECRET", "n", "", "1", "not-a-token", "1", "", ""}
	term := &script{t: t, answers: append(append([]string{}, answers...), "n")}
	fs := &fakeSys{}
	var out bytes.Buffer
	if code := Run(context.Background(), opts(dir), Values{}, term, &out, fs.sys(nil)); code != ExitFailed || !strings.Contains(strings.Join(term.prompts, "\n"), "Проверки не пройдены (1)") {
		t.Fatalf("code %d\n%s", code, out.String())
	}
	if _, err := os.Stat(".env"); err == nil {
		t.Fatal("written although declined")
	}
	// Yes: written anyway; up is declined.
	os.WriteFile(composeFile, []byte("services: {}\n"), 0o644)
	term.answers = append(append([]string{}, answers...), "y", "n")
	out.Reset()
	if code := Run(context.Background(), opts(dir), Values{}, term, &out, fs.sys(nil)); code != ExitOK || !strings.Contains(out.String(), "Запуск: docker compose pull && docker compose up -d") {
		t.Fatalf("code %d\n%s", code, out.String())
	}
}

func TestComposeFailures(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gl := fakeGitLab(t)
	os.WriteFile(composeFile, []byte("services: {}\n"), 0o644)
	env := map[string]string{"GITLAB_TOKEN": botTok, "ANTHROPIC_API_KEY": apiKey}
	o := opts(dir)
	o.NonInteractive, o.Up, o.Force, o.GitLabURL, o.Groups, o.Events = true, true, true, gl.URL, "backend", "poll"
	for name, se := range map[string]map[string]error{
		"build": {"docker compose pull": errors.New("x"), "docker compose build": errors.New("y")},
		"up":    {"docker compose up -d": errors.New("z")},
	} {
		fs := &fakeSys{streamE: se}
		var out bytes.Buffer
		if code := Run(context.Background(), o, Values{}, nil, &out, fs.sys(env)); code != ExitFailed || !strings.Contains(out.String(), "docker compose") {
			t.Fatalf("%s: code %d\n%s", name, code, out.String())
		}
	}
	// docker compose missing (init inside the bot image): the commands are printed.
	fs := &fakeSys{fail: map[string]string{"docker compose version": "unknown command"}}
	var out bytes.Buffer
	if code := Run(context.Background(), o, Values{}, nil, &out, fs.sys(env)); code != ExitOK || !strings.Contains(out.String(), "в каталоге с docker-compose.yml выполните") || len(fs.streams) != 0 {
		t.Fatalf("no compose: code %d\n%s", code, out.String())
	}
}

func TestRefusals(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var out bytes.Buffer
	fs := &fakeSys{}
	// No terminal in interactive mode.
	o := opts(dir)
	o.HaveTTY = false
	if code := Run(context.Background(), o, Values{}, nil, &out, fs.sys(nil)); code != ExitUsage || !strings.Contains(out.String(), "--non-interactive") {
		t.Fatalf("no tty: %d %s", code, out.String())
	}
	// config.yaml is a directory (compose created it).
	os.Mkdir("config.yaml", 0o755)
	out.Reset()
	if code := Run(context.Background(), opts(dir), Values{}, nil, &out, fs.sys(nil)); code != ExitFailed || !strings.Contains(out.String(), "каталог") {
		t.Fatalf("dir: %d %s", code, out.String())
	}
	os.Remove("config.yaml")
	// .env unreadable (a directory).
	os.Mkdir(".env", 0o755)
	out.Reset()
	if code := Run(context.Background(), opts(dir), Values{}, nil, &out, fs.sys(nil)); code != ExitFailed {
		t.Fatalf(".env dir: %d %s", code, out.String())
	}
	os.Remove(".env")
	// Interrupted at a prompt.
	out.Reset()
	if code := Run(context.Background(), opts(dir), Values{}, &script{t: t, answers: []string{"^C"}}, &out, fs.sys(nil)); code != ExitInterrupted || !strings.Contains(out.String(), "ничего не записано") {
		t.Fatalf("^C: %d %s", code, out.String())
	}
	// .env in a git work tree, not ignored.
	gl := fakeGitLab(t)
	env := map[string]string{"GITLAB_TOKEN": botTok, "ANTHROPIC_API_KEY": apiKey}
	o = opts(dir)
	o.NonInteractive, o.GitLabURL, o.Groups, o.Events = true, gl.URL, "backend", "poll"
	git := &fakeSys{}
	gitSys := git.sys(env)
	gitSys.Exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		k := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasSuffix(k, "rev-parse --is-inside-work-tree"):
			return []byte("true\n"), nil
		case strings.Contains(k, "check-ignore"):
			return nil, errors.New("exit 1")
		}
		return git.exec(ctx, name, args...)
	}
	out.Reset()
	if code := Run(context.Background(), o, Values{}, nil, &out, gitSys); code != ExitFailed || !strings.Contains(out.String(), ".gitignore") {
		t.Fatalf("git: %d %s", code, out.String())
	}
	if _, err := os.Stat(".env"); err == nil {
		t.Fatal(".env written into a git work tree")
	}
	// A broken config.yaml is not overwritten.
	os.WriteFile("config.yaml", []byte("gitlab: [\n"), 0o644)
	out.Reset()
	if code := Run(context.Background(), o, Values{}, nil, &out, fs.sys(env)); code != ExitFailed || !strings.Contains(out.String(), "config.yaml") {
		t.Fatalf("broken yaml: %d %s", code, out.String())
	}
}

func TestDefaults(t *testing.T) {
	w := &wizard{opt: Options{ConfigPath: "conf/c.yaml"}, sys: System{Getenv: func(string) string { return "" }, GOOS: "darwin", Wd: "/Users/me/rb"},
		old: Values{DataDir: "./data", Events: "webhook"}}
	v := w.defaults()
	if v.DataDir != "/Users/me/rb/data" || v.Events != "webhook" || v.SandboxImage != defaultImage {
		t.Fatalf("darwin defaults %+v", v)
	}
	w.v = v
	m := w.envSets()
	if m["REVIEWBOT_CONFIG"] != "./conf/c.yaml" {
		t.Fatalf("env sets %v", m)
	}
	if _, ok := m["DOCKER_GID"]; ok {
		t.Fatal("DOCKER_GID on macOS")
	}
	w.opt.ConfigPath = "/etc/rb.yaml"
	w.sys.GOOS = "linux"
	w.sys.SocketGID = func() (string, error) { return "", errors.New("no socket") }
	if m := w.envSets(); m["REVIEWBOT_CONFIG"] != "/etc/rb.yaml" || m["DOCKER_GID"] != "" {
		t.Fatalf("env sets %v", m)
	}
	w.sys.Wd = ""
	if v := w.defaults(); v.DataDir != defaultDataDir {
		t.Fatalf("no wd: %+v", v)
	}
}

func TestEditConfig(t *testing.T) {
	got, err := editConfig([]byte("listen: \":1\"\n"), []setting{{[]string{"gitlab", "url"}, "u"}, {[]string{"gitlab", "groups"}, []string{"a", "b"}}, {[]string{"listen"}, ":2"}})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got); !strings.Contains(s, "listen: \":2\"") || !strings.Contains(s, "gitlab:\n  url: \"u\"") || !strings.Contains(s, `groups: ["a", "b"]`) {
		t.Fatalf("got\n%s", got)
	}
	for _, bad := range []string{"a: [\n", "a: 1\n---\nb: 2\n", "# only a comment\n- x\n"} {
		if _, err := editConfig([]byte(bad), []setting{{[]string{"x", "y"}, "v"}}); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
	if _, err := editConfig([]byte("x: 1\n"), []setting{{[]string{"x", "y"}, "v"}}); err == nil {
		t.Error("a key under a scalar: want an error")
	}
}

func TestDiffLines(t *testing.T) {
	if d := diffLines("a\nb\n", "a\nb\n"); d != "" {
		t.Fatalf("equal: %q", d)
	}
	d := diffLines("a\n  token: \"x\"\nc\n", "a\nb\nc\nd\n")
	if d != "-   token: ***\n+ b\n+ d\n" {
		t.Fatalf("diff %q", d)
	}
}

// The template of a new config.yaml has the values of the documented
// compose config (deploy/config.compose.yaml).
func TestTemplateMatchesComposeConfig(t *testing.T) {
	doc, err := os.ReadFile("../../deploy/config.compose.yaml")
	if err != nil {
		t.Fatal(err)
	}
	strip := func(b []byte) string {
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			if i := strings.Index(l, "#"); i >= 0 && !strings.Contains(l[:i], `"`) {
				l = l[:i]
			}
			if l = strings.TrimRight(l, " "); l != "" {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	if a, b := strip(configTemplate), strip(doc); a != b {
		t.Fatalf("config.template.yaml and deploy/config.compose.yaml differ:\n%s", diffLines(a, b))
	}
	if !bytes.Contains(RepoTemplate, []byte("$schema=")) {
		t.Fatal("repo template without the schema line")
	}
}

// Ctrl-C at every prompt: nothing is written, exit 130.
func TestInterruptAtEveryPrompt(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	gl := fakeGitLab(t)
	os.WriteFile(composeFile, []byte("services: {}\n"), 0o644)
	os.WriteFile("config.yaml", []byte("gitlab:\n  url: \"x\"\n"), 0o644)
	// written: the index of the first answer given after .env is written.
	for _, c := range []struct {
		full    []string
		written int
	}{
		{[]string{"bad", "backend", "y", gl.URL, "backend", "bad", "y", botTok, svcTok, "1", oat, "1", "", "", "y", "y"}, 15},
		{[]string{gl.URL, "backend", botTok, "", "2", apiKey, "2", ""}, 99},
	} {
		full := c.full
		for k := range full {
			os.Remove(".env")
			term := &script{t: t, answers: append(append([]string{}, full[:k]...), "^C")}
			var out bytes.Buffer
			fs := &fakeSys{}
			code := Run(context.Background(), opts(dir), Values{}, term, &out, fs.sys(nil))
			if code != ExitInterrupted {
				t.Fatalf("^C at answer %d: code %d\n%s", k, code, out.String())
			}
			if _, err := os.Stat(".env"); err == nil && k < c.written {
				t.Fatalf("^C at answer %d: .env written", k)
			}
			if k >= c.written && !strings.Contains(out.String(), "уже записаны") {
				t.Fatalf("^C after writing, at answer %d: %s", k, out.String())
			}
		}
	}
}

type fakeStty struct {
	calls []string
	fail  map[string]bool
}

func (f *fakeStty) run(args ...string) (string, error) {
	a := strings.Join(args, " ")
	f.calls = append(f.calls, a)
	if f.fail[a] {
		return "", errors.New("stty: not a tty")
	}
	if a == "-g" {
		return "saved-state\n", nil
	}
	return "", nil
}

func TestTTY(t *testing.T) {
	var out bytes.Buffer
	st := &fakeStty{}
	tty := &TTY{In: strings.NewReader("  answer \n" + oat + "\nlast"), Out: &out, Stty: st.run}
	if s, err := tty.Line("Q: "); err != nil || s != "answer" {
		t.Fatalf("line %q %v", s, err)
	}
	if s, err := tty.Secret("T: "); err != nil || s != oat {
		t.Fatalf("secret %q %v", s, err)
	}
	if strings.Join(st.calls, ";") != "-g;-echo;saved-state" {
		t.Fatalf("stty calls %v", st.calls)
	}
	if s, err := tty.Line("L: "); err != nil || s != "last" {
		t.Fatalf("last line without a newline: %q %v", s, err)
	}
	if _, err := tty.Line("E: "); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("EOF: %v", err)
	}
	if strings.Contains(out.String(), oat) || out.String() != "Q: T: \nL: E: " {
		t.Fatalf("out %q", out.String())
	}
	// No terminal.
	for _, tt := range []*TTY{{In: strings.NewReader("x\n"), Out: &out}, {In: strings.NewReader("x\n"), Out: &out, Stty: (&fakeStty{fail: map[string]bool{"-g": true}}).run},
		{In: strings.NewReader("x\n"), Out: &out, Stty: (&fakeStty{fail: map[string]bool{"-echo": true}}).run}} {
		if _, err := tt.Secret("T: "); !errors.Is(err, errNoTerminal) {
			t.Fatalf("no tty: %v", err)
		}
	}
	// Ctrl-C at a plain prompt (init catches SIGINT for the whole run).
	pr0, pw0 := io.Pipe()
	defer pw0.Close()
	sig0 := make(chan os.Signal, 1)
	sig0 <- os.Interrupt
	if _, err := (&TTY{In: pr0, Out: &out, Interrupt: sig0}).Line("Q: "); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("line interrupt: %v", err)
	}
	// A read error.
	if _, err := (&TTY{In: errReader{}, Out: &out}).Line("Q: "); err == nil || errors.Is(err, ErrInterrupted) {
		t.Fatalf("read error: %v", err)
	}
	// Ctrl-C while the secret is read: the echo comes back.
	pr, pw := io.Pipe()
	defer pw.Close()
	sig := make(chan os.Signal, 1)
	sig <- os.Interrupt
	st = &fakeStty{}
	if _, err := (&TTY{In: pr, Out: &out, Stty: st.run, Interrupt: sig}).Secret("T: "); !errors.Is(err, ErrInterrupted) {
		t.Fatalf("interrupt: %v", err)
	}
	if st.calls[len(st.calls)-1] != "saved-state" {
		t.Fatalf("echo not restored: %v", st.calls)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken") }

// The sandbox image: init pulls a missing one itself (no question), not a
// present one, not with -no-pull; a failed pull does not stop the files
// from being written and shows as a warning; Ctrl-C during the pull ends
// init with nothing written.
func TestSandboxImagePull(t *testing.T) {
	gl := fakeGitLab(t)
	env := map[string]string{"GITLAB_TOKEN": botTok, "CLAUDE_CODE_OAUTH_TOKEN": oat}
	pullCmd := "docker pull " + defaultImage
	run := func(t *testing.T, fs *fakeSys, mod func(*Options, *System)) (int, string) {
		t.Helper()
		t.Chdir(t.TempDir())
		o := opts("")
		o.NonInteractive, o.GitLabURL, o.Groups, o.Events = true, gl.URL, "backend", "poll"
		sys := fs.sys(env)
		if mod != nil {
			mod(&o, &sys)
		}
		var out bytes.Buffer
		code := Run(context.Background(), o, Values{}, nil, &out, sys)
		noSecrets(t, out.String())
		return code, out.String()
	}
	missing := map[string]string{"docker image inspect": "No such image"}
	t.Run("missing: pulled", func(t *testing.T) {
		fs := &fakeSys{fail: missing}
		code, out := run(t, fs, nil)
		if code != ExitOK || !slices.Contains(fs.streams, pullCmd) || !strings.Contains(out, "скачиваю образ "+defaultImage) ||
			!strings.Contains(out, "✓ образ песочницы: "+defaultImage+" скачан") {
			t.Fatalf("code %d streams %v\n%s", code, fs.streams, out)
		}
		if strings.Index(out, "скачиваю образ") > strings.Index(out, "Записано:") {
			t.Fatalf("the image is pulled in the Docker step, before writing:\n%s", out)
		}
	})
	t.Run("present: not pulled", func(t *testing.T) {
		fs := &fakeSys{}
		if code, out := run(t, fs, nil); code != ExitOK || slices.Contains(fs.streams, pullCmd) || strings.Contains(out, "скачиваю") {
			t.Fatalf("code %d streams %v\n%s", code, fs.streams, out)
		}
	})
	t.Run("-no-pull", func(t *testing.T) {
		fs := &fakeSys{fail: missing}
		code, out := run(t, fs, func(o *Options, _ *System) { o.NoPull = true })
		if code != ExitOK || slices.Contains(fs.streams, pullCmd) || !strings.Contains(out, "! образ песочницы") || !strings.Contains(out, "reviewbot doctor --pull") {
			t.Fatalf("code %d streams %v\n%s", code, fs.streams, out)
		}
	})
	t.Run("pull fails: files written, a warning", func(t *testing.T) {
		fs := &fakeSys{fail: map[string]string{"docker image inspect": "No such image", "docker manifest inspect": "unauthorized: authentication required"},
			streamE: map[string]error{pullCmd: errors.New("exit status 1")}}
		code, out := run(t, fs, nil)
		if code != ExitOK || strings.Contains(out, "✗ образ песочницы") || !strings.Contains(out, "! образ песочницы") ||
			!strings.Contains(out, "docker pull не удался") || !strings.Contains(out, "docker login ghcr.io") || !strings.Contains(out, "Записано:") {
			t.Fatalf("code %d\n%s", code, out)
		}
		if _, err := os.Stat(".env"); err != nil {
			t.Fatal(".env not written after a failed pull")
		}
	})
	t.Run("Ctrl-C during the pull", func(t *testing.T) {
		fs := &fakeSys{fail: missing, block: map[string]bool{pullCmd: true}}
		sig := make(chan os.Signal, 1)
		sig <- os.Interrupt
		code, out := run(t, fs, func(_ *Options, s *System) { s.Interrupt = sig })
		if code != ExitInterrupted || !strings.Contains(out, "ничего не записано") {
			t.Fatalf("code %d\n%s", code, out)
		}
		if _, err := os.Stat(".env"); err == nil {
			t.Fatal(".env written after Ctrl-C")
		}
	})
	t.Run("interactive: pulled without a question", func(t *testing.T) {
		t.Chdir(t.TempDir())
		fs := &fakeSys{fail: missing}
		term := &script{t: t, answers: []string{gl.URL, "backend", botTok, "", "1", oat, "2", ""}}
		var out bytes.Buffer
		if code := Run(context.Background(), opts(""), Values{}, term, &out, fs.sys(nil)); code != ExitOK || !slices.Contains(fs.streams, pullCmd) {
			t.Fatalf("code %d streams %v\n%s", code, fs.streams, out.String())
		}
		for _, p := range term.prompts {
			if strings.Contains(p, "скачать") || strings.Contains(p, "Скачать образ песочницы") {
				t.Fatalf("asked before pulling: %q", p)
			}
		}
	})
}

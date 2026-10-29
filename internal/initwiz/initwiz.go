// Package initwiz is `reviewbot init`: it asks for (or takes from flags and
// the environment) what a first start needs, checks it with the doctor
// checks, writes config.yaml (no secrets, other keys kept) and .env (the
// secrets, mode 600, other keys kept) and offers `docker compose pull` and
// `docker compose up -d`.
//
// Secrets are read from the terminal without echo or from the environment
// and never printed: a token is "принят" or not.
package initwiz

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/doctor"
	"github.com/fixcik/gitlab-review-bot/internal/dotenv"
)

// Events modes (gitlab.events).
const (
	eventsWebhook = "webhook"
	eventsPoll    = "poll"
	eventsBoth    = "both"
)

// Defaults of a new installation.
const (
	defaultImage   = "ghcr.io/fixcik/gitlab-review-bot-sandbox:latest"
	defaultDataDir = "/var/lib/reviewbot"
	defaultNetwork = "reviewbot-sandbox"
	composeFile    = "docker-compose.yml"
)

// Values are the answers. The secrets go to .env only.
type Values struct {
	GitLabURL    string
	Groups       []string
	Events       string
	WebhookURL   string
	DataDir      string
	SandboxImage string
	// CAFile: gitlab.ca_file of the existing config (used by the checks,
	// not asked or written).
	CAFile string

	BotToken, ServiceToken, WebhookSecret string
	ClaudeOAuth, ClaudeAPIKey             string
}

// Options are the command line.
type Options struct {
	ConfigPath, EnvPath string
	// NonInteractive asks nothing: values from the flags below, the
	// environment and the existing files; missing ones are an error.
	NonInteractive bool
	// Force overwrites a changed config.yaml without asking (needed with
	// NonInteractive); Up runs `docker compose pull` and `up -d` without
	// asking.
	Force, Up bool
	// HaveTTY: stdin is a terminal (interactive mode needs one).
	HaveTTY bool
	// Flag values; empty = not given.
	GitLabURL, Groups, Events, WebhookURL, DataDir, SandboxImage string
}

// System is the outside world (tests replace it).
type System struct {
	Getenv func(string) string
	// Exec runs docker and git and returns the output.
	Exec doctor.Exec
	// Stream runs a long command (docker compose pull/up) with its output
	// shown to the user.
	Stream func(ctx context.Context, name string, args ...string) error
	HTTP   *http.Client
	GOOS   string
	// SocketGID is the group of /var/run/docker.sock (Linux).
	SocketGID func() (string, error)
	Rand      io.Reader
	Now       func() time.Time
	// Wd is the working directory (the default data dir on macOS is under it).
	Wd string
}

// Exit codes.
const (
	ExitOK          = 0
	ExitFailed      = 1 // a check failed or nothing was written
	ExitUsage       = 2 // missing values, no terminal
	ExitInterrupted = 130
)

type wizard struct {
	ctx  context.Context
	opt  Options
	sys  System
	term Terminal
	out  io.Writer
	old  Values // from the existing config.yaml and .env
	v    Values
	// envOld: the existing .env (keys kept on write).
	envOld []byte
	fails  int
	// written: config.yaml and .env are on disk (an interrupt after that
	// must not say "nothing written").
	written bool
}

// Run is `reviewbot init`; old holds the values read from the existing
// config.yaml and .env (the defaults of the questions).
func Run(ctx context.Context, opt Options, old Values, term Terminal, out io.Writer, sys System) int {
	if sys.Rand == nil {
		sys.Rand = rand.Reader
	}
	if sys.Now == nil {
		sys.Now = time.Now
	}
	w := &wizard{ctx: ctx, opt: opt, sys: sys, term: term, out: out, old: old}
	if !opt.NonInteractive && !opt.HaveTTY {
		fmt.Fprintln(out, errNoTerminal.Error())
		return ExitUsage
	}
	if st, err := os.Stat(opt.ConfigPath); err == nil && st.IsDir() {
		fmt.Fprintf(out, "%s — каталог, а не файл: его создал docker при запуске compose без конфига. Удалите его (rmdir %s) и запустите init снова.\n", opt.ConfigPath, opt.ConfigPath)
		return ExitFailed
	}
	b, err := os.ReadFile(opt.EnvPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(out, "%s: %v\n", opt.EnvPath, err)
		return ExitFailed
	}
	w.envOld = b
	code, err := w.run()
	if errors.Is(err, ErrInterrupted) {
		if w.written {
			fmt.Fprintf(out, "\nПрервано: %s и %s уже записаны, docker compose не запускался.\n", opt.ConfigPath, opt.EnvPath)
		} else {
			fmt.Fprintln(out, "\nПрервано: ничего не записано.")
		}
		return ExitInterrupted
	}
	if err != nil {
		fmt.Fprintln(out, err.Error())
	}
	return code
}

func (w *wizard) printf(format string, a ...any) { fmt.Fprintf(w.out, format, a...) }

// show prints check results under a step.
func (w *wizard) show(rs []doctor.Result) (failed bool) {
	marks := map[doctor.Status]string{doctor.OK: "✓", doctor.Warn: "!", doctor.Fail: "✗", doctor.Skip: "·"}
	for _, r := range rs {
		line := "     " + marks[r.Status] + " " + r.Title
		if r.Detail != "" {
			line += ": " + r.Detail
		}
		w.printf("%s\n", line)
		if r.Action != "" && r.Status != doctor.OK {
			w.printf("       → %s\n", r.Action)
		}
		failed = failed || r.Status == doctor.Fail
	}
	return failed
}

// pick returns the results with one of the id prefixes.
func pick(rs []doctor.Result, prefixes ...string) []doctor.Result {
	var out []doctor.Result
	for _, r := range rs {
		for _, p := range prefixes {
			if strings.HasPrefix(r.ID, p) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

func (w *wizard) gitlab() []doctor.Result {
	return doctor.GitLab(doctor.GitLabInput{
		URL: w.v.GitLabURL, Groups: w.v.Groups, BotToken: w.v.BotToken, ServiceToken: w.v.ServiceToken, CAFile: w.v.CAFile,
		Events: w.v.Events, WebhookURL: w.v.WebhookURL, WebhookSecret: w.v.WebhookSecret, HTTP: w.sys.HTTP, Now: w.sys.Now,
	}).Check(w.ctx)
}

// ask reads a line with a default ("" input = def).
func (w *wizard) ask(prompt, def string) (string, error) {
	p := "     " + prompt
	if def != "" {
		p += " [" + def + "]"
	}
	s, err := w.term.Line(p + ": ")
	if s == "" {
		s = def
	}
	return s, err
}

// yes asks a yes/no question.
func (w *wizard) yes(prompt string, def bool) (bool, error) {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	s, err := w.term.Line("     " + prompt + " " + hint + " ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(s) {
	case "":
		return def, nil
	case "y", "yes", "д", "да":
		return true, nil
	}
	return false, nil
}

// secret asks for a token; keep: an existing value is kept on Enter.
func (w *wizard) secret(prompt string, existing string) (string, error) {
	p := "     " + prompt
	if existing != "" {
		p += " (Enter — оставить текущий)"
	}
	s, err := w.term.Secret(p + ": ")
	if err != nil {
		return "", err
	}
	if s == "" {
		return existing, nil
	}
	return s, nil
}

func splitGroups(s string) []string {
	var out []string
	for _, g := range strings.Split(s, ",") {
		if g = strings.Trim(strings.TrimSpace(g), "/"); g != "" && !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// defaults: flags, then the environment, then the existing files, then the
// built-in defaults.
func (w *wizard) defaults() Values {
	o, env := w.opt, w.sys.Getenv
	v := Values{
		GitLabURL:     strings.TrimRight(first(o.GitLabURL, env("GITLAB_URL"), w.old.GitLabURL), "/"),
		Groups:        w.old.Groups,
		Events:        first(o.Events, w.old.Events, eventsBoth),
		WebhookURL:    first(o.WebhookURL, w.old.WebhookURL),
		DataDir:       first(o.DataDir, env("REVIEWBOT_DATA_DIR"), w.old.DataDir),
		SandboxImage:  first(o.SandboxImage, env("REVIEWBOT_SANDBOX_IMAGE"), w.old.SandboxImage, defaultImage),
		CAFile:        w.old.CAFile,
		BotToken:      first(env("GITLAB_TOKEN"), w.old.BotToken),
		ServiceToken:  first(env("GITLAB_SERVICE_TOKEN"), w.old.ServiceToken),
		WebhookSecret: first(env("GITLAB_WEBHOOK_SECRET"), w.old.WebhookSecret),
		ClaudeOAuth:   first(env("CLAUDE_CODE_OAUTH_TOKEN"), w.old.ClaudeOAuth),
		ClaudeAPIKey:  first(env("ANTHROPIC_API_KEY"), w.old.ClaudeAPIKey),
	}
	if o.Groups != "" {
		v.Groups = splitGroups(o.Groups)
	}
	if !filepath.IsAbs(v.DataDir) {
		v.DataDir = defaultDataDir
		if w.sys.GOOS == "darwin" && w.sys.Wd != "" {
			// Docker Desktop shares the home directory; /var/lib is not
			// writable without sudo.
			v.DataDir = filepath.Join(w.sys.Wd, "data")
		}
	}
	return v
}

func (w *wizard) run() (int, error) {
	w.v = w.defaults()
	var err error
	if w.opt.NonInteractive {
		err = w.collectFlags()
	} else {
		err = w.collectInteractive()
	}
	if err != nil {
		if errors.Is(err, ErrInterrupted) {
			return ExitInterrupted, err
		}
		return ExitUsage, err
	}
	if w.fails > 0 {
		if w.opt.NonInteractive {
			return ExitFailed, fmt.Errorf("\nПроверки не пройдены (%d): ничего не записано", w.fails)
		}
		ok, err := w.yes(fmt.Sprintf("Проверки не пройдены (%d). Всё равно записать?", w.fails), false)
		if err != nil || !ok {
			return ExitFailed, errors.Join(err, errors.New("Ничего не записано."))
		}
	}
	code, err := w.write()
	if err != nil || code != ExitOK {
		return code, err
	}
	return w.compose()
}

// collectFlags: --non-interactive.
func (w *wizard) collectFlags() error {
	v := &w.v
	var missing []string
	if v.GitLabURL == "" {
		missing = append(missing, "адрес GitLab (--gitlab-url или GITLAB_URL)")
	}
	if len(v.Groups) == 0 {
		missing = append(missing, "группы (--groups)")
	}
	if v.BotToken == "" {
		missing = append(missing, "GITLAB_TOKEN")
	}
	if v.ClaudeOAuth == "" && v.ClaudeAPIKey == "" {
		missing = append(missing, "CLAUDE_CODE_OAUTH_TOKEN или ANTHROPIC_API_KEY")
	}
	if !slices.Contains([]string{eventsWebhook, eventsPoll, eventsBoth}, v.Events) {
		missing = append(missing, "--events: webhook, poll или both")
	}
	if len(missing) > 0 {
		return errors.New("не хватает: " + strings.Join(missing, ", "))
	}
	if v.Events != eventsPoll && v.WebhookSecret == "" {
		v.WebhookSecret = w.newSecret()
	}
	// The sandbox image is pulled after the files are written: a missing
	// image does not block them.
	rs := append(w.gitlab(), w.claude()...)
	w.printf("Проверки:\n")
	w.show(append(rs, w.docker()...))
	w.fails = countFails(rs)
	return nil
}

func countFails(rs []doctor.Result) int {
	n := 0
	for _, r := range rs {
		if r.Status == doctor.Fail {
			n++
		}
	}
	return n
}

func (w *wizard) newSecret() string {
	b := make([]byte, 32)
	_, _ = io.ReadFull(w.sys.Rand, b)
	return hex.EncodeToString(b)
}

func (w *wizard) claude() []doctor.Result {
	return doctor.Claude(doctor.ClaudeInput{OAuthToken: w.v.ClaudeOAuth, APIKey: w.v.ClaudeAPIKey}).Check(w.ctx)
}

// docker: daemon and sandbox image (the network is created by compose).
func (w *wizard) docker() []doctor.Result {
	rs := doctor.Docker(doctor.DockerInput{Image: w.v.SandboxImage, Network: defaultNetwork, Exec: w.sys.Exec, GOOS: w.sys.GOOS}).Check(w.ctx)
	return pick(rs, "docker.access", "docker.sandbox_image")
}

// retry: a step whose check failed asks to enter the value again.
func (w *wizard) retry(failed bool, prompt string) (bool, error) {
	if !failed {
		return false, nil
	}
	return w.yes(prompt, true)
}

func (w *wizard) collectInteractive() error {
	v := &w.v
	w.printf("\n1/6  GitLab\n")
	for {
		u, err := w.ask("Адрес", first(v.GitLabURL, "https://gitlab.example.com"))
		if err != nil {
			return err
		}
		v.GitLabURL = strings.TrimRight(u, "/")
		g, err := w.ask("Группы (через запятую)", strings.Join(v.Groups, ","))
		if err != nil {
			return err
		}
		v.Groups = splitGroups(g)
		failed := w.show(pick(w.gitlab(), "gitlab.version"))
		if len(v.Groups) == 0 {
			w.printf("     ✗ нужна хотя бы одна группа\n")
			failed = true
		}
		again, err := w.retry(failed, "Ввести заново?")
		if err != nil {
			return err
		}
		if !again {
			if len(v.Groups) == 0 {
				w.fails++
			}
			break
		}
	}

	w.printf("\n2/6  Токен бот-аккаунта\n")
	for {
		t, err := w.secret("GITLAB_TOKEN (scope api, ввод скрыт)", v.BotToken)
		if err != nil {
			return err
		}
		v.BotToken = t
		rs := pick(w.gitlab(), "gitlab.version", "gitlab.bot_")
		failed := w.show(pick(rs, "gitlab.bot_"))
		again, err := w.retry(failed, "Ввести токен заново?")
		if err != nil {
			return err
		}
		if !again {
			w.fails += countFails(rs)
			break
		}
	}

	w.printf("\n3/6  Служебный токен (необязательно)\n")
	t, err := w.secret("GITLAB_SERVICE_TOKEN (роль Maintainer, Enter — пропустить)", v.ServiceToken)
	if err != nil {
		return err
	}
	v.ServiceToken = t
	w.show(pick(w.gitlab(), "gitlab.service_"))

	w.printf("\n4/6  Claude\n")
	def := "1"
	if v.ClaudeOAuth == "" && v.ClaudeAPIKey != "" {
		def = "2"
	}
	way, err := w.ask("Способ: [1] claude setup-token  [2] ANTHROPIC_API_KEY", def)
	if err != nil {
		return err
	}
	if way == "2" {
		k, err := w.secret("ANTHROPIC_API_KEY (ввод скрыт)", v.ClaudeAPIKey)
		if err != nil {
			return err
		}
		v.ClaudeAPIKey, v.ClaudeOAuth = k, ""
	} else {
		k, err := w.secret("CLAUDE_CODE_OAUTH_TOKEN из `claude setup-token` (ввод скрыт)", v.ClaudeOAuth)
		if err != nil {
			return err
		}
		v.ClaudeOAuth, v.ClaudeAPIKey = k, ""
	}
	cr := w.claude()
	w.show(cr)
	w.fails += countFails(cr)

	w.printf("\n5/6  Как GitLab сообщает о MR\n")
	def = "1"
	if v.Events == eventsPoll {
		def = "2"
	}
	mode, err := w.ask("[1] вебхуки (с догоняющим опросом)  [2] только опрос API (если до бота не достучаться)", def)
	if err != nil {
		return err
	}
	if mode == "2" {
		v.Events = eventsPoll
		w.printf("     ✓ опрос API раз в 30 секунд, вебхуки не нужны\n")
	} else {
		if v.Events == eventsPoll {
			v.Events = eventsBoth
		}
		u, err := w.ask("Публичный адрес вебхука (Enter — настроить позже)", v.WebhookURL)
		if err != nil {
			return err
		}
		v.WebhookURL = u
		if v.WebhookSecret == "" {
			v.WebhookSecret = w.newSecret()
			w.printf("     ✓ секрет вебхука сгенерирован\n")
		} else {
			w.printf("     ✓ секрет вебхука: оставлен текущий\n")
		}
	}

	w.printf("\n6/6  Песочница\n")
	d, err := w.ask("Каталог данных (одинаковый путь на хосте и в контейнере)", v.DataDir)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(d) {
		return fmt.Errorf("каталог данных должен быть абсолютным путём: %s", d)
	}
	v.DataDir = d
	dr := w.docker()
	w.show(dr)
	return nil
}

// configSets are the keys init owns in config.yaml.
func (w *wizard) configSets() []setting {
	v := w.v
	s := []setting{
		{[]string{"data_dir"}, v.DataDir},
		{[]string{"gitlab", "url"}, v.GitLabURL},
		{[]string{"gitlab", "groups"}, v.Groups},
		{[]string{"gitlab", "events"}, v.Events},
	}
	if v.Events != eventsPoll && v.WebhookURL != "" {
		s = append(s, setting{[]string{"gitlab", "webhook_url"}, v.WebhookURL})
	}
	return append(s, setting{[]string{"sandbox", "image"}, v.SandboxImage})
}

// envSets are the keys init owns in .env. A key is written only with a
// value or when it is already in the file (then it is cleared).
func (w *wizard) envSets() map[string]string {
	v, old := w.v, dotenv.Parse(w.envOld)
	m := map[string]string{
		"GITLAB_TOKEN":            v.BotToken,
		"GITLAB_SERVICE_TOKEN":    v.ServiceToken,
		"CLAUDE_CODE_OAUTH_TOKEN": v.ClaudeOAuth,
		"ANTHROPIC_API_KEY":       v.ClaudeAPIKey,
		"GITLAB_WEBHOOK_SECRET":   v.WebhookSecret,
		"REVIEWBOT_DATA_DIR":      v.DataDir,
		"REVIEWBOT_SANDBOX_IMAGE": v.SandboxImage,
	}
	if v.Events == eventsPoll {
		delete(m, "GITLAB_WEBHOOK_SECRET") // kept as it is, not needed
	}
	if p := filepath.Clean(w.opt.ConfigPath); p != "config.yaml" {
		if !filepath.IsAbs(p) {
			p = "./" + p
		}
		m["REVIEWBOT_CONFIG"] = p
	}
	if w.sys.GOOS == "linux" && w.sys.SocketGID != nil {
		if gid, err := w.sys.SocketGID(); err == nil {
			m["DOCKER_GID"] = gid
		}
	}
	for k, val := range m {
		if _, had := old[k]; val == "" && !had {
			delete(m, k)
		}
	}
	return m
}

// envIgnored: .env inside a git work tree must be git-ignored.
func (w *wizard) envIgnored() error {
	abs, err := filepath.Abs(w.opt.EnvPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	out, err := w.sys.Exec(w.ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return nil // not a git work tree (or no git)
	}
	if _, err := w.sys.Exec(w.ctx, "git", "-C", dir, "check-ignore", "-q", filepath.Base(abs)); err != nil {
		return fmt.Errorf("%s в каталоге под git и не в .gitignore: секреты попали бы в репозиторий. Добавьте строку `%s` в .gitignore и запустите init снова", w.opt.EnvPath, filepath.Base(abs))
	}
	return nil
}

func (w *wizard) write() (int, error) {
	if err := w.envIgnored(); err != nil {
		return ExitFailed, err
	}
	oldCfg, err := os.ReadFile(w.opt.ConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ExitFailed, err
	}
	newCfg, err := editConfig(oldCfg, w.configSets())
	if err != nil {
		return ExitFailed, err
	}
	envNew, err := dotenv.Update(w.envOld, w.envSets())
	if err != nil {
		return ExitFailed, err
	}
	w.printf("\n")
	writeCfg := true
	switch d := diffLines(string(oldCfg), string(newCfg)); {
	case len(oldCfg) == 0:
	case d == "":
		writeCfg = false
		w.printf("%s: без изменений\n", w.opt.ConfigPath)
	default:
		w.printf("%s изменится:\n%s", w.opt.ConfigPath, d)
		switch {
		case w.opt.Force:
		case w.opt.NonInteractive:
			return ExitFailed, fmt.Errorf("%s уже есть и изменится: добавьте --force, чтобы перезаписать (ничего не записано)", w.opt.ConfigPath)
		default:
			ok, err := w.yes("Записать изменения?", false)
			if err != nil {
				return ExitFailed, err
			}
			if !ok {
				return ExitFailed, errors.New("Ничего не записано.")
			}
		}
	}
	if writeCfg {
		if err := os.WriteFile(w.opt.ConfigPath, newCfg, 0o644); err != nil {
			return ExitFailed, err
		}
	}
	if err := dotenv.WriteFile(w.opt.EnvPath, envNew); err != nil {
		return ExitFailed, err
	}
	w.written = true
	w.printf("Записано: %s (без секретов), %s (права 600)\n", w.opt.ConfigPath, w.opt.EnvPath)
	if _, err := os.Stat(w.v.DataDir); err != nil {
		w.printf("Каталог данных %s не найден, создайте его для uid 1000:\n     sudo mkdir -p %s && sudo chown 1000:1000 %s\n", w.v.DataDir, w.v.DataDir, w.v.DataDir)
	}
	return ExitOK, nil
}

// compose offers `docker compose pull` (fallback build) and `up -d`.
func (w *wizard) compose() (int, error) {
	next := func() {
		if w.v.Events != eventsPoll && w.v.WebhookURL != "" && w.v.ServiceToken != "" {
			w.printf("Вебхуки в проектах групп: reviewbot sync-hooks -config %s (или gitlab.sync_hooks: true)\n", w.opt.ConfigPath)
		}
		w.printf("Проверить ещё раз: reviewbot doctor -config %s\n", w.opt.ConfigPath)
		w.printf("Дальше: добавьте .reviewbot.yaml в репозитории (reviewbot init --repo-config > .reviewbot.yaml)\n")
	}
	_, noCompose := os.Stat(composeFile)
	if _, err := w.sys.Exec(w.ctx, "docker", "compose", "version"); noCompose != nil || err != nil {
		w.printf("Запуск: в каталоге с %s выполните docker compose pull && docker compose up -d\n", composeFile)
		next()
		return ExitOK, nil
	}
	run := w.opt.Up
	if !run && !w.opt.NonInteractive {
		ok, err := w.yes("Скачать образы и запустить: docker compose pull && docker compose up -d?", true)
		if err != nil {
			return ExitFailed, err
		}
		run = ok
	}
	if !run {
		w.printf("Запуск: docker compose pull && docker compose up -d\n")
		next()
		return ExitOK, nil
	}
	if err := w.sys.Stream(w.ctx, "docker", "compose", "pull"); err != nil {
		w.printf("docker compose pull не удался (пакеты ghcr.io приватные? docker login ghcr.io с PAT read:packages); собираю локально: docker compose build\n")
		if err := w.sys.Stream(w.ctx, "docker", "compose", "build"); err != nil {
			return ExitFailed, fmt.Errorf("docker compose build: %w", err)
		}
	}
	if err := w.sys.Stream(w.ctx, "docker", "compose", "up", "-d"); err != nil {
		return ExitFailed, fmt.Errorf("docker compose up -d: %w", err)
	}
	w.printf("Бот запущен: docker compose logs -f reviewbot\n")
	next()
	return ExitOK, nil
}

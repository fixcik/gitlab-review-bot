package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/dotenv"
	"github.com/fixcik/gitlab-review-bot/internal/initwiz"
)

// initIO is the terminal of `reviewbot init`.
type initIO struct {
	in         *os.File
	out, errw  io.Writer
	sys        *initwiz.System // nil = the real one (tests replace it)
	stty       func(args ...string) (string, error)
	getwd      func() (string, error)
	interrupts <-chan os.Signal
}

// runInit is `reviewbot init`: see internal/initwiz. Exit codes: 0 written,
// 1 a check failed or nothing written, 2 usage error or missing values,
// 130 interrupted.
func runInit(ctx context.Context, args []string, tio initIO) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(tio.errw)
	var o initwiz.Options
	fs.StringVar(&o.ConfigPath, "config", "config.yaml", "service config to write (no secrets)")
	fs.StringVar(&o.EnvPath, "env", ".env", "file for the secrets (mode 600)")
	fs.BoolVar(&o.NonInteractive, "non-interactive", false, "ask nothing: values from the flags, the environment (GITLAB_TOKEN, GITLAB_SERVICE_TOKEN, CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY, GITLAB_WEBHOOK_SECRET) and the existing files")
	fs.BoolVar(&o.Force, "force", false, "overwrite a changed config.yaml without asking")
	fs.BoolVar(&o.Up, "up", false, "run docker compose pull and up -d without asking")
	fs.StringVar(&o.GitLabURL, "gitlab-url", "", "GitLab address (or GITLAB_URL)")
	fs.StringVar(&o.Groups, "groups", "", "comma-separated GitLab groups")
	fs.StringVar(&o.Events, "events", "", "webhook, poll or both (default both)")
	fs.StringVar(&o.WebhookURL, "webhook-url", "", "public webhook address of the bot")
	fs.StringVar(&o.DataDir, "data-dir", "", "data directory, same absolute path on the host and in the container (or REVIEWBOT_DATA_DIR)")
	fs.StringVar(&o.SandboxImage, "sandbox-image", "", "sandbox image (or REVIEWBOT_SANDBOX_IMAGE)")
	repoConfig := fs.Bool("repo-config", false, "print a starter .reviewbot.yaml and exit")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return initwiz.ExitUsage
	}
	if *repoConfig {
		_, _ = tio.out.Write(initwiz.RepoTemplate)
		return initwiz.ExitOK
	}
	old, err := initDefaults(o.ConfigPath, o.EnvPath)
	if err != nil {
		fmt.Fprintln(tio.errw, err)
		return initwiz.ExitFailed
	}
	if tio.stty == nil {
		tio.stty = realStty(tio.in)
	}
	if tio.getwd == nil {
		tio.getwd = os.Getwd
	}
	if tio.interrupts == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		defer signal.Stop(ch)
		tio.interrupts = ch
	}
	_, ttyErr := tio.stty("-g")
	o.HaveTTY = ttyErr == nil
	term := &initwiz.TTY{In: tio.in, Out: tio.out, Stty: tio.stty, Interrupt: tio.interrupts}
	sys := realInitSystem(tio.getwd)
	if tio.sys != nil {
		sys = *tio.sys
	}
	return initwiz.Run(ctx, o, old, term, tio.out, sys)
}

// initDefaults reads the existing config.yaml (without validation) and
// .env: the defaults of init's questions.
func initDefaults(cfgPath, envPath string) (initwiz.Values, error) {
	var v initwiz.Values
	if st, err := os.Stat(cfgPath); err == nil && !st.IsDir() {
		cfg, err := config.LoadUnvalidated(cfgPath)
		if err != nil {
			return v, err
		}
		g := cfg.GitLab
		v.GitLabURL, v.Groups, v.Events, v.WebhookURL = g.URL, g.Groups, g.Events, g.WebhookURL
		v.DataDir, v.SandboxImage, v.CAFile = cfg.DataDir, cfg.Sandbox.Image, cfg.GitLab.CAFile
	}
	env, err := dotenv.Read(envPath)
	if err != nil {
		env = map[string]string{} // a directory or unreadable file: initwiz.Run reports it
	}
	v.BotToken, v.ServiceToken, v.WebhookSecret = env["GITLAB_TOKEN"], env["GITLAB_SERVICE_TOKEN"], env["GITLAB_WEBHOOK_SECRET"]
	v.ClaudeOAuth, v.ClaudeAPIKey = env["CLAUDE_CODE_OAUTH_TOKEN"], env["ANTHROPIC_API_KEY"]
	return v, nil
}

// realStty runs stty on the terminal in (stdin of stty = the terminal).
func realStty(in *os.File) func(args ...string) (string, error) {
	return func(args ...string) (string, error) {
		c := exec.Command("stty", args...)
		c.Stdin = in
		out, err := c.Output()
		return strings.TrimSpace(string(out)), err
	}
}

func realInitSystem(getwd func() (string, error)) initwiz.System {
	wd, _ := getwd()
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	}
	return initwiz.System{
		Getenv: os.Getenv,
		Exec:   run,
		Stream: func(ctx context.Context, name string, args ...string) error {
			c := exec.CommandContext(ctx, name, args...)
			c.Stdout, c.Stderr = os.Stdout, os.Stderr
			return c.Run()
		},
		// HTTP nil: the doctor's client (10 s timeout, gitlab.ca_file).
		GOOS: runtime.GOOS,
		SocketGID: func() (string, error) {
			out, err := run(context.Background(), "stat", "-c", "%g", "/var/run/docker.sock")
			return strings.TrimSpace(string(out)), err
		},
		Wd: wd,
	}
}

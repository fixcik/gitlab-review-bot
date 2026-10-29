package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/doctor"
	"github.com/fixcik/gitlab-review-bot/internal/dotenv"
)

// doctorTimeout bounds one doctor run (GitLab, docker, the egress proxy).
const doctorTimeout = 60 * time.Second

// doctorInputs fills the standard checks from a config read without
// validation (validErr: its validation error) and the environment.
func doctorInputs(path string, cfg *config.Service, validErr error, getenv func(string) string) doctor.Inputs {
	return doctor.Inputs{
		ConfigPath: path,
		ConfigErr:  validErr,
		GitLab: doctor.GitLabInput{
			URL: cfg.GitLab.URL, Groups: cfg.GitLab.Groups, CAFile: cfg.GitLab.CAFile,
			BotToken: cfg.GitLab.Token, ServiceToken: cfg.GitLab.ServiceToken,
			Events: cfg.GitLab.Events, WebhookURL: cfg.GitLab.WebhookURL, WebhookSecret: cfg.GitLab.WebhookSecret,
		},
		Docker: doctor.DockerInput{Image: cfg.Sandbox.Image, Network: cfg.Sandbox.Network, GOOS: runtime.GOOS},
		Egress: doctor.EgressInput{Proxy: cfg.Sandbox.EgressProxy, Allow: cfg.Sandbox.EgressAllow},
		Claude: doctor.ClaudeInput{OAuthToken: getenv("CLAUDE_CODE_OAUTH_TOKEN"), APIKey: getenv("ANTHROPIC_API_KEY")},
	}
}

// loadDotenv puts the keys of the .env at path into the process
// environment, like compose does for the bot container: a variable already
// set in the environment wins. A missing file is nothing to load.
func loadDotenv(path string) error {
	if path == "" {
		return nil
	}
	m, err := dotenv.Read(path)
	if err != nil {
		return err
	}
	for k, v := range m {
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}

// runDoctor is `reviewbot doctor [-config config.yaml] [-env .env] [-json]`:
// the setup checks on the given config, read-only. Exit code 0 = no
// failed check (warnings allowed), 1 = a check failed, 2 = usage error or
// the config cannot be read.
func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer, deps doctorDeps) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "config.yaml", "path to config.yaml")
	envPath := fs.String("env", ".env", "the .env with the secrets (missing = only the environment)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return 2
	}
	if err := loadDotenv(*envPath); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", *envPath, err)
		return 2
	}
	cfg, err := config.LoadUnvalidated(*cfgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "%s не найден: выполните `reviewbot init`\n", *cfgPath)
		} else {
			fmt.Fprintf(stderr, "%v\n", err)
		}
		return 2
	}
	in := doctorInputs(*cfgPath, cfg, cfg.Validate(), os.Getenv)
	deps.apply(&in)
	ctx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	rep := doctor.Run(ctx, time.Now(), in.Providers()...)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		rep.WriteText(stdout)
	}
	return rep.ExitCode()
}

// doctorDeps replaces the outside world in tests (nil = the real one).
type doctorDeps struct {
	exec doctor.Exec
	dial func(ctx context.Context, hostport string) (string, error)
}

func (d doctorDeps) apply(in *doctor.Inputs) {
	in.Docker.Exec = d.exec
	in.Egress.Dial = d.dial
}

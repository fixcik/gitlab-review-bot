// Command reviewbot is an AI code review bot for self-managed GitLab.
//
//	reviewbot serve  -config config.yaml   # review service: webhook and/or polling (default)
//	reviewbot egress -listen :3128 [-mcp-forward reviewbot:8091]  # sandbox egress proxy (+ MCP port forward)
//	reviewbot sync-hooks -config config.yaml [-dry-run]  # sync group project webhooks
//	reviewbot runs [-since 48h] [-status failed] [-json]  # diagnostics of recent runs
//	reviewbot runs show <id>                             # one run's timeline
//	reviewbot validate-repo-config [path]                # check a .reviewbot.yaml (exit 1 on errors)
//	reviewbot init [-non-interactive] [-repo-config]     # write config.yaml and .env, check them
//	reviewbot doctor [-config config.yaml] [-json] [-pull] # setup checks (exit 1 on a failed one)
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))

	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	if code, ok := runCLI(cmd, args); ok { // terminal commands: no JSON logger
		os.Exit(code)
	}

	if err := runCommand(logger, cmd, args); err != nil {
		os.Exit(fatal(logger, os.Stderr, err))
	}
}

// runCLI runs the commands for a person at a terminal (plain output, an
// exit code); ok is false for the others.
func runCLI(cmd string, args []string) (code int, ok bool) {
	switch cmd {
	case "validate-repo-config":
		return runValidateRepoConfig(args, os.Stdout, os.Stderr), true
	case "doctor":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runDoctor(ctx, args, os.Stdout, os.Stderr, doctorDeps{}), true
	case "init":
		// No NotifyContext: Ctrl-C at a prompt must end init (the hidden
		// token prompt restores the terminal echo itself).
		return runInit(context.Background(), args, initIO{in: os.Stdin, out: os.Stdout, errw: os.Stderr}), true
	}
	return 0, false
}

// fatal reports the error of a command and returns the exit code: 2 for
// an installation that is not set up (errNeedsInit), else 1.
func fatal(logger *slog.Logger, stderr io.Writer, err error) int {
	if errors.Is(err, errNeedsInit) {
		fmt.Fprintln(stderr, "reviewbot: "+err.Error())
		return 2
	}
	logger.Error("fatal", "err", err)
	return 1
}

// runCommand runs one subcommand until it finishes or SIGINT/SIGTERM.
func runCommand(logger *slog.Logger, cmd string, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "serve":
		return serve(ctx, logger, args)
	case "egress":
		return runEgress(ctx, logger, args)
	case "sync-hooks":
		return runSyncHooks(ctx, args)
	case "runs":
		return runRuns(ctx, args, os.Stdout)
	case "help", "-h", "--help":
		fmt.Println("usage: reviewbot [serve -config config.yaml | egress -listen :3128 [-config config.yaml] [-mcp-listen :8091 -mcp-forward host:port] | sync-hooks -config config.yaml [-dry-run] | runs [-config config.yaml] [-since 48h] [-status failed] [-json] | runs show <id> | validate-repo-config [path] | init [-non-interactive] [-no-pull] [-repo-config] | doctor [-config config.yaml] [-env .env] [-json] [-pull]]")
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func logLevel() slog.Level {
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

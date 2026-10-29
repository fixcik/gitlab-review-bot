package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/fixcik/gitlab-review-bot/internal/config"
)

// runValidateRepoConfig is `reviewbot validate-repo-config [path]`: checks a
// .reviewbot.yaml exactly as the bot does (unknown keys, types, values) and
// prints every problem as "<path>:<line>: <key>: <message>". Exit code 0 =
// valid, 1 = invalid, 2 = usage error or the file cannot be read. For local
// use and CI (no config.yaml, tokens or network needed).
func runValidateRepoConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate-repo-config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: reviewbot validate-repo-config [path]   (default ./"+config.RepoConfigPath+")")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	path := config.RepoConfigPath
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", path, err)
		return 2
	}
	_, issues := config.ValidateRepo(b)
	if len(issues) == 0 {
		fmt.Fprintf(stdout, "%s: ok\n", path)
		return 0
	}
	for _, is := range issues {
		loc := path
		if is.Line > 0 {
			loc = fmt.Sprintf("%s:%d", path, is.Line)
		}
		key := ""
		if is.Key != "" {
			key = is.Key + ": "
		}
		fmt.Fprintf(stdout, "%s: %s%s\n", loc, key, is.Message)
	}
	fmt.Fprintf(stderr, "%s: %d error(s); reference: docs/configuration.md\n", path, len(issues))
	return 1
}

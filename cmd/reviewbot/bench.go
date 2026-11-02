package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/claude"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/output"
	"github.com/fixcik/gitlab-review-bot/internal/review/verify"
)

// runBench is the ReviewBench adapter (bench/README.md): it reviews the pull
// request mounted by the benchmark (AGENT_CONTRACT.md) and writes RB_OUT.
func runBench(ctx context.Context, logger *slog.Logger) error {
	repo := envOr("RB_REPO", "/work/repo")
	var pr struct {
		Repo, Title, Body, Base, Head string
		PRNumber                      int `json:"pr_number"`
	}
	raw, err := os.ReadFile(os.Getenv("RB_PR_JSON"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &pr); err != nil {
		return fmt.Errorf("pr.json: %w", err)
	}
	diff, err := os.ReadFile(os.Getenv("RB_DIFF"))
	if err != nil {
		return err
	}
	namesOut, err := exec.CommandContext(ctx, "git", "-C", repo, "diff", "--name-only", pr.Base+"..."+pr.Head).Output()
	if err != nil {
		return fmt.Errorf("git diff --name-only: %w", err)
	}
	files := strings.Fields(string(namesOut))

	model := os.Getenv("RB_CONFIG_MODEL")
	opt := review.BenchOptions{Deep: os.Getenv("RB_CONFIG_DEPTH") == "deep", Recall: os.Getenv("RB_CONFIG_RECALL") == "high", Explore: os.Getenv("RB_CONFIG_EXPLORE") == "high", Lenses: os.Getenv("RB_CONFIG_LENSES") == "high", Scenario: os.Getenv("RB_CONFIG_SCENARIO") == "high", Rev: os.Getenv("RB_CONFIG_REVPROMPT") == "high"}
	if os.Getenv("RB_CONFIG_FILES") == "high" {
		opt.FullFiles = fileBlock(repo, files)
	}
	panel := os.Getenv("RB_CONFIG_PANEL") == "high"
	fmt.Printf("model=%s depth=%t recall=%t explore=%t lenses=%t files=%t scenario=%t panel=%t revprompt=%t\n", model, opt.Deep, opt.Recall, opt.Explore, opt.Lenses, opt.FullFiles != "", opt.Scenario, panel, opt.Rev)

	runner, err := claude.NewRunner(claude.Options{Mode: "local", ClaudeBin: "claude", Logger: logger})
	if err != nil {
		return err
	}
	lenses := []string{""}
	if panel {
		lenses = review.BenchLenses
	}
	type pass struct {
		out  *output.ReviewOutput
		err  error
		read []string
	}
	passes := make([]pass, len(lenses))
	var wg sync.WaitGroup
	for i, lens := range lenses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := opt
			o.Lens = lens
			req, err := review.BenchRequest(repo, pr.Title, pr.Body, pr.Base, pr.Head, string(diff), files, 25*time.Minute, o)
			if err != nil {
				passes[i].err = err
				return
			}
			req.Model = model
			res, err := runner.Run(ctx, req)
			if err != nil {
				passes[i].err = err
				return
			}
			fmt.Printf("pass=%d turns=%d files_read=%d\n", i, res.NumTurns, len(res.ReadFiles))
			passes[i].read = res.ReadFiles
			printReads(fmt.Sprintf("pass=%d", i), res.ReadFiles)
			passes[i].out, passes[i].err = review.ParseBench(res.Output, 30)
		}()
	}
	wg.Wait()
	var all []outFinding
	failed := 0
	add := func(pi int, out *output.ReviewOutput) {
		for _, f := range out.Findings {
			end := f.EndLine
			if end < f.Line {
				end = f.Line
			}
			all = append(all, outFinding{f.Path, f.Line, end, f.Title + "\n\n" + f.Body, pi, f})
		}
	}
	for pi, p := range passes {
		if p.err != nil {
			fmt.Printf("pass failed: %v\n", p.err)
			failed++
			continue
		}
		add(pi, p.out)
	}
	if failed == len(passes) {
		return passes[0].err
	}

	// RB_CONFIG_FOLLOWUP=high: a second review in a NEW conversation (RevEngine's
	// follow-up), told what the first one found and which changed files it never
	// opened. A failed follow-up keeps the first review.
	if os.Getenv("RB_CONFIG_FOLLOWUP") == "high" && !panel && passes[0].err == nil {
		o := opt
		o.FollowUp = followUpBlock(repo, files, passes[0].out, passes[0].read)
		req, err := review.BenchRequest(repo, pr.Title, pr.Body, pr.Base, pr.Head, string(diff), files, 25*time.Minute, o)
		if err == nil {
			req.Model = model
			res, err := runner.Run(ctx, req)
			if err == nil {
				fmt.Printf("followup turns=%d files_read=%d\n", res.NumTurns, len(res.ReadFiles))
				printReads("followup", res.ReadFiles)
				if out, err := review.ParseBench(res.Output, 30); err == nil {
					add(len(passes), out)
				} else {
					fmt.Printf("followup parse failed: %v\n", err)
				}
			} else {
				fmt.Printf("followup failed: %v\n", err)
			}
		}
	}
	merged := mergeFindings(all)
	// RB_CONFIG_VERIFY=high: the bot's own verification before publishing (a
	// run that tries to refute the findings by reading the code), for all of
	// them, in batches. A rejected finding is dropped; a failed run keeps them.
	if os.Getenv("RB_CONFIG_VERIFY") == "high" {
		merged = verifyAll(ctx, runner, repo, pr.Base, pr.Head, string(diff), model, merged)
	}

	agent := envOr("RB_AGENT", "reviewbot")
	type finding struct {
		File      string `json:"file"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
		Message   string `json:"message"`
		Producer  string `json:"producer"`
	}
	findings := []finding{}
	for _, f := range merged {
		findings = append(findings, finding{f.file, f.start, f.end, f.message, agent})
	}
	number, _ := strconv.Atoi(os.Getenv("RB_PR_NUMBER"))
	doc := map[string]any{
		"pr":       map[string]any{"repo": pr.Repo, "pr_number": number, "base": pr.Base, "head": os.Getenv("RB_HEAD")},
		"agent":    agent,
		"findings": findings,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(os.Getenv("RB_OUT"), data, 0o644)
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

type outFinding struct {
	file       string
	start, end int
	message    string
	pass       int
	f          output.Finding
}

var wordRe = regexp.MustCompile(`[a-z0-9_]{3,}`)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range wordRe.FindAllString(strings.ToLower(s), -1) {
		m[w] = true
	}
	return m
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

// mergeFindings drops a finding of a later pass that repeats one of an
// earlier pass: the same file, lines within 3 of each other and a similar text
// (Jaccard of the words >= 0.45, as in RevEngine). Findings of one pass are
// never merged with each other: two problems on the same lines are two
// findings.
func mergeFindings(in []outFinding) []outFinding {
	var out []outFinding
next:
	for _, f := range in {
		for i, k := range out {
			if k.pass != f.pass && k.file == f.file && f.start <= k.end+3 && k.start <= f.end+3 &&
				jaccard(wordSet(k.message), wordSet(f.message)) >= 0.45 {
				if len(f.message) > len(k.message) {
					f.pass = k.pass
					out[i] = f
				}
				continue next
			}
		}
		out = append(out, f)
	}
	return out
}

// printReads logs the files a run opened, one JSON line per run, for the
// coverage diagnosis (was a missed defect in a file the model never read).
func printReads(label string, read []string) {
	b, _ := json.Marshal(read)
	fmt.Printf("READS %s %s\n", label, b)
}

// followUpBlock is the text of the second review: the findings of the first
// one and the changed files it did not open.
func followUpBlock(repo string, files []string, first *output.ReviewOutput, read []string) string {
	opened := map[string]bool{}
	for _, p := range read {
		opened[strings.TrimPrefix(strings.TrimPrefix(p, repo), "/")] = true
	}
	var b strings.Builder
	b.WriteString("A first reviewer has already reported these findings. Do not repeat them:\n")
	for _, f := range first.Findings {
		fmt.Fprintf(&b, "- %s:%d (%s) %s\n", f.Path, f.Line, f.Severity, f.Title)
	}
	if len(first.Findings) == 0 {
		b.WriteString("- (none)\n")
	}
	var unread []string
	for _, f := range files {
		if !opened[f] {
			unread = append(unread, f)
		}
	}
	if len(unread) > 0 {
		b.WriteString("\nChanged files the first reviewer never opened. Start with these:\n")
		for _, f := range unread {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if os.Getenv("RB_CONFIG_SWEEP") == "high" {
		b.WriteString("\nSibling sweep: for every finding above, search the repository (Grep) for other places with the same pattern or the same mistake: copies in other files, other callers, parallel handlers, sibling configs and workflows. Report each further occurrence as its own finding with its own file and line; an occurrence elsewhere is not a repeat.\n")
	}
	b.WriteString("\nRead the changed files above, the callers of the changed code and the tests yourself, in a fresh and independent review. Report only additional real problems the first review missed, including ones it may have left out because it could not fully prove them. Do not repeat its findings.")
	return b.String()
}

const (
	maxFileBytes   = 60 << 10
	maxTotalBytes  = 200 << 10
	fileBlockFence = "```"
)

// fileBlock renders the changed files at head for the prompt: text files up
// to maxFileBytes each and maxTotalBytes in all.
func fileBlock(repo string, files []string) string {
	var b strings.Builder
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil || len(data) > maxFileBytes || strings.IndexByte(string(data), 0) >= 0 {
			continue
		}
		if b.Len()+len(data) > maxTotalBytes {
			break
		}
		fmt.Fprintf(&b, "### %s\n%s\n%s\n%s\n\n", name, fileBlockFence, data, fileBlockFence)
	}
	return b.String()
}

const verifyBatch = 12

// verifyAll runs the verification over the findings in batches of verifyBatch,
// in parallel, and returns those it did not reject.
func verifyAll(ctx context.Context, runner core.Runner, repo, base, head, diff, model string, in []outFinding) []outFinding {
	rejected := make([]bool, len(in))
	var wg sync.WaitGroup
	for start := 0; start < len(in); start += verifyBatch {
		end := min(start+verifyBatch, len(in))
		wg.Add(1)
		go func() {
			defer wg.Done()
			fs := make([]output.Finding, end-start)
			for i := range fs {
				fs[i] = in[start+i].f
			}
			req, parse, err := verify.BenchRequest(repo, base, head, diff, fs, 15*time.Minute)
			if err != nil {
				fmt.Printf("verify batch failed: %v\n", err)
				return
			}
			req.Model = model
			res, err := runner.Run(ctx, req)
			if err != nil {
				fmt.Printf("verify batch failed: %v\n", err)
				return
			}
			verdicts, err := parse(res.Output)
			if err != nil {
				fmt.Printf("verify batch parse failed: %v\n", err)
				return
			}
			for i, v := range verdicts {
				if v.Verdict == "reject" {
					rejected[start+i] = true
				}
			}
		}()
	}
	wg.Wait()
	var out []outFinding
	for i, f := range in {
		if !rejected[i] {
			out = append(out, f)
		}
	}
	fmt.Printf("verify: %d findings, rejected %d\n", len(in), len(in)-len(out))
	return out
}

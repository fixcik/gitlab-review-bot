// Package review is the orchestrator of the bot: it handles every
// core.JobKind the queue hands it, runs the review flow, answers the bot's
// commands and questions, and wires the feature packages under it.
//
// # Jobs
//
// Reviewer.Handle (review.go) dispatches a job by kind, inside a per-job
// context (token accounting in review/jobctx, the memo of MR reads in
// glmemo.go, the award-emoji lifecycle in reactions*.go):
//
//   - review jobs (auto, review, full, force): handleReview (flow.go);
//   - questions: handleChat (chat.go); "@bot help": handleHelp (help.go);
//   - "@bot check": handleCheck (check.go, check_prompt.go): re-check open
//     findings against the current head;
//   - pause, resume, ignore, resolve, config: handleMRCommand (commands.go,
//     commands_pause.go, commands_resolve.go, commands_config.go), no Claude;
//   - thanks / acknowledgements: handleReact (reactions_thanks.go), no Claude;
//   - jobs owned by one feature go straight to it: the title refresh
//     (task), the status refresh and failed pipelines (pipeline), thread
//     replies, "@bot rules" and "@bot memory" (learning).
//
// # The review flow
//
// handleReview (flow.go) runs numbered steps over a reviewRun: 1-4 preflight
// (the MR, .reviewbot.yaml opt-in and validation in repoconfig.go, the
// auto-review gate, permissions); 5-6 the reviewed range, checkout and diff,
// with the trivial-increment shortcut (trivial.go); 7 the size limit
// (bigmr.go, bigmr_thread.go) and 7a the limits booking; 8 the scan of the
// bot's open threads (review/publish); 9 the analyzers (analyzers.go,
// concurrent through parallel.go) and the model route (route.go); 10 the
// Claude run with the MCP tools and its fallback (review_prompt.go,
// prompt_cache.go, prompts.go, paths.go, skills.go, problems.go), the
// answer parsing (format.go) and the verification of findings; 11-12
// publishing new findings (review_publish.go) and closing fixed ones; 13
// the summary note (review_summary.go); 14 the MR state. User-facing strings
// shared across files are in replies.go.
//
// # Features
//
// New builds the shared env (review/env) and publisher (review/publish),
// then one Feature per package: task, learning, linters, compatcheck,
// migcheck, verify, pipeline, crossrepo, labels and mcp (their dependency
// DAG is in docs/DESIGN.md, "Package layout"). The orchestrator calls their
// flow steps; no feature imports review. Where a feature needs the
// orchestrator back, it declares an interface and New passes an
// implementation (callbacks.go): repoConfigs (env.RepoConfig),
// learnReactions (learning.NoteReactions) and pipelineHost (pipeline.Host).
// The verifier's tools (verify.Tools) come from the review's MCP run
// (mcp.Run.VerifyTools).
//
// # Test support
//
// internals.go exposes the wiring (Reviewer.Internals) to the shared test
// harness review/reviewtest, through which the feature packages' tests
// drive a whole Reviewer. export_test.go gives the orchestrator's own
// external tests (package review_test) its unexported hooks and constants.
// Neither is used by production code.
package review

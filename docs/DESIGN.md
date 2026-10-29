# gitlab-review-bot — design

AI code review bot for **self-managed GitLab 17.x+**, similar to CodeRabbit.
Reviews are produced by headless Claude Code (`claude -p`) running inside a
sandbox container against a local checkout of the MR.

Module: `github.com/fixcik/gitlab-review-bot`, Go 1.24.7+ (minimum in go.mod; the images build with Go 1.27). Single binary
`reviewbot` with subcommands `serve` (default), `egress`, `sync-hooks`,
`runs` and `validate-repo-config`. User-facing documentation (Russian) lives in [README.md](README.md).

## Decisions

| Area | Decision |
|---|---|
| Trigger | Webhook service (MR, Note and Pipeline events) or API polling (`gitlab.events`), not CI |
| GitLab | self-managed 17.x+, bot is a dedicated user with PAT (`api` scope), **Reporter** in projects (cannot push/merge/change settings); the client also refuses every write except MR notes/discussions/draft notes, award emoji (reactions) on an MR and its notes, MR labels (body with `add_labels`/`remove_labels` only; a Reporter gets 403) and comments on commits (`POST …/repository/commits/:sha/comments`, only for a main branch broken by a direct push, review.ci). A separate Maintainer service token `GITLAB_SERVICE_TOKEN` (alias `GITLAB_HOOKS_TOKEN`) syncs hooks, adds the bot as MR reviewer, resolves threads and sets MR labels when the bot token cannot; its client may write only project hooks, an MR's reviewer list (body with `reviewer_ids` only), MR labels (`add_labels`/`remove_labels` only) and discussion resolution |
| Merge gate | Findings >= `review.block_min_severity` (default critical) stay open threads (block with "All threads must be resolved"); lower ones are resolved right after publishing (by the service token, or the bot's own token without one). The review system prompt states the configured threshold (`prompts.BlockRule`) and asks for the lowest blocking level only when confident. "Request changes" does not block in Free/CE, so it is not used. GitLab 19.4 makes a plain MR comment a resolvable thread, so a service command's own thread (`KindInfo.ClosesThread`: review, help, config, pause, ...) is resolved with the service token after the bot answered (`Env.closeCommandThread`; only when the command is the first note, never a finding thread) |
| Auth to Claude | subscription token `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) or `ANTHROPIC_API_KEY`. Never use `claude --bare` (it ignores OAuth). |
| Storage | SQLite (`github.com/mattn/go-sqlite3`, cgo) |
| Queue | in-process, durable: every job is persisted in SQLite (auto reviews in `queue_pending`, one per MR; every other job in `queue_jobs`, one row each) and reloaded at startup; SIGTERM drains (`queue.shutdown_grace`). See "Queue durability and shutdown" |
| Language of review text | Russian by default (`language` in config) |
| Load | ~20 MRs/day, max 2 concurrent Claude runs |
| Claude permissions | Read/Grep/Glob only (+ the read-only `code-search` subagent and the bot's MCP tools), scoped by path to the clone, `/base`, `/repos`, `/skills`; everything else is denied (`/proc`, `/etc`, `$HOME`, the MCP config…). No Bash, no git, no repository commands, no setup, no web tools. The code before the MR is a read-only export of the merge base at `/base`. |
| Drafts | Draft MRs are never auto-reviewed. Draft -> Ready triggers a full review. |
| Opt-in | Only repositories with `.reviewbot.yaml` on the MR's target branch are reviewed (`review.require_repo_config`, default true); the file is validated strictly (all problems, with lines); an invalid one stops reviews of the repo and is reported in one MR note. Webhooks are still installed in every group project. |

Dependencies are limited to what is in go.mod: `goccy/go-yaml`,
`mattn/go-sqlite3`, `bmatcuk/doublestar/v4`, stdlib. The GitLab client is
hand-written on `net/http` (no SDK).

## Package layout and ownership

The packages form layers; `.go-arch-lint.yml` names them as components and
lists, for each, the components it may import (only the edges the code
really has). `scripts/arch-lint.sh` checks it (CI runs it on every push; the
script pins the release archive's sha256 per platform and re-verifies the cached
archive on every run);
`cmd/reviewbot` may import anything. Bottom up:

```
common (any package may import them)
  internal/core      shared types & interfaces (DO NOT change without coordination)
  internal/tracing   dependency-free OpenTelemetry tracer: spans, W3C traceparent, ratio sampler, OTLP/HTTP JSON batch exporter, slog trace_id handler

config               imports nothing but core
  internal/config    service config + .reviewbot.yaml (DO NOT change without coordination)

util                 leaves
  internal/safefs    bot-created paths inside the MR clone (.reviewbot-lint/-compat/-cache, .git/info/exclude) without following planted symlinks
  internal/tasklink  pure functions: Jira keys in MR texts, title parsing, title hint
  internal/webassets static files shared by the web pages (ECharts, filters.js), versioned URLs
  internal/dotenv    compose .env read/update keeping foreign keys and comments, literal quoting, mode 600
  internal/webui     shared CSS/JS inlined by every page (tokens, layout, D helpers, filter bar CSS) and the shared header (Nav.Header: sections, problems badge, theme switch)
  internal/metrics   dependency-free Prometheus metrics (counter, gauge, histogram vectors)

domain               pure feature logic: config, util, domain
  internal/compat    backward-compat: spec discovery, base/head materialization, oasdiff changelog (+ Swagger 2.0 via oas2to3)/buf/graphql-inspector/custom checks via Runner.Exec
  internal/migrations  migration safety: detection (SQL/Flyway/goose/golang-migrate/sqitch/dbmate/Doctrine/Laravel/Alembic/Django), forward-only extractors, SQL rule engine, usage check (pure; reads trees via interfaces)
  internal/lint      gitleaks via Runner.Exec (the only scanner the bot runs); CI linter reports (CodeClimate/Code Quality, SARIF, Checkstyle) parsing; changed-line filter; built-in "todo" check (todo.go, no Runner); code equivalence for trivial pushes (trivial.go, `CompareCode`, reuses the todo lexer)
  internal/sysmap    system map: contracts (http/webhook/ws/sse) per repo from snapshots (OpenAPI parser + light model), matching, impact of a change, MR dependencies (phase B)
  internal/sysmap/model  the map's stored records (Entry, FileRecord, RepoState, RunRecord) + sysmap.Store (leaf; the store implements it, sysmap aliases them)
  internal/diffcov   diff coverage: coverage report parsers + changed-line coverage
  internal/diffmap   unified-diff parser: which new-file lines are commentable
  internal/events    GitLab events in neutral form -> core.Job (events.Router: commands, drafts, thread replies, groups filter) + the poller's stored state (PollMR, PollState)
  internal/learn     team rules: model, learn.Store interface, command parsing, scrubbing, prompt selection
  internal/memory    per-project operational memory: sections, memory.Store interface, merge/caps, command parsing
  internal/problems  verification problems: what a review could not check (kinds, fingerprint, per-job collector, notify rules)
  internal/reactions award emoji: thanks/ack classifier (shared by webhook and review), ordered idempotent worker
  internal/skills    team skills (SKILL.md) from review.skills.dir: load, validate, stage, prompt list

adapter-out          clients of the outside world: util, adapter-out
  internal/gitlab    REST v4 client implementing core.GitLab
  internal/jira      read-only Jira DC/Server client (REST v2, PAT): issue, comments, parent, epic
  internal/claude    core.Runner: docker / local modes, output parsing
  internal/repo      core.Repos: mirror cache + cached read-only trees per (project, commit) (or disposable clones) + context snapshots
  internal/contextrepos  group project list (cached) -> default-branch snapshots for a run
  internal/egress    CONNECT-only HTTPS proxy with host allowlist (sandbox egress) + the one-port MCP forward
  internal/mcpserver MCP server (Streamable HTTP, JSON-RPC 2.0) with per-run sessions: review tools for the model

app                  application services: util, domain, app, adapter-out (TEMP: diag -> claude)
  internal/limits    core.Limiter on top of core.Store
  internal/queue     core.Queue: debounce, per-MR serialization, priorities, retry, durable jobs
  internal/meter     core.Runner decorator: records every Run with its attribution; owns the run records (ClaudeRun, RunFilter, RunStats, ...)
  internal/diag      run records of the last 48h: job timeline, Claude sessions with transcripts
  internal/diag/diagindex  rows of the diag index (Row, Filter): leaf, so the store does not import diag
  internal/telemetry Prometheus metrics (reviewbot_*) and the decorators that feed them
  internal/doctor    setup checks (GitLab GETs, docker inspect, egress CONNECT, Claude token format) as providers -> Report; reviewbot doctor, init and /setup
  internal/initwiz   reviewbot init: questions or flags, doctor checks, config.yaml edit keeping foreign keys (YAML AST), .env, docker compose pull/up

store                domain, app
  internal/store     SQLite implementation of core.Store and of every record store; imports the record types of their owners, is imported only by cmd

ingress              event sources: domain, adapter-out
  internal/webhook   HTTP handler: secret check, hook payloads -> internal/events -> queue
  internal/poller    API polling instead of / next to webhooks -> same core.Job via events.Router
  internal/hooksync  keeps the bot's project webhook in every project of gitlab.groups

web                  pages: util, domain, app, web
  internal/usageweb  GET /usage (HTML) and /usage/data.json: token consumption
  internal/diagweb   GET /runs, /runs/<id>, raw transcripts (same access model as /usage)
  internal/mapweb    GET /map, /map/data.json: graph, unresolved calls, scan status (same access model as /usage)
  internal/problemsweb GET /problems, POST /problems/ack (same access model as /usage + same-origin + CSRF)
  internal/setupweb  GET /setup, /setup/data.json: doctor report of the running config, read-only, cached (same access model as /usage)

review               internal/review/..., see below
cmd/reviewbot        wiring
tools/oas2to3        separate Go module (not a dependency of the bot): Swagger 2.0 -> OpenAPI 3.0 converter for the sandbox image
```

**The review packages.** `internal/review` is split by feature; every
feature is one package with a `Feature` type built by its `New(...)`:

```
rv-leaf (import only core and each other)
  review/mdtext      text and GitLab Markdown helpers: plurals, shortening, code spans, defusing untrusted text
  review/marker      every "<!-- reviewbot:… -->" marker: build, find, parse, strip
  review/finding     the finding model: stable id, severities, normalization, the first note of a finding thread
  review/output      Claude's structured answers (ReviewOutput, ChatOutput, Finding, TaskAlignment, Compat*); core keeps Severity
  review/jsonschema  edits the JSON schemas of Claude's answers and parses the answers
  review/prompts     what the prompt templates share: template funcs, rendering, language, tools and blocking rules
  review/summary     the summary note as a document: render, footer, gate line, edits of an existing body
  review/mrthread    the bot's threads in an MR: summary, open finding/compat/lint/size/CI threads
  review/diffcap     per-file diff chunks and the cap of what Claude gets
  review/cijob       what the CI features share: artifacts API, parallel job names, job patterns, pipeline links
  review/citelink    `group/project:path:line` citations as links

rv-shared (+ config, domain, app: meter)
  review/env         what a feature gets from the orchestrator: GitLab (with the job's memo), store, repos, limits,
                     config, logger, clock, bot identity, the structured Claude run, replies, access checks,
                     env.RepoConfig (repository config), the merge base export (/base)
  review/publish     writing to an MR's threads: draft batches, anchoring and placement, marking fixed, resolving, Scan
  review/jobctx      per-job state in the context: bot problems, token attribution, files read, memo bypass, reaction marks

features (f-*)
  review/task        Jira issues of the MR, the title check and its refresh (JobRefreshTitle)
  review/learning    team rules (learning from replies and outcomes), project memory, "@bot rules"/"@bot memory"
  review/linters     gitleaks, TODOs, CI linter reports: threads on changed lines, the lint part of the merge gate
  review/compatcheck backward compatibility of API specs and the API changelog
  review/migcheck    migration safety analysis and its evidence
  review/verify      adversarial verification of findings before publishing
  review/pipeline    CI: failed pipelines (digest, explanation, threads, main branch), TBD block, diff coverage
  review/crossrepo   beyond the repository: context repositories, system map (phases A and B), diagrams
  review/labels      automatic MR labels
  review/mcp         the bot's MCP tools of a review run (internal/mcpserver), in-loop finding validation, fallback

orchestrator
  internal/review    core.Handler for every JobKind: the review flow, chat, help, check, MR commands, reactions,
                     repository config; wires the features (package doc: internal/review/doc.go)

test support (excluded from arch-lint like _test.go files)
  review/reviewtest  the review harness: exported fakes and a whole Reviewer built by review.New
```

The features form a DAG (A → B: A imports B); `task`, `learning`,
`compatcheck` and `migcheck` import no other feature:

```
linters   → task
verify    → learning, linters, compatcheck, migcheck
pipeline  → linters
crossrepo → task, compatcheck
labels    → linters, pipeline, compatcheck, migcheck
mcp       → task, linters, compatcheck, migcheck, verify, pipeline, crossrepo
```

No feature imports `internal/review`. Where a feature needs the orchestrator
back, it declares the interface and `review.New` passes an implementation
(`internal/review/callbacks.go`): `env.RepoConfig` (repository config of an MR:
`repoConfigs`), `learning.NoteReactions` (award emoji around learning:
`learnReactions`) and `pipeline.Host` (the queue and the auto-review gate:
`pipelineHost`). The verifier's tools (`verify.Tools`) come from the review's
MCP run (`mcp.Run.VerifyTools`). The orchestrator owns what spans features:
`Reviewer.Handle` (dispatch by kind, the per-job context: token accounting,
the GitLab read memo `glmemo.go`, the reaction lifecycle `reactions*.go`), the
review flow (`flow.go` and its `review_*.go`, `analyzers.go`, `parallel.go`,
`route.go`, `trivial.go`, `bigmr*.go`), chat and help, `@bot check`, the MR
commands (`commands*.go`), the repository config (`repoconfig.go`), path
instructions, skills, verification problems and prompt caching.

The one TEMP rule left: `diag` (app) imports `claude` (adapter-out), because
its runner decorator reads `claude.ProcessInfo` through `claude.WithObserver`
and records `claude.AllowedTools`/`DisallowedTools`; removing it means moving
those into core or a leaf package.

**Stored records: one ownership rule.** The package that uses a record owns
its type and the interface that persists it; `internal/store` imports those
types and implements the interfaces (`learn.Store`/`OutcomeStore`,
`memory.Store`, `problems.Store`, `sysmap/model.Store`, `events.PollState`,
`queue.Persister`, `meter.Store` + `meter.ClaudeRun`/`RunFilter`,
`diag.Index` + `diagindex.Row`, `pipeline.CIMainStore` + `core.CIMain` for the CI digest,
`labels.Store`, `review.MRFlags`). Store
exports no record types of its own, and only `cmd/reviewbot` imports it
(features, domain, app and web packages may not; enforced by
`.go-arch-lint.yml`). When a record's owner is too heavy for the store to
import (sysmap, diag), its types live in a leaf sub-package and the owner
aliases them. GitLab size/report sentinels the features match on are core
errors (`core.ErrTooLarge`, `core.ErrReportNeedsArchive`; the client's
`gitlab.ErrTooLarge` is the same value).
Optional record stores are typed `review.Deps` fields (`Learnings`, `Outcomes`,
`Memory`, `MRFlags`, `CIMain`, `LabelStore`), never type assertions on
`Deps.Store`: `cmd/reviewbot` wires `*store.SQLite` into each, so a missing
method fails to compile (`capabilities_test.go` asserts them too); a nil
field turns its feature off. `cmd/reviewbot` names the collaborator types of
`review.Deps` from their packages (`task.Source`, `crossrepo.ContextRepos`,
`pipeline.CIMain`, `cijob.Artifacts`, `labels.Client`, `env.Moderator`, …);
`review` re-exports none of them.

**Tests of internal/review.** Tests that drive a whole reviewer use
`internal/review/reviewtest` (`reviewtest.New(t)`: fakes of GitLab, repos,
runner, store, limiter, diff index, compat checker; the Reviewer is built
from `h.Deps` through `review.New` on the first `h.R()`). They live with
what they exercise as external test packages (`pipeline_test`,
`verify_test`, …, files `*_reviewer_test.go`); tests spanning several
features, the golden prompt tests (`internal/review/testdata`, flag
`-update-golden`) and the run's JSON schema stay in `review_test`. Tests set
collaborators through `review.Deps`, never by assigning feature fields; a
feature of the running reviewer is reached through `Reviewer.Internals()`
(documented test support) and the orchestrator's unexported methods through
`review/export_test.go`. Pure tests of unexported code stay internal
(`*_internal_test.go` in review, the features' own `*_test.go`).
`reviewtest` is excluded from go-arch-lint like `_test.go` files, which still
reports any production import of it.

### Job kinds: one table

Per-kind metadata lives in one table, `core.KindInfo` / `jobKinds` in
`internal/core/job.go`: `Auto` (`Manual()`), `Usage` (limiter usage kind,
`review/env.JobUsage`), `RunKind` (`meter.KindForJob`: /usage and metrics
labels), `Reacts` (the 👀 → ✅/❌ lifecycle, `review/reactions.go`), `Label`
(/runs), `Commands`/`MRCommand`/`CommandReason` (command parsing, `internal/events`)
and `Help` (the "@bot help" lines; the order of the help groups stays in
`summary.CommandsList`, `Reviewer.mrCommandsList` and learning's
`RulesCommandsList`/`MemoryCommandsList`). `AllJobKinds()` is the table's
order. A new kind = its constant, one table entry and its case in the
`Reviewer.Handle` switch; `core/kinds_test.go` requires a complete entry for
every constant.

### Adding a feature, a job kind or an event source

**A review feature** (a new step of the review or a new kind of thread):

1. `internal/review/<name>/`: a package doc, a `Feature` type and its
   `New(...)` taking `*env.Env`, `*publish.Publisher` (if it writes notes),
   the lower features it calls and its own collaborators. It imports leaves,
   `env`/`publish`/`jobctx`, domain packages and lower features only; when it
   needs the orchestrator, declare an interface in the feature and implement
   it in `internal/review/callbacks.go`.
2. `.go-arch-lint.yml`: an `f-<name>` component with the edges it needs, and
   `f-<name>` in the `mayDependOn` of the orchestrator (and of any feature
   that calls it).
3. `internal/review/review.go`: its collaborators as `Deps` fields (typed,
   nil = off), a `Reviewer` field and the `New` call; `internals.go`: the
   field in `Internals` for the tests.
4. The flow: call its steps from `flow.go` / `analyzers.go` (a concurrent
   analyzer step with its dependencies), `review_prompt.go` (prompt
   sections, schema), `review_publish.go` / `review_summary.go`; a config row
   in `commands_config.go`.
5. `cmd/reviewbot`: wire the new `Deps` fields (`serve.go`, `deps.go`).
6. Tests: `package <name>_test` files `*_reviewer_test.go` on
   `reviewtest` (add fakes there when needed), pure tests next to the code.
7. Docs: a section in this file and the user docs (`docs/features/`).

**A job kind:** the `core.JobKind` constant and its `jobKinds` entry in
`internal/core/job.go` (usage, run kind, reactions, label, command phrases,
help lines; `core/kinds_test.go` checks it), its case in `Reviewer.Handle`
(`internal/review/review.go`) calling the handler (in the orchestrator, or
`HandleX` of the feature that owns it), and how it is produced: command
phrases are parsed by `internal/events` from the table, other kinds need a
rule in `internal/events` (`router.go`, `note.go`, `pipeline.go`). A new run
kind also needs its `meter.Kind*` value.

**An event source** (next to the webhook and the poller): a package in the
ingress layer that turns its input into `events.MREvent` / `NoteEvent` /
`PipelineEvent` and hands them to the shared `events.Router` (`MR`, `Note`,
`Pipeline`), so drafts, commands and the groups filter behave the same.
Start it in `startEventSources` (`cmd/reviewbot/serve.go`) with the shared
`enqueue` there (the event dedup `poller.Dedup` around
`reviewer.Accepted(q.Enqueue)`: 👀 at enqueue time); persisted state of the
source belongs to `internal/events` (like `PollState`) and is implemented by
`internal/store`.
Add the package to the `ingress` component in `.go-arch-lint.yml`.

## Cross-package contracts beyond internal/core

### internal/diffmap

```go
func Build(diffs []core.FileDiff) *Map
// Anchor returns a GitLab text position for a NEW-file line when that line
// is visible in the MR diff (added line -> NewLine only; context line ->
// OldLine and NewLine both, as GitLab requires). Uses diff OldPath/NewPath.
func (m *Map) Anchor(refs core.DiffRefs, path string, newLine int) (*core.Position, bool)
// Snap returns the closest visible new-file line within maxDist of newLine.
func (m *Map) Snap(path string, newLine, maxDist int) (int, bool)
func (m *Map) HasFile(path string) bool
func (m *Map) Files() []string
```

### Constructors (used by cmd/reviewbot)

```go
gitlab.New(baseURL, token string, opts ...gitlab.Option) (*gitlab.Client, error)
gitlab.WithHTTPClient(*http.Client) gitlab.Option
gitlab.WithCAFile(path string) gitlab.Option

store.OpenSQLite(path string) (*store.SQLite, error)
limits.New(st core.Store, globalPerHour int, exemptUsernames []string, now func() time.Time) *limits.Limiter
queue.New(queue.Options{Concurrency int; Debounce time.Duration; Logger *slog.Logger; Persist queue.Persister;
    ShutdownGrace, ManualMaxAge time.Duration; OnRestore func(core.JobKind, string);
    OnDrop func(core.Job, string)}) *queue.Queue   // OnDrop: answered/expired rows dropped on restore (reactions cleanup)
queue.Persister{SavePending, DeletePending, ListPending,   // auto reviews (queue_pending_v1)
    AddJob, UpdateJob, FinishJob, ListJobs, ListDone}       // every other job (queue_jobs_v1, queue_done_v1); implemented by *store.SQLite
(*queue.Queue).Restore() []core.Job   // load persisted jobs now; the result (queued, dropped, finished within DoneTTL) seeds the event dedup
(*queue.Queue).Abort()                // second signal: cancel running jobs of a drain now
queue.WrapLimiter(core.Limiter) core.Limiter  // marks a job charged; admitted (charged) jobs skip user limits

repo.New(repo.Options{BaseDir, GitLabURL, Token string; Logger *slog.Logger}) (*repo.Manager, error)
claude.NewRunner(claude.Options{...}) (core.Runner, error)   // see internal/claude
egress.NewProxy(allow []string, logger *slog.Logger) http.Handler

events.NewRouter(events.Options{BotUsername, Mention string; Groups []string; LearnReplies, ReactThanks, ReactAck, Pipelines bool; Excluded func(path string) bool; MainBranch func(path, defaultBranch string) string; Now}) *events.Router   // shared by the webhook and the pollers
webhook.NewHandler(webhook.Options{Secret string; Router *events.Router; Enqueue func(core.Job); Logger *slog.Logger}) http.Handler
poller.NewPipelines(poller.PipelineOptions{Source, State, Router, Enqueue, Groups, Interval, ...}) *poller.PipelinePoller   // review.ci with polling
reactions.New(reactions.Options{API, BotID, BotUsername, Observe, OnForbidden, Step, ...}) *reactions.Reactor
(*review.Reviewer).Accepted(next func(core.Job)) func(core.Job)   // 👀 at enqueue, wraps q.Enqueue
hooksync.New(hooksync.Options{API, Groups, URL, Secret, SSLVerify, DryRun, Logger}) *hooksync.Syncer
review.New(review.Deps{...}) *review.Reviewer   // (*Reviewer).Handle is a core.Handler
```

## Markers in GitLab notes

All bot notes carry an HTML-comment marker so state can be rebuilt from GitLab.
Markers are written, and recognized, only in the leading lines of a note that
are HTML comments (`marker.Header`); the rest of a note carries untrusted text
(Claude's prose, linter and checker output), and Claude's text fields are
stripped of `reviewbot:` comments before rendering.

The summary is the exception: its blocks (CI, CI linters and their pending
list, coverage, TBD, diagram, title hint, problems, verify, system map) are
found again by regular expressions over the whole body, so no untrusted text
in it may open an HTML comment at all. Every `<!--` of a path, branch, job
name, rule, tool message, MR title or model text is broken with U+200B
(`mdtext.DefuseComments`) at three chokepoints: `summary.Render` defuses all
it writes itself (the model's summary, walkthrough, omitted files, findings
outside the diff); every summary block is built with `summary.Block(start,
content, end)`, which defuses the content (the bot's own markers inside a
block, such as `cilint-pending` and the JSON `title-suggestion`, go with start
or end); the plain sections (secrets, lint, compat, API changes, task, team
rules, path instructions, missing related repositories) defuse their output.
The code-span and table-cell helpers (`mdtext.InlineCode`, `Code`,
`CodeSpan`, `CodeLoc`, `CodeCell`, `FileLineRU`, `TableCell`) defuse too, and
values go into code spans only through them (`mdtext.Code` drops backticks,
so a branch name like ``main`@all`x`` cannot close the span and mention
anybody). Regression test: `TestSummaryHostileTextCannotFakeBotMarkers`.

- Summary note: `<!-- reviewbot:summary sha=<head sha> -->`
- Finding thread (first note): `<!-- reviewbot:finding id=<id> -->`
  where `id` = first 12 hex chars of sha1(path + "\n" + lowercased trimmed title).
- Merge-blocking thread: `<!-- reviewbot:blocking -->` (findings, compat,
  blocking lint, the size thread). `scanDiscussions` counts every open bot
  thread with it in the marker header: findings / compat / lint by kind, the
  size thread as `openSizeBlocking`, any other kind as `openOtherBlocking`;
  every gate recount (`gateCount`: "@bot check", the CI lint refresh, the
  "nothing to review" path) agrees with the review's gate line.
- Breaking-change thread (first note): `<!-- reviewbot:compat id=<id> source=<source> -->`
  where source is `oasdiff|buf|graphql-inspector|custom|claude` and `id` =
  first 12 hex chars of sha1(source + "\n" + path + "\n" + lowercased key):
  key = rule + "\n" + (oasdiff fingerprint, or the normalized message;
  "command" for custom commands) for tools, the title for Claude.
- Bot "fixed" reply: `<!-- reviewbot:fixed -->` (thread no longer open for the bot).
  A blocking thread with that reply that is still unresolved in GitLab (Reporter
  bot, no service token) still counts in the gate line ("из них M исправлены,
  ждут закрытия"; `mrthread.Threads.FixedPendingBlocking`, `markFixed` returns
  `pending`). A size thread the bot could not read or resolve (another
  GitLab error) counts as open, not as "исправлены, ждут закрытия"
  (`sizeCount`, review/bigmr_thread.go).
- Diagram section of the summary: `<!-- reviewbot:diagram -->` … `<!-- /reviewbot:diagram -->`
  around a ```mermaid block (re-used by incremental reviews without a new diagram).
- System map block of the summary: `<!-- reviewbot:sysmap -->` … `<!-- /reviewbot:sysmap -->`
  (consumers of the contracts the MR touches; re-rendered by every review).
- System map phase B block of the summary: `<!-- reviewbot:sysmap-deps -->` …
  `<!-- /reviewbot:sysmap-deps -->` (undeployed dependencies and linked open MRs in
  other repositories; right after the sysmap block; re-rendered by every review;
  never a thread, never blocking).

- Verification block of the summary: `<!-- reviewbot:verify -->` …
  `<!-- /reviewbot:verify -->` ("🧪 Перепроверка" counts, rejected and
  downgraded findings; re-rendered by every review that verified findings).

- Verification problems block of the summary: `<!-- reviewbot:problems -->` …
  `<!-- /reviewbot:problems -->` (this run's problems, after the TBD and coverage
  blocks, before the system map; re-rendered by every review). The separate note of
  `notify_mode: note` starts with `<!-- reviewbot:problems-notify -->`.

- Repository config note (one per MR, general note, updated in place):
  `<!-- reviewbot:config-error state=error|ok -->` (`marker.ConfigNoteRe`,
  review/repoconfig.go). Never a thread, never blocking.

- Size limit block of the summary: `<!-- reviewbot:too-big -->` …
  `<!-- /reviewbot:too-big -->` (only in the summary of a skipped review). The
  merge-blocking size thread (`review.big_mr.block`) starts with
  `<!-- reviewbot:too-big-gate -->` and `<!-- reviewbot:blocking -->`.

- CI linters block in the summary: `<!-- reviewbot:cilint -->` …
  `<!-- reviewbot:cilint-pending ci:a,ci:b -->` `<!-- /reviewbot:cilint -->`
  (the pending list drives the refresh).
- CI failure block of the summary: `<!-- reviewbot:ci pipeline=<id> sha=<sha> fp=<fingerprint> -->` …
  (`fp` optional when parsing: blocks written before it lack it)
  `<!-- /reviewbot:ci -->` (the failed head pipeline; kept by a re-rendered summary
  of the same head, removed when a later pipeline passes).
- CI explanation thread (first note): `<!-- reviewbot:ci-explain pipeline=<id>
  fp=<16 hex|-> explained=<pipeline id|0> state=red|green -->` (not blocking; the
  newest unresolved red one is `mrthread.Threads.CI`).
- Main branch note in the culprit MR: `<!-- reviewbot:ci-main pipeline=<id> state=red|green -->`.
- Lint thread (first note): `<!-- reviewbot:lint id=<id> tool=<tool> -->` (tool
  `gitleaks`, `todo` or `ci:<name>`; `[a-z0-9:._-]`) where
  `id` = first 12 hex chars of sha1(tool + "\n" + rule + "\n" + path + "\n" +
  lowercased message with digits replaced by `N`); for gitleaks the "message"
  is `commit <sha12> line <n>` (a commit's lines never move). For the todo
  check the rule is the keyword (`TODO`, …) or `task-not-found` and the
  message is `<KW> без задачи — «<comment from the keyword on>»`, so the id is
  file + normalized comment text (line moves keep the thread).

## Review flow (internal/review)

1. Re-read the MR. Skip unless `state == opened`.
2. Draft: auto -> skip silently; `@bot review`/`full review` -> reply that the MR is a draft.
3. Repository config gate (`reviewConfigGate`, review/repoconfig.go, see
   "Repository config: opt-in and validation" below): `.reviewbot.yaml` of the
   **target branch** (cached `GetRawFile`), validated (`config.ValidateRepo`);
   not opted in or invalid -> skip (commands get a reply and 🚫); the config
   note is kept up to date. Merge with service config (`config.Merge`).
   Disabled -> skip (commands get a reply).
   Auto reviews only: MR paused (`@bot pause`, `mr_flags`) or the MR
   description contains `@bot ignore` / `@bot игнорировать` -> skip (log).
4. Manual commands: require access level >= Developer (the per-user limits
   are checked in step 7). An admitted attempt (`core.RunAdmitted`: the
   queue's run state says an earlier attempt of this job was charged, then
   postponed by `RetryLater` or interrupted and replayed) skips the check and
   the per-user limits (see "Queue durability", Limits).
5. Decide the diff range. Full review or no state -> `DiffRefs.BaseSHA` (merge base; StartSHA is the target tip, not an ancestor of head).
   Incremental -> `ReviewedSHA` if it is an ancestor of head, else start SHA.
   `ReviewedSHA == head` -> auto: skip; manual: reply "no new commits"
   (no usage spent).
6. Checkout, compute diff (ignore globs). Empty range -> close an open size
   thread (`closeSizeThreadNoChanges`), save the head, then (a command) reply
   "нет файлов для ревью" — a failed reply is only logged, the job does not
   fail — and return. Size limit
   (`checkMRSize`, review/bigmr.go): changed lines of the range (`diffSize` =
   `countDiffLines` per file, ignore globs, the TBD count) over
   `review.max_mr_lines` and no override (`JobForceReview`, the
   `review.big_mr.override_label` label) -> `reviewTooBig` and return: no
   Claude run of any kind and no booking (the size gate is decided before the
   limits, so a too-big MR is never postponed or denied by them), the reviewed
   head is not advanced (see "Size limit" below).
6a. Trivial increment (`trivialIncrement`, review/trivial.go; before the size
   gate and the limits): an AUTOMATIC incremental review
   (`JobAutoReview`, `review.skip_trivial_increments`, default true) whose
   every changed file (ignore globs applied) is a documentation file
   (`trivialDocGlobs`: md/markdown/rst/adoc/asciidoc) or has the same code at
   `from` and head (`lint.CompareCode`: the todo comment lexer, comments
   dropped, whitespace normalized per language — C-like: dropped between
   tokens unless word/word or operator/operator, a line break between words
   kept; Python/layout languages: per line, indentation kept; everything else
   (YAML with whole-line comments only, shell, Makefile, Ruby, HTML, config):
   per line, indentation kept, inner runs collapsed; string literals verbatim;
   unknown types, JSON, .txt never equivalent). `CompareCode` is fail-closed
   (lint/trivial.go header): a file where the lexer guessed (unterminated
   quote, comment starter after `\` or `$`, heredoc, Lua/CMake long
   brackets, SQL `$$`, PHP `?>`, `__END__`, raw strings with `#"`, a
   docstring followed by code, a block/string/bracket open at EOF) is never
   equivalent; Python triple-quoted strings are comments only as a statement
   of their own (depth 0, no `\` continuation, no f/b prefix); "* " lines
   count as docblock lines only after a gap (todo diff fallback); comments a
   tool reads (`sensitiveComment`: no space after the opener as in
   `//go:build`/`#!`/`/*!`, a leading `!+@$<[/`, "key: value", `@Name`,
   directive words like noqa/type:/eslint-/pragma/syntax=, doctests, code
   fences, template tags, a trailing `\`) are compared verbatim as code
   together with their comment group (no code or blank line between);
   cgo files, HTML with `<script>`/`<style>`, JSX, Vue/Svelte/Astro, MDX,
   JSP, TeX compare every comment as code, Twig/Handlebars their `<!--`
   comments; Dockerfile/config files and YAML have whole-line `#` comments
   only, YAML block scalars and quotes inside plain scalars are text) ->
   `reviewTrivialIncrement`: no
   Claude run, no booking; gitleaks + todo (`runTrivialLint`) and the CI
   linter reports (`addCILint`), lint threads published / closed; the summary
   keeps its body and gets the status line "ℹ️ Инкремент `a..b` без значимых
   изменений (только …) — ревью не требуется" (`summary.IsStatusLine`, replaced by a
   later status, dropped by the next full render), the marker sha, gate line
   (`gateCount` after a rescan), TBD block (when present), CI lint block and
   problems block refreshed; the TBD/CI refresh chain is started; head saved
   (threads kept). Never trivial: new/deleted/renamed/copied/binary files,
   mode changes, identical bytes (unexpected), > 50 files, an unreadable
   version. Manual reviews and full reviews never take this path.
   `OnSkip("trivial_increment")`; the log line "trivial increment, claude
   review skipped" makes the `/runs` record `skipped`.
7. `Limiter.Reserve` (after the size gate, before any Claude run): the
   per-user limits (manual: UsageRerun with the MR cooldown) and the global
   limit are checked and one run is booked in one step under a mutex, so
   concurrent jobs cannot both take the last slot. A booking counts toward
   every limit (in memory) until the job's first Claude run commits it
   (`runStructured` records it then) or the job ends without a run
   (`Release`, deferred). Every later Claude run of the job (verification,
   the invalid-JSON retry, the MCP fallback) is recorded anew (`env.charge`):
   for a job with a user without the user and the MR (`UserID 0`,
   `MRKey ""`), so the per-user windows and the MR cooldown count the job
   once while the global window counts every run — the same rule as a
   replay; automatic jobs' runs are recorded as they are. Denied -> auto: return `*core.RetryLater`; manual:
   reply with reason and time. Then cap by `max_diff_bytes` / `max_files`
   (too large -> review the largest-impact subset and say so in the summary).
8. Collect open (unresolved) bot finding threads with their ids.
9. Analyzers (`runAnalyzers`, review/analyzers.go): export the merge base for
   Claude (`prepareBase`, review/env/base.go, see "The code before the MR (/base)"
   below), compat / API changelog, lint (+ CI linter reports), Jira, reviewer
   assignment, context repositories, coverage, system map A and B, migrations.
   None fails the review. With `review.parallel_analyzers` (default true) they
   run concurrently in a dependency-aware step group (review/parallel.go,
   stdlib only, at most 4 at a time): sysmap A waits for compat (API
   endpoints), context and the sysmap diff; sysmap B for sysmap A, task and
   context; migrations for the sysmap diff. Each step runs with the job's
   context (its spans and diag steps nest under the review span); its bot
   problems are buffered and added in the sequential order after the group
   (the summary's problem order does not depend on timing); a panic is
   recovered in the goroutine and re-raised on the job goroutine after every
   step finished (resources of the other steps are released). A cancelled
   job returns `ctx.Err()` after the group. Shared state: each step writes its
   own result only; reviewer caches have locks; `safefs.ExcludeFromGit` is
   serialized; the GitLab memo, problems collector, diag runs and spans are
   concurrency-safe. `false` runs the steps one by one in the same order.
9a. Compat (when `review.compat_check` and repo `compat.enabled`): see
    "Backward compatibility" below. Never fails the review. With the check
    off, only the OpenAPI changelog runs (`compat.Request.SummaryOnly`) for
    the "📘 Изменения API" summary.
9b. MCP (when `review.mcp.enabled`, on by default, and repo `mcp.enabled`,
    and the server's fallback breaker is closed): open the run's MCP session
    over the analyzer results (see "MCP server" below); the big prompt
    sections become one-line summaries and the run request carries the MCP
    endpoint. The request is built by a closure (`buildReview(ms)`) so the
    fallback can build it again without MCP. The light route (9c) opens no
    MCP session.
9c. Model route (`routeReview`, review/route.go, `review.routing` + repo
    `routing`, see "Models" below): light or normal, decided after every
    analyzer (lint, compat/API, migrations, system map) and before MCP; both
    the first run and the MCP fallback get it (`withRoute` in `buildReview`).
10. Commit the booking (later runs of the job: `Limiter.Record`, global
    only, see step 7), then `Runner.Run` with the review prompt + JSON schema.
    `*core.ErrClaudeRateLimited` -> `*core.RetryLater` (`env.RetryAt`: the
    reset time, `DefaultBackoff` 30m when unknown or past, at most
    `MaxRetryWait` 24h ahead — a later reset, bogus or a weekly limit, is
    tried again after a day),
    and note "review postponed" in the summary.
10a. Post-run (on the parsed answer): `enforceMigrations` (migration
    evidence), `dropLintDuplicates`, then ONE verification run
    (`verifyBeforePublish`, see "Verification before publishing" below) for
    the new critical/major findings and Claude's new breaking items.
11. Publish: delete leftover drafts; each finding -> `diffmap.Anchor`
    (or `Snap` within 3 lines) -> draft note with marker, severity badge, body
    and optional ```suggestion:-0+N``` block; skip duplicates of open threads
    (same id). Unanchorable findings go to the summary ("Замечания вне диффа").
    Then `PublishDraftNotes`. Every non-CI thread batch (findings, compat,
    lint — also in the too-big review and the CI lint refresh — and the size
    thread) goes through `publishDrafts`: delete leftover drafts of an
    interrupted job (`PublishDraftNotes` publishes all of the bot's drafts),
    create, publish; a failed publish deletes the batch again.
    **Fail closed:** a BLOCKING result whose thread could not be created or
    published (a finding, a breaking change, a blocking lint result — also
    when the lint thread scan failed — or the size thread) still counts in
    the gate line (`reviewTally.unpublished`, `compatcheck.Run.Unpublished`,
    `linters.Run.UnpublishedBlocking`, `sizeCount.unpublished`), the summary
    says so right after the gate line ("⚠️ Не удалось опубликовать N
    блокирующих тредов …"; the findings stay listed under "Замечания вне
    диффа") and step 14 keeps the reviewed head. A non-blocking one is only
    listed. Nothing re-runs the review by itself: the next push or command
    reviews the same range again (no retry loop, no extra Claude run).
11a. Compat: each breaking change (deterministic, or Claude's after the
    verification of step 10a) -> its own blocking draft note (inline or
    general).
12. Resolved: for every id Claude reports fixed and which is open -> reply with
    the comment and resolve. Same for fixed compat threads. Only where the
    run could see the fix (review/resolve_scope.go): the finding's file
    (inline position, or the "_Файл `path`_" line of a general thread) must
    be a file of the reviewed range that Claude was shown (the diff cap's
    omitted files excluded; old paths of renamed/copied files included), and
    the light route never closes a blocking finding or a breaking change.
    Anything else is ignored (logged) and stays open: closing it would rest
    on the model's word alone, or on text planted in the diff.
12a. Labels (`review.labels`, see "Automatic MR labels"): after the TBD
    status is collected, before the problems block; never fails the review.
12b. Description summary (`review.description_summary`, review/descsum,
    service AND repo, default off): the review schema has a required
    `description_summary` {overview, groups[{kind new|fix|refactor|test|
    docs|other, items}]} and the system prompt its rules. `descsum.Apply`
    renders the block (escaped one-line items, links/mentions/refs broken,
    quick actions neutralized, comments defused; caps 400/200 runes, 8 items,
    6000 bytes), reads the MR again (`GetMR`), skips drafts / not opened,
    `descsum.Merge`s it into the description (replace between
    `<!-- reviewbot:description-summary -->` markers or append; never when
    the author's text has `<!-- reviewbot:description-summary-off -->`,
    broken markers, a quick-action line, or the block the bot wrote before
    was removed — the summary note keeps `<!-- reviewbot:description-summary-written -->`)
    and PUTs it only when changed and <= 1 MB (`gitlab.UpdateMRDescription`,
    `WriteDescription`: body = description only): bot token, then service
    token on 403/404 (a refused token is skipped for an hour); a failure is
    `description_summary_failed`, never a failed review.
13. Create or update the summary note (found by marker or MRState).
14. Save MRState{ReviewedSHA: head} — the previous reviewed head instead when
    a blocking thread could not be published (step 11).

Check (`@bot check`, see "MR commands"): re-check open findings on the
current head without a new commit; the same `markFixed` / outcome / gate
line as steps 12-13, one small run.

Chat (`@bot <question>`): Developer+ check, `Reserve` (UsageChat per-user
limit + global, booked), checkout head, merge base export (/base), prompt with
the thread + MR context, read-only tools, reply in the same discussion; if the
thread is a bot finding and `resolve: true`, resolve it.

Help: reply with commands and `Limiter.Remaining`.

### Per-job memo of GitLab reads (review/glmemo.go)

`review.New` wraps `Deps.GitLab` in `memoGitLab` and `Deps.Moderator` in
`memoModerator`; `Handle` puts a fresh memo into every job's context
(`withGitLabMemo`). `GetMR`, `GetMRDiffs` and `ListDiscussions` of the same MR
are made once per job (concurrent misses share one call, errors are not
memoized, callers get copies). Every bot write to the MR (draft notes,
publish, notes, replies, resolve — bot or service token) drops the memoized
discussions before and after the call, so post-publish reads
(`resolveNonBlocking`, gate recounts, `setSummaryStatus`) see the new
threads; the MR and its diffs are not changed by those writes and stay.
`withFreshGitLab(ctx)` forces a real read and refreshes the memo:
`diffIndex` uses it for the "MR changed during the review" check (memoized
diffs are of the same version when the refs still match the job's start).
Optional capabilities stay reachable (`Unwrap`). Saved reads:
`Deps.OnGitLabSaved` -> `reviewbot_gitlab_reads_saved_total{op}`.

## Jira and MR title

Optional `jira` section (Jira Data Center / Server, REST v2, PAT in
`Authorization: Bearer`, env `JIRA_TOKEN`/`JIRA_URL`). The token stays in the
service; issue texts go to the prompt inside `<untrusted_jira_issues>`.

- Keys (`\b[A-Z][A-Z0-9_]+-\d+\b`, prefixes from `jira.project_keys`, any
  when empty) are collected from title > source branch > description > commit
  messages (`ListMRCommits`), deduped, at most 3 (`internal/tasklink`).
- Before the Claude run the reviewer fetches each issue (`review.Deps.Tasks`,
  nil when Jira is off): summary, description, status, type, last N comments,
  parent and epic (Epic Link custom field; summary + description, one level).
  Fetch errors are reported in the summary and never fail the review.
- The schema gains `task_alignment` (only when an issue was fetched) and
  `suggested_title` (only when the title is not Conventional Commits; the
  "MR title format" section of the user prompt is `config.TitleInstructions`:
  `jira.title_prompt` — repo over service, empty = `DefaultTitlePrompt` —
  with `{types}` / `{language}` replaced literally, in `jira.title_language`,
  default "en", independent of the review language). The
  summary note gets "📋 Соответствие задаче" and a title hint between
  `<!-- reviewbot:title-hint -->` … `<!-- /reviewbot:title-hint -->`; the block
  also stores Claude's suggestion as `<!-- reviewbot:title-suggestion {json} -->`.
- Expected title: `type(scope)!: описание (KEY-123)`; draft prefixes are
  stripped. The bot fills in the key part of the suggested title.
- MR update with `changes.title` and no `oldrev` -> `core.JobRefreshTitle`
  (drafts skipped). The handler re-reads the MR, recomputes the hint without
  Claude and without limits, and rewrites only the hint block of an existing
  summary note (does nothing if there is none). `JobKind.Manual()` is true for
  it, so it is routed explicitly and never enters the command flow.

## Team rules (learning from replies)

The bot learns review rules from people's replies in its threads across all
repositories and applies them to every later review and chat run.

- **Trigger.** A Note Hook on an MR by a non-bot user that does not mention
  the bot and is threaded (`object_attributes.type` is `DiscussionNote` or
  `DiffNote` and `discussion_id` is set) -> `core.JobThreadReply` (no
  debounce; `Manual()` is true but it is routed explicitly, never a command).
  The payload does not say whether the note is the first of its discussion,
  so the handler checks that via `GetDiscussion`: the note must not be the
  first one and the first note must be the bot's (finding, summary, any bot
  thread). Enqueued when `review.learn.enabled`, and also with learning off
  when the note is a pure thanks / acknowledgement and reactions are on (see
  "Reactions"); such notes are answered with an award emoji and never
  reach the extraction run.
- **Handler** (`internal/review/learning/learn.go`): skip trivial acknowledgements;
  repo `enabled` and `learn.enabled`; author `AccessLevel >= Developer`
  (otherwise silently ignored); `learn.Store.MarkNoteProcessed` (one
  extraction per note); `reserveLearn`: one `Limiter.Reserve` of a
  `core.UsageLearn` run (the author's) under the global limit and
  `review.learn.max_per_hour` (`ReserveRequest.KindPerHour`, booked runs
  count, so concurrent learning jobs cannot all take the last slot; denied
  -> skip, log only); `env.RunToolLess` (commits the booking, notes run
  problems, collects reads) with **`NoTools`**, an
  empty scratch dir under `<data_dir>/scratch`, `LearnSchema`
  (`{learn, rule, scope, reason}`) and model `review.learn.model` or
  `claude.light_model` (default `haiku`). `learn:true` -> rule scrubbed
  (tokens, URLs with credentials, keys), stored, and the bot replies
  "🧠 Запомнил #N (для всех репозиториев | для group/project): … Отменить:
  `@bot забудь #N`." `learn:false` -> nothing, no reply.
- **Chat.** When learning is on for the repository, the chat schema has an
  optional `learning {rule, scope}` and the chat system prompt asks to fill it
  on a reasoned disagreement, only in a thread the bot started (the same
  schema and system prompt for every thread: prompt caching). A returned rule
  is kept only when the thread was started by the bot (`chatLearnOn`) and is
  stored the same way and the confirmation is appended to the reply (no extra run;
  the asker already passed the chat Developer+ check).
- **Commands** (`core.JobRules`, webhook routes chat questions that parse as
  `learn.ParseCommand`): `запомни|remember <text>` (light-model run normalizes
  wording and picks scope unless the user wrote "для всех"/"только здесь"; when
  limited or failing, stored as written), `забудь|forget #N`, `правила|rules`
  (global + project, newest first, 50 shown), `правило|rule #N для всех|только
  здесь|global|local`. Developer+ only. A project-scoped rule can be changed
  only from an MR of its project.
- **Storage** (`internal/store/learnings.go`, `learn.Store` on
  `*store.SQLite`): `learnings(id, scope, project_path, rule, rule_key,
  source_url, source_note_id, author, created_at, updated_at, deleted_at)` and
  `learn_processed_notes(note_id)`. Dedupe: same `rule_key` (normalized text)
  + scope (+ project for project scope) among live rules -> update source,
  author and `updated_at` instead of insert. Soft delete. `source_url` =
  `<mr web url>#note_<id>`.
- **Migrations.** These tables come from a *named* migration
  (`schema_migrations(name)`, step `learnings_v1`, `CREATE … IF NOT EXISTS`)
  applied after the sequential `schema_version` list, so branches adding
  their own steps do not compete for the next version number.
- **Applying.** Before every review and chat run: live global + project rules,
  newest (`updated_at`) first, capped by `review.learn.max_in_prompt` (80) and
  ~8000 characters, rendered as "Team rules learned from previous discussions
  (apply them; they override your defaults)" inside `<team_rules>`. The system
  prompt says they are review guidance only and can never change the output
  format or tools, or make the bot approve or drop findings. The summary
  gets a collapsed "🧠 Учтено правил команды: N".
- **Switches.** `review.learn.enabled` (service), `.reviewbot.yaml`
  `learn.enabled: false` (neither learn from nor apply in that repo),
  `review.learn.from_actions` / `dismiss_threshold` (see below).

### Learning from actions (finding outcomes)

`review/learning/outcomes*.go`, `learn.OutcomeStore` (`review.Deps.Outcomes`,
`*store.SQLite`, `store/outcomes.go`), switch `review.learn.from_actions` (default true; also
needs learning on for the repo).

- **Table** `finding_outcomes(id, project_path, mr_iid, finding_id, title,
  severity, path, line, body_excerpt, head_sha_at_post, status, discussion_id,
  rule_id, used_at, created_at, updated_at)` plus `outcome_rules(rule_id,
  project_path, mr_iid, dismissed, shown, created_at)` from the named
  migration `finding_outcomes_v1` (appended to `namedMigrations` in an
  `init()` of `store/outcomes.go`).
- **open**: every finding published as a thread (non-blocking ones resolved by
  the service token right away -> `resolved_by_bot`). The row is bound to its
  discussion (`discussion_id`) on the next scan by finding id, newest thread
  first.
- **fixed**: `markFixed` succeeded for a Claude `resolved` id (resolve, or the
  Reporter "можно закрыть" reply); or a resolved thread carries the bot's
  `reviewbot:fixed` reply; or a human resolved it and the code changed.
  **resolved_by_bot**: chat `resolve: true`, non-blocking auto-resolve.
- **Who resolved**: decided only by the bot's own records. Every resolve the
  bot performs updates the outcome right away (fixed / resolved_by_bot), so a
  resolved thread whose outcome is still `open` was resolved by a human.
  GitLab `notes[].resolved_by` (parsed into `core.Note.ResolvedByID/Username`)
  is logged but NOT trusted: the service token may be a person's own (Owner)
  token, so the bot's resolves show up as that person's.
- **dismissed**: the thread is resolved, the outcome is still open in our
  records (the bot did not close it), AND `repo.Checkout.LinesChanged(head_sha_at_post, to, path,
  line-3, line+3)` is false (`git diff -U0 --no-renames` with the forced safe
  git config, hunk old-side ranges overlap / pure insertions inside the
  window; line 0 = whole file). Changed -> fixed. Posted head missing (force
  push) -> `closed` (ignored). A rule learned from a human reply in a finding
  thread also marks the outcome dismissed and links it (`rule_id`, used).
- **When**: after publishing, in every review of an MR (`to` = the checkout
  HEAD), plus the open outcomes of up to 3 other MRs of the project, least
  recently checked first (`to` = their current head when the clone has it;
  covers threads resolved right before a merge).
- **Synthesis**: >= `review.learn.dismiss_threshold` (3) dismissed, unused
  outcomes in the project -> `reserveLearn` (global + `learn.max_per_hour`,
  one booking; denied -> skip, log only) -> `env.RunToolLess` (commits the
  `UsageLearn, UserID 0` booking; meter kind `learning`, charged to nobody)
  with `NoTools`, scratch dir, `SynthSchema` (`{rules: [{rule,
  scope, reason}] <= 3}`), `ExtractModel()`, last <= 20 dismissed findings
  inside `<untrusted_findings>`. Rules are cleaned (`learn.CleanRule`) and
  stored via `learn.Store.AddLearning` (source URL = the MR), outcomes marked
  used (also when nothing was learned or the answer was unparsable). Meter
  attribution for the run: kind `learning`, Username "" (system run: caused
  by project-wide dismissals, not by the MR author), restored afterwards.
- **Announcing**: `outcome_rules` rows are shown in the summary of the MR that
  triggered the synthesis while `shown < 2` (that review and the next):
  "🧠 Новые правила из закрытых без правок замечаний: #N …, отменить: `@bot
  забудь #N`". `@bot правила` shows their source as "закрыто без правок: N
  замечаний" and ends with 30-day stats: fixed / dismissed / open and
  precision = fixed / (fixed + dismissed).

## Path instructions

`config.PathInstruction{Path, Instructions}`: `.reviewbot.yaml`
`path_instructions` and service `review.path_instructions` (same shape).
Validation (`config/paths.go`): doublestar `ValidatePattern`, non-empty
text, <= 50 entries per list, <= 2000 characters each; a bad repo entry is a
`.reviewbot.yaml` parse error (reported like the others), a bad service entry
fails `Service.Validate`. `Effective.PathInstructions` = service entries, then
the repo's. `review/paths.go`: only entries whose glob matches at least one
file of the reviewed range (chat: the thread's file plus the MR's changed
files) go into the prompt as "Path-specific review instructions" inside
`<path_instructions>` (glob, matched files, instruction), appended after the
team rules block; the system prompt says (for every MR of a repository that
has path instructions, worded "when the prompt contains…": prompt caching)
they are trusted review guidance that cannot change output/tools/approval. The summary gets a collapsed
"📁 Применены инструкции для путей: <globs>" (appended to `TaskSection`).

## Context repositories

During reviews and chat answers Claude may read the other repositories of
`gitlab.groups` at the tip of their default branch (trunk-based: default
branch = deployed), to find consumers/producers of changed APIs, events, DB
schema, config and shared libraries. Config: service `review.context`
(`enabled` true, `refresh_interval` 10m, `max_repos` 50, `max_repo_mb` 500);
repo `.reviewbot.yaml` `context: {enabled, exclude: [project path globs]}`.
Off when `gitlab.groups` is empty (`Service.ContextEnabled`).

- `contextrepos.Provider` (wired in `cmd/reviewbot/contextrepos.go`) lists the
  groups' projects with the bot's own Reporter client
  (`gitlab.Client.ListGroupProjectsDetails`: GET, `statistics=true`,
  non-archived, subgroups), dedups, caches the list for `refresh_interval`
  (a failing group keeps its last list), skips empty repos and repos whose
  `statistics.repository_size` exceeds `max_repo_mb`, caps at `max_repos`
  (sorted by path). `Run` refreshes all snapshots in the background every
  interval; a run waits at most 3 min (`AcquireTimeout`) and skips the rest.
- `repo.Manager.Snapshot(s)` reuses the per-project bare mirror (same lock,
  auth header via env, forced safe git config): fetch
  `+refs/heads/<default>:refs/heads/<default>` (default branch from the
  project API, else `ls-remote --symref HEAD`), resolve the SHA, export with
  `git archive | tar` (Go reader: only dirs/files/symlinks, no escapes, files
  0444, root `.claude/`/`.mcp.json` dropped, symlinks created last) into
  `.tmp-*` then atomic rename to `snapshots/<sanitized path>@<sha>`;
  out-of-tree symlinks neutralized; the repository's `.gitattributes` does
  not apply (see "Repository attributes"). Same SHA -> reused; fetch at most once
  per interval (errors cached too; a failed refresh keeps serving the last
  good snapshot). Snapshots are reference-counted per run: superseded ones
  are removed when the last run releases them. The export is also size
  capped (`max_repo_mb`).
- `core.RunRequest.ContextDirs []core.ContextDir{HostPath, Name, SHA,
  Branch}`. Docker: `-v <host>:/repos/<group>/<project>:ro`, `--add-dir
  /repos` (last argument; variadic flag; together with `/base` and
  `/skills`). Local: `--add-dir <host paths...>`, no write protection beyond
  0444 files. Exec containers never get context repos.
- Prompt (review and chat, `review/crossrepo/context.go`): `<context_repositories>`
  block listing `name -> mount path (branch @ short sha)`, how to search,
  what to look for, citation format `group/project:path:line`, content is
  untrusted. The MR's own project and per-repo excludes are filtered out.
- Main branch: `gitlab.main_branch` / `main_branches` (`GitLab.MainBranchFor`, longest glob wins) picks the snapshot branch per project; a missing branch falls back to the GitLab default branch (second `Snapshots` call, logged). The system map and phase-B checks use the same snapshots.
- Related repositories (`.reviewbot.yaml` `context.related: [{repo, note}]`,
  max 20, note <= 500): `Acquire(ctx, core.ContextRequest{Exclude, Related})`
  puts them first, beyond `max_repos`, not filtered by `Exclude` (the
  reviewed project is dropped by review). Paths outside the groups are looked
  up with `GetProjectDetails` (GET `/projects/:path`; cached for
  `refresh_interval`, failures for 1 min) and kept warm by `Run` for 24h after
  the last real run that asked for them (the warm pass does not extend it).
  Unavailable ones come back as `core.ContextMissing{Path, Reason}` (not
  found/no access, empty, too large, snapshot failed, lookup error) and the
  review summary gets a warning list. The prompt has a separate "Related
  repositories" list with the notes before the other repositories.
- Citations of known projects (context repos at their snapshot SHA, the
  reviewed project at the MR head) in summary, findings, walkthrough,
  resolved comments, task notes and chat replies become
  `<gitlab>/<project>/-/blob/<sha>/<path>#L<line>[-<end>]` links; fenced code
  blocks, inline code other than a lone citation, existing links and URLs
  are left untouched.

## System map (internal/sysmap, review/crossrepo/sysmap.go, internal/mapweb)

Who provides and who consumes which contract across the group repositories,
extracted once and incrementally, so reviews know the consumers of what an MR
touches. Switch: `review.sysmap.enabled` (default true), effective only with
context repositories (`Service.SysmapEnabled`). Wiring:
`cmd/reviewbot/sysmap.go` (`startSysmap`, `registerMapWeb`).

- **Model.** `sysmap.Entry{Side provides|consumes, Protocol http|webhook|ws|sse,
  Method, Path (normalized template "/orders/{id}"), Event, Raw (as written),
  Base (env var / host expression), File, Line, EndLine, Note, Source
  openapi|code}`. Provides = serves an endpoint / receives webhooks / is the
  WS/SSE server or emits its events; consumes = calls / sends a webhook /
  connects / listens.
- **Scanner** (`Map.Run`: `Load`, 1 min delay, then `Pass` every
  `refresh_interval`). A pass: drop repos matching `review.sysmap.exclude`
  and repos absent from every non-empty acquisition for 24h (in-memory);
  `Snapshots.Acquire` (the contextrepos provider; held only while hashing and
  reading); repos whose SHA, status ok and version (`SpecVersion +
  CodeVersion`, or `+nocode` with a zero budget, plus the selection settings
  `+sel<max_files_per_repo>/<max_file_bytes>`) are unchanged are skipped (a
  state of an older build re-plans once; only files without a record of the
  same hash and extractor version are extracted);
  others are walked (`selectFiles`: code extensions, skip dirs, test/minified/
  generated names, `max_file_kb`, regex hint, `max_files_per_repo` by path; the
  snapshot's `.reviewbot.yaml` `sysmap.exclude`; OpenAPI by `compat.LooksLikeOpenAPI`).
  Specs are parsed right away (`ParseSpec`: goccy AST for lines, no `$ref`
  resolution, Swagger 2.0 `basePath`). Changed code files (hash or version
  differs from `sysmap_files`) are read while the character budget of the
  remaining runs lasts; the rest is "pending". After `release()`: batches
  (~80k chars / 25 files, a file at the size cap always fits) -> one tool-less
  run each (`NoTools`, empty scratch dir under `<data_dir>/scratch`,
  `ExtractSchema`, model `review.sysmap.model` (default sonnet) or `claude.light_model`, 5 min
  timeout, `Attribute` -> `meter.With(KindMap, ProjectPath)`), files numbered
  `N| ` inside `<untrusted_file path=...>`. `parseExtraction` keeps entries of
  batch files with valid lines/enums and a path or event, normalizes and caps
  strings. Run error -> files stay pending (retried); unusable answer -> file
  rows with `Error` (not retried until the content changes); rate limit ->
  pass stops. Every run is recorded in `sysmap_runs` (budget: runs of the last
  hour vs `max_runs_per_hour`; cost for the page). Repo state: ok when nothing
  is pending (then `Version` is set), else partial. `OnScan` feeds metrics.
- **Storage** (named migration `sysmap_v1`, `store/sysmap.go`):
  `sysmap_repos(repo, sha, branch, scanned_at, status, error, candidates,
  specs, pending, capped, too_large, version)`, `sysmap_files(repo, path, hash,
  version, entries JSON, error, extracted_at)`, `sysmap_runs(at, repo, files,
  ok, error, cost_usd, tokens, duration_ms)` (pruned after 400 days). Matching
  is not stored: `Map.Load` rebuilds an immutable `Index` (`sysmap.Build`)
  after every pass and at startup; readers take it from an atomic pointer.
- **Matching** (`Build`, deterministic): path key = `PathKey(NormalizePath)`
  (scheme/host/base expression and query dropped; `{id}`, `:id`, `<int:id>`,
  `${id}`, `%s`, `{0}`, Django groups, `[id]` -> `{}`; lower case; no trailing
  slash). 1) a hint (`review.sysmap.hints` key found as a whole token in Base
  or Raw, longest first) wins; 2) providers of the same family (http+webhook
  together, ws, sse) with a compatible method ("" matches any) and the same
  key, else the same after `StripPrefix` (`/api`, `/api/vN`, `/vN`) on both
  sides; generic keys ("/", only placeholders) never match; first of all a
  consumer whose file matches `review.sysmap.externals` (repo + doublestar
  globs; `externalByConfig`, before hints) is `external` with that system's
  name/host unless path matching finds exactly one provider repo or its own
  route; the topology skips files naming a third-party system
  (`thirdPartyPath`); spec endpoints
  of client repos (calls and no route in code: `dropClientSpecs`,
  `Index.clients`; also skipped as new endpoints in phase B) are dropped; provider
  entries of mock/stub/fake/fixture/example/test files (`mockPath`: test
  dirs, path words split at - _ .) are dropped when the index is built (old
  records too, no re-extraction) and are not new contracts in phase B; the
  consumer's own repo is no candidate when it has the route only in a spec
  (`SourceOpenAPI`) or when another repo is a candidate too; of several provider repos, the only one with the route
  in code wins; 3) exactly one
  provider repo -> resolved (own repo -> internal), else unresolved with the
  candidates. External: a consumer the model marked (`Entry.ExternalHost`, a
  public dotted host validated by `NormalizeHost`; private suffixes, IPs and
  single labels rejected) is `external` right after the hint check, without
  path/event matching; after both passes a consumer left unresolved with a
  literal http(s)/ws(s) URL of a public host in Raw/Base becomes `external`
  too, unless a resolved/internal consumer's URL has that host
  (`Index.ownHosts`; also for phase B) — covers records not re-extracted yet;
  hosts under `OwnDomain(gitlab.url)` (GitLab host minus its first label;
  `Options.GitLabURL`) are never external, the model's mark included;
  `CodeVersion` is `code-3` (code-2: the marks; code-3: literal event names,
  the same rule for senders and receivers) (a bump re-extracts every code
  file within `max_runs_per_hour` and invalidates the phase B cache, which is
  keyed by version and hash: paid). External calls make no
  edges, are not unresolved (metrics, home page) and are not attributed in
  phase B. Event-only consumers: the same event among the repos the
  consumer is connected to by path in that family (`pair`), else a specific
  event name (separator, >= 5 chars) only one repo provides (`event`).
  Events compare by `EventKey` (lower case, `.` `_` `-` space one separator;
  the name is kept as written). A webhook sent without a path with no exact
  provider: a receiver (webhook provider with a path) whose event is a
  whole-segment suffix of it or the other way round (`usage` / `llm.usage`),
  among the repos connected to the sender by a resolved path call either way
  (`Index.connected`), exactly one repo with exactly one such event
  (`event-suffix`); else unresolved.
  `Edges` aggregate resolved consumers per (from, to) and protocol.
- **Review** (`collectSysmap`, after context acquisition and coverage; nil
  `Deps.SystemMap` or the switch off -> nothing; panics recovered). Old-side
  line ranges of the full MR diff (`git diff merge-base..HEAD`; computed again
  for incremental reviews) + changed/removed endpoints of the OpenAPI
  changelog -> `Index.Impact(repo, ranges, endpoints)`: nil when the repo is
  not in the map. Touched = the repo's provides entries (and webhook sends)
  whose span [line, end_line or next entry - 1, max +60] overlaps a range,
  plus changelog endpoints; merged per contract key. Usages = other repos'
  consumers resolved to this repo with a matching key/event (or, for a sent
  webhook, the receivers in the target repo); possible = unresolved consumers
  whose candidates include this repo. Contracts without usages are dropped.
  Summary: `renderSysmapBlock` between the sysmap markers, prepended to
  `CompatSection` before coverage and TBD (so: TBD, coverage, sysmap, compat),
  collapsed above 6 usages or 3 contracts, citations `group/project:path:line`
  as inline code (linked by the citation linker), everything through
  `mdtext.CodeSpan`. User prompt (end, per-MR facts; not the system prompt: prompt
  caching): `renderSysmapPrompt` lists contracts and usages
  with `/repos/...` mount paths (only paths matching a safe character set; raw
  expressions never), asks to verify them first and to delegate to
  `code-search`.
- **Phase B** (`sysmap/deps.go`, `review/crossrepo/sysmapdeps.go`; `review.sysmap.deps`,
  default true; NEVER blocks: summary block + a user prompt section only, no threads, no
  blocking marker, no gate change; the prompt caps findings based on it at "major",
  never "critical"). `collectSysmapDeps` runs after `collectSysmap` on the same full
  MR diff (`sysmapDiff`: the reviewed diff, or merge-base..HEAD for incremental
  reviews). `parseDiffLines` gives exact added (new side) / removed (old side) lines,
  changed and deleted paths; changed paths passing `sysmap.DepsCandidate` (name-only
  prefilter) and not in the repo's `sysmap.exclude` (`Effective.SysmapExclude`) are
  read with `Checkout.ShowFile(head)` (at most 3 x `deps_max_files`).
  `Map.Dependencies(DepsRequest)`: OpenAPI files -> `ParseSpec`; code files -> size /
  NUL / `hintRe` (no hint = analysed, no entries), cap `deps_max_files`; results by
  content hash from `SysmapEntriesByHash(CodeVersion)` (= `sysmap_files` of any repo
  UNION `sysmap_cache`, migration `sysmap_cache_v1`, rows pruned after 30 days);
  the rest in batches via `extractWith` (prompt says "merge request head", same
  schema/model, attribution `meter.With(KindMap, project, MR)`, recorded in
  `sysmap_runs` with `kind = 'deps'` = counts toward `deps_max_runs_per_hour`, separate from the map's `max_runs_per_hour`), at most min(runs left, `deps_max_runs`) runs;
  results saved with `SaveSysmapCache`; budget exhausted / rate limit / run error ->
  those files are not analysed (`SkipReason`, logged). Entries on added lines (2 lines
  of slack above) absent from the repo's own default-branch entries (`repoHas`:
  family, compatible method, key or stripped key / event): provides -> `provides-new`;
  consumes -> `resolveNew`: `Index.matchPath` / `matchEvent` (Build's matching, kept
  tables), and for "no provider" reasons `baseTarget` (the repo's resolved/internal
  default-branch consumers with the same normalized Base map to exactly one other
  repo) -> `consumes-new` with `InProviderMain` (`repoHas(provider, Provides)`) and
  `ProviderComplete` (status ok). Unknown, ambiguous, internal, outside-the-map ->
  dropped. Default-branch resolved consumers on removed lines missing from the
  analysed head file (or the file deleted) -> `consumes-dropped`. `Consumers` = repos
  with edges to the reviewed one; `Branches` = default branch per repo.
  Review side: checks = consumes-new missing (only with a complete provider scan),
  other consumes-new / dropped (counterpart = provider), phase A touched contracts
  except sent webhooks (`provides-changed`, counterparts = usage repos), provides-new
  (counterparts = `Consumers`). Per counterpart repo (<= 8) one `OpenMRs.ListOpenMRs`
  (read-only GET, one page of `deps_max_mrs`, filtered to the repo's default
  branch), per MR one `GetMRDiffs`; all within `deps_max_api_calls`. Diff lines of
  code/spec files are matched with `contractRegexp` (stripped path key, placeholder
  segments = any placeholder syntax or digits, boundary after; quoted event name):
  only added -> adds, only removed -> removes, both -> changes. A provider MR adding a
  missing contract -> "добавляется в X (не смержен): мержить после него"; none ->
  "открытого MR, который его добавляет, не найдено"; budget out -> "не проверены".
  Other matches -> linked MRs with `relation` (role x action -> wording and
  `mergeOrder`; `combineOrder`: one clear order, conflict wins, mixed -> expand/
  contract). Jira (`depsShared`, <= 5 lookups, only MRs that matched): keys from the
  other MR's title/branch/description, `sharedTask` = same issue > parent story >
  epic (a parent equal to either epic counts as epic). Rendering:
  `renderDepsBlock` (sections ⏳ / 🔗 and one "🧭 Вне заявленной архитектуры" line for
  new consumers with `DepContract.OffTopology` — the repo has topology rules, none to
  that provider over that protocol; at most 5 named; not a routing fact, not in the
  prompt; collapsed above 4 lines, MR links built from
  `gitlab.url` + safe project path, titles via `mdtext.Inline`, names via `mdtext.CodeSpan`,
  `mdtext.NeutralizeQuickActions`), inserted before the compat section right after the
  sysmap block; `renderDepsPrompt` (normalized names, safe paths, MR refs, Jira keys;
  no titles or raw code).
- **Page** (`mapweb.Register`): `/map` and `/map/data.json` (repos with scan
  state and counts, edges, contracts <= 5000, unresolved <= 2000, external
  calls grouped by host (host, service, repos with counts, sites <= 2000 over
  all groups), `spec_only` on repos and their endpoints (provider entries
  only from OpenAPI/Swagger files, no route in code; shown as «только по
  спецификации», not hidden), hints, cost
  from `sysmap_runs`). `usageweb.Guard` with `MAP_TOKEN` ->
  `review.sysmap.token` -> usage token; 404 when the map is off. Embedded HTML
  reusing `webui.Assets()` (theme, DOM helpers, ECharts theming, filter bar) under
  a nonce CSP; a C4-like container diagram (`map.js`: SVG blocks per repository
  and per external system, one arrow per pair with parallel protocol lines,
  rule links thin; laid out by the embedded elkjs 0.9.3 — `webassets.ElkPath`,
  EPL-2.0, its in-thread worker, no eval — layered DOWN, orthogonal, edge
  labels placed by ELK, externals in partition 1; hover/focus highlight,
  Tab/Enter/Esc, a details panel built in the page from data.json); data
  inserted as text only; file links at
  the scanned SHA through `safeHref`.
- **Metrics**: `reviewbot_sysmap_scans_total{status}`, scrape-time
  `reviewbot_sysmap_repos{status}`, `reviewbot_sysmap_entries{side}`,
  `reviewbot_sysmap_unresolved`.

## Backward compatibility (internal/compat, review/compatcheck/compat.go)

Trunk-based development: old and new versions run side by side during a
rolling deploy, so a change must be compatible both ways; incompatible
changes block the merge and are shown explicitly.

- Switches: service `review.compat_check` (default true) AND repo
  `compat.enabled` (default true). Repo `compat.openapi/proto/graphql` add
  path globs. `compat.commands` (repository checker commands run in the MR
  checkout) was removed: a config with it is refused (`removedRepoKeys`,
  own reason), and open `custom` threads from before are never
  auto-resolved (`resolveFixedCompat`).
- Discovery: files changed `merge base..head`; OpenAPI by name
  (`**/openapi*.{yaml,yml,json}`, `**/swagger*...`) or by a top-level
  `openapi:`/`swagger:` key; `**/*.proto`; `**/*.graphql`, `**/*.graphqls`,
  `**/schema.gql` (only schema documents, not operations). New specs are
  ignored, deleted ones go to the prompt as data.
- Materialization: `Checkout.ShowFile` (`git cat-file blob`, no textconv)
  writes base files to `<clone>/.reviewbot-compat/old/<path>` and head files
  to `.../new/<path>` (git-excluded, removed afterwards). OpenAPI also gets
  sibling YAML/JSON files (relative `$ref`s); proto gets every `.proto` and
  buf config of the module.
- Tools run through `Runner.Exec` (sandbox image, `--network none`, no
  credentials), working dir = the scratch dir:
  - `oasdiff changelog --fail-on ERR --format json --lang ru|en
    --allow-external-refs=false|true [--auto-upgrade] old/<p> new/<p>`; exit
    0/1; JSON array of changes (same items and fingerprints as `oasdiff
    breaking`, plus INFO): `level` 3=ERR (published) / 2=WARN (summary only)
    / 1=INFO (API summary only), `revisionSource.line` -> anchor;
    101/102/103/123 -> "проверка не выполнена". `--lang` = repo language
    (ru, else en). `--auto-upgrade` only when the sides declare different
    3.x minors.
  - External `$ref`s (`compat/refs.go`): the host scans the materialized
    spec and the files it references (transitively). No external ref ->
    `--allow-external-refs=false`; only relative files inside the side dir
    -> `=true` (that flag also refuses relative files; the sandbox has no
    network); URL / absolute / `../` out of the tree / unparsable value
    (block scalar, tag, alias, escapes) -> oasdiff not run, issue.
  - Swagger 2.0 (`swagger:` top-level key, per side, detected on the host
    with `compat.SpecVersion`): oasdiff silently reports nothing for it, so
    `oas2to3 <side>/<p> <side>/<p>.reviewbot-oas3.yaml` (tools/oas2to3,
    separate module, kin-openapi openapi2conv, built in Dockerfile.sandbox)
    converts that side first. Converted NEW side -> every change at
    `Path=<p>, Line=0` (lines point into the converted file); converted OLD
    side -> its file name is mapped back to `<p>`. Exit 127 -> unavailable,
    other failures -> failed (never "no changes").
  - `buf breaking new/<root> --against old/<root> --error-format json` per
    module root (outermost dir with `buf.yaml`/`buf.work.yaml`, else `.` =
    default module); exit 100 + JSON lines `{path,start_line,type,message}`;
    `type: COMPILE` = build error (issue, not a change).
  - `graphql-inspector diff old/<p> new/<p>`; exit 1 + lines starting with
    `✖` = breaking.
  - custom commands in the clone with `REVIEWBOT_BASE_SHA/HEAD_SHA`; exit != 0
    = breaking (output in the thread).
  Exit 127 / "not found" = "проверка X недоступна" (never breaking).
- Claude: schema gains required `compatibility {breaking[], resolved[]}`;
  the system prompt explains TBD/expand-contract and that every item blocks;
  tool results go to the prompt inside `<untrusted_tool_results>`.
- Verification: Claude's NEW items (not already open threads) go to the
  verification run of the review (`review/verify/verify.go`, shared with the
  verification of findings; "Verification before publishing" below):
  Read/Grep/Glob only, schema part `{verdicts:[{index, verdict:
  confirmed|rejected, reason}]}`, usage recorded like the main run after
  `CheckGlobal`. With no findings to verify the run is exactly the former
  compat-only run (prompt, system prompt, schema `compatVerifySchema`, no
  MCP). Rejected -> summary "Отклонено при перепроверке"; denied / failed /
  no answer -> published as "не перепроверено" (still blocking); a failed
  run also records a `verify_failed` problem. Tool results are not
  re-verified.
- Publishing: every item is a blocking thread (`🧩 Breaking change`) whatever
  `block_min_severity` says, deduped by id against open compat threads, and
  counted in the gate line. Fixed: tool threads when that tool ran completely
  (`Result.SourceOK`) and no longer reports the id; Claude threads when Claude
  lists the id in `compatibility.resolved`. Resolution goes through the same
  path as findings (Moderator, else the Reporter "можно закрыть" reply).
  Chat never resolves a compat thread. A compat thread resolved by a human
  (no bot "fixed" reply) is an explicit override: the same id is not
  re-posted and does not block; the summary lists it.
- Summary: "🧩 Обратная совместимость" right after the gate line.
- API changelog (`review/compatcheck/apichanges.go`): `Result.API` = every oasdiff item
  per spec (`compat.APIChange{Level, Rule, Operation, Endpoint, Text, File,
  Line}`), grouped by `METHOD path`, added (`endpoint-added`) and removed
  (`api-removed-*`, `api-path-removed-*`) endpoints listed apart; icons ⛔ 3
  / ⚠️ 2 / ➕ 1 additions / ℹ️ 1 other. Caps: 150 items per spec, 300 per MR
  (most severe first, the rest counted); 60/120 for the verification
  prompt. Prompt: "## API changes" after the compat section, inside
  `<untrusted_api_changes>` (implementation vs spec, consumers in context
  repos via `code-search`). Compat verification prompt: the same list as
  context after `<untrusted_claims>`. Summary: one collapsed `<details>`
  per spec ("📘 Изменения API: <spec> — counts") right after the compat
  section; spec path HTML-escaped in `<summary>`, tool text escaped with
  `mdtext.Escape` except its backtick spans (code), quick actions neutralized.
  With the compat check off (`SummaryOnly`): no proto/GraphQL/custom checks,
  `Changes`/`Warnings` empty, no compat schema/threads; issues are shown as
  "ℹ️ Сводка изменений API … не построена".

## Verification before publishing (review/verify/verify.go, review.verify)

Owner's decision: critical AND major findings are verified adversarially
before they are published.

- Where: `handleReview` after the answer is parsed (MCP-submitted findings
  merged), `enforceMigrations` and `dropLintDuplicates`, before
  `diffIndex`/`publishFindings`. Nothing is posted before the verdicts.
- Selection (`selectForVerify`): findings in `out.Findings` order (sorted by
  severity), first of each finding id, not an open bot thread
  (`mrthread.Threads.Open`), severity in `review.verify.severities` (default
  critical, major), not proven deterministically (`provenBy`,
  review/verify/verify_proven.go: a migration finding whose `deployed_usage` passed
  `enforceMigrations` — recorded in `migRun.proven` by FindingID; a finding
  about a secret (`secretWordsRe`) whose lines ±1 hold a gitleaks result; a
  finding within ±2 lines of a deterministic compat change on the same
  file AND on its topic (`compatTopic`: compatibility words, a name the tool
  quotes, or the same kind of change — removal, type, required, rename,
  enum); any other finding on those lines is verified — those are counted as `verifyRun.proven`, published unchanged, the
  summary block says "🧪 Без перепроверки: N … доказано проверками бота (…)"
  (the block exists with only that line when nothing else was sent), verdict
  metric `proven`); at most `review.verify.max_findings` (default 10), the
  rest are published unverified and counted in the summary. Incremental and
  manual reviews behave the same (only the findings of the run are new).
  Switch: `review.verify.enabled` (default true) AND repo `verify.enabled`
  (may only turn it off) -> `config.Effective.Verify`.
- ONE run for the findings and Claude's new breaking items (merged with the
  compat verification; compat semantics unchanged, see "Backward
  compatibility"). Findings get ids `F1..Fn`; the prompt fences each finding
  (severity, path:line, title, body) in `<untrusted_findings>` with the diff
  of its file around its lines (`diffExcerpt`, new line numbers, ±12 lines,
  <= 80 lines) in `<untrusted_code>`; claims in `<untrusted_claims>` plus
  the OpenAPI changelog. The system prompt (`verifySystemTmpl`) makes the
  run adversarial: try to REFUTE each finding from the code (working tree =
  head, `/base/<path>` = the merge base, /repos);
  reject only with a concrete cited reason (does not exist / handled
  elsewhere, cite where / line not changed and not affected / misread);
  downgrade = real but less severe, strictly lower; confirm otherwise and
  when unsure; never add, never raise. The block rule of
  `review.block_min_severity` is included as the stakes. Migration findings:
  `enforceMigrations` already ran; the verifier may confirm, downgrade or
  reject them, never raise.
- Schema: no findings -> `compatVerifySchema`; else `{findings:[{id,
  verdict: confirm|downgrade|reject, severity?, reason, evidence?}]}` plus
  `verdicts` (required; an empty array without claims) when the repository
  has the compat check on (`verifyInput.compatOn`) or there are claims; the
  claim rules are then in the system prompt too, so the system prompt and the
  schema do not depend on whether this MR has claims (prompt caching). Parsing
  (`parseVerifyOutput`): ids upper-cased, verdicts lower-cased, markers
  stripped; no verdict for any known finding (or, with claims, for any
  claim) = invalid -> the usual one retry with the nudge.
- Apply (`applyFindingVerdicts`): reject -> the finding (and later
  duplicates of its id) is dropped; downgrade to a valid strictly lower
  severity -> new severity + a "🧪 Перепроверка понизила важность (было …):
  reason" line in the thread body (blocking follows the new severity);
  confirm, missing verdict, unknown verdict, upgrade or same-level
  "downgrade" -> unchanged; verdicts for unknown ids are ignored.
- Run: `CheckGlobal` first (the job's booking was committed by the review
  run), then `runStructured` (Record per attempt, retry checks the global
  limit). Read/Grep/Glob and the merge base export; `review.verify.model` (empty =
  `claude.model`, advisor and search agent as for the review),
  `review.verify.timeout` (0 = `claude.timeout`); context dirs and the
  read-only project memory as for the compat run. Meter kind
  `verification` ("Перепроверка" on /usage), back to `review` after. With
  MCP on and findings to verify, a separate read-only session
  (`mcpRun.openVerify`: diff_lines, api_changes, lint_findings, coverage,
  map_lookup, migrations, task, related_repos; no submit/withdraw/list/
  progress/report_problem) is opened for the run and closed after it.
- Failure (global limit, Claude rate limit, invalid answer twice, other
  error): findings published unchanged, summary note "перепроверка не
  выполнена: <reason>", problem `verify_failed` (subject "перепроверка:
  <reason>"; not on cancellation); claims "не перепроверено" as before.
- Summary (`verifyRun.summaryBlock`, in the task section after the rules
  line): `<!-- reviewbot:verify -->` "🧪 Перепроверка: подтверждено N,
  понижено M, отклонено K" + collapsed list of rejected and downgraded
  findings (severity, path:line, title, reason) + the max_findings overflow
  line; nothing when no finding was sent to the verification.
- Outcomes: rejected findings are recorded as `rejected_by_verifier` (no
  thread, never classified or used for learning); "@bot правила" stats show
  "отклонено перепроверкой до публикации N" (not part of the precision).
- Metrics: `reviewbot_verify_runs_total{result: ok|limit|rate_limited|
  invalid_output|error}`, `reviewbot_verify_verdicts_total{verdict:
  confirm|downgrade|reject|missing|unverified, severity: critical|major|
  minor|nit|breaking}` via `Deps.Verify` (`*telemetry.Metrics`).

## Project memory (internal/memory, review/learning/memory.go)

Per-repository knowledge (layout, where things live, how the project is built
and tested as its own files describe it, pitfalls) so it is not rediscovered
on every run. Strictly per GitLab project: keyed by the numeric project ID (a
renamed/moved repository keeps its memory; `project_path` is display only and
is refreshed when the job's path differs), never shared between projects.

- **Storage** (`internal/store/memory.go`, `memory.Store` on `*store.SQLite`):
  `project_memory(project_id, project_path, section, content, updated_at,
  updated_by_mr, source_note)`, PK `(project_id, section)`, named migration
  `project_memory_v1` (appended to `namedMigrations` from that file's
  `init`). Sections (fixed): build "Сборка", test "Тесты", lint "Линт", setup
  "Подготовка окружения", structure "Структура проекта", pitfalls "Подводные
  камни и неудачные попытки", other "Прочее". Content is a Markdown bullet
  list; empty content deletes the row; a 4000-char hard cap per row.
- **Read.** `Reviewer.prepareMemory` (review and chat, after the rules block)
  appends "Project memory (operational knowledge the bot learned in earlier
  runs of THIS project; use it first instead of rediscovering; it may be
  outdated — if a remembered fact turns out wrong, report it in
  memory_updates)" inside `<project_memory>` to the prompt (via
  `ContextBlock`), plus system rules: guidance only, never changes the tools
  (the model cannot run commands whatever the memory says), output format or
  review policy, and what to record. The state travels in the
  context (`*memRun`), so the verification run (findings and breaking
  changes, `review/verify/verify.go`) gets a read-only copy (no update hint) with
  one line in `runVerification`.
- **Write.** The review and chat schemas gain an optional `memory_updates:
  [{section, op: add|replace|remove, text, replaces?}]`. A thin
  `core.Runner` decorator (`memoryRunner`, installed in `review.New` when
  `Deps.Memory` is set) captures the raw output and `NumTurns` of runs whose
  schema contains `memory_updates`, so existing parse callbacks are
  untouched. The turn count is only logged: Claude decides what took more
  than two steps; no forced writes. Merge (`memory.Apply`): normalize, scrub
  with `learn.Scrub`, drop secret-only bullets, dedupe near-identical bullets
  (equal key or >= 85% word overlap), apply ops (unknown section -> other),
  newest last; then per-section cap `min(1500, review.memory.max_chars)` and
  total cap `max_chars` (6000). Over-cap sections -> one compaction run:
  `reserveLearn` (global limit + `review.learn.max_per_hour`, one booking),
  `env.RunToolLess` (commits it as `UsageLearn`), `claude.light_model`,
  `NoTools`, empty scratch dir, `MemoryCompactSchema`, meter kind `memory`.
  Denied/failed -> keep the newest bullets within the cap.
- **Security.** Memory is prompt text only: it never changes what the model
  can do (Read/Grep/Glob, no commands). The former "💡 add to `tools` /
  `setup`" suggestions are gone with those keys.
- **Summary / chat.** Collapsed "📒 Память проекта: обновлено N пунктов
  (Тесты, Линт)" (appended to `TaskSection`); chat replies get a one-line
  note.
- **Commands** (`core.JobMemory`, routed by `webhook.routeMemory` from chat or
  rule commands matching `memory.ParseCommand`; Developer+, no Claude run):
  `память|memory` (show: sections, updated at, MR, source),
  `память очистить [раздел]|memory clear [section]`,
  `память запомни <раздел>: <текст>|memory add <section>: <text>` (scrubbed,
  deduped, capped). Listed in `@bot help` when memory is on.
- **Switches.** `review.memory.enabled` (default true), `.max_chars`; repo
  `.reviewbot.yaml` `memory.enabled: false`; nil `Deps.Memory` = off.

## Secrets and linters (internal/lint, review/linters/lint.go, review/linters/cilint.go)

Deterministic results the bot publishes itself, limited to lines the MR adds
(removed-only hunks and deleted files contribute nothing). The bot runs only
gitleaks; linters run in the repository's own CI and the bot reads their
reports (it used to run phpstan/phpcs/ruff/eslint/golangci-lint/semgrep and
lint.custom commands itself: removed).

- Switches: service `review.lint` (`enabled` true, `timeout` 3m per gitleaks
  run, `tools` [gitleaks], `severity_overrides` {"ci:<tool>" |
  "ci:<tool>:<rule>": severity}, `allow_disable_secrets` false); repo
  `lint: {enabled, disable: [gitleaks], ci: [...]}`. A repo can turn gitleaks
  off only when `allow_disable_secrets` (else it keeps running and the summary
  says so). Removed keys (`review.lint.semgrep_rules`, `allow_custom`, removed
  tool names in `tools` / `severity_overrides` / `lint.disable`,
  `lint.custom`, `lint.semgrep_configs`) are validation errors ("removed:
  linters now come from CI reports — configure `lint.ci`…"); the service
  config is checked by a second, generic YAML decode (`removedKeys`), the repo
  config in `ParseRepo`.
- gitleaks runs after compat, before Claude, through
  `Runner.Exec` (`--network none`, no credentials, argv without a shell,
  cwd = checkout): `gitleaks git --log-opts=<base>..<head> --config
  .reviewbot-lint/gitleaks.toml --gitleaks-ignore-path .reviewbot-lint/ignore
  --report-format json --report-path - --redact --ignore-gitleaks-allow
  --no-banner --no-color --log-level error --platform none --exit-code 2 .git`.
  Config and `.gitleaksignore` come from the TARGET branch (`ShowFile(base,
  …)`, default `[extend] useDefault = true`), the source is `.git` so the
  MR's own `.gitleaksignore` is not loaded. Exit 0/2 = ok/leaks, other =
  failed. Every commit of the MR counts (a secret removed later stays in
  history): `InDiff` says whether the line is an added line (inline) or only
  in the history (general blocking thread). The parser decodes only RuleID,
  Description, StartLine/EndLine, File, Commit — never Secret/Match/Line.
  Exit 127 / "not found" = unavailable; never a finding, never fails the
  review. Scratch files live in `<clone>/.reviewbot-lint/` (git-excluded,
  removed). `lint.Checker.Check` keeps a per-tool loop (`AllTools`) for
  in-process checks of the bot.
- CI linters (`lint.ci`, up to 10: `job` name or glob, `artifact` path or
  `report: codequality`, `format` auto|codeclimate|sarif|checkstyle, `tool`,
  `severity`, `blocking`), `collectCILint` after the lint step: head pipeline
  (`mr.Pipeline`; nil = unavailable; SHA ≠ head = pending), its jobs
  (`CIArtifacts.ListPipelineJobs`), matched by name, glob (`path.Match`) or
  parallel base name ("phpstan 1/2"). Any matched job not finished (created,
  pending, running, scheduled, preparing, waiting_*) = `StatePending`. Each
  finished job (success or failed: linters fail when they find something) is
  downloaded: `GetJobArtifact(path)` or `CIReports.GetJobReport(job,
  "codequality")` (looked up with `core.GitLabAs`; GET /jobs/:id lists the
  artifacts by `file_type`, GET /jobs/:id/artifacts?file_type=codequality
  downloads it on GitLab 19.4+; an old server returns the zip archive, then
  the report's filename is read from the archive or
  `ErrReportNeedsArchive`). Cap `lint.MaxReportBytes` (20 MB, also after
  gunzip). Not found + job failed = "job упала, отчёта нет" (failed);
  manual/skipped/canceled = unavailable; missing job = unavailable.
  `lint.CheckReport` parses (CodeClimate: description, check_name,
  severity info|minor|major|critical|blocker → nit/minor/major/critical/
  critical, location.path, lines.begin / positions.begin.line; SARIF error →
  major, warning → minor, note → nit; Checkstyle error/warning/info), maps
  runner paths to the MR's files (suffix match), keeps changed lines
  (`ChangedFromFileDiffs` of GetMRDiffs, review ignore globs), applies the
  entry severity and overrides, caps 300 (then partial). Tool name
  `ci:<tool>`; `MayBlock` = entry `blocking`. Statuses and findings are
  appended to the lint result, so publishing, dedupe and auto-close are
  shared with gitleaks; failed/unavailable/partial entries are `lint_failed`
  problems. A CI result repeating an OPEN Claude finding (same line, names
  rule or tool) is treated as already discussed (`markClaudeDuplicates`).
- Pending CI jobs: the summary block says "⏳ ждём job …" and lists the
  waiting tools in its pending marker; `watchPipeline` starts the
  JobRefreshStatus chain (also when TBD and coverage are off).
  `refreshCILint` (no Claude, no limits): only when the block still waits,
  re-collects, records problems for waited entries that ended without a
  report (once), publishes / closes `ci:` threads (`publishLint`,
  `resolveFixedLint`; gitleaks has no status then, so its threads are never
  touched), re-scans the discussions for the gate line (`gateCount`,
  `summary.SetGateLine`) and replaces the block; the chain ends when nothing waits.
- Severity: gitleaks critical (always blocking). CI linters: from the
  report (or the entry), never blocking unless `blocking: true`, then when it
  reaches `block_min_severity`.
- Threads: badge `🔧 <tool>` (+ `🔑` for secrets), rule id, severity,
  message (escaped), rule docs link. Any bot thread with the id (open or
  resolved) suppresses a repost; a blocking one a human resolved is listed as
  "закрыт вручную". Non-blocking threads are resolved right after publishing
  (service token). An open thread is closed (Moderator, else the Reporter
  "можно закрыть" reply) only when its tool is configured and its state is
  `ran`/`n/a` this time (`Result.ToolOK`: for `ci:` — the report of this head
  was read completely) and it no longer reports the id; unavailable /
  failed / partial / pending never close anything.
- Claude: results go into the prompt inside `<untrusted_tool_results>` with
  "do NOT report them again" (gitleaks shown as "leaked secret (value
  hidden)"), plus the tools that could not run and the CI linters still
  waiting; Claude findings on the same file/line that name the rule or the
  tool (bare name without `ci:`; secrets: any secret wording) are dropped as
  duplicates.
- Summary: "🔑 Найдены секреты: N" right after the gate line, then compat, then
  a collapsed "🔧 Линтеры и сканеры" table (gitleaks and other in-process
  checks), then the "🔧 Линтеры из CI" block (per-entry job links and
  counts, missing reports, results that could not be posted). Open blocking
  lint threads count in the gate line.

### TODO without a task (internal/lint/todo.go, review/linters/todo.go)

Built-in tool `todo`: runs in the bot after the linters (no Runner, no
Claude), also when `review.lint` is off; results join `lintRun` (threads,
auto-close, summary row, prompt, `dropLintDuplicates` — the rule "TODO" (the
keyword) named in a Claude finding on the same line drops it, unless the finding
is more severe or blocking; the tool name "todo" is generic and never counts).

- Switches: service `review.todo` (`enabled` true, `keywords` [TODO, FIXME,
  XXX, HACK], `require_task` true, `severity` minor, `blocking` false,
  `ignore` [vendor/**, node_modules/**, **/*.min.js, **/*.min.css],
  `verify_keys` false); repo `todo: {enabled, keywords, severity, blocking,
  ignore}` — disable, replace keywords / severity, add ignore globs; blocking
  only stricter (`true` on, `false` cannot relax the service). Effective
  `Enabled` also needs `require_task` or `verify_keys`. When on, "todo" is
  appended to `lintRun.cfg.Tools` so `resolveFixedLint` may close its threads.
- Input: added lines of `git diff <merge base>..HEAD` (review ignore applied;
  recomputed when the review range is incremental); files read with
  `ShowFile(head, path)` (≤ 4 MiB, NUL = binary, skipped), else only the diff's
  lines. Skipped: `ignore` globs (a glob without `/` also matches the base
  name) and files whose first 20 lines carry a generated header (`Code
  generated … DO NOT EDIT`, `@generated`, `<auto-generated`, …).
- Lexer: comment syntax by file name (C-like `//` `/* */` + `* ` docblock
  lines; Go/JS/TS backtick strings across lines; PHP `//` `#`; `#` languages
  with "after whitespace" rule for shell/YAML; Python `#` + docstrings =
  triple-quoted string starting a line; SQL/Lua/Haskell `--`; HTML/XML/MD
  `<!-- -->`; Vue/Svelte both; Twig/Jinja `{# #}`; Handlebars `{{!-- --}}`;
  INI `;` at line start; Lisp `;`; Erlang/TeX `%`; JSON/TXT none; unknown
  extension: `//` `#` `--` after whitespace, `/* */`, `<!-- -->`). Quotes
  skip string literals; a quote not closed on the line is an apostrophe, not
  a string. Marker: a keyword, uppercase, whole word, not after `.` and not
  followed by `()` (`context.TODO()`); the first one of the line.
- Task reference (the line's comments + the next line when it is
  comment-only without its own marker): `tasklink.ExtractKeys` with
  `eff.JiraProjectKeys` (any key when none, minus keyword prefixes and
  non-task prefixes UTF/SHA/ISO/RFC/…), a GitLab ref `#12` /
  `group/project#12`, or a URL to `/browse/KEY-1`, `selectedIssue=`,
  `/-/issues/N`, `/issues/N`, `/work_items/N`.
- `verify_keys` (Jira configured): comments whose only references are Jira
  keys are looked up (`TaskSource.GetIssue`; each key once per run; at most
  `config.MaxTodoKeyLookups` = 10 lookups per review, the rest unchecked +
  summary note; `Reviewer.todoKeys` caches answers 1h). 404/403 → result
  `task-not-found` (minor, never blocking) when no referenced key exists;
  other errors → key treated as fine, problem `jira_failed`, tool state
  partial (threads not auto-closed on a guess).
- Results: Tool `todo`, Rule = keyword, Severity from config, `MayBlock` =
  `blocking`, `Finding.Todo` (keyword, comment, key, project keys) for the
  note text (`todoNoteBody`: title "TODO без задачи" / "задача KEY не
  найдена", quoted comment, `TODO(KEY-123): …` with the configured key
  format). Cap 100 results (partial). Status `n/a` when nothing was added.

## Commands

`@bot review` (`ревью`), `@bot full review` (`полное ревью`), `@bot review
force` (`ревью принудительно`; `core.JobForceReview`: a full review that ignores
`review.max_mr_lines`, parsed by `ParseCommand` next to `full review`), `@bot check`
(`перепроверь`, see below), `@bot help` (`помощь`), rule commands (`запомни`, `забудь #N`, `правила`, `правило #N для
всех|только здесь`, see "Team rules"), memory commands (`память`, `память
очистить [раздел]`, `память запомни <раздел>: <текст>`, see "Project memory"),
MR commands (below); a note that only thanks or acknowledges is answered
with an award emoji (`core.JobReact`, see "Reactions"); anything else after
the mention is a chat question.
Every command with its aliases: [features/commands.md](features/commands.md). The mention is
always `@<username of the GITLAB_TOKEN account>` (no static alias; `review.bot_mention` is ignored with a warning).

### MR commands (internal/review/commands*.go, internal/events/commands.go)

The router parses them (`events.ParseMRCommand`) in what `ParseCommand`
classified as chat, before rule commands: the whole single-line text must be
the command (trailing punctuation ignored; pause/resume may carry a reason
after `,` `:` `;` `—`). Kinds `core.JobPause`, `JobResume`, `JobIgnore`,
`JobResolveAll`, `JobConfig`, `JobCheck`. All require Developer+ (otherwise the
`msgNoAccess` reply); all but `check` never run Claude and never touch limits.

- `pause` / `пауза`: `SetMRPause` (by, at); reply "⏸ Авторевью на паузе.
  Продолжить: `@bot resume`"; the summary gets "⏸ Авторевью на паузе
  (поставил @user)" right after the title (the username carries a zero-width
  space so re-renders do not ping). Manual reviews keep the line.
- `resume` / `продолжить`: `ClearMRPause`, removes the line; when the MR is
  open, not a draft, auto review on, no ignore marker and
  `MRState.ReviewedSHA != head`, enqueues a `JobAutoReview` through
  `Reviewer.SetEnqueue(q.Enqueue)` (wired in main; debounced like any auto job).
- `ignore` / `игнорировать`: works only in the MR **description** (checked at
  review time, no state; the mention `@<bot username>` as a whole word, no
  static alias). As a note command the bot replies how to use it.
- `resolve` / `закрой треды`: every unresolved resolvable discussion whose
  first note is the bot's and carries a `<!-- reviewbot:<kind> ...` remark
  marker (findings, compat, lint; not the summary) is resolved via the
  Moderator; blocking ones too (a Developer+ decided). The summary's gate line
  becomes "✅ Блокирующих замечаний нет." when all succeeded. Without a
  Moderator: reply that the bot lacks rights + the number of open threads.
- `check` / `recheck` / `проверь` / `перепроверь` (`core.JobCheck`,
  internal/review/check.go): re-check the bot's OPEN findings against the
  current head without a new commit. GetMR (opened), repo config (enabled),
  Developer+ (skipped on an admitted attempt, `core.RunAdmitted`), then `publish.Scan`. Scope:
  the command's discussion is an open bot finding thread -> only that finding;
  an open compat thread or a bot lint thread -> reply that it is not
  re-checked (tools: linters / oasdiff), no run; a finding thread the bot no
  longer considers open -> reply, no run; anything else (general note, human
  thread) -> every open finding (`openOrder`). Nothing to check -> "Открытых
  замечаний бота нет." (+ the open lint/compat counts) BEFORE any booking: no
  run, no usage. Then `Limiter.Reserve`: `UsageRerun` with the per-user
  limits minus the cooldown (`MRRerunCooldown = 0`) and the global limit;
  `jobUsage` records a check without `MRKey`, so the per-MR cooldown never
  applies to it and never starts from it. Checkout head, context repos, one
  `runStructured` run: dedicated system/user prompt (findings id, path:line,
  title, body shortened to 1000 runes, inside `<untrusted_findings>`),
  `checkSchema` (`findings[]{id, status: fixed|still_open, comment}`),
  `Read/Grep/Glob` (read-only git built in), `claude.model` only (no advisor,
  no search subagent, no MCP: the analyzers its read tools serve are not
  run), timeout `claude.timeout`. Parse: unknown ids dropped, first verdict
  per id wins, missing id or unknown status = still open, no verdict for any
  id = invalid (one retry with the nudge). Apply: `fixed` -> `markFixed`
  (service token; Reporter fallback "Похоже, исправлено"; other resolve
  errors keep it open) + `outcomeFixed`. Gate: `gateCount` of the scan
  (open blocking findings, open compat, open blocking lint, the open size
  thread and any other open bot thread with the blocking marker,
  fixed-pending) minus the findings closed now, plus the new fixed-pending;
  `summary.GateLine` / `summary.SetGateLine` rewrite only that line of the existing
  summary (no summary or no gate line: untouched). Reply: "Проверил N
  открытых замечаний на `<sha8>`: закрыто K, остаются M." (+ pending count,
  the still-open list: 10, the rest in `<details>`, + the lint/compat note in
  the general scope). Claude rate limit -> "⏳ Перепроверка
  отложена до HH:MM: …", `*core.RetryLater` (summary untouched); invalid
  output -> `problems.InvalidOutput` + "❌ Перепроверка не удалась: …";
  other errors -> internal error reply. Run kind on `/usage`: verification.
- `config` / `конфиг`: Markdown table (setting, value, source: сервис /
  `.reviewbot.yaml` / по умолчанию) of language, enabled, auto_review, pause,
  ignore marker, effective limits, block_min_severity, compat (+ extra paths
  and commands), context (+ excludes), learn, the model's tools (fixed:
  read-only), ignore globs, max_files,
  instructions (length only), Jira on/off and keys, title check, diagram. No
  tokens, secrets or URLs are printed; values pass `learn.Scrub` as well.

Per-MR state: `mr_flags(project_id, mr_iid, paused, paused_by, paused_at)`
from the named migration `mr_flags_v1` (registered in
`internal/store/mrflags.go` via `init`). The reviewer uses it through the
`review.MRFlags` interface wired as `Deps.MRFlags` (`*store.SQLite`; nil = no
flags).

## Diagram (internal/review/crossrepo/diagram.go)

`output.ReviewOutput.Diagram` (optional string, in `jsonschema.Review`; removed from
the schema when off). The system prompt asks for `sequenceDiagram`
(interactions) or `flowchart TD` (control/data flow), ≤ 25 nodes/messages,
labels in the review language, new/changed parts highlighted (`Note over`,
`:::changed`), and `""` when a diagram adds nothing; it says that in an
incremental review (the user prompt says which) the previous diagram is kept
when `""` (one wording for full and incremental runs: prompt caching).
Validation (`sanitizeDiagram`): first line `sequenceDiagram` or
`flowchart|graph <TB|TD|BT|RL|LR>`; ≤ 60 lines, ≤ 200 runes per line; no
control characters, `click`/`link(s)` statements, `call x(`, `href`, `%%{`,
`<script`, `javascript:`/`vbscript:`/`data:<mime>`, `://`, HTML tags (`<x`,
`</x`, `<!`), code fences. Invalid -> omitted (warn log), the review goes on.
Rendering: "🗺 Схема изменений" between the diagram markers, after Claude's
summary text and before the walkthrough (prepended to
`summary.Data.TaskSection`); collapsed in `<details>` above 20 lines.
Incremental review with `""` or an invalid diagram re-uses the previous
summary's block (re-validated); a full review never does. Switches:
`review.diagram` (default true) AND `.reviewbot.yaml` `diagram` (default true).

## Automatic MR labels (review/labels/labels.go)

Owner's request: labels from a configured list, optionally with an
instruction for when a label applies. Config: service `review.labels`
(`enabled` true, `remove_stale` true, `create_missing` false, `rules`) and repo
`labels` (`enabled` can only turn off; `remove_stale` / `create_missing`
override; `rules` extend, same `name` replaces in place, `enabled: false`
drops). Rule = `name` + any of `paths` (doublestar, matched against every file
of merge base..head, ignore globs applied), `when_fact` (string or list) and
`instruction`. Validation (`config/labels.go`): name non-empty, <= 255 runes,
no comma, no control chars, unique per list; known facts; valid globs (<= 50);
instruction <= 1000 runes; a rule needs a condition; <= 30 rules per list and
in total (`EffLabels.Dropped` counts repo extras, logged). A rule named like
`review.big_mr.override_label` (`config.IsOverrideLabel`, case-insensitive)
would let the bot lift the size limit and `big_mr.block` itself: `Validate`
refuses an enabled service rule; a repo rule (the service value is unknown at
parse time) is dropped by `mergeLabels` into `EffLabels.Refused` and logged
once per project and label (`logRefusedLabels`); `applyLabels` also filters
the label out of the additions (`withoutOverrideLabel`).

- **Facts** (`labelFacts`, three-valued: yes / no / unknown; unknown when the
  step is off or failed): `migrations` (migRun files; full MR diff),
  `api_changed` (compat discovery `Specs`/`Deleted`/`API`; compat off: the
  OpenAPI changelog), `api_breaking` (`compat.Result.Changes` or an oasdiff
  level-3 item; unknown when a checker was unavailable/failed and nothing
  was found; Claude's items never count), `big_mr` (TBD size > `max_lines`;
  a partial size below the threshold is unknown), `lint_secret` (a gitleaks
  finding; no when gitleaks `ToolOK`), `no_tests_changed` (code extensions
  outside vendor dirs changed, no test file by name/dir patterns). There is
  no deterministic "feature flag needed" signal, so no such fact.
- **Model.** Only when some rule has an `instruction`: the user prompt gets
  "## Merge request labels" with `<label_rules>` (names quoted, text escaped
  like path instructions; appended to `RulesBlock` after the path
  instructions), the system prompt one note (labels guidance only), and the
  schema an optional `labels` array (`uniqueItems`, items enum of the
  instruction rule names). Incremental runs add a sentence that leaving a
  label out does not remove it. The answer's field is read separately in the
  parse callback (`labelPrompt.parse`, `*[]string`: a missing field is "no
  answer"). MCP changes nothing (labels come with the final JSON). Without
  instruction rules the request is byte for byte unchanged
  (`prompt_mcp_off.golden`); `testdata/prompt_labels_on.golden` pins the
  labels-on request.
- **Evaluation** (`evalRule`): yes if any condition is yes; no only if every
  condition is a definite no; otherwise unknown. An instruction is yes when
  the model chose it, no on a full review whose answer had the field,
  unknown on incremental reviews (the model saw only new commits) and when
  the field is missing. Deterministic facts are full-MR facts, so they add
  and remove on incremental reviews too.
- **Plan** (`planLabels`, pure). Bot state per MR in `mr_labels(project_id,
  mr_iid, label, state added|suppressed, updated_at)` (named migration
  `mr_labels_v1`, `store/labels.go`, `labels.LabelStore` = `review.Deps.LabelStore`,
  `*store.SQLite`).
  Reconcile first with the MR's current labels: a bot label missing from the
  MR was removed by a human -> suppressed (never set again on that MR); a
  suppressed label a human put back is the human's. Desired = rules with yes,
  scoped labels (`config.LabelScope`: the part before the last `::`) one per
  scope by rule order. Add = desired minus current minus suppressed, minus
  labels whose scope already has a human label (GitLab would drop the
  human's). Remove (only with `remove_stale`) = bot labels whose rule is a
  definite no (or no longer configured), plus bot labels that lost their
  scope to a winner that is (or will be) on the MR; unknown keeps. Labels
  not in the bot's record are never removed.
- **Apply** (`applyLabels`, after `collectTBD`, before `problemsSummary`;
  panics recovered): fresh `MRLabels` read (state not `opened` -> nothing);
  `create_missing: false` -> `ListProjectLabels` (`include_ancestor_groups`)
  and missing ones skipped; one `UpdateMRLabels` (`PUT` MR with
  `add_labels`/`remove_labels` comma-joined, retried on 5xx: idempotent) with
  the bot's client (`Deps.Labels`), then whatever is still not applied (403,
  other error, or 200 with the labels unchanged: no metadata right / label not
  creatable) with the service client (`Deps.ServiceLabels`). A 403 on the bot
  token is remembered per project for an hour (`labelDeny`). The bot record
  becomes: previous added kept on the MR + added now present in the response;
  labels GitLab dropped (scoped replacement) are forgotten, not suppressed.
- **Failures** -> `labels_failed` problems ("метки MR: нет прав",
  "метки нет в проекте и его группах: X", "GitLab не применил изменения",
  "ошибка GitLab API", read errors) and `reviewbot_labels_total{action,result}`
  via `Deps.OnLabel` (ok, forbidden, missing, ignored, error).
- **Summary**: one line appended to `TaskSection`: "🏷 Метки: +`a` −`b`
  [не удалось: `c`]" when something changed or failed, else "🏷 Метки бота:
  `a`" when the bot has labels on the MR, else nothing. `@bot config` has a
  "метки MR" row. Drafts are not reviewed, so not labeled.

## Sandbox

```
            +-------------------- docker network "reviewbot-sandbox" (internal) ----+
reviewbot   |  claude container (per run)  --HTTPS_PROXY-->  egress (allowlist) ---+--> api.anthropic.com
 (host)  ---+  /work = disposable clone, /base /repos /skills read-only            |
            |  tools: Read, Grep, Glob only (no Bash)                               |
            +-------------------------------------------------------------------------+
```

- Review containers join an **internal** docker network; the only route out is
  the `egress` proxy (CONNECT to allowlisted hosts on :443).
- The model never runs commands: Read/Grep/Glob only, no Bash, no setup, no
  repository-defined commands, so nothing from the MR executes in the review
  container. Claude Code's own Bash sandbox (bubblewrap/socat) is not used
  and not in the image.
- The bot's checkers (`Runner.Exec`: gitleaks, oasdiff, oas2to3, buf,
  graphql-inspector — programs of the image, never repository commands) run
  in a separate container with `--network none`, no credentials, argv
  without a shell. The image has no language runtimes (Go/PHP/Python),
  Composer, linters, make/gcc, bubblewrap/socat or curl.
- The GitLab token never enters any container. Claude only produces JSON; the
  service publishes.
- Each run gets its own self-contained clone (real `.git` dir, no links to the
  mirror). The merge base is a read-only export at `/base`; other projects
  are visible only as the read-only context snapshots (no `.git`) mounted
  under `/repos` (see "Context repositories").
- `--cap-drop ALL`, `--security-opt no-new-privileges`, `--pids-limit`,
  cpu/memory limits, `--read-only` root fs with tmpfs for /tmp and HOME.
- With `review.mcp` on, the sandbox reaches the bot's MCP listener only
  through the egress container's one-port forward (see "MCP server").

### Claude's tools (internal/claude/args.go)

- `--tools Read,Grep,Glob` (+ `Task` with the search subagent; `Read` only
  for `NoTools` runs), `--allowedTools` = path-scoped Read rules of the run's
  directories (`readscope.go`: docker `Read(//work/**)` + `//base`, `//repos`,
  `//skills` when the run has them; local mode the host paths, also with
  symlinks resolved), `Task(code-search)`, `mcp__<server>__<tool>` per MCP
  tool; no bare `Read`/`Grep`/`Glob` (a bare rule allows any path; Grep and
  Glob fall back to the Read rules outside the working dirs); nothing
  configurable. `--disallowedTools Bash WebFetch WebSearch Edit Write
  NotebookEdit` + Read deny rules for every top-level directory of the image
  that is not an input (`//proc/**`, `//etc/**`, `//home/**`, `//tmp/**`, …),
  the MCP config dir (`//etc/reviewbot-mcp/**`, local: its temp dir),
  `~/**` and the credential paths under `$HOME` (`~/.claude/**`,
  `~/.claude.json`, `~/.ssh/**`, …) (+ the built-in agents), for every run
  including text-only ones. A deny rule never covers an allowed directory
  (local mode drops e.g. `//home/**` when `data_dir` is under it); paths that
  cannot be a literal rule (spaces, commas, glob characters) get no rule.
  `claude.AllowedTools` / `DisallowedTools` give the docker-mode lists for
  `/runs`.
  `--permission-mode dontAsk --permission-prompts none`; no `--settings`
  (no Claude Code sandbox payload). `core.RunRequest` has no AllowedTools
  field and `core.Runner` no Setup method any more.
- Removed with Bash: `.reviewbot.yaml` `setup` / `tools`,
  `review.default_tools`, `sandbox.setup_timeout`,
  `sandbox.allow_unsandboxed_bash` (refused with "removed: the review model
  no longer runs commands; see docs/security.md", `config/removed.go`), the
  Bash deny rules, the in-clone `.reviewbot-cache` and toolchain cache env,
  the `diffcov` tool, bubblewrap/socat/Composer/make/gcc and the Go/PHP/Python
  runtimes in the image; `compat.commands` (own reason: "removed: the bot no
  longer runs repository commands …").

### The code before the MR (/base, review/env/base.go)

Claude has no git, so the merge base is handed over as files:
`Reviewer.prepareBase` asks the checkout (optional `core.TreeExporter`,
`repo.Checkout.ExportTree`) for an export of `fullDiffBase(mr)`. With the
checkout cache (default) that is the shared cached tree
`trees/<project>@<sha>-base` (see "Checkout cache" below; same file rules,
cap checked before writing, kept for the TTL after release). Without it:
`git archive` of the commit in the clone, unpacked with the context-snapshot code
(`extractTar`: regular files 0444, no `.git`, `.claude/`, `.mcp.json`,
symlinks last, out-of-tree ones removed) into `<work>/<clone>-tree-<random>`
(a sibling: same host path inside and outside the bot container, so the
daemon can mount it), capped by `review.context.max_repo_mb`
(`core.ErrTreeTooLarge`). Released after the run; `Checkout.Close` removes
leftovers; stale work entries are removed at startup.
- Runs: review, chat, `@bot check` and the verification run
  (`verifyInput.baseDir`); not the big-MR path (no Claude) and not the
  tool-less runs.
- Runner: `RunRequest.BaseDir` -> `-v <export>:/base:ro` and `/base` first in
  the one `--add-dir` (docker); the host path in local mode
  (`core.BaseMountPath`). A path inside the clone or with docker `-v`
  separators is refused (`usableBaseDir`).
- Prompts: `prompts.ToolsRule(base)` in every system prompt with tools ("You can only
  read and search files … /base/<path> is <path> before the change"); the
  review / chat / check / verify / compat-verify user prompts and the task
  section point to `/base/<path>` instead of `git diff` / `git show`;
  migrations: `migDeployedRead` ("read /base/<path> to see a file as
  deployed"; `deployed_usage.path` stays repo-relative and the bot's
  evidence check still reads the merge base via the checkout). Without an
  export the sentences are left out and the prompts rely on the diff.
- Only the merge base is exported: an incremental review's own range is in
  the prompt's diff; /base vs the working tree shows the whole MR.
- Failure (too large, git error): `problems.BaseUnavailable`, the run goes on
  without /base.


### Checkout cache (internal/repo/treecache.go, cached.go; review.checkout_cache)

With `review.checkout_cache.enabled` (default) `Manager.Checkout` updates the
mirror under the project lock (as before) and then, outside the lock, takes the
cached tree of the head: `trees/<project>@<sha>` — the full set of files of that
commit (not a diff), written from the mirror with a throwaway index
(`GIT_INDEX_FILE=<tmp> git --git-dir=<mirror> --work-tree=<tree> read-tree -u
--reset <sha>`). This is a worktree in effect, but deliberately not
`git worktree add`: no `.git` link file in the tree (the /base contract is "no
.git"; Claude needs no git), no `worktrees/` admin entry in the mirror (nothing to
prune, a fetch or `gc --auto` in the mirror cannot invalidate a live tree;
commit-to-commit diffs need no per-tree index). History and objects stay in the
mirror only: the extra disk per tree is just its working files. A tracked `.git`
is removed, out-of-tree symlinks are neutralized like in clones, then files are
made 0444 and directories 0555, and the tree is renamed into place. The base
variant `<project>@<sha>-base` (ExportTree, /base) also drops `.claude/` and
`.mcp.json`; `review.context.max_repo_mb` is checked from `git ls-tree -r -l`
before anything is written (and against the cached size on reuse).

Trees are ref-counted: concurrent jobs of the same key share one build (a failed
build is not cached; a cancelled waiter gives up); a released tree stays for
`ttl` (2h) after its last use; `sweep` (after builds and releases, and a janitor
goroutine every ttl/4 within 30s..5m) evicts idle trees past the TTL, then idle
trees LRU-first while the sum of tree sizes is over `max_gb` (10). Trees in use
are never evicted (over-budget is logged). Evicted trees are renamed aside under
the cache lock before removal (a rebuild of the same key never meets the old
directory). The index lives in memory: `trees/` is emptied at startup.

A cached `repo.Checkout` runs every git command in the mirror with the head SHA
instead of `HEAD` (`repoDir()`, `head()`); `Close` releases its trees and removes
its scratch dir `work/<random>`. Nothing writes into a cached tree: compat
materializes specs in `ScratchDir(".reviewbot-compat")` (found via
`core.CheckoutAs`), gitleaks runs in `HistoryDir`: a per-job
`git clone --no-checkout --local` of the mirror (hard-linked objects, no
alternates, placeholder origin, made under the project lock) with its config in
`.reviewbot-lint/` next to `.git`; the MCP config dir is a sibling of the tree
(`trees/.reviewbot-mcp-*`, removed by its run). Docker mounts `/work` read-only
for Claude runs (`containerSpec.WorkRO`; Exec keeps its writable scratch dir);
the mount paths inside the sandbox stay fixed (`/work`, `/base`, `/repos`,
`/skills`), so prompts do not change per tree. `enabled: false` restores the
fresh clone per job and the `git archive` export per run.

Close vs. calls in flight: a cached `ExportTree` rechecks `closed` after
acquiring its tree and releases it when Close ran meanwhile (error
"checkout closed"); a clone `ExportTree` creates its directory before
registering it (a racing Close removes it, or the export sees `closed` and
removes it itself). `HistoryDir` is serialized by `historyMu` (concurrent
callers share one clone); Close takes the same mutex before removing the
scratch dir, so it waits for a history clone in progress, which then fails
and removes itself.

### Repository attributes (internal/repo/attributes.go)

A repository's `.gitattributes` is MR data and cannot change what git shows the
bot. Without this, `*.go -diff` (or `binary`, or a macro) made the MR's code
"Binary files differ" in `Diff` (size gate, routing and the trivial-push check
saw nothing), in `git grep -I` and in `LinesChanged`; `export-ignore` dropped
files from /base, cached trees and context snapshots; `export-subst`, `ident`,
`eol`/`text`, `working-tree-encoding` and `filter` rewrote what Claude reads.
git takes them from the working tree/index (clone), from the tree being
archived or read into an index (`git archive`, `read-tree -u`) and, in a bare
repository on git >= 2.43, from HEAD (the mirror's default branch).

Every repository the bot runs git in (mirror: `ensureMirror` on every
checkout/snapshot, `checkoutTree`, `exportSnapshot`; clone: before its
`git checkout`; history clone) gets `$GIT_DIR/info/attributes` =
`* !diff !binary !text !eol !crlf !filter !ident !working-tree-encoding
!export-ignore !export-subst`: the highest-precedence attribute file on every
git version (the image has 2.39, before `--attr-source`/`GIT_ATTR_SOURCE`); `!`
sets an attribute to unspecified (`ATTR__UNSET`), which a lower-precedence
line or a macro expansion never overrides. Unspecified `diff` keeps git's own
binary detection, so real binaries are still "Binary files differ". The
`Checkout` methods whose output the bot reads (`ChangedFiles`, `Diff`,
`LinesChanged`, `GrepWords`, `ExportTree`) rewrite the file first
(`neutralAttrs`; compare, else temp file + rename, never through a symlinked
`info/`) and fail closed when they cannot. `ShowFile` (`cat-file blob`),
`ListFiles` and `IsAncestor` do not read attributes. The snapshots directory
carries a format marker (`.format-raw-attributes`); without it every
snapshot is removed at startup (made by a version that applied
`export-ignore`).
## MCP server (internal/mcpserver, review/mcp/mcp.go, review/mcp/validate.go)

On by default (`review.mcp.enabled: true`), guarded by the fallback below
while the path is not verified live everywhere. The review model
calls bot-side tools during a review; nothing is published before the run
ends.

- **Protocol.** Hand-rolled MCP over the Streamable HTTP transport: one POST
  with one JSON-RPC 2.0 message, one `application/json` answer (no SSE, no
  batches, no `Mcp-Session-Id`). `initialize` negotiates the version (the
  client's when it is 2025-06-18, 2025-03-26 or 2024-11-05, else the newest),
  `notifications/*` get 202, `ping`, `tools/list`, `tools/call` (tool errors
  are results with `isError`, unknown tools are JSON-RPC errors).
- **Listener.** `review.mcp.listen` (default `:8091`), its own `http.Server`,
  never the public mux; its port must differ from `listen`. `startMCP`
  (cmd/reviewbot) never fails the start: no client URL or a listen error ->
  warning, MCP off (nil server, request byte for byte the MCP-off one).
- **Sessions.** `Server.Open(Spec)` per review run: 256-bit random bearer
  token (only its SHA-256 kept, constant-time compare), bound to project, MR,
  head SHA and run id; its own tool set; hard TTL 2 x `claude.timeout` + 5m;
  closed right after `runStructured` returns (before publishing).
  Unknown/closed/expired token -> 401 before the body is read. Per-session
  call cap (`review.mcp.max_calls`), response cap
  (`review.mcp.max_response_kb`), 1 MiB request cap, 60s per call under the
  job's context, panics in handlers become `isError` results. `OnCall`
  (cmd/reviewbot) turns every call into a `/runs` step and a
  `reviewbot_mcp_tool_calls_total{tool,status}` sample.
- **Claude launch (internal/claude/mcp.go).** `core.RunRequest.MCP` (name,
  URL, token, tool names). The runner writes
  `{"mcpServers":{"reviewbot":{"type":"http","url":…,"headers":{"Authorization":"Bearer …"}}}}`
  to a 0600 file in a fresh temp dir (docker: next to the clone, mounted
  read-only at `/etc/reviewbot-mcp`; local: system temp), passes
  `--mcp-config <file>` right after `--strict-mcp-config`, allows each tool as
  `mcp__reviewbot__<tool>`, adds the MCP host to `NO_PROXY`/`no_proxy`, and
  removes the dir afterwards. The token is never on a command line or in an
  environment; `--mcp-config` is redacted in recorded argv; diag records only
  the URL and tool names. If the file cannot be written the run goes on
  without MCP (the prompt tells the model to fall back to the final JSON).
- **Networking.** The bot is NOT attached to the internal sandbox network
  (its public port would become reachable). The egress container forwards one
  port to one fixed target (`reviewbot egress -mcp-listen :8091 -mcp-forward
  reviewbot:8091`; plain TCP relay, connection cap, idle timeout). Default
  client URL: `http://<egress_proxy host>:<mcp listen port>/mcp`; local mode
  `http://127.0.0.1:<port>/mcp`; `review.mcp.url` overrides.
- **Tools.** Registered only when the run has the data: `diff_lines` (GitLab's
  MR line map loaded before the run: added and commentable ranges),
  `api_changes`, `lint_findings`, `coverage`, `map_lookup` (touched contracts
  with usages, phase B facts, and the whole map's entries restricted to this
  repository and the repositories mounted for the run), `migrations`, `task`,
  `related_repos`, plus `submit_finding`, `withdraw_finding`, `list_findings`,
  `report_progress` and (with `review.problems`) `report_problem`. Repository-derived text is wrapped in `<untrusted_…>`
  tags with `untrusted()`; secrets are never shown; path arguments must be
  clean relative paths.
- **Prompt switch.** With MCP on, the API changelog, linter results,
  migration hints, system map usages and coverage details are replaced by
  one-line summaries pointing at the tools, and a system section explains the
  tools and the submit workflow (`testdata/prompt_mcp_on.golden`, the
  default path). With MCP off (service or repository switch, no server, the
  fallback re-run) the request is byte for byte unchanged
  (`testdata/prompt_mcp_off.golden`).
- **Fallback (review/mcp/fallback.go).** After every MCP review run
  (`runStructuredCheck` hook, before parsing) `mcpRunFailure`: config
  dropped -> `config_dropped`; init event status (`RunResult.MCPStatus`,
  parsed from `system/init` `mcp_servers[{name,status}]` by
  internal/claude streamCapture) `connected` -> fine; any other non-empty
  status except `pending` -> `not_connected`; no status / `pending` and no
  authenticated request reached the session (`Session.Connected`) ->
  `no_request`. On a failure: the answer (and submitted drafts) is
  discarded, `mcp_failed` problem, `mcp: fallback` diag step, the session
  is closed and `m.srv = nil` (every helper, `openVerify` included, now
  answers as MCP off), `Server.Fallback(reason)` opens the per-process
  breaker for `DefaultFallbackCooldown` (30m; `openMCP` skips MCP while
  `Degraded`; `reviewbot_mcp_fallbacks_total{reason}`,
  `reviewbot_mcp_degraded`), and the request is rebuilt without MCP and
  run once: `CheckGlobal` first, `runStructured` records it (the booking was
  committed by the first run). Denied -> the first answer is parsed and used.
- **In-loop validation.** `submit_finding` runs the publishing pipeline's
  functions in its order: input checks -> `finding.Normalize` ->
  `migrationVerdict` (enforceMigrations' evidence check; downgrade with the
  concrete reason, e.g. the merge-base line that lacks the name) ->
  `lintDuplicateOf` (same path, line ±1, the rule id or a non-generic tool
  name as a whole word, `containsWord`; never a result of a lower severity
  than the finding, nor a non-blocking result for a blocking finding,
  `lintGate`) -> open thread with the same `finding.ID` -> findings cap
  -> `placeFinding` (inline / snapped / general thread / summary, with the
  nearest changed ranges). Answers `accepted` / `adjusted` / `rejected` with
  `reasons` and a `hint`; same path + title replaces. Accepted findings live
  in the session's memory.
- **End of run.** Submitted findings are merged with the final answer's
  (submitted first, dedupe by path + line + title, severity order, cap) inside
  the parse callback, then the unchanged post-run pipeline (enforceMigrations,
  dropLintDuplicates, the verification run of findings and breaking changes,
  publishFindings, resolve) runs on the merged list: everything is enforced
  again, and submitted critical/major findings are verified like any other.

## Verification problems (internal/problems, review/problems.go, internal/problemsweb)

What the bot itself could not check during a review, so a partial review is
visible instead of silent. Switch: `review.problems.enabled` (default true).

- **Kinds** (fixed, `problems.Kinds()`, each with a Russian title, severity
  info/warn/error and a "how to fix" hint). Bot side: `lint_failed`,
  `base_unavailable`, `compat_failed`, `coverage_missing`, `context_unavailable`,
  `jira_failed`, `advisor_unavailable`, `deep_review_unavailable`, `description_summary_failed`, `sysmap_failed`, `sandbox_image_missing`, `diff_truncated`,
  `invalid_output`, `claude_auth_failed` (Claude answered 401: the token is replaced by hand, the command author is told so instead of "internal error"), `evidence_failed`, `mcp_failed`, `verify_failed`, `labels_failed`, `ci_fetch_failed`,
  `main_pipeline_failed` (review.ci, outside a review). Model reported:
  `model_tool_failed`, `model_missing_dependency`, `model_no_access`,
  `model_too_large`, `model_config_unclear`, `model_other` (short names without
  `model_` in the schema / tool enum).
- **Fingerprint** = sha256(kind \n lower(project) \n `NormalizeSubject(subject)`)[:32
  hex]; the subject is lower-cased, temp dir paths (`/tmp/…`, `/var/folders/…`,
  `.tmp-*`, `reviewbot-*-N`), hex ids with a digit (7+) and numbers are stripped.
- **Collector.** `problems.Service.Handler` wraps the queue handler inside
  `diag.Recorder.Handler` (so `RunID` = the diag run id): every job gets a
  `*problems.Run` in its context. `problems.Add(ctx, …)` (one-line hooks, no-op
  without a collector) dedups by fingerprint within the run (last detail wins);
  subject and detail are scrubbed (`diag.ScrubFunc`: config secrets + credential
  patterns) and cleaned (`CleanText`: HTML comments/tags, control chars, one line,
  caps 200/500; 120 for model subjects). `problems.Commit(ctx)` persists what is
  pending and returns `Outcome`s; the handler's `Finish` (with
  `context.WithoutCancel`) persists the rest, also after an error or a panic.
  Background work (system map scan errors via `sysmap.Options.OnScan`) uses
  `Service.Record`. Recording never fails a review: store errors are logged.
- **Bot-side hooks** (review flow): lint tool states unavailable/failed/partial
  or the step not starting (`noteLintProblems`); merge base export failed or
  over `review.context.max_repo_mb` (`prepareBase`); compat /
  API changelog checker error and unavailable/failed issues; coverage `covError`,
  per-report warnings, and a report without any changed file (running or failed
  pipelines are the MR's state, not a problem); `rc.missing` (related
  repositories); Jira `tc.failures`; `RunResult.AdvisorDropped` /
  `MCPDropped` (set by internal/claude when a run went without the advisor or
  without its MCP config) in `runStructured`; `openMCP` failures; the MCP
  fallback (CLI not connected, `mcp_fallback.go`); system map and
  phase B panics, errors and `SkipReason` (except "code extraction off");
  `diffcap.Cap` truncation; `invalidOutputError`; `collectMigrations` and
  `enforceMigrations` panics.
- **Model reports.** Without MCP (or when the session did not open) the review
  schema gains an optional `problems` array (`withProblemsSchema`, maxItems 5,
  items `{kind enum, subject ≤120, detail ≤500}`), parsed from the accepted answer
  (`addModelProblems`). With MCP the `report_problem` tool records them (same
  schema, `mcpserver.DecodeArgs`, unknown fields refused). Either way
  `problems.AddModel` enforces the kind, a non-empty subject and at most
  `MaxModelPerRun` (5) per run; text goes through `marker.StripBot` +
  `mdtext.NeutralizeQuickActions` + `CleanText`. One system prompt paragraph
  (`problemsSystemNote`) asks for reports of what could not be verified, not code
  problems. With `review.problems.enabled: false` the request is unchanged
  (`testdata/prompt_mcp_off.golden` carries the paragraph since this feature).
- **Storage** (named migrations `problems_v1` and `problems_v2` (`resolved_at`),
  `store/problems.go`):
  `problems(fingerprint PK, kind, source, project_path, subject, detail,
  first_seen, last_seen, count, last_mr_iid, last_run_id, acked_at, resolved_at,
  notified_at)`.
  `UpsertProblem` (one transaction) inserts or bumps count/last_* and clears
  `acked_at` and `resolved_at`, returning New / Reappeared (hidden or fixed
  before) and the previous `notified_at`; `AckProblem`, `ResolveProblem` (the
  bot saw the cause gone: `problems.Resolve(ctx, kind, subject)`, used for
  `repo_config_invalid` by the config gate; a problem the same job just
  recorded is left alone), `MarkProblemsNotified`, `ListProblems` (kind,
  project, status new = neither acked nor resolved / acked / resolved, newest
  `last_seen` first), `PruneProblems` (rows not seen for
  `retention`, default 90 days; at most once a day after a persist).
- **Summary** (`problemsSummary` right before rendering, after `collectTBD`):
  this run's outcomes, severity order, at most 5 lines + "и ещё N", collapsed in
  `<details>` above 3, kind title + `mdInline(subject)`; inserted into
  `CompatSection` after TBD and coverage, before the system map / deps / secrets /
  compat / API / lint sections. `.reviewbot.yaml` `problems.summary: false` hides
  it (and its mentions); problems are still recorded.
- **Notify.** `Outcome.Notify` = store ok AND a bot-side problem (never
  `SourceModel`: the model reads untrusted code, an injection could mint a new
  fingerprint on every push and ping the users each time) AND
  `review.problems.notify` non-empty AND (new OR re-appeared after an ack OR never notified OR notified ≥
  `renotify_after` ago, default 168h). `notify_mode: summary` adds
  `cc @u1 @u2 — новая проблема проверки, подробнее: <page_url>/problems` to the
  block; `note` posts a separate note (`marker.ProblemsNotify`) with the same line
  and the kind titles. `notified_at` is set only after the summary write / note
  succeeded. Usernames are validated by config (`[A-Za-z0-9_][A-Za-z0-9_.-]*`, no
  trailing dot, ≤ 20) and again before rendering. Whether GitLab notifies on a
  mention added by EDITING a note is unverified; `note` is the
  fallback.
- **Page** (`problemsweb.Register`, wired in `cmd/reviewbot/problems.go`):
  `GET /problems` server-rendered with html/template (all data escaped; MR links
  only for safe project paths, run links only for `diag.ValidID` ids), filters
  `kind`/`project`/`status` (default `new`), common /runs CSS/JS (themes), nonce
  CSP with `form-action 'self'`, `Referrer-Policy: same-origin` (so the form's
  Origin is not `null`). `POST /problems/ack`: `usageweb.Guard` with the usage
  token, then Origin (else Referer) host must equal `Host`, then the form's
  `csrf` must equal HMAC-SHA256(per-process secret, "problems-ack" +
  `Guard.Binding()` = hex sha256(token)); body ≤ 8 KB; fingerprint must be 32 hex;
  303 back to the filtered list. Other methods: 405 with `Allow`.
- **Metrics**: `reviewbot_problems_total{kind,source}` (every persisted
  occurrence: once per job and fingerprint) and `reviewbot_problems_new_total{kind}`
  (new or re-appeared), via `Options.OnRecord`.

## Reactions (internal/reactions, review/reactions.go)

Award emoji on notes addressed to the bot and on MRs; best effort (never
fails or noticeably slows a job). Switch `review.reactions.enabled` (default
true); every event has a GitLab emoji name (`picked_up` eyes, `done`
leftwards_arrow_with_hook, `failed` x, `postponed` hourglass_flowing_sand, `denied`
no_entry_sign, `thanks` heart, `ack` thumbsup, `learned` brain, `clean_review`
tada), `""` turns one off, names are validated (`^[a-z0-9_+\-]{1,64}$`).
`.reviewbot.yaml` `reactions.enabled: false` can only turn them off
(`Effective.Reactions`).

- **Classifier** (`reactions.Classify(text, mention)`): deterministic, RU+EN,
  conservative. Lower-cases, `ё`→`е`, GitLab shortcodes (`:+1:`) to emoji; any
  `?`, other `@mention`, unknown word/emoji, quote or > 200 runes -> `None`.
  Words between punctuation separators (`, . ! ; : … ( ) « » — *` and the like) are covered greedily by
  known phrases (thanks / ack / fix claim / neutral "бот"); emoji from fixed
  sets (variation selectors, skin tones, ZWJ ignored). Precedence `Fixed` >
  `Thanks` > `Ack`. `review.trivialReply` (learning) uses it too.
- **Routing** (`events/react.go`, the same `events.Router` for webhook and poller): a mention
  note classified (whole note) `Thanks` / `Ack` -> `core.JobReact` (Question =
  the chat question, for the fallback); `Fixed` stays `JobChat` (the chat run
  re-checks and may resolve). A threaded note without the mention that is
  `Thanks`/`Ack`/`Fixed` -> `JobThreadReply` even when learning is off.
- **Worker** (`reactions.Reactor`): one goroutine, FIFO channel (256; full ->
  `dropped`), each `Change{Event, Target, Add, Remove, Allowed}` = list awards
  (all pages) + delete the bot's own awards in Remove (never another user's;
  `UserID == bot id`, else username) + add missing Add names; a failed add is
  re-checked by listing (GitLab answers a duplicate with a 4xx), 403 ->
  `forbidden` and `OnForbidden` once per project (problem
  `reactions_failed`). 10 s per change. `Allowed` (the repo switch) is
  evaluated in the worker: cached per MR for 2 min from every
  `effectiveConfig`, else GetMR + `.reviewbot.yaml`. `Observe` ->
  `reviewbot_reactions_total{event,result}` (added, exists, disabled, error,
  forbidden, dropped); `Step` -> a /runs step in the job's run (no-op after
  the run finished).
- **Lifecycle** (`review/reactions.go`): `Reviewer.Accepted` wraps
  `q.Enqueue` (inside the poller dedup) and queues 👀 for command/question
  kinds (review, full review, check, chat, help, rules, memory, MR commands) with a
  note, not by the bot, not by an ignored user (`ignore_users`, GitLab bot
  users `project_N_bot…`, `group_N_bot…`, support-bot, ghost, …).
  `Handle` keeps a per-job state in ctx: `reactBegin` re-queues 👀 (removing
  ⏳) when the note was postponed or not seen by Accepted; `reactEnd`
  queues `postponed` (`*core.RetryLater`, note remembered as postponed),
  `failed` (error), `denied` (`reactDenied` in `checkAccess` and both "⛔"
  limit replies) or `done` (removes 👀 and ⏳), plus `learned`
  (`reactLearned`: thread-reply extraction, chat `learning`, `запомни`), then
  waits ≤ 3 s for the job's last change. Cancellation (shutdown) changes
  nothing. Thread replies get 👀 only right before the extraction run
  (`reactPickedUp`) and "done" only removes it. A job the queue drops on
  restore (`answered`, `expired`; `queue.Options.OnDrop` ->
  `Reviewer.Dropped`) gets a `done` change that only removes 👀 and ⏳ (its
  outcome is unknown).
- **JobReact** (`handleReact`): re-classifies Question; thanks -> ❤️; ack ->
  👍 only when the discussion's first note is a bot remark (finding, compat,
  lint marker); otherwise, or when reactions are off for the repo / user, it
  falls back to `handleChat` (with the 👀 lifecycle; the meter kind becomes
  `chat`, so /usage bills it as a question, not "other"). No Claude run,
  reply or limit otherwise. Plain thread replies (`reactThreadReply` in
  `handleThreadReply`, after the bot-thread check): thanks -> ❤️ only when
  the previous non-system note is the bot's; ack/fix claim -> 👍 only in a
  bot remark thread; either way no extraction.
- **Clean review** (`reactReview` after the summary): `reviewClean` = no new
  findings (published/outside), no open bot finding thread, no lint
  published/outside/remaining, no compat published/outside, gate count 0,
  not partial -> 🎉 on the MR; otherwise the bot's 🎉 is removed.
- **GitLab** (`gitlab/awards.go`): `ListAwards`, `AddAward` (JSON `name`,
  retried on 5xx), `DeleteAward` on `/merge_requests/:iid[/notes/:id]/award_emoji`;
  write policy `WriteAwards` (POST on `…/award_emoji`, DELETE on
  `…/award_emoji/:id`) is part of `BotWrites`, not of `ServiceWrites`.
- `@bot config` shows a "реакции (`reactions`)" row.

## CI failures (review/pipeline/ci.go, ci_digest.go, ci_explain.go, ci_main.go)

Owner's request: catch failures of lint, tests and any pipeline. Switch
`review.ci` (`enabled`, `explain`, `model`, `max_log_lines` 40, `max_tests` 10,
`main_branch`, `main_sources` (default `[push]`, GitLab pipeline sources,
`parent_pipeline` refused), `ignore_jobs` path.Match globs, `notify_main` usernames); repo
`ci: {enabled (only off), ignore_jobs (added)}` -> `config.EffCI`.

- **Events.** `core.JobPipeline` with `Job.Pipeline *core.PipelineRef{ID, Ref,
  SHA, Status, Main, DefaultBranch, Source}`: the Pipeline Hook (`webhook/pipeline.go` -> `events/pipeline.go`:
  finished `failed`/`success` only, tags and child pipelines (`source:
  parent_pipeline`: the parent's event covers the MR head and the main state)
  ignored, also for polled pipelines; MR iid from the payload's
  `merge_request`; `Main` when the ref is `gitlab.MainBranchFor(path,
  default_branch)` and there is no MR), the pipeline poller (see Polling), and the
  review itself: after the summary is written `watchCI` enqueues the job when the
  head pipeline has failed and the summary does not show it, or passed while a
  red block/thread exists; while it runs, the JobRefreshStatus chain (tbd.go)
  keeps polling and calls `watchCI` when it ends. `Manual()` is true but the kind
  is routed explicitly (no reply, no reactions, no per-user limits). Dedup key
  `pipeline:<project>:<pipeline>:<mr>:<status>` plus `PipelineRef.At` (see
  Polling: `poller.Dedup`); the handler is idempotent anyway.
- **Routing** (`handlePipeline`): MRIID set -> MR scope; `Main` -> main scope;
  otherwise `ListCommitMRs(sha)` (opened, `source_branch == ref`) -> one
  JobPipeline per MR through `enqueueJob` (so the MR job is serialized with the
  MR's reviews by the queue's MR key).
- **Digest** (`buildCIDigest`): `ListPipelineJobs` (latest attempt of each job:
  GitLab omits retried ones without `include_retried`) -> `failed` jobs; ignore
  globs -> "not analysed" line; `allow_failure` -> warning line; manual, skipped,
  canceled, success are not failures. Tests: `GetTestReportSummary` and, when
  something failed, `GetTestReport` (32 MB cap; failed/error cases kept) mapped
  to jobs by `build_ids`, else suite name = job name / parallel base name; at most
  `max_tests` cases per pipeline (first 5 lines x 200 runes of the message).
  `lint.ci` jobs (`ciLintToolOf`, `matchCIJobs`) are only referenced (their
  findings are lint threads). Every other failed job (at most 6): `GetJobTrace`
  tail (512 KiB, `request.tail`: the body is read through a tail buffer, at most
  64 MiB) -> `cleanCILog`: GitLab section markers, ANSI CSI/OSC, `\r` progress
  overwrites (the text after the last `\r`), leading timestamps (`2026-…Z 01O`),
  runner boilerplate, control characters and empty lines removed, lines cut at 300
  runes, last `max_log_lines` kept, then the joined text is scrubbed
  (`Deps.Scrub` = `diag.ScrubFunc(diagSecrets(cfg))`). CI is read with the bot's
  token only (`ciRead`; no service-token fallback: the content is published in
  the MR and sent to the model, so it must be what the bot's identity may see);
  a 403 on a job log -> the digest line "лог job `X` боту недоступен (нет прав) —
  откройте job: <url>"; failures -> problem `ci_fetch_failed` (403: the hint to
  give the bot access or allow CI visibility). Fingerprint = sha256 of per job: name, failed test ids,
  last 8 log lines through `problems.NormalizeSubject` (digits/hex/temp paths
  stripped), sorted, 16 hex.
- **MR scope** (`handleMRPipeline`): MR opened and not draft, not paused and
  without the ignore marker (`skipAuto`, like auto reviews: no summary edit, no
  explanation, no thread), repo config valid,
  `eff.CI.Enabled`, head pipeline for the head SHA (else nothing). `failed` ->
  `ciRed`: digest; only ignored jobs -> block removed. A failure is (pipeline,
  fingerprint): the same pipeline failing again after a job retry with another
  fingerprint is a new failure. Explanation decision: the active thread
  (`mrthread.Threads.CI`, newest unresolved red `ci-explain` thread) already shows this
  pipeline with this fingerprint and an explanation -> nothing; explain off or only `lint.ci` jobs -> none;
  same fingerprint as the explained pipeline -> no run, `ciSameFailure` rewrites the
  thread head ("🔁 Упал так же, как пайплайн #P"), result `duplicate`; else
  `explainMR` (checkout + `/base` + capped diff). Metrics once per new (pipeline,
  fingerprint) (neither the block's `pipeline`/`fp` nor the thread's are it). The summary block (`renderCIBlock`:
  digest with collapsed log tails, the "why: see the thread" line, or the reason
  there is no explanation) is written into the existing summary (`replaceCI`: in
  place, else after the TBD block, before the compat section, or before the footer);
  a re-rendered summary keeps it when its sha is the head (`keptCIBlock`, flow.go
  and bigmr.go). Thread (`renderCIThread`): explanation (+ the digest when there is
  no summary); without an explanation a thread is opened only when there is no
  summary. Created as a general draft note + publish, edited in place
  (`UpdateNote`); no blocking marker, not counted by `gateCount`. Claude usage
  limit -> the digest is out with "Объяснение не построено: достигнут лимит
  использования Claude…", no `RetryLater`, no
  problem, result `rate_limited`; the next failure tries again. `success` ->
  `ciGreen`: block removed, thread rewritten "✅ Пайплайн #M прошёл" (old text in
  `<details>`, marker `state=green`) and resolved (Moderator, else bot token; 403
  only logged). A later failure opens a new thread.
- **Explanation** (`explainCI`): `CheckGlobal` (denied -> `limit`), `meter.KindCI`,
  `runStructured` (records `UsageAuto` per attempt, retry with the nudge),
  `RunRequest{Model: cfg.CIExplainModel() (review.ci.model, else
  claude.light_model, else claude.model), no advisor, no search agent,
  Read/Grep/Glob over the checkout + BaseDir, claude.timeout}`. Prompt: the failed
  jobs inside `<untrusted_ci_failures>` (`renderCIDigestPrompt`; job names appear
  only there, the closing instruction gives their count), the diff (diffcap.Cap
  60 KB) inside `<untrusted_diff>`, MR title / candidate MRs as untrusted text.
  Schema `{jobs: [{job, cause: mr|infra|flaky|unknown, summary, fix,
  locations[]{path, line}}]}` (+ required `culprit_mr` in the main scope).
  `parseCIExplanation`: known jobs only (first wins), unknown causes -> unknown,
  markers stripped (`marker.StripBot` repeats until stable: nested markers
  cannot reassemble), quick actions neutralized, `@x`/`#1`/`!1` broken outside code
  spans (`mdtext.DefuseMentions`: only a backtick run closed by a run of the same length
  on the same line is code; unpaired, escaped or inside `<…>` backticks open
  nothing), 1000 runes; locations: relative clean paths only, <= 5; no entry for any
  job -> invalid. Untrusted CI text in notes: every `<!--` is broken with U+200B
  (`mdtext.DefuseComments`) at the render chokepoints (`renderCIDigest`,
  `renderCIExplanation`, the content of `renderCIBlock` / `renderCIThread` /
  `renderMainNote`, old text kept by the green and "same failure" rewrites), so
  only the bot's own markers are comments and `marker.CIBlockRe` always spans the whole
  block. Rendering: cause badge, `group/project:path:line` citations
  linked through the citation linker to the failing commit. Metrics
  `reviewbot_ci_explain_runs_total{result}` (ok, limit, rate_limited,
  invalid_output, error, duplicate) and `reviewbot_ci_failures_total{scope,cause}`
  via `Deps.CI` (`*telemetry.Metrics`).
- **Size.** The explanation runs for MRs over `review.max_mr_lines` too (it is
  about the failure, not a review); the diff it gets is capped.
- **Main scope** (`handleMainPipeline`): `effectiveConfig` with a synthetic MR
  whose target branch is the main branch (so `require_repo_config` and repo
  `ci.enabled` apply), `eff.CI.MainBranch`; `GetPipeline` re-reads the status;
  a pipeline whose source (API, else the event) is not in `main_sources`
  (`CI.MainSourceAllowed`: default push; `parent_pipeline` never; "" allowed) is
  ignored before the state is read, so it neither reports nor moves
  `last_pipeline`; state `core.CIMain` (`ci_main(project_id, branch, green_pipeline, green_sha,
  red_pipeline, red_sha, culprit_mr, note_id, last_pipeline, updated_at)`, named
  migration `ci_main_v1`, `pipeline.CIMainStore` = `review.Deps.CIMain`, `*store.SQLite`); a pipeline
  older than `last_pipeline` is ignored. Failed while green: digest (only ignored
  jobs -> nothing), last green sha (state, else the newest of `ListPipelines(status=success,
  limit 20)` with a counted source), `CompareCommits(green, failed)` (newest 30), `ListCommitMRs` per
  commit -> merged MRs into the branch (newest merge first, <= 20) and commits
  without one; explanation over the green..failed diff with the candidates (it
  may name `culprit_mr`); culprit = the named one, else the newest merged MR. Note
  (`CreateNote` in the culprit MR, marker `ci-main`): author and merger (`merge_user`,
  else `merged_by`) mentioned unless every cause is infra/flaky, `notify_main` cc,
  other candidate MRs, explanation, digest. Failed while red: state only (one note
  per incident). No candidate (direct push): problem `main_pipeline_failed`,
  `problems.Commit` -> mention `review.problems.notify` when due plus
  `notify_main` in a commit comment (`CommitComments`, `WriteCommitComments`), then
  `MarkNotified`. Passed: an open incident's note is rewritten (`ListDiscussions`
  of the culprit MR to read it): "✅ `main` снова зелёный (пайплайн #M)" + the old
  text in `<details>` with `@` defused; state green.
- **Capabilities** (`core.GitLabAs`): `CIFailures` (jobs, trace, test reports),
  `CIMain` (pipelines, compare, commit MRs), `CommitComments`;
  `cmd/reviewbot/capabilities_test.go` checks the diag wrapper keeps them.

## Groups and webhook sync (internal/hooksync)

GitLab Free/CE has no group webhooks. `gitlab.groups` lists full group paths
(subgroups included). Hook sync authenticates with `GITLAB_SERVICE_TOKEN`, a
separate Maintainer account; the bot's own token stays Reporter.

- Exclude: `gitlab.exclude` globs (`GitLab.Excluded`) drop events in `events.Router` (webhook and poller; the poller skips such MRs before reading notes) and are skipped by hook sync (`Report.Excluded`). `review.context.exclude` hides projects from context repositories (related ones included, reported as missing).
- Filter: `events.Options.Groups` — MR and Note events are accepted only when
  `project.path_with_namespace` equals a group or starts with `group + "/"`
  (case-insensitive); others get 200 "ignored". Empty = accept all (warning at
  startup). This also rejects deliveries from foreign hooks pointed at the bot.
- Sync (`gitlab.sync_hooks`, requires `webhook_url` + groups): `hooksync.Syncer`
  lists projects of every group (`include_subgroups`, non-archived, no shared),
  dedups by ID and reconciles one project hook per project, matched by URL
  (scheme/host case-insensitive, trailing slash ignored): missing -> create
  (MR + note events, pipeline events = `review.ci.enabled`, `push_events=false`,
  ssl per `hook_ssl_verify`, token = webhook secret); events off / pipeline
  events differ / ssl differs -> edit; duplicates -> keep first,
  warn, never delete. 403/404 -> Skipped; other errors -> Failed; the pass
  continues. `Sync` errors only when every group listing failed.
- Tokens: GitLab never returns hook tokens, so `Sync(resetTokens=true)`
  re-writes every bot hook. `serve` runs `Syncer.Run`: a reset pass at startup,
  then non-reset passes every `sync_interval` (picks up new projects).
- `reviewbot sync-hooks -config config.yaml [-dry-run]`: one reset pass,
  report on stdout, exit 1 when `Failed > 0`. Loads the config with
  `config.LoadSyncHooks` (only gitlab.url, service token, webhook secret,
  webhook_url, groups are validated; no `GITLAB_TOKEN`). Skipped projects:
  the service token's user needs Maintainer/Owner, the bot stays Reporter.
- The hook endpoints (`ListGroupProjects`, `ListProjectHooks`, `AddProjectHook`,
  `EditProjectHook`) are methods on `*gitlab.Client` only, not on `core.GitLab`.

## Repository config: opt-in and validation (review/repoconfig.go, config/repovalidate.go)

- Validation: `config.ValidateRepo(b) (*Repo, RepoErrors)` collects every
  problem with its 1-based line (goccy/go-yaml AST): syntax (then stop), more
  than one document, size > 256 KiB, unknown keys at any depth (`checkKeys`
  walks the AST against the struct tags; suggestion by edit distance <= 2),
  duplicate keys, removed keys (`removedRepoKeyReason`: `setup`, `tools`,
  `compat.commands` -> the removed.go reasons, `lint.custom`,
  `lint.semgrep_configs` -> `LintRemovedHint`; such issues make the error
  match `ErrRemovedKey`), types (whole document, then each top-level key
  separately so every bad section is reported), values (every section's
  `normalize`, also after another one failed). `ParseRepo` = ValidateRepo
  returning `RepoErrors` as the error. Unknown keys of `config.yaml` stay
  ignored (removed ones fail the start, removed.go).
- JSON Schema `docs/reviewbot.schema.json` is generated from the Repo structs
  and their field doc comments by `internal/config/schema_test.go`
  (`UPDATE_SCHEMA=1`); `TestRepoSchemaUpToDate` fails when it is stale. Closed
  value sets come from the code (`LintTools`, `ciFormats`, `LabelFacts()`,
  severities). `additionalProperties: false` everywhere, so removed keys show
  up as unknown in an editor.
- `reviewbot validate-repo-config [path]` (cmd/reviewbot/validate.go): the
  same ValidateRepo, `path:line: key: message` per issue; exit 0 valid, 1
  invalid, 2 usage / unreadable file. No config, no network.
- Reading: `rawRepoConfig` caches found / not found per (project, ref) for
  `Deps.RepoConfigTTL` (default 5 min; errors are never cached; at most 2000
  entries and 32 MiB of content; from 200 entries every insert drops the
  expired ones; over a bound everything is dropped). The read is capped at
  `config.MaxRepoConfigBytes` (256 KiB, `gitlab.Client.GetRawFileMax`,
  `core.ErrTooLarge`); a larger file is cached as "too big" without its content
  and reported like `ValidateRepo` does (`config.RepoTooLarge`). The target
  branch is read by name; the MR head and merge base by SHA (immutable).
- Opt-in: `optedIn` = the target branch has the file (valid or not) or
  `review.require_repo_config` is false. `targetRepoConfig` /
  `effectiveConfig` return `*repoConfigError{notOptedIn}` or
  `{err: RepoErrors}`; every command path answers through `denyRepoConfig`
  (reply + `reactDenied`; for an invalid file also the note and the problem).
  `help` answers with a "not connected" line; `pause`/`resume`/`ignore`/
  `resolve all` are refused only when not opted in. Background jobs
  (learning from replies, refresh status, title hints, resume) skip silently.
- Review gate (`reviewConfigGate`): read the target file and, when the MR
  adds or changes the file (`headRepoConfig`: HEAD differs from the target
  branch and from the merge base), the HEAD file. Not opted in and no HEAD
  file -> skip without listing discussions (the cheap path for every event
  of a not connected project). Otherwise `syncConfigNote` lists the
  discussions, finds the oldest bot note with the marker and creates /
  updates / leaves it (`configNoteBody`: write=false when there is nothing
  to say). Then: target invalid -> problem `repo_config_invalid` (subject =
  file name: one fingerprint per project), `OnSkip("repo_config_invalid")`,
  manual reply + 🚫; not opted in -> `OnSkip("no_repo_config")`, manual reply
  + 🚫; else the review goes on, and an invalid HEAD file adds a line at the
  top of the summary.
- Note lifecycle: `state=error` while the target (or the MR's own) file has
  errors; when the target is valid again (or the MR fixes its file) the same
  note is edited to a `state=ok` text; with no error before and nothing new
  in the MR, no note is written. The note is never deleted and never blocks
  the merge; text from the file goes through `cfgText` / `cfgCode` (no
  mentions, no `#12` / `!12` references — a zero-width space after `#`/`!`,
  links, HTML comments, code-span breakouts).
- Hook sync and polling are not affected: hooks go to every group project
  (minus `gitlab.exclude`), because the opt-in is per target branch, can
  appear at any time, and the onboarding MR must be seen to be validated. A
  not connected project costs one cached file read per event.

## Trunk-based status (review/pipeline/tbd.go)

`review.tbd` (+ repo `tbd`, thresholds override both ways). After the review,
`collectTBD` reads `GetMRDiffs` (size without ignored files; empty diffs mark
the count as a lower bound), `ListMRCommits` (oldest `authored_date` = branch
age, else MR `created_at`) and the MR (`diverged_commits_count` via
`include_diverged_commits_count=true`, `head_pipeline`). `renderTBD` emits a
block between `<!-- reviewbot:tbd -->` markers, placed before the compat
section. While the pipeline runs, `watchPipeline` enqueues one
`JobRefreshStatus` per MR (in-memory `tbdWatch`); its handler re-renders only
the block (`replaceTBD`; also the coverage block and, while CI linter jobs
are awaited, their threads and block — review/linters/cilint.go) and returns
`core.RetryLater{+2m}` until the pipeline finishes or 3h pass (with review.ci
on, also while the head pipeline runs; when it has finished, `watchCI` enqueues
the `JobPipeline` — ci.go). No Claude, no
limits. The feature-flag rule is
a system prompt note (`tbdRulesNote`): behavior changes of functionality in
use without an off switch → one `major` finding; additive changes exempt;
`flags_hint` names the team's mechanism.

## Size limit (review/bigmr.go)

`review.max_mr_lines` (default 2000, 0 = off; the repo's `max_mr_lines` only
lowers it, `stricter`) and `review.big_mr` (`deterministic`, `override_label`,
`block`; the repo may only turn `block` on) merge into `Effective.BigMR`.

- Count: `diffSize` over the review's own `git diff from..head` (already
  without ignored files; `ignoredPath` is applied again so the globs match the
  TBD count exactly), `countDiffLines` per file chunk (header lines before the
  first `@@` are not changes). The range is the review's: whole MR on a full
  review, the new commits on an incremental one.
- Over the limit, checked in this order: `JobForceReview` -> review;
  the override label on the MR (read through `core.GitLabAs[mrLabelReader]`,
  else the label clients; read only when over the limit) -> review; else
  `reviewTooBig`: scan threads; with `deterministic`: compat (deterministic
  items only — `compatCandidates` with an empty answer, so nothing to verify)
  and lint restricted to gitleaks (other tools' threads stay as they are, see
  `lintToolOn`); publish/auto-close their threads; the size thread; TBD;
  labels (the migration fact forced unknown; instruction rules unanswered);
  problems; summary (gate line, too-big block, TBD, problems, secrets, compat,
  API, lint, labels line, "Не проверено Claude" footer). State: the summary
  note id, `ReviewedSHA` unchanged. Manual job: reply "📦 MR слишком
  большой…". `OnSkip("too_big")` -> `reviewbot_skipped_total`; the log line
  "mr too large, claude review skipped" makes the `/runs` record `skipped`. Not
  a verification problem.
- Reviewed over the limit (override): one summary line naming the override.
- `block`: `tooBigThread` opens one general draft-published thread with the
  blocking marker (counted in the gate line) unless an open one exists or a
  person resolved the last one (deliberate override). After any review that
  runs Claude, `closeTooBigThread` resolves an open one with the fixed reply
  (`resolveSizeThread` -> `markFixed`; a Reporter bot without a service token
  leaves it pending, and it keeps counting as `fixedPendingBlocking`; any
  other resolve error, or discussions that cannot be read, keep it counted as
  open — not as "исправлены, ждут закрытия"), whether or not `block` is
  still on. A size thread that cannot be posted counts in the gate anyway
  (fail closed, "⚠️ Не удалось опубликовать …" under the gate line); the next
  too-big review posts it. With `block` off a too-big review resolves an open one too
  (`tooBigThread`), and the "nothing to review" path (empty range: the big
  change was reverted) closes it and recounts the summary's gate line
  (`closeSizeThreadNoChanges`, `gateCount`).

## Diff coverage (internal/diffcov, review/pipeline/coverage.go)

Repo `.reviewbot.yaml` `coverage: {job, path, min}` or `reports: [...]`;
service `review.coverage`. `collectCoverage`: head pipeline (must be for the
MR head SHA) running/missing -> waiting; not success -> reason; success ->
`ListPipelineJobs` (exact name or parallel `name N/M`, success only) ->
`GetJobArtifact` (size cap; redirects to object storage never carry the
token, `gitlab.Client.checkRedirect`) -> `diffcov.Parse` (Cobertura, Clover,
Go profile, LCOV) -> `Merge` -> changed lines from `GetMRDiffs` hunks
(ignore applied) -> `Compute` (longest-suffix path match, ties to the
shortest report path; executable lines only). Summary block between
`<!-- reviewbot:coverage -->` markers after the
TBD block; the JobRefreshStatus chain also runs while coverage waits and
re-renders it. When known before Claude runs, Claude gets the computed
numbers, not the reports: MCP off -> `coverageForClaude` puts the totals and
the uncovered changed lines per file at the end of the user prompt (least covered
first, at most 20 files x 10 ranges, "and N more"); MCP on -> a one-line
summary plus the `coverage` tool (per file, optional path filter). Nothing is
written into the clone and nothing runs in the sandbox (the former
`.reviewbot-coverage/` + `diffcov` tool are gone).

## Migration safety (internal/migrations, review/migcheck/migrations.go)

`review.migrations` (+ repo `migrations`: enabled can only turn it off,
paths/exclude append, dialect overrides). `collectMigrations` runs before
Claude on the FULL MR diff (merge base .. head; incremental reviews reuse the
system map's full diff or compute it): changed `.sql/.php/.py` files (max 300
reads) are read at head and `migrations.Analyze` detects migrations by path
globs and content, extracts operations of the forward direction only
(Doctrine/Laravel `up()`, Alembic `upgrade*()`, Django `operations`, goose/dbmate
up sections; `*.down.sql`, Flyway `U*`, sqitch `revert/` `verify/` skipped),
keeps the ones on added lines (all lines of new files), skips tables created
and columns added in the same MR, and adds `modified_migration` for files that
exist at the merge base. Rules are a table (`migrations.Rules`: kind, level,
contract, Russian title/risk/safer text, English reason, MySQL overrides).

Contract kinds (drop/rename table/column) get a usage check:
`migrations.CheckUsages` searches the merge base and the head through
`core.WordGrepper` (repo.Checkout: `git grep -n -w -F -I --null --no-textconv
<rev> -- <source pathspecs>`, output capped at 8 MB; read-only, never touches
the work tree) or, when the checkout cannot grep, `ListFiles`+`ShowFile` with
file/byte caps. Variants: snake/camel/Pascal for columns, plus the singular
entity class for tables. Tests, vendored code, migrations, schema dumps and
comment-only lines do not count. Generic names (id, status, created_at, …,
< 3 chars) are `Ambiguous`: only files that also mention the table count.
The analyzer still computes levels internally (`levelOf`, `Counts` for the
log line), but nothing model- or user-facing shows them: the analyzer only
gives HINTS, Claude decides.

Output: hints as a user prompt section "Database migrations: where to look"
inside `<untrusted_migration_facts>` (paths, names, snippets via
`untrusted()`/`mdtext.OneLine`): migration files (framework, new/changed/MODIFIED),
operations ordered by path and line (`path:line kind object [-> new]`, the
rule's English "why to check", "uncertain extraction" for weak ones) and, for
contract kinds, the text-search candidates in the merge base and at head
(flagged as candidates to read; "no occurrence" reminds of dynamic use); the
section says the list may be incomplete (`DO $$`, SQL from variables). A
static system note (`migrationsSystemNote`, only when the MR has migrations;
it names no SHA — "its commit is given in the user prompt" — and is the LAST
part of the system prompt, after the models note, so the static prefix is the
same with and without it: prompt caching): expand/contract; the list is only a pointer; critical
ONLY for a drop/rename verified in the merge base, with `deployed_usage`;
everything else at most major; no compatibility items for migration files.
No summary block, no bot threads. Schema: `withMigrationsSchema` adds the
optional `deployed_usage` {path, line ≥ 1, name} to `findings.items` (only
when the MR has migrations); `output.Finding.DeployedUsage *output.UsageRef`.

Enforcement (`enforceMigrations(ctx, log, co, out, mg, lang)`, after parsing,
before the verification run and publishing): every blocking finding on a
migration file of the MR keeps its severity only if `deployed_usage` passes
all checks — clean relative path (no `..`, not absolute), not a migration
(`IsMigrationPath`/`res.IsMigration`), `migrations.IsSourceFile`, line ≥ 1,
name `[A-Za-z0-9_]{2,128}`; the name or a variant (`migrations.NameVariants`:
snake/camel/Pascal, singular entity, plural table of a class name) occurs as
a whole word in the migration's head content (kept in `migRun.heads` from
`collectMigrations`); `ShowFile(merge base, path)` succeeds, the line exists,
is not comment-only and contains the name as a whole word
(`migrations.UsesName`). The analyzer does not need to have detected the
operation. Otherwise the finding goes to `nonBlockingCeiling()` and the reason
is logged. Kept criticals get "Использование в задеплоенном коде: `p:l`"
(English for non-Russian `language`) unless the body names the place.
Breaking-change items on migration files always become findings at the
ceiling (`compatToFinding`). `deployed_usage` is dropped on every other
finding. Budget: 20 merge-base reads per review (cached per path), 20 s.
Panics are recovered; the analysis has a 90 s budget; nothing here fails a
review.

## Models: reviewer, advisor, search subagent

Runs with tools (review, chat, the verification run of findings and
breaking changes) get `claude.model` (default sonnet; the verification run:
`review.verify.model` when set) plus, via `review.withModels`:
- `--advisor <claude.advisor_model>` (default opus): Claude Code's
  server-side advisor tool (2.1.310 has the flag and the `advisorModel`
  setting; the advisor must be at least as capable as the model). The system
  prompt says when to consult it (blocking findings, breaking changes,
  concurrency/security/data loss). If a run fails with an advisor error
  ("does not support the advisor tool", "not a valid advisor model", …) the
  runner sets a process-wide flag and retries once without it.
- `--agents {"code-search": {tools: [Read, Grep, Glob], model:
  <claude.search_model>}}` (default haiku), `--tools …,Task`,
  `--allowedTools … Task(code-search)` and explicit denies for the built-in
  agents seen in `system/init.agents` (general-purpose, Explore, Plan, claude,
  claude-code-guide, statusline-setup); dontAsk refuses any other agent.
- The MR review run on the normal route only (`env.WithReviewModels`; not
  the light route, chat or verification) also gets `"deep-review": {tools:
  [Read, Grep, Glob], model: <claude.deep_review_model>}` (default opus) in the
  same `--agents` value and `Task(deep-review)`. `ModelsNote(advisor, search,
  deep)` makes one call mandatory on a non-trivial MR (changed files with
  new-side ranges + draft findings; the subagent has no git) and turns the
  advisor note into a one-line fallback; the text is MR-independent (prompt
  cache). A failed call is an error `tool_result` of its Task/Agent
  `tool_use` (the run goes on): `streamCapture` records the first one ->
  `RunResult.DeepReviewError` -> one runner warning per process and the
  `deep_review_unavailable` problem; no process-wide off switch (a transient
  overload must not disable it). Its tokens are in `modelUsage` (subagents
  included) -> per-model records -> `/usage`.
Tool-less runs (`NoTools`) never get any of them.

Routing by risk (`review.routing`, review/route.go, config/routing.go): the
review run of a low-risk range uses the light model (`EffRouting.LightModel`:
`review.routing.light_model`, else `claude.light_model`) with no advisor and
no MCP session (the search subagent stays; `withRoute` appends
`modelsNote(false, search)` — a per-route constant, the system prompt stays
byte-stable per repository and route). Low risk (`decideRoute`, no I/O):
changed lines of the reviewed diff (`diffSize`) <= `max_lines` (60), every
reviewed file matches `low_risk_paths` (a renamed or copied file by its new
AND its old path: `oldPaths` of the diff, review/resolve_scope.go), and no
signal: a file (old paths included) under `sensitive_paths`, `migRun` present, an API spec
in the compat result (`Specs`/`Deleted`/`Changes`/`Warnings`, or `cr.err`
with no result) or in the OpenAPI changelog, a gitleaks result, system map
contracts touched (`sysmapRun.imp.Contracts`), phase B dependencies
(`depsRun.missing`/`linked`); then (`routeReview`) the whole MR (merge base ..
head: the reviewed diff, else `smDiff`, else `co.Diff`) must not exceed
`review.tbd.max_lines` (the TBD size warning; unknown -> normal). Models and
routing are service-only (operator's decision): `.reviewbot.yaml` cannot
change them, and a `routing` section there is a removed key
(`RemovedRepoModelReason`), so the repository config is refused. The verification run never uses the route
(`withModels` + `review.verify.model`), so a critical/major from the light
model is verified by the normal model. Recorded: log "review: model route",
`Deps.OnRoute` -> `reviewbot_review_route_total{route=light|normal|heavy}` (normal
also when routing is off), `diag.SetRoute` -> run.json `route {route, model,
reason}` (the /runs page shows "Маршрут модели") and the job span attributes
`reviewbot.review.route` / `reviewbot.review.model`, and the summary footer
(`summary.WithRouteFooter`: "Проверено: `a..b` (…) · модель: haiku (низкий риск:
только docs, 12 строк)" / "· модель: sonnet (риск: миграции)"; nothing when
routing is off).

Heavy route (`heavyReason`, after the light decision, which keeps priority;
off while `review.routing.heavy_model` is empty, independent of
`routing.enabled`): a range with changed lines >= `heavy_min_lines` (300) or
a file (old paths included) under `heavy_paths` is reviewed by
`heavy_model` with no advisor and no deep-review subagent (the review model
did not call them; the search subagent and MCP stay). Footer "· модель: opus
(тяжёлый: 1356 строк ≥ 300)" / "(тяжёлый: путь db/migrations/1.sql)", /runs
"тяжёлый", `route=heavy`. The whole-MR TBD fallback of a light range stays
normal.

## Team skills (internal/skills)

`review.skills.dir` holds skills in the Claude Code format. The CLI's own
skill loading stays off (`--disable-slash-commands`; without it the CLI also
offers its built-in skills, and `--plugin-dir` skills need it removed), and
`--setting-sources user` keeps `.claude/skills` / `.claude/commands` of the
clone out (verified on 2.1.310: a project skill appears in `system/init` only
with `project` in the sources). The bot does the progressive disclosure
itself:

- `skills.Load` re-reads the dir on every review/chat run: one subdir per
  skill, `SKILL.md` frontmatter (`description` required, `name` = dir name;
  strict YAML, then a lenient line parser for unquoted colons), regular files
  only, hidden entries skipped, caps 2 MB/200 files per skill, 20 MB/100
  skills total. Broken skills are skipped with a warning.
- `.reviewbot.yaml` `skills.enabled` / `skills.disable` select; the selection
  is staged into a scratch dir (0444 files, 0555 dirs) and passed as
  `RunRequest.SkillsDir`: docker mounts it `:ro` at `/skills` (+ `--add-dir`,
  + sandbox `denyWrite`), local mode passes the host path.
- The system prompt lists `name — description (path/SKILL.md)`; Claude reads
  what it needs with Read.
- `claude/stream.go` records Read `file_path`s into `RunResult.ReadFiles`;
  `runStructured` hands them to a ctx collector; files under the skills root
  give the "used" list, logged to the job log (/runs) only, never into notes.

## Polling (internal/poller)

`gitlab.events`: `webhook` (default) | `poll` | `both`. Polling exists for bots
GitLab cannot reach (laptop behind NAT); it needs `gitlab.groups` (otherwise it
would see every MR the bot account can read) and no webhook secret; `/webhook`
is not mounted in `poll` mode and `sync_hooks` is rejected there.

- Classification is shared: `events.Router` (MR / Note / Pipeline events in
  neutral form; the webhook decodes the hook payloads into the same events)
  applies the rules plus the group filter, so both sources produce identical
  jobs.
- Each tick, per group: `GET /groups/:g/merge_requests?scope=all&updated_after=
  cursor-2m&order_by=updated_at&sort=asc` (all states). GitLab bumps
  `updated_at` on pushes, state/title/draft changes and new notes, but a note
  only if the MR was not updated in the last minute (measured on 19.4.1:
  bumps at t+0 and t+60 s, none between), so a command right after a push or
  after the bot's own note leaves `updated_at` unchanged.
- Per MR the stored `events.PollMR` (store `poll_mrs`) is compared with the list
  entry: same `updated_at` and older than `HotWindow` (2 m, = overlap) -> skip
  (overlap re-reads are free); same `updated_at` but hot -> only the discussions
  are read again (the throttled-bump case above); the first poll of a process
  looks back `CatchUp` (1 h) instead of the overlap and reads the discussions
  of all those MRs once (a throttled comment written while the bot was down);
  unknown + opened
  -> `open`; opened again -> `reopen`; draft -> ready -> `update` with
  BecameReady (ForceFull); new sha -> `update` with OldRev; title only ->
  refresh title. Then `ListDiscussions`: notes with ID > `LastNoteID` and not
  older than `NotesSince` become Note events (threaded = `!individual_note`),
  in ID order, after the MR job.
- First run per group (no cursor in `poll_cursors`): baseline of every open MR,
  nothing enqueued, `NotesSince` = now; history is never replayed.
- Cursor = max processed `updated_at`, never past an MR whose processing
  failed (it is retried next tick).
- `poller.Dedup` (in memory, 24h, every mode; seeded at startup with the
  jobs the queue restored, dropped or finished within the last 24h, see
  "Queue durability") drops the second copy of an
  event keyed by note ID, by MR + head SHA + ForceFull, or by project + pipeline +
  MR + status + the time the pipeline reached it (`JobPipeline`,
  `PipelineRef.At`: the hook's `finished_at`, the API's `updated_at`; two events
  within `PipelineSlack` = 5 s are one, so the webhook and the poller agree,
  while the same pipeline failing again after a job retry, or failed -> success
  -> failed, is a new event): with `both` the two
  sources see the same events; with `poll` alone a failed `SavePollMR` makes
  the next tick enqueue them again. Title refreshes are not deduplicated
  (idempotent).
- Pipelines (review.ci, `poller.PipelinePoller`, poller/pipelines.go): every
  max(2 x `poll_interval`, 1m) the projects of the groups (`ListGroupProjectsDetails`,
  cached 10 min, a failing group keeps its list; excluded/empty/archived skipped) ->
  `ListPipelines(updated_after = cursor - 2m, order_by updated_at asc, 100, page)`
  per project, page by page until a short page, at most 10 pages (1000 pipelines)
  per project per tick -> `events.Router.Pipeline` (MR iid from
  `refs/merge-requests/<iid>/…`). Cursor per project in `poll_cursors` (scope
  `pipelines:<id>`) = the newest `updated_at` read; when the page cap is hit, the
  cursor becomes last `updated_at` + 2m, so the next query starts right after the
  last pipeline read (no overlap for that step): a burst of pipelines inside the
  overlap window can never stall it. The first sight only sets it (no history).
- Metrics: `reviewbot_poll_ticks_total{result}`, `reviewbot_poll_jobs_total`,
  `reviewbot_poll_last_success_timestamp_seconds`, `reviewbot_poll_last_round_timestamp_seconds`
  and `reviewbot_poll_interval_seconds` (the configured interval: the
  PollerStalled / PollingFailing alerts use max(3 × interval, 10 / 5 min) as their
  window, so a long `gitlab.poll_interval` does not make them fire between rounds).

## Rate limits

Sliding 1h windows over the `usage` table:
- per user reruns (`@bot review` / `full review` / `check`) and per-MR rerun
  cooldown (not for `check`: recorded without the MR key);
- per user chat;
- global: all Claude runs (including rule extraction, `UsageLearn`);
- learning runs (rule extraction and synthesis, `запомни`, memory
  compaction: `UsageLearn`): `review.learn.max_per_hour`, a per-kind cap of
  the limiter (`ReserveRequest.KindPerHour`) checked and booked with the
  global limit in one `Reserve` (booked runs count). A replayed or resumed
  job (`queue.WrapLimiter`) drops only the user from its booking; the
  per-kind cap and the global limit still apply.
Every tool-less learning run goes through `env.RunToolLess` (booking
committed or usage recorded, `/usage` attribution, run problems, files read),
like the review's runs go through `env.RunStructured`.
System map extraction runs are background work with their own budget
(`review.sysmap.max_runs_per_hour`, counted from `sysmap_runs`), outside the
`usage` table and the global limit; they are still metered (`claude_runs`, kind
`map`). Phase B extraction runs for an MR's changed files have a separate budget
(`review.sysmap.deps_max_runs_per_hour`, default 60, rows with `kind = 'deps'`),
at most `deps_max_runs` per review (default 30), so the map and reviews never
take runs from each other.
Repo config can only make per-user limits stricter. Usage is recorded when a
Claude run actually starts, so debounced/skipped jobs cost nothing.

## Queue durability and shutdown (internal/queue, store/queuepending.go, store/queuejobs.go)

Every job survives a restart; a restart costs neither events nor running
Claude runs (within `queue.shutdown_grace`).

- **Auto reviews** (`queue_pending`, one row per MR, unchanged): saved at
  enqueue / debounce / RetryLater, deleted when a handler finished for good;
  a newer auto job of the MR replaces the row (ForceFull merged); a running
  job keeps its row (interrupted -> runs again). Restore re-queues at the
  saved ready time; an auto job already pending in memory wins.
- **Every other job** (`queue_jobs`, `queue_jobs_v1`): `AddJob` at enqueue
  (row id = AUTOINCREMENT, never reused, so `ORDER BY id` is the enqueue
  order), `UpdateJob` when dispatched (`started_at`, `answered` reset, written
  before the handler runs), when it answers / is charged, and on RetryLater
  (`ready_at` = At, `started_at`/`answered` reset: the handler asked for
  another attempt; `charged` kept); `FinishJob` when a handler finished for
  good or restore dropped the row: the row moves to `queue_done`
  (`queue_done_v1`: job, `done_at`) and markers older than `queue.DoneTTL`
  (24h) are deleted in the same call (separate table: an older binary
  never sees finished jobs as rows to replay). The stored job is explicit fields (`storedJob`): kind, project, path,
  MR, head SHA, user id/name, discussion id, note id, question, ForceFull,
  the whole PipelineRef, EnqueuedAt; only TraceParent is not stored (a test
  walks core.Job by reflection). Restore re-queues the rows by id with fresh
  seq numbers before any new job, so per-MR FIFO (`headManual`) holds across
  the restart; a row whose ready time is in the future is a retry wait again.
- **Order per MR** (`pickLocked`): one job of an MR runs at a time; ready
  manual jobs run in FIFO order (seq). A manual job waiting for its
  `RetryLater` time (the `refresh_status` chain every 2 min for up to 3h, a
  command postponed by the Claude usage limit) is not the MR's FIFO head:
  the MR's later jobs (commands, chat, thread replies, pipelines) run
  meanwhile; when its time comes it keeps its seq and goes before the jobs
  enqueued after it. `Stats.Pending` (`reviewbot_queue_jobs{state="pending"}`)
  therefore counts only jobs that run as soon as a worker and their MR are
  free (or whose debounce is running); waiting retries are
  `state="retry_wait"`.
- **Replay rule** (restore), same for every kind: older than
  `queue.manual_max_age` (by EnqueuedAt) -> dropped (`expired`); never started
  (or reset by RetryLater) -> `queued`; started and interrupted ->
  `replayed`, unless `answered` -> dropped (`answered`); unusable (no kind,
  no project, no MR — a `pipeline` job without an MR, main branch or branch
  fan-out, is kept when it carries its pipeline) -> `invalid`. `answered` is
  set explicitly by the job's final reply: `env.Reply` (every command's and
  question's answer, in its discussion or as a top-level note addressing the
  author) calls `core.RunState.MarkAnswered` once GitLab accepted it, for
  the running job (same project, MR and note). No other bot note marks it:
  a `check` or `review` written inside a finding thread posts the
  per-finding "fixed" reply (`publish.MarkFixed`) into the same thread
  first, and that is not the job's answer. Per kind:

  | Kind | Interrupted mid-run |
  |---|---|
  | auto_review | replayed (row in `queue_pending`; marker-based publishing, leftover drafts deleted) |
  | review, full_review, force_review | replayed unless the command already got its reply (✅/❌/⛔/too big/no new commits): marker-based, idempotent until the final reply |
  | check | same (the per-finding "fixed" replies are not its answer, also when the `check` was written inside a finding thread) |
  | chat | replayed unless the answer (or ⛔/❌) is in the thread |
  | help, config, ignore, pause, resume, resolve_all, rules, memory | replayed unless replied; their store effects are idempotent on a replay (pause -> "already paused", rules/memory dedup, forget -> "not found") |
  | thread_reply | replayed unless the "🧠 learned" reply is out; `MarkNoteProcessed` before the Claude run keeps a replay from learning twice |
  | react | replayed (award emoji are idempotent; the chat fallback replies, so it is covered like chat) |
  | refresh_title, refresh_status, pipeline | replayed (no discussion; handlers re-read and rewrite marker blocks, `ci.go` is idempotent) |

  The window between GitLab accepting the final reply and the row update can
  still duplicate that reply after a crash in that moment (best effort, no
  two-phase commit with GitLab); a replay of a review/check interrupted
  before its final reply re-posts nothing marker-based but may repeat a
  per-finding "fixed" reply whose thread it could not resolve. A reply that
  announces a postponement ("⏳ … отложено до …") marks the job answered
  until the `RetryLater` returned right after it resets the mark; a crash
  in between drops the postponed job. Handlers never reply on cancellation
  (`failManual`/`failChat`/`reactEnd` check ctx), so a drain does not mark
  jobs answered.
- **Limits.** Bookings (`Limiter.Reserve`) live in memory and are committed
  by the job's first Claude run (`usage` row) or released; a restart loses
  uncommitted bookings (nothing counted) and keeps committed rows (a real
  run happened). `queue.WrapLimiter` (around `Deps.Limiter`, inside the
  metrics wrapper) marks the row `charged` when a booking with a user is
  committed or a run with a user is recorded. A charged job is admitted for
  every later attempt: postponed by `RetryLater` in the same process
  (`retryLocked` sets `admitted` from `charged`), or restored with `charged`
  (replayed, or postponed before the restart). Its `Reserve` drops `User`
  (per-user limits and the MR rerun cooldown — the cooldown would otherwise
  deny the replay of `review` for 10 minutes) and books/records its runs with
  `UserID 0, MRKey ""`, so the per-user windows count the command once while
  the global window (and a per-kind cap, `ReserveRequest.KindPerHour`) counts
  every real run. An uncharged retry or replay goes through the normal path.
- **Run state (`core.RunState`).** The queue puts the running attempt's state
  into the handler's context (`core.WithRunState`; `core.RunStateFrom`):
  `Job`, `Admitted`, `MarkCharged`, `MarkAnswered`. `WrapLimiter` is
  written against it, `env.Reply` calls `MarkAnswered`, and the reviewer reads `Admitted`
  (`core.RunAdmitted`) to skip the access check and the per-user limits of a
  resumed command (review, check, chat) — the one rule for an in-process
  retry and a restart. It is the seam another job runner implements to keep
  the accounting and replay rules.
- **Event dedup.** `serve` calls `q.Restore()` before the event sources start
  and feeds its result to `poller.Dedup.First`: every job whose event is
  handled or queued — the jobs queued again, the rows dropped as `answered`
  or `expired`, and the jobs finished within `queue.DoneTTL` before the
  restart (`ListDone`). With `events: both` the poller would otherwise
  enqueue such a comment again (its `poll_mrs` record may not have advanced
  past a comment the webhook delivered and the bot answered) and the bot
  would answer twice.
- **Drain.** `Run(ctx)`: when ctx is done the dispatch loop stops (`Stats.
  Draining`), handlers keep running on `jobCtx` (`WithoutCancel(ctx)` + own
  cancel) for `ShutdownGrace`; then (or on `Abort`, a second SIGINT/SIGTERM
  via `abortOnSignal`, or at once with grace 0) jobCtx is cancelled and Run
  waits for the handlers. Interrupted = handler error with jobCtx cancelled:
  rows kept. RetryLater during the drain is a normal retry (row updated).
  Enqueue works during and after the drain (persists; nothing dispatched).
  `serve`: the pollers run on `evCtx` (`WithoutCancel(ctx)`), `/webhook`
  stays mounted; `runQueue` (drain.go) runs the queue on its own ctx and
  drains it on the signal or on a listener failure alike (the listener's
  error is returned after the drain, so restored jobs are never left
  running on a closed store); order: queue drain -> cancel evCtx -> `srv.Shutdown` (10 s)
  -> wait for the pollers -> flush traces -> store closed (deferred). Loops
  on the signal ctx stop at once: hook sync, context repositories refresh,
  system map scanner, diagnostics retention (reviews do not need them
  running: they read cached state with the job's own ctx). The MCP server
  stops on return (after the drain: running reviews use it). The container
  stop timeout must exceed the grace (compose `stop_grace_period: 6m`, local
  stand 3m, Kubernetes `terminationGracePeriodSeconds`).
- **events: webhook.** GitLab does not retry failed deliveries; startup logs
  a warning (`warnEventsWebhook`). `both` (poller cursors in the store,
  duplicates dropped) catches up after downtime; the production compose
  config uses it. Default unchanged.
- **Metrics.** `reviewbot_queue_restored_total{kind,result}` (kind =
  `meter.KindForJob`; result queued, replayed, expired, answered, invalid),
  `reviewbot_queue_draining` (gauge). Interrupted jobs count in
  `reviewbot_jobs_total{status="interrupted"}` (lost with the process unless
  scraped during the drain).

## Prompt layout and caching (review/prompt_cache.go)

Claude Code caches the request prefix (Anthropic prompt caching: cached input
tokens cost about a tenth). The prefix is, in order: the tool definitions
(`--tools` built-ins, `Task` with the `--agents` code-search definition, the
`StructuredOutput` tool made from `--json-schema`, the MCP tools of the run),
the system prompt (Claude Code's own, then ours via `--append-system-prompt`),
then the messages (our prompt on stdin is one text block). One differing byte
invalidates everything after it, and a hit is only possible at a boundary
Claude Code marks for caching (end of the system prompt, the last message), not
inside our one-block user prompt. So the rule, per kind of run:

- **System prompt** = text fixed by the bot version and the repository's
  configuration, in a fixed order (static rules first, then per-repository
  notes), without SHAs, counts, file lists, timestamps or MR text.
- **Per-run data** (SHAs, diff, coverage lines, system map facts, migration
  hints, task, open findings, claims) is in the user prompt, after the static
  instructions; the facts that used to be in the system prompt (coverage,
  system map, phase B dependencies) are appended at its end (`factsBlock`).
- **Rules that apply only to some runs** are worded conditionally ("when the
  prompt contains…", "in an INCREMENTAL review (the user prompt says
  which)…") and included for the whole repository when that keeps the answer
  schema unchanged. When it would not (the migrations policy goes with
  `findings[].deployed_usage`), the rule stays conditional, names no SHA and
  is the last part of the system prompt (`withLateSystem`, after the models
  note), so the static prefix is the same with and without it.
- **Schema**: the same for every run of a kind in a repository where possible:
  the verifier's `verdicts` is required (possibly empty) whenever the repo has
  the compat check on; the chat `learning` field exists whenever learning is
  on for the repo (a rule is kept only in a bot thread).

Inventory (✓ = identical for every run of the kind in a repository):

| Run (meter kind) | System prompt | Varies with the repo config | Varies per run (all in the user prompt unless noted) |
|---|---|---|---|
| review (`review`) | ✓ (+ migrations policy for MRs with migrations, last) | language, block rule, `/base`, compat rules, diagram rules, team-rules note, path-instructions note, labels note, memory note, skills list, TBD note, MCP note, problems note, helpers | title, description, range and SHAs, full/incremental, files, diff, `.reviewbot.yaml` instructions, open findings, Jira/title, team rules, matched path instructions, labels, context repos, memory, compat/API/linters, migration hints (merge base SHA), coverage, system map, dependency facts. **Schema**: `task_alignment` (MR has a Jira issue), `suggested_title` (title not Conventional Commits), `deployed_usage` (MR has migrations) |
| verification (`verification`) | ✓ | language, block rule, `/base`, context repos, claim rules (compat on), memory note, MCP tools, helpers | SHAs, findings with diff excerpts, claims, API changelog, memory |
| compat-only verification (`verification`) | ✓ | language, `/base` | SHAs, claims, API changelog |
| `@bot check` (`verification`) | ✓ | language, `/base` | SHAs, findings, context repos |
| thread answer (`chat`) | ✓ | language, `/base`, team-rules note, learning rule, path note, memory note, skills, helpers | title, description, SHAs, files, thread, question, coverage, context, rules, memory, path instructions |
| CI explanation (`ci`) | ✓ per MR pipeline / main branch | language, `/base` | pipeline, SHA, title, failed jobs, diff, merged MRs |
| learning, synthesis, memory compaction (`learning`, `memory`) | ✓ (no tools) | language | the texts |
| contract extraction (`map`) | ✓ (no tools) | — | repository, files |

Known variance left (documented, not removed):

- The per-MR schema parts above (task, title, `deployed_usage`): stable across
  re-reviews of one MR, different between MRs. Making them unconditional would
  drop the schema's "required when asked" guarantee.
- `review.mcp` (on by default): the MCP tool list (and the system note listing
  the data tools, the verifier's tool list, `submit_finding`'s
  `deployed_usage`) follows which analyzers produced data for the MR (API
  changes, linter results, coverage, system map, migrations, Jira, related
  repositories); MRs with the same kinds of data share it
  (`TestPromptCacheReviewStableWithMCP`). The MCP fallback re-run
  (mcp_fallback.go) is the MCP-off request: a cache variant of its own.
- A failed merge-base export (no `/base`) or context mount changes the tools
  rule / verifier text for that run.
- Local sandbox mode: the working directory and the staged `/skills` path are
  per-run host directories (Claude Code's environment block and our skills
  list); docker mode uses the fixed `/work`, `/base`, `/skills`.
- Claude Code's own system prompt carries the date and the environment of the
  run; the cache entry lives about 5 minutes (API default), so hits come from
  runs of the same kind in the same repository close in time (a re-review
  after a push, `@bot review`, the next verification).

Tests: `prompt_cache_test.go` (two MRs of one repo: system prompt, schema,
tools byte-identical; no run data in the system prompt; the migrations policy
is the only MR-dependent tail; the same for verification, check, CI, chat).
Visibility: `cache_share` (cache_read / (input + cache_read + cache_write)) per
aggregate in `/usage/data.json` and the run-kind table of `/usage`, per Claude
session on `/runs/<id>`, and the Grafana "Доля кэша во входных токенах" panel
(total and per `kind`, from `reviewbot_claude_tokens_total`).

## Token accounting (internal/meter, internal/usageweb)

- `internal/claude` parses `usage` (`input_tokens`, `output_tokens`,
  `cache_read_input_tokens`, `cache_creation_input_tokens`; main loop only) and
  `modelUsage` (`{model: {inputTokens, outputTokens, cacheReadInputTokens,
  cacheCreationInputTokens, costUSD}}`; subagents included) from the
  `result` object (the last event of `claude -p --output-format stream-json --verbose`) into `core.RunResult.Usage` (sum of
  `modelUsage` when present, else `usage`) and `.Models`. A failed run that
  still reported cost/tokens returns `*core.FailedRun{Err, Partial}`
  (`errors.As` still finds the inner error, e.g. `*core.ErrClaudeRateLimited`).
- `meter.Wrap(runner, store, now, log, meter.Options{Retention})` records
  every `Run` (ok or not, with an error class) in `claude_runs` (one row per
  run) and `claude_run_models` (per-model split; a run without `modelUsage`
  gets one row under the requested model). Recording errors are logged and
  never fail the run. `Exec` passes through; any new `core.Runner` method
  needs a one-line forwarder in `meter.Runner`. Pruning (default 365 days) runs
  at most once a day after a recorded run.
- Attribution travels in the context: `review.Reviewer.Handle` calls
  `meter.With(ctx, meter.ForJob(job))` (manual jobs -> the command user), and
  the review/chat flow calls `meter.Update` right after `GetMR` (MR title;
  auto reviews -> `mr.AuthorUsername`). `meter.Update(ctx, ...)` changes the
  shared attribution in place, so a later run in the same job (e.g.
  `Kind = meter.KindVerification`: the verification run of findings and
  breaking changes, "Перепроверка" on /usage) is attributed accordingly;
  `JobCheck` ("@bot check") is a verification from the start
  (`meter.KindForJob`).
  Kinds: review, chat, verification, learning, memory, map (system map extraction, no user,
  ProjectPath = scanned repo), ci (explanation of a failed pipeline, review.ci), other.
- Tables are created idempotently (`CREATE ... IF NOT EXISTS`) on first use,
  outside the numbered `migrations` list, so they never compete for a schema
  version with other features.
- `usageweb.Register(mux, Options{Store, Enabled, Token, GitLabURL, Logger})`:
  `usage.enabled` (default true) -> 404 when false. `usage.token` / env
  `USAGE_TOKEN`: when set, `Authorization: Bearer` or `?token=` (sets an
  HttpOnly SameSite=Strict cookie holding sha256(token), then redirects without
  the token) is required, compared in constant time; when empty, the page is
  open and a startup warning says how to protect it. The page is one embedded
  HTML file with inline CSS/JS under a nonce CSP (the shared `webui.Assets()`
  plus its chart-only rules) and loads everything from `data.json`; untrusted strings (MR titles) are inserted as text only.
  Charts use the embedded Apache ECharts (`webassets.EChartsPath`), so the CSP
  is `script-src 'nonce-…' 'self'`; `style-src` stays nonce-only (ECharts
  styles via CSSOM, which CSP does not restrict, and the tooltip formatters
  emit class-only markup with every data string escaped). Each chart keeps a
  text summary and a table; if the bundle fails to load the page shows only
  those. With a user filter (and no project filter) `data.json` also carries
  `by_day_project` (top 5 repositories per day) for the per-repo stacks.

## Run diagnostics (internal/diag, internal/diagweb)

Goal: keep enough to diagnose the runs of the last 48 hours.

- **Recorder in the context.** `diag.Recorder.Handler(reviewer.Handle)` wraps
  the queue handler (main.go): every job gets a `*diag.Run` in its context
  (`diag.FromContext`), finished with the handler's result: nil -> `ok`
  (`skipped` when the job logged a skip reason, e.g. "draft mr, skipping", and
  ran no Claude session), `*core.RetryLater` -> `postponed` (+ retry time and
  reason), `context.Canceled` -> `interrupted`, else `failed`; panics are
  recorded and re-raised. Repeated attempts of the same job count up.
  A nil `*Recorder` (`diag.enabled: false`) makes every hook a no-op.
- **Decorators, no edits in internal/review.** `WrapRunner` (Exec; Run ->
  full Claude record),
  `WrapGitLab` (reads of MR/diffs/discussions/file/commits, every write; note
  bodies are never stored, only ids/size/marker kind), `WrapModerator`,
  `WrapLimiter` (allowed/denied + reason), `WrapRepos` (checkout, `git diff`
  size and `max_diff_bytes` cap, changed files, ancestry), `WrapLabels` (MR and
  project labels of the bot and the service token; the bot's 403 is a
  warning). The wrappers embed the interface, so methods added later are
  forwarded unrecorded instead of breaking the build. Every decorator (diag,
  telemetry, meter, the queue's markers) is applied in
  `cmd/reviewbot/instrument.go` (`instrumentRunner`, `instrument`,
  `instrumentHandler`), whose comment documents the nesting order. `diag.Recorder.LogHandler` wraps the service's slog
  handler: records with an `mr` attribute of a job being recorded (or logged
  with its context) become timeline entries, debug included. `diag.Step` /
  `diag.Note` are available for explicit steps.
- **Runner observation.** `claude.WithObserver(ctx, o)` gets every
  stream-json event line of a Claude run and a `ProcessInfo` per child
  process (sanitized argv: `--append-system-prompt`/`--json-schema` values
  replaced by `<… N bytes>`; exit code; duration; 8 KB stdout/stderr tails).
- **Claude CLI output.** `--output-format stream-json --verbose` (with `-p`
  stream-json requires `--verbose`; verified on Claude Code 2.1.310).
  `internal/claude/stream.go` splits stdout into events, forwards them to the
  observer and hands `parseOutput` the last `result` event, which carries the
  same fields as the former single json object (result, structured_output,
  usage, modelUsage, cost, turns), so parsing, usage and `FailedRun` are
  unchanged. Without a result event (crash) parseOutput sees the non-event
  output plus synthesized lines for assistant API errors and a rejected
  `rate_limit_event` (`status: rejected`, `resetsAt`), so rate limits are still
  detected (and a generic failure after a rejected rate-limit event becomes
  `*core.ErrClaudeRateLimited`). Event lines over 32 MB are dropped with a
  marker. Rate limits come only from CLI-level signals: an error result
  (`is_error` or a non-`success` subtype), a rejected `rate_limit_event`,
  stderr, and the CLI's own plain-text lines when there is no result (lines
  that look like JSON, i.e. truncated events, are skipped). The text of a
  successful result is the model's answer and is never searched: a prompt
  injection could otherwise fake "usage limit reached|<epoch>" and postpone
  the review at will; a successful result without the requested JSON is an
  invalid output.
- **Scrubbing.** Every stored string (and every string inside transcript
  JSON, keys included; lines are re-encoded so JSON stays valid) is scrubbed:
  literal values of the service secrets and Claude/page tokens from config and
  env, the credential patterns of `learn.Scrub`, `key=value` secrets, URLs with
  credentials (`scheme://[скрыто]@host`), and long mixed-case+digit opaque
  strings; commit SHAs (lower-case hex, and SHAs seen in the job) are kept.
- **Storage.** `<data_dir>/diag/<YYYYMMDD>/<id>/run.json` (job, steps,
  Claude runs; rewritten at most every 2 s and at the end) and
  `claude-<n>.jsonl`; index `diag_runs` (named migration `diag_runs_v1`,
  registered from `store/diagruns.go`; rows `diagindex.Row`, aliased as
  `diag.IndexRow`). Caps: `diag.max_run_mb` (transcript
  events beyond the budget are replaced by one `reviewbot_truncated` marker,
  the `result` event is always kept; long run.json strings are cut with a
  marker), `diag.max_total_mb` (oldest evicted after each run and by the
  cleanup). Cleanup every 10 min removes runs older than `diag.retention`
  (48h) and orphan dirs; rows left `running` by a previous process become
  `interrupted` at startup.
- **Web** (`diagweb.Register`): `/runs` (list; `data.json` with
  status/kind/project/mr/since filters), `/runs/<id>` (header, timeline,
  collapsible Claude runs with prompts, schema, tools, argv, stderr and the
  transcript rendered as a chat), `/runs/<id>/claude-<n>.jsonl` (download,
  `Content-Disposition: attachment`, sandbox CSP). Access: `usageweb.Guard`
  (same as /usage), token `DIAG_TOKEN` / `diag.token`, falling back to the
  usage token; open + startup warning without one; `diag.enabled: false` ->
  404. Pages are embedded HTML with inline CSS/JS under a nonce CSP plus
  `'self'` in script-src for the ECharts bundle (`webassets.EChartsPath`) and
  the filter bar (`webassets.FiltersPath`, filters enhance the native controls);
  data is inserted as text only (no innerHTML; tooltips are DOM nodes). The
  hourly chart has a table alternative. Themes follow /usage (same storage
  key); charts re-init on theme change.
- **CLI.** `reviewbot runs [-config] [-data-dir] [-since] [-status] [-kind]
  [-project] [-mr] [-json]`, `reviewbot runs show <id> [-json]`.

## Tracing (internal/tracing, internal/diag/trace.go)

User docs: [features/tracing.md](features/tracing.md). Goal: one OpenTelemetry
trace per queue job, exported over OTLP/HTTP, with no new dependencies.

- **Own SDK.** The official Go SDK (`go.opentelemetry.io/otel`, grpc,
  protobuf) cannot be fetched in this build environment, so
  `internal/tracing` implements the small part we need on the stdlib:
  `Tracer.Start` (root or child of the span / remote parent in ctx),
  package-level `tracing.Start` (child only: no span in ctx -> no-op, so code
  outside a traced job pays one ctx lookup), `Span.SetAttr/SetAttrIfAbsent/
  AddEvent/RecordError/SetStatus/End`, kinds, W3C `traceparent`
  parse/format, ids from crypto/rand. A nil `*Tracer`/`*Span` is a no-op;
  tracing off = nil tracer everywhere. Sampling: TraceIdRatioBased on the
  root (low 8 bytes of the trace id), children follow the parent; an
  unsampled span keeps ids (for propagation) but records nothing.
  `Tracer.Handler` (the `/webhook` server span) never adopts an inbound
  `traceparent` as parent: the request is unauthenticated when the span
  starts, so a remote sampled flag or chosen trace id would bypass
  `sample_ratio`. The span is a new root and links the remote context.
- **Wire format.** `otlp.go` encodes ExportTraceServiceRequest with the
  OTLP/JSON mapping (lowerCamelCase, hex ids, int64 as decimal strings, enum
  ints, `flags` sampled bit, status only when set); golden test
  `testdata/otlp_traces.golden.json` plus a shape test. Verified live
  against Tempo 3.0.5 and OpenTelemetry Collector 0.163.2
  (`TestLiveTempo`, skipped without `REVIEWBOT_OTLP_ENDPOINT`).
- **Exporter.** `BatchExporter`: bounded channel (2048; `Enqueue` never
  blocks, overflow is dropped and counted), one goroutine, batches of 512 or
  every 5 s, gzip POST `{endpoint}/v1/traces`, headers, CA file, retries with
  backoff + `Retry-After` on 429/502/503/504 and network errors only,
  `partialSuccess.rejectedSpans`, warnings rate-limited to 1/min (URL without
  userinfo/query; headers never logged). `Shutdown(ctx)` drains and stops;
  at its deadline pending retries are abandoned. Results feed
  `reviewbot_tracing_spans_total{result}` / `..._export_errors_total{reason}`.
- **Bridge from diag (one place).** Every step the diag decorators record
  is started by `startStep(ctx, spanName, kind, name, fields)`: a timeline
  step of the Run in ctx (if any) and a child span (if the job is traced);
  it returns ctx with the span, which the decorators pass to the wrapped
  call, so the GitLab client's HTTP spans nest under `gitlab.get_mr` etc.
  `pending.end/done` end both. Span attributes come from the step fields
  through an allowlist (`stepAttrs`: numbers, bools, a few id-like keys);
  paths, argv, file lists, names and details never reach a span.
  `diag.Hooks{Rec, Trace}` installs the decorators when either sink is on
  (`Recorder.Wrap*` = `Hooks{Rec: rec}.Wrap*`), so tracing works with
  `diag.enabled: false`. Log lines and `diag.Note` stay timeline-only.
- **Job span.** `diag.TraceJobs(tr, active, h)` is the outermost queue
  handler wrapper: a new root `review.job` (consumer) per job, linked to the
  event span from `core.Job.TraceParent` (set by the webhook handler from the
  request span and by the pollers from their round span; not persisted, not
  in the dedup key). `diag.Recorder.Start` reads the span from ctx: the run
  record stores `trace_id`, `Run.Finish` sets `reviewbot.job.result/reason`
  (same classification as /runs); without diagnostics TraceJobs derives them
  from the error. The job span is registered in `tracing.Active` under the
  MR key for log correlation.
- **Explicit spans.** `internal/gitlab/trace.go`: a client span per HTTP
  attempt with a templated path (`URLTemplate`: numeric ids, project paths,
  SHAs, repository file paths replaced) and no query/headers.
  `internal/review`: one span per analyzer/publisher (`review.compat`,
  `review.lint`, …, `review.publish.drafts`, `review.summary`).
  `internal/mcpserver`: a server span per `tools/call` under the session's
  ctx (the job span). Claude runs: `startClaudeSpan`/`endClaudeSpan` in the
  diag Runner decorator (GenAI attributes; the span ctx goes to the runner).
- **Privacy.** Besides the allowlist, every string is scrubbed with
  `diag.ScrubFunc(diagSecrets)` (tracing header values included), stripped
  of URL queries/fragments and capped at 256 bytes inside the tracer.
- **Logs.** `tracing.LogHandler` (installed under the diag log handler)
  adds `trace_id`/`span_id` from the span in ctx or, since most logging has
  no ctx, from `tracing.Active` by the record's `mr` attribute.
- **Claude CLI (experimental).** `claude.Options.Telemetry` adds Claude
  Code's documented env (`CLAUDE_CODE_ENABLE_TELEMETRY`,
  `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA`, `OTEL_TRACES_EXPORTER=otlp`,
  http/json, traces endpoint, metrics/logs off) and `TRACEPARENT` of the
  Claude-run span to review runs only; headers only with
  `tracing.claude_cli_headers`, passed by name. The endpoint variable is a
  literal `docker run -e` argument, so `user:password@` of the collector URL
  is stripped from it; with `claude_cli_headers` it becomes an
  `Authorization: Basic` header (unless `headers` set Authorization). Header
  values are percent-encoded with `%20` for spaces (the JS SDK decodes with
  `decodeURIComponent`, which keeps `+`). An https:443 collector is
  added to the egress allowlist by `config.LoadSandbox`; any other is put in
  NO_PROXY and must be on the sandbox network. `Options.HideEnv` removes the
  variables referenced by `tracing.headers` from every child environment
  (`repo.Options.HideEnv` from host git);
  `OTEL_EXPORTER_OTLP_HEADERS`/`_TRACES_HEADERS` are service secrets.
- **Wiring** (`cmd/reviewbot/tracing.go`): `startTracing` (nil tracer when
  off or when the exporter cannot start), `tracedLogger`, `traceWebhook`,
  `cliTelemetry`; spans are flushed after the queue stops on SIGTERM
  (5 s budget). `/runs/<id>` links `trace_id` through `tracing.ui_url`
  (`diagweb.TraceLink`, http(s) only).

## Dev environment notes

In this build sandbox only github.com is reachable. Use:

```
export GOPROXY=direct GOSUMDB=off GOFLAGS=-mod=mod NO_PROXY=localhost,127.0.0.1 no_proxy=localhost,127.0.0.1
```

Do not run `go mod tidy` and do not add new modules.

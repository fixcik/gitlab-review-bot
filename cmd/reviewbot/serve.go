package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/diag"
	"github.com/fixcik/gitlab-review-bot/internal/diffmap"
	"github.com/fixcik/gitlab-review-bot/internal/events"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/limits"
	"github.com/fixcik/gitlab-review-bot/internal/poller"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
	"github.com/fixcik/gitlab-review-bot/internal/review"
	"github.com/fixcik/gitlab-review-bot/internal/review/descsum"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/labels"
	"github.com/fixcik/gitlab-review-bot/internal/store"
	"github.com/fixcik/gitlab-review-bot/internal/sysmap"
	"github.com/fixcik/gitlab-review-bot/internal/telemetry"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
	"github.com/fixcik/gitlab-review-bot/internal/usageweb"
	"github.com/fixcik/gitlab-review-bot/internal/webassets"
	"github.com/fixcik/gitlab-review-bot/internal/webhook"
)

// errNeedsInit: serve started without the tokens; main prints it and exits
// with 2 (not a crash: the installation is not set up yet).
var errNeedsInit = errors.New("нет токенов GitLab или Claude: выполните `reviewbot init` (или задайте GITLAB_TOKEN и CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_API_KEY в .env)")

// loadServeConfig loads and validates the config; with no GitLab token or
// no Claude credential at all it returns errNeedsInit instead of the
// validation error.
func loadServeConfig(path string, getenv func(string) string) (*config.Service, error) {
	cfg, err := config.LoadService(path)
	noClaude := getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" && getenv("ANTHROPIC_API_KEY") == ""
	if c, rerr := config.LoadUnvalidated(path); rerr == nil && (c.GitLab.Token == "" || noClaude) {
		return nil, errNeedsInit
	}
	return cfg, err
}

func serve(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "path to config.yaml")
	fs.Parse(args)

	cfg, err := loadServeConfig(*cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return err
	}

	// Observability: Prometheus metrics (metrics.go; nil when disabled) and
	// OpenTelemetry traces (tracing.go; nil tracer when off): one trace per
	// queue job; log lines of a traced job carry trace_id/span_id.
	tm := newTelemetry(cfg)
	tr, stopTracing := startTracing(cfg, tm, logger)
	defer stopTracing()
	activeJobs := &tracing.Active{}
	logger = tracedLogger(tr, logger, activeJobs)

	// GitLab: the bot's own client, and the optional service client.
	gl, bot, err := connectBot(ctx, cfg, tm, logger)
	if err != nil {
		return err
	}
	// Service token (Maintainer): hook sync, reviewer assignment, resolving
	// threads. Optional: without it the bot works as a plain Reporter.
	svc, err := newOptionalServiceClient(cfg, tm, logger) // nil without a token
	if err != nil {
		return err
	}
	var moderator env.Moderator // nil interfaces without a service token
	var svcLabels labels.Client
	var svcDesc descsum.Client
	if svc != nil {
		moderator, svcLabels, svcDesc = svc, svc, svc
	}
	if err := startGroupFeatures(ctx, cfg, svc, logger); err != nil {
		return err
	}

	// Storage, diagnostics of recent runs (/runs: the job's log lines go to
	// its record), limits and what the bot could not check (review.problems;
	// nil when off).
	st, err := store.OpenSQLite(filepath.Join(dataDir, "reviewbot.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	diagRec := startDiag(ctx, cfg, dataDir, st, logger)
	logger = diagRec.Logger(logger)
	lim := limits.New(core.Store(st), cfg.Limits.GlobalRunsPerHour, cfg.Limits.ExemptUsers, time.Now)
	probs := newProblems(cfg, st, tm, logger)

	// Checkouts and the Claude runner.
	repos, err := newRepos(cfg, dataDir, logger)
	if err != nil {
		return err
	}
	defer repos.Close()
	// Timeline steps (diagnostics) and spans (tracing) come from the same
	// decorators, metrics from telemetry ones; any sink may be off. Every
	// wrapper is applied in instrument.go (one documented order).
	in := instruments{hooks: diag.Hooks{Rec: diagRec, Trace: tr.Enabled()}, tm: tm}
	runner, err := newRunner(cfg, st, in, logger)
	if err != nil {
		return err
	}
	tasks, err := newTaskSource(cfg, logger)
	if err != nil {
		return err
	}

	// Optional features (each nil when off).
	mcpSrv, stopMCP := startMCP(cfg, tm, logger) // review.mcp, on its own listener
	defer stopMCP()
	ctxRepos := startContextRepos(ctx, cfg, gl, repos, logger)
	sysMap := startSysmap(ctx, cfg, ctxRepos, st, runner, dataDir, tm, probs, logger) // review.sysmap
	// Award emoji on notes addressed to the bot and on MRs (review.reactions).
	react := newReactions(cfg, gl, bot.id, bot.name, tm, probs, diagRec, logger)

	// The reviewer and its queue.
	reviewer := review.New(instrument(in, cfg, review.Deps{
		GitLab:    gl,
		Moderator: moderator,
		Repos:     repos,
		Runner:    runner,
		Store:     st,
		Limiter:   lim,
		Config:    cfg,
		Tasks:     tasks,
		// Team rules learned from thread replies (review.learn).
		Learnings:  st,
		ScratchDir: filepath.Join(dataDir, "scratch"),
		// Per-project operational memory (review.memory).
		Memory: st,
		// Optional record stores: finding outcomes (review.learn.from_actions),
		// MR flags (pause), main branch CI state (review.ci), bot labels.
		Outcomes:   st,
		MRFlags:    st,
		CIMain:     st,
		LabelStore: st,
		BuildIndex: func(d []core.FileDiff) env.DiffIndex {
			return diffmap.Build(d)
		},
		BotUserID:   bot.id,
		BotUsername: bot.name,
		Logger:      logger,
		Now:         time.Now,

		ContextRepos: ctxRepos,
		SystemMap:    systemMapDep(sysMap),
		MCP:          mcpSrv,
		Reactions:    react,
		// Automatic MR labels (review.labels): bot token first, service
		// token on 403.
		Labels:        gl,
		ServiceLabels: svcLabels,
		// The summary in MR descriptions (review.description_summary): bot
		// token first, service token on 403.
		Description:        gl,
		ServiceDescription: svcDesc,
		OnLabel:            tm.ObserveLabel,
		// reviewbot_skipped_total (review.max_mr_lines, review/bigmr.go;
		// review.require_repo_config and invalid configs, review/repoconfig.go).
		OnSkip: tm.ObserveSkip,
		// reviewbot_gitlab_reads_saved_total (per-job memo, review/glmemo.go).
		OnGitLabSaved: tm.ObserveGitLabSaved,
		// reviewbot_review_route_total (review.routing, review/route.go).
		OnRoute: tm.ObserveRoute,
		// CI failure digest (review.ci): read with the bot's token only
		// (never the service token: the digest is published in the MR);
		// logs are scrubbed of the configured secrets.
		Scrub: diag.ScrubFunc(diagSecrets(cfg)),
	}))
	q := queue.New(queue.Options{
		Concurrency:   cfg.Limits.MaxConcurrent,
		Debounce:      cfg.Review.Debounce.D(),
		Logger:        logger,
		Persist:       st, // every queued or running job survives a restart
		ShutdownGrace: cfg.Queue.ShutdownGrace.D(),
		ManualMaxAge:  cfg.Queue.ManualMaxAge.D(),
		OnRestore:     tm.ObserveRestore,
		// A command dropped on restore (answered, expired) loses 👀 / ⏳.
		OnDrop: reviewer.Dropped,
	})
	tm.ObserveQueue(q)
	reviewer.SetEnqueue(q.Enqueue) // "@bot resume" enqueues a review of unreviewed commits

	// Event sources (gitlab.events) and the HTTP endpoints. They outlive the
	// signal (evCtx) and keep accepting events during the queue drain: the
	// jobs are persisted for the next start. The restored jobs seed the
	// event dedup, so polling does not enqueue them again.
	warnEventsWebhook(cfg, logger)
	restored := q.Restore()
	evCtx, stopEvents := context.WithCancel(context.WithoutCancel(ctx))
	defer stopEvents()
	mux := http.NewServeMux()
	// reviewer.Accepted puts 👀 on a command note the moment it is enqueued.
	pollDone := startEventSources(evCtx, mux, cfg, gl, st, bot, reviewer.Accepted(q.Enqueue), restored, tr, tm, logger)
	registerWeb(mux, cfg, gl, st, diagRec, sysMap, probs, tm, logger)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	srvErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.Listen, "sandbox", cfg.Sandbox.Mode)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
		close(srvErr)
	}()

	// Run until a signal or a listener failure, then drain
	// (queue.shutdown_grace): nothing new starts, running jobs (restored
	// ones included) may finish (a second signal cancels them); /webhook and
	// the pollers keep accepting events meanwhile. Then the event sources
	// stop, and only then the store closes (deferred). The handler chain
	// (job span, diagnostics run, metrics, problems): instrument.go.
	err = runQueue(ctx, q, instrumentHandler(in, tr, activeJobs, diagRec, probs, reviewer.Handle), srvErr,
		cfg.Queue.ShutdownGrace.D(), logger)
	stopEvents()
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shCtx)
	<-pollDone    // the poller writes to the store, which closes on return
	stopTracing() // flush the spans of the last jobs (deferred call is then a no-op)
	return err
}

// startEventSources wires gitlab.events: webhook, polling, or both. Both
// share the same classification rules. Duplicates are dropped in every
// mode: with both, the two sources see the same events; with polling alone,
// a failed save of the poll record makes the next poll enqueue them again.
// The returned channel is closed when the pollers have stopped (at once
// when nothing polls). restored are the jobs the queue reloaded at startup:
// their events count as seen.
func startEventSources(ctx context.Context, mux *http.ServeMux, cfg *config.Service, gl *gitlab.Client, st *store.SQLite,
	bot botUser, accept func(core.Job), restored []core.Job, tr *tracing.Tracer, tm *telemetry.Metrics, logger *slog.Logger) <-chan struct{} {
	dedup := poller.NewDedup(24*time.Hour, nil)
	for _, j := range restored { // already queued (restored after a restart)
		dedup.First(j)
	}
	enqueue := dedup.Wrap(accept)
	// One router for every event source: the webhook and the pollers
	// classify events with the same rules (internal/events).
	router := events.NewRouter(events.Options{
		BotUsername: bot.name,
		Mention:     bot.mention,
		Groups:      cfg.GitLab.Groups,
		// Replies in bot threads feed review.learn.
		LearnReplies: cfg.Review.Learn.Enabled,
		Excluded:     cfg.GitLab.Excluded, // gitlab.exclude
		// Pure thanks / acknowledgements get ❤️ / 👍 instead of a Claude run.
		ReactThanks: cfg.Review.Reactions.Emoji("thanks") != "",
		ReactAck:    cfg.Review.Reactions.Emoji("ack") != "",
		// Finished pipelines feed the CI failure digest (review.ci).
		Pipelines:  cfg.Review.CI.Enabled,
		MainBranch: cfg.GitLab.MainBranchFor,
	})
	if cfg.GitLab.UsesWebhook() {
		h := webhook.NewHandler(webhook.Options{Secret: cfg.GitLab.WebhookSecret, Router: router, Enqueue: enqueue, Logger: logger})
		mux.Handle("/webhook", traceWebhook(tr, tm.WrapWebhook(h)))
	}
	done := make(chan struct{})
	if cfg.GitLab.UsesPolling() {
		tm.SetPollInterval(cfg.GitLab.PollInterval.D())
		p := poller.New(poller.Options{
			Source:   gl,
			State:    st,
			Router:   router,
			Enqueue:  enqueue,
			Groups:   cfg.GitLab.Groups,
			Interval: cfg.GitLab.PollInterval.D(),
			Logger:   logger,
			Observe:  tm.ObservePoll,
			Tracer:   tr, // each round is a short trace; its jobs link to it
		})
		pp := startPipelinePoller(ctx, cfg, gl, st, router, enqueue, tr, logger) // review.ci
		go func() {
			defer close(done)
			p.Run(ctx)
			<-pp
		}()
	} else {
		close(done)
	}
	logger.Info("event source", "events", cfg.GitLab.Events, "poll_interval", cfg.GitLab.PollInterval.D())
	return done
}

// knownClaudeModel: a model name of the Claude families (aliases and full
// ids); /usage names failed runs on any other model as a config mistake.
func knownClaudeModel(m string) bool { return config.ModelRank(m) > 0 }

// registerWeb mounts the HTTP endpoints next to /webhook.
func registerWeb(mux *http.ServeMux, cfg *config.Service, gl *gitlab.Client, st *store.SQLite, diagRec *diag.Recorder,
	sysMap *sysmap.Map, probs *problems.Service, tm *telemetry.Metrics, logger *slog.Logger) {
	nav := webNav(cfg, st, diagRec != nil, sysMap != nil, probs != nil, logger) // the shared header of every page
	usageweb.Register(mux, usageweb.Options{
		Store: st, Enabled: cfg.Usage.On(), Token: cfg.Usage.Token, GitLabURL: cfg.GitLab.URL, Logger: logger,
		BudgetUSD: cfg.Usage.BudgetUSD, KnownModel: knownClaudeModel, Nav: nav,
	})
	webassets.Register(mux)
	registerDiagWeb(mux, cfg, diagRec, st, nav, logger)                              // /runs
	registerMapWeb(mux, cfg, sysMap, nav, logger)                                    // /map
	registerProblemsWeb(mux, cfg, st, probs, nav, logger)                            // /problems
	registerKnowledgeWeb(mux, cfg, st, nav, logger)                                  // /knowledge
	registerHomeWeb(mux, cfg, st, diagRec != nil, sysMap, probs != nil, nav, logger) // /
	registerSetupWeb(mux, cfg, nav, logger)                                          // /setup
	registerTelemetry(mux, tm, cfg, gl, st, logger)                                  // /metrics, /readyz
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
}

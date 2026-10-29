// Package telemetry defines the bot's Prometheus metrics (reviewbot_*) on
// top of internal/metrics and the decorators that feed them without editing
// the packages they observe: the queue handler, core.Runner, core.Limiter,
// the compat/lint checkers, the webhook handler and the GitLab clients'
// request observer. It also serves GET /metrics and GET /readyz.
//
// Labels never carry users, projects or MRs (cardinality): the per-user
// breakdown lives on /usage, the per-job drill-down on /runs.
package telemetry

import (
	"runtime"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/meter"
	"github.com/fixcik/gitlab-review-bot/internal/metrics"
	"github.com/fixcik/gitlab-review-bot/internal/queue"
)

// Histogram buckets, seconds.
var (
	JobDurationBuckets    = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1200}
	ClaudeDurationBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 900, 1800}
)

// Label values.
const (
	// claude_runs_total result.
	ClaudeOK          = "ok"
	ClaudeError       = "error"
	ClaudeRateLimited = "rate_limited"
	ClaudeTimeout     = "timeout"

	// tool_runs_total result.
	ToolOK          = "ok"
	ToolFindings    = "findings"
	ToolUnavailable = "unavailable"
	ToolFailed      = "failed"
	ToolPartial     = "partial"

	// limit_denials_total scope.
	ScopeGlobal     = "global"
	ScopeUserRerun  = "user_rerun"
	ScopeUserChat   = "user_chat"
	ScopeMRCooldown = "mr_cooldown"
	ScopeLearn      = "learn"

	// webhook_events_total.
	EventMergeRequest = "merge_request"
	EventNote         = "note"
	EventPipeline     = "pipeline"
	EventOther        = "other"

	WebhookAccepted         = "accepted"
	WebhookIgnored          = "ignored"
	WebhookUnauthorized     = "unauthorized"
	WebhookBadRequest       = "bad_request"
	WebhookMethodNotAllowed = "method_not_allowed"

	// DefaultModel labels runs whose model is unknown.
	DefaultModel = "default"
)

// Metrics holds every reviewbot_* family. A nil *Metrics disables
// everything: decorators return their argument unchanged.
type Metrics struct {
	Registry *metrics.Registry

	JobsTotal        *metrics.CounterVec   // kind, status
	JobDuration      *metrics.HistogramVec // kind
	QueueJobs        *metrics.GaugeVec     // state
	QueueRestored    *metrics.CounterVec   // kind, result
	QueueDraining    *metrics.Gauge
	ClaudeRuns       *metrics.CounterVec   // kind, model, result
	ClaudeTokens     *metrics.CounterVec   // kind, model, type
	ClaudeCost       *metrics.CounterVec   // kind, model
	ClaudeDuration   *metrics.HistogramVec // kind
	ClaudeRateLimit  *metrics.Counter
	LimitDenials     *metrics.CounterVec // scope
	ToolRuns         *metrics.CounterVec // tool, result
	WebhookEvents    *metrics.CounterVec // event, result
	PollTicks        *metrics.CounterVec // result
	PollJobs         *metrics.Counter
	PollLastSuccess  *metrics.Gauge
	PollLastRound    *metrics.Gauge
	PollInterval     *metrics.Gauge
	GitLabRequests   *metrics.CounterVec // method, code
	BuildInfo        *metrics.GaugeVec   // version, go_version
	ReadinessCheckOK *metrics.GaugeVec   // check
	SysmapScans      *metrics.CounterVec // status
	SysmapRepos      *metrics.GaugeVec   // status
	SysmapEntries    *metrics.GaugeVec   // side
	SysmapUnresolved *metrics.Gauge
	MCPToolCalls     *metrics.CounterVec // tool, status
	MCPFallbacks     *metrics.CounterVec // reason
	MCPDegraded      *metrics.Gauge
	ProblemsTotal    *metrics.CounterVec // kind, source
	ProblemsNew      *metrics.CounterVec // kind
	ReactionsTotal   *metrics.CounterVec // event, result
	LabelsTotal      *metrics.CounterVec // action, result
	VerifyRuns       *metrics.CounterVec // result
	VerifyVerdicts   *metrics.CounterVec // verdict, severity
	Skipped          *metrics.CounterVec // reason
	GitLabSaved      *metrics.CounterVec // op
	ReviewRoutes     *metrics.CounterVec // route
	CIFailures       *metrics.CounterVec // scope, cause
	CIExplainRuns    *metrics.CounterVec // result
	TracingSpans     *metrics.CounterVec // result
	TracingErrors    *metrics.CounterVec // reason

	goroutines, memAlloc, memSys, startTime *metrics.Gauge
}

// New registers every family. version is the build version ("" = "dev").
func New(version string) *Metrics {
	if version == "" {
		version = "dev"
	}
	r := metrics.NewRegistry()
	m := &Metrics{Registry: r}
	m.JobsTotal = r.NewCounterVec("reviewbot_jobs_total",
		"Queue jobs handled, by job kind and outcome (ok, skipped, postponed, failed, interrupted; same classification as /runs).",
		"kind", "status")
	m.JobDuration = r.NewHistogramVec("reviewbot_job_duration_seconds",
		"Wall time of one queue job handler call.", JobDurationBuckets, "kind")
	m.QueueJobs = r.NewGaugeVec("reviewbot_queue_jobs",
		"Jobs in the queue by state: pending (ready to run, waiting only for a worker or for their MR's running job; also auto reviews in their review.debounce), running, retry_wait (rescheduled or postponed until a later time; they hold no other job back).", "state")
	m.QueueRestored = r.NewCounterVec("reviewbot_queue_restored_total",
		"Persisted queue jobs found at startup, by job kind and result: queued (had not started, or waited for a retry; queued again), replayed (interrupted by the shutdown before answering; runs again), expired (older than queue.manual_max_age; dropped), answered (interrupted after it had replied in its thread; dropped), invalid (unreadable row; dropped).",
		"kind", "result")
	m.QueueDraining = r.NewGauge("reviewbot_queue_draining",
		"1 while the queue drains on shutdown (SIGTERM/SIGINT): nothing new starts, running jobs get queue.shutdown_grace to finish.")
	m.ClaudeRuns = r.NewCounterVec("reviewbot_claude_runs_total",
		"Claude CLI runs by run kind, model and result (ok, error, rate_limited, timeout).", "kind", "model", "result")
	m.ClaudeTokens = r.NewCounterVec("reviewbot_claude_tokens_total",
		"Tokens reported by the Claude CLI by run kind, model and type (input, output, cache_read, cache_write).",
		"kind", "model", "type")
	m.ClaudeCost = r.NewCounterVec("reviewbot_claude_cost_usd_total",
		"Notional cost of Claude runs in USD at API prices (client-side estimate of the CLI).", "kind", "model")
	m.ClaudeDuration = r.NewHistogramVec("reviewbot_claude_run_duration_seconds",
		"Wall time of one Claude run including the sandbox container start.", ClaudeDurationBuckets, "kind")
	m.ClaudeRateLimit = r.NewCounter("reviewbot_claude_rate_limited_total",
		"Claude runs that hit the subscription/API usage limit.")
	m.LimitDenials = r.NewCounterVec("reviewbot_limit_denials_total",
		"Requests denied by the bot's own rate limits, by scope (global, user_rerun, user_chat, mr_cooldown, learn).", "scope")
	m.ToolRuns = r.NewCounterVec("reviewbot_tool_runs_total",
		"Deterministic checker runs (compat and lint tools) by tool and result (ok, findings, unavailable, failed, partial).",
		"tool", "result")
	m.WebhookEvents = r.NewCounterVec("reviewbot_webhook_events_total",
		"GitLab webhook deliveries by event and result.", "event", "result")
	m.PollTicks = r.NewCounterVec("reviewbot_poll_ticks_total",
		"Polling rounds over the GitLab API (gitlab.events poll/both) by result (ok, error).", "result")
	m.PollJobs = r.NewCounter("reviewbot_poll_jobs_total",
		"Jobs found by polling (before duplicates from the webhook are dropped).")
	m.PollLastSuccess = r.NewGauge("reviewbot_poll_last_success_timestamp_seconds",
		"Unix time of the last polling round without errors (0 = none yet).")
	m.PollLastRound = r.NewGauge("reviewbot_poll_last_round_timestamp_seconds",
		"Unix time of the last polling round, with or without errors (0 = none yet).")
	m.PollInterval = r.NewGauge("reviewbot_poll_interval_seconds",
		"Configured gitlab.poll_interval in seconds (0 = polling off); the polling alerts scale their windows with it.")
	m.GitLabRequests = r.NewCounterVec("reviewbot_gitlab_requests_total",
		"HTTP requests to the GitLab API (every attempt, retries included) by method and status code (2xx/3xx/4xx class, exact 403/404/429/5xx, error = no response).",
		"method", "code")
	m.BuildInfo = r.NewGaugeVec("reviewbot_build_info", "Build information; the value is always 1.", "version", "go_version")
	m.BuildInfo.With(version, runtime.Version()).Set(1)
	m.ReadinessCheckOK = r.NewGaugeVec("reviewbot_readiness_check_ok",
		"Result of each readiness check at scrape time (1 ok, 0 failing); same checks and cache as /readyz.", "check")

	m.SysmapScans = r.NewCounterVec("reviewbot_sysmap_scans_total",
		"System map repository updates by resulting status (ok, partial = waits for the run budget, error).", "status")
	m.SysmapRepos = r.NewGaugeVec("reviewbot_sysmap_repos",
		"Repositories in the system map by scan status (ok, partial, error), sampled at scrape time.", "status")
	m.SysmapEntries = r.NewGaugeVec("reviewbot_sysmap_entries",
		"Contract entries in the system map by side (provides, consumes), sampled at scrape time.", "side")
	m.SysmapUnresolved = r.NewGauge("reviewbot_sysmap_unresolved",
		"Consumer entries of the system map not matched to exactly one provider, sampled at scrape time.")

	m.MCPToolCalls = r.NewCounterVec("reviewbot_mcp_tool_calls_total",
		"Tool calls of the review model to the bot's MCP server (review.mcp) by tool and status (ok, error = the tool answered with an error, limit = review.mcp.max_calls reached).",
		"tool", "status")
	m.MCPFallbacks = r.NewCounterVec("reviewbot_mcp_fallbacks_total",
		"Review runs whose CLI did not use the bot's MCP server and that ran once more without MCP, by reason (not_connected = the CLI reported the server as not connected, no_request = no status and no request reached the server, config_dropped = the MCP config could not be written).",
		"reason")
	m.MCPDegraded = r.NewGauge("reviewbot_mcp_degraded",
		"1 while new review runs skip MCP after a fallback (circuit breaker, 30 minutes after the last fallback), else 0; sampled at scrape time.")

	m.ProblemsTotal = r.NewCounterVec("reviewbot_problems_total",
		"Verification problems (what the bot could not check: linter did not run, no coverage report, related repository unavailable, cut diff, unparsable answer, model reports, ...) by kind and source (bot, model); once per job and fingerprint.",
		"kind", "source")
	m.ProblemsNew = r.NewCounterVec("reviewbot_problems_new_total",
		"Verification problems seen for the first time, or again after an acknowledgement on /problems, by kind (alert on increase).",
		"kind")
	m.LabelsTotal = r.NewCounterVec("reviewbot_labels_total",
		"Automatic MR label changes (review.labels) by action (add, remove) and result (ok, forbidden = no right to change MR labels, missing = the label does not exist and create_missing is off, ignored = GitLab answered but did not apply it, error = other API error).",
		"action", "result")

	m.VerifyRuns = r.NewCounterVec("reviewbot_verify_runs_total",
		"Verification runs of a review (review.verify: findings and breaking-change claims in one run) by result (ok, limit = global run limit, rate_limited, invalid_output, error).",
		"result")
	m.VerifyVerdicts = r.NewCounterVec("reviewbot_verify_verdicts_total",
		"Verdicts of the verification run by verdict (confirm, downgrade, reject, missing = no verdict, counted as confirm; unverified = the run failed; proven = not sent: the bot proved it deterministically — migration deployed_usage evidence, gitleaks, a deterministic compat change) and the original severity (critical, major, minor, nit; breaking = a breaking-change claim).",
		"verdict", "severity")

	m.ReactionsTotal = r.NewCounterVec("reviewbot_reactions_total",
		"Award emoji changes of the bot (review.reactions) by event (picked_up, done, failed, postponed, denied, thanks, ack, learned, clean_review) and result (added, exists, disabled = off in the repository, error, forbidden = GitLab answered 403, dropped = worker queue full).",
		"event", "result")
	m.Skipped = r.NewCounterVec("reviewbot_skipped_total",
		"Reviews that deliberately ran no Claude, by reason: too_big (the reviewed range is over review.max_mr_lines; only the deterministic checks ran), trivial_increment (an automatic incremental review whose new commits change only whitespace, comments or docs; review.skip_trivial_increments), no_repo_config (no .reviewbot.yaml on the target branch while review.require_repo_config is on), repo_config_invalid (the target branch's .reviewbot.yaml is invalid).",
		"reason")
	m.ReviewRoutes = r.NewCounterVec("reviewbot_review_route_total",
		"Reviews by model route (review.routing): light (low-risk range: the light model, no advisor), heavy (big or risky range: review.routing.heavy_model, no advisor, no deep-review), normal (claude.model; also every review when routing is off).",
		"route")

	m.GitLabSaved = r.NewCounterVec("reviewbot_gitlab_reads_saved_total",
		"GitLab API reads a job did not make because the same read of the same MR was already done in that job (per-job memo, review/glmemo.go), by op (get_mr, get_mr_diffs, list_discussions).",
		"op")

	m.CIFailures = r.NewCounterVec("reviewbot_ci_failures_total",
		"Failed jobs of failed pipelines (review.ci) by scope (mr = an MR head pipeline, main = a main branch pipeline) and cause (mr = the change, infra, flaky, unknown = not explained); once per failed pipeline.",
		"scope", "cause")
	m.CIExplainRuns = r.NewCounterVec("reviewbot_ci_explain_runs_total",
		"Explanations of failed pipelines (review.ci.explain) by result (ok, limit = global run limit, rate_limited, invalid_output, error, duplicate = the same failure was explained already, no run).",
		"result")

	m.TracingSpans = r.NewCounterVec("reviewbot_tracing_spans_total",
		"Finished trace spans handed to the OTLP exporter (tracing.enabled) by result: exported (accepted by the collector), dropped (export queue full or exporter stopped; never sent), failed (rejected by the collector or retries exhausted).",
		"result")
	m.TracingErrors = r.NewCounterVec("reviewbot_tracing_export_errors_total",
		"Failed OTLP export requests by reason: network (no response: refused, DNS, TLS, timeout), throttled (429), server (5xx), client (other 4xx, e.g. bad credentials; not retried), partial (the collector rejected some spans). Retried attempts count each time.",
		"reason")

	m.goroutines = r.NewGauge("go_goroutines", "Number of goroutines that currently exist.")
	m.memAlloc = r.NewGauge("go_memstats_alloc_bytes", "Number of bytes allocated and still in use.")
	m.memSys = r.NewGauge("go_memstats_sys_bytes", "Number of bytes obtained from system.")
	m.startTime = r.NewGauge("process_start_time_seconds", "Start time of the process since unix epoch in seconds.")
	m.startTime.Set(float64(time.Now().UnixNano()) / 1e9)
	r.OnCollect(m.collectRuntime)
	return m
}

func (m *Metrics) collectRuntime() {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	m.goroutines.Set(float64(runtime.NumGoroutine()))
	m.memAlloc.Set(float64(ms.Alloc))
	m.memSys.Set(float64(ms.Sys))
}

// QueueStats is implemented by *queue.Queue.
type QueueStats interface{ Stats() queue.Stats }

// ObserveQueue samples q at every scrape.
func (m *Metrics) ObserveQueue(q QueueStats) {
	if m == nil || q == nil {
		return
	}
	pending, running, retry := m.QueueJobs.With("pending"), m.QueueJobs.With("running"), m.QueueJobs.With("retry_wait")
	m.Registry.OnCollect(func() {
		s := q.Stats()
		pending.Set(float64(s.Pending))
		running.Set(float64(s.Running))
		retry.Set(float64(s.RetryWait))
		draining := 0.0
		if s.Draining {
			draining = 1
		}
		m.QueueDraining.Set(draining)
	})
}

// ObserveRestore counts one persisted queue job found at startup
// (queue.Options.OnRestore). Nil-safe.
func (m *Metrics) ObserveRestore(kind core.JobKind, result string) {
	if m == nil {
		return
	}
	m.QueueRestored.With(meter.KindForJob(kind), result).Inc()
}

package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// JobKind is what a queued job asks the bot to do.
type JobKind string

const (
	// JobAutoReview is triggered by GitLab events: MR opened/reopened, new
	// commits pushed, or Draft -> Ready. Incremental when possible.
	JobAutoReview JobKind = "auto_review"
	// JobReview is "@bot review": incremental review right now.
	JobReview JobKind = "review"
	// JobFullReview is "@bot full review": review the whole MR from scratch.
	JobFullReview JobKind = "full_review"
	// JobChat is "@bot <question>" in a thread or top-level note.
	JobChat JobKind = "chat"
	// JobHelp is "@bot help".
	JobHelp JobKind = "help"
	// JobRefreshTitle is triggered when only the MR title changed: refresh
	// the title hint in the existing summary note. No Claude run, no usage.
	// It is not an auto job (the queue runs it without debounce) and
	// Manual() reports true for it, so handlers must treat it explicitly.
	JobRefreshTitle JobKind = "refresh_title"
	// JobRefreshStatus re-renders the trunk-based status block of the
	// summary (size, age, behind target, pipeline) without Claude; enqueued
	// by the reviewer while the MR pipeline is still running.
	JobRefreshStatus JobKind = "refresh_status"
	// JobThreadReply is a reply (without a bot mention) inside an existing
	// MR discussion. The handler learns a team rule from it when the thread
	// was started by the bot. Like JobRefreshTitle it runs without debounce,
	// Manual() reports true for it and handlers route it explicitly (it is
	// not a command: no reply on refusal, no per-user command limits).
	JobThreadReply JobKind = "thread_reply"
	// JobRules is a rule management command: "@bot запомни <текст>",
	// "@bot забудь #12", "@bot правила", "@bot правило #12 для всех|только
	// здесь" (English: remember / forget / rules / rule #12 global|local).
	// Question holds the command text after the mention.
	JobRules JobKind = "rules"
	// JobPause / JobResume: "@bot pause" ("пауза") / "@bot resume"
	// ("продолжить") stop / restart automatic reviews of one MR. JobIgnore
	// is "@bot ignore" written in a note (it only works in the MR
	// description; the bot replies how to use it). JobResolveAll is "@bot
	// resolve" ("закрой треды"), JobConfig is "@bot config" ("конфиг").
	// None of them runs Claude or consumes limits (internal/review/commands.go).
	JobPause      JobKind = "pause"
	JobResume     JobKind = "resume"
	JobIgnore     JobKind = "ignore"
	JobResolveAll JobKind = "resolve_all"
	JobConfig     JobKind = "config"
	// JobMemory is a project memory command: "@bot память", "@bot память
	// очистить [раздел]", "@bot память запомни <раздел>: <текст>" (English:
	// memory / memory clear [section] / memory add <section>: <text>). No
	// Claude run. Question holds the command text after the mention.
	JobMemory JobKind = "memory"
	// JobReact is a note addressed to the bot ("@bot спасибо", "@bot ок")
	// that only thanks or acknowledges (internal/reactions.Classify on the
	// text after the mention). The handler puts an award emoji on the note:
	// no Claude run, no reply, no limits. Question holds the text after the
	// mention, so the handler can fall back to JobChat (reactions off for
	// the repository, an acknowledgement outside a bot remark thread).
	JobReact JobKind = "react"
	// JobCheck is "@bot check" ("перепроверь"): re-check the bot's open
	// findings against the current head without a new commit and close the
	// fixed ones (internal/review/check.go). In a bot finding thread only
	// that finding. One small Claude run; counts as a rerun (UsageRerun,
	// per-user hourly limit and the global limit) without the per-MR rerun
	// cooldown.
	JobCheck JobKind = "check"
	// JobForceReview is "@bot review force" ("force review", "ревью
	// принудительно"): a full review that ignores review.max_mr_lines (an MR
	// over it otherwise gets no Claude run, internal/review/bigmr.go). A
	// rerun like "full review": Developer+, per-user limits apply.
	JobForceReview JobKind = "force_review"
	// JobPipeline is a finished CI pipeline (GitLab Pipeline Hook, or the
	// poller's pipeline listing): Job.Pipeline says which one. With MRIID it
	// is the head pipeline of that MR (failure digest, explanation thread,
	// green again); without MRIID and Pipeline.Main it is the main branch of
	// the project (who broke main); otherwise the handler looks up the open
	// MRs of the branch and enqueues one JobPipeline per MR
	// (internal/review/pipeline/ci.go). Like JobRefreshStatus it runs without
	// debounce, Manual() reports true for it and handlers route it
	// explicitly.
	JobPipeline JobKind = "pipeline"
)

// KindInfo is what the bot knows about a JobKind besides its handler (the
// switch in internal/review/review.go). Packages derive their per-kind
// behaviour from it instead of keeping their own lists: the queue and meter
// (Auto), the limiter accounting (Usage), metrics and /usage (RunKind), the
// reaction lifecycle (Reacts), /runs (Label), webhook command parsing
// (Commands, MRCommand, CommandReason) and "@bot help" (Help). Adding a kind
// means one entry in jobKinds; a test checks every JobKind constant has one.
type KindInfo struct {
	Kind JobKind
	// Auto: started by GitLab events and debounced per MR by the queue (not
	// persisted in the job table, never charged to a user). Only
	// JobAutoReview; every other kind, internal ones included, is Manual().
	Auto bool
	// Usage is the limiter usage kind of the job's Claude runs
	// (review/env.JobUsage). Kinds without a Claude run have one too.
	Usage UsageKind
	// RunKind is the run kind of the job's Claude runs on /usage and in the
	// metrics labels (meter.KindForJob): one of the meter.Kind* values.
	RunKind string
	// Reacts: the job is started by a note addressed to the bot and gets the
	// 👀 → ✅/❌ reaction lifecycle on it (internal/review/reactions.go).
	Reacts bool
	// ClosesThread: a service command ("review", "help", "config", ...): once
	// the bot has answered, the thread the command started is resolved with
	// the service token, so it does not hold up "All threads must be
	// resolved". Questions and rule/memory talk stay open for a follow-up.
	ClosesThread bool
	// Label is the display name on /runs (internal/diagweb).
	Label string
	// Commands are the normalized phrases after the mention that request the
	// kind ("@bot full review"); empty for kinds that are not a fixed phrase
	// (chat, rules and memory commands are parsed by their packages).
	Commands []string
	// MRCommand: Commands are MR commands (webhook.ParseMRCommand), matched
	// after a chat question was recognized, also when the mention ends the
	// note. Otherwise webhook.ParseCommand matches them as the whole text
	// right after the mention.
	MRCommand bool
	// CommandReason: an MR command that may carry a short reason after a
	// separator on the same line ("@bot пауза — идёт рефакторинг").
	CommandReason bool
	// Help is the kind's part of the "@bot help" answer: Markdown list
	// lines, "{mention}" stands for the bot mention (see JobKind.Help).
	Help string
}

// Run kinds of KindInfo.RunKind; the same strings as meter.Kind* (meter
// checks it in a test).
const (
	runKindReview       = "review"
	runKindChat         = "chat"
	runKindVerification = "verification"
	runKindLearning     = "learning"
	runKindOther        = "other"
)

// jobKinds is the kind table, in display order (AllJobKinds).
var jobKinds = []KindInfo{
	{Kind: JobAutoReview, Auto: true, Usage: UsageAuto, RunKind: runKindReview, Label: "авторевью"},
	{Kind: JobReview, ClosesThread: true, Usage: UsageRerun, RunKind: runKindReview, Reacts: true, Label: "ревью",
		Commands: []string{"review", "ревью"},
		Help:     "- `{mention} review` (`{mention} ревью`) — ревью новых коммитов с прошлого ревью\n"},
	{Kind: JobFullReview, ClosesThread: true, Usage: UsageRerun, RunKind: runKindReview, Reacts: true, Label: "полное ревью",
		Commands: []string{"full review", "full-review", "полное ревью"},
		Help:     "- `{mention} full review` (`{mention} полное ревью`) — полное ревью MR заново\n"},
	{Kind: JobChat, Usage: UsageChat, RunKind: runKindChat, Reacts: true, Label: "вопрос",
		Help: "- `{mention} <вопрос>` — вопрос по коду; в треде замечания бот учитывает обсуждение и может закрыть тред\n"},
	{Kind: JobHelp, ClosesThread: true, Usage: UsageRerun, RunKind: runKindOther, Reacts: true, Label: "помощь",
		Commands: []string{"help", "помощь"},
		Help:     "- `{mention} help` (`{mention} помощь`) — эта справка\n"},
	{Kind: JobRefreshTitle, Usage: UsageRerun, RunKind: runKindOther, Label: "заголовок"},
	{Kind: JobRefreshStatus, Usage: UsageRerun, RunKind: runKindOther, Label: "статус TBD"},
	{Kind: JobThreadReply, Usage: UsageRerun, RunKind: runKindLearning, Label: "обучение",
		Help: "- Ответ Developer+ в треде замечания бота с объяснением, почему оно неверно, бот запоминает как правило\n"},
	{Kind: JobRules, Usage: UsageRerun, RunKind: runKindLearning, Reacts: true, Label: "правила",
		Help: "- `{mention} запомни <правило>` (`remember`) — запомнить правило команды; область (этот репозиторий или все) выберет бот, либо допишите «для всех» / «только здесь»\n" +
			"- `{mention} правила` (`rules`) — правила, которые действуют в этом репозитории\n" +
			"- `{mention} забудь #N` (`forget #N`) — удалить правило\n" +
			"- `{mention} правило #N для всех` / `только здесь` (`rule #N global|local`) — изменить область правила\n"},
	{Kind: JobPause, ClosesThread: true, Usage: UsageRerun, RunKind: runKindOther, Reacts: true, Label: "пауза",
		Commands: []string{"pause", "пауза", "на паузу", "поставь на паузу"}, MRCommand: true, CommandReason: true,
		Help: "- `{mention} pause` (`{mention} пауза`) — остановить автоматическое ревью этого MR (ручное ревью, вопросы и правила работают)\n"},
	{Kind: JobResume, ClosesThread: true, Usage: UsageRerun, RunKind: runKindOther, Reacts: true, Label: "продолжить",
		Commands: []string{"resume", "unpause", "продолжить", "продолжи", "продолжай", "сними паузу"}, MRCommand: true, CommandReason: true,
		Help: "- `{mention} resume` (`{mention} продолжить`) — снова включить авторевью; непроверенные коммиты будут проверены\n"},
	{Kind: JobIgnore, ClosesThread: true, Usage: UsageRerun, RunKind: runKindOther, Reacts: true, Label: "ignore",
		Commands: []string{"ignore", "игнорировать", "игнорируй"}, MRCommand: true,
		Help: "- `{mention} ignore` (`{mention} игнорировать`) **в описании MR** — не запускать авторевью, пока строка есть в описании\n"},
	{Kind: JobResolveAll, ClosesThread: true, Usage: UsageRerun, RunKind: runKindOther, Reacts: true, Label: "закрыть треды",
		Commands: []string{"resolve", "resolve all", "resolve threads", "закрой треды", "закрыть треды", "закрой все треды"}, MRCommand: true,
		Help: "- `{mention} resolve` (`{mention} закрой треды`) — закрыть все открытые треды бота в этом MR, включая блокирующие (нужен служебный токен)\n"},
	{Kind: JobConfig, ClosesThread: true, Usage: UsageRerun, RunKind: runKindOther, Reacts: true, Label: "конфиг",
		Commands: []string{"config", "configuration", "конфиг", "настройки"}, MRCommand: true,
		Help: "- `{mention} config` (`{mention} конфиг`) — действующие настройки бота для этого MR и откуда они взяты\n"},
	{Kind: JobMemory, Usage: UsageRerun, RunKind: runKindOther, Reacts: true, Label: "память",
		Help: "- `{mention} память` (`memory`) — память проекта: что бот выяснил о сборке, тестах, линте и структуре этого репозитория\n" +
			"- `{mention} память очистить [раздел]` (`memory clear [section]`) — очистить раздел или всю память\n" +
			"- `{mention} память запомни <раздел>: <текст>` (`memory add <section>: <text>`) — добавить пункт вручную (разделы: сборка, тесты, линт, подготовка окружения, структура проекта, подводные камни, прочее)\n"},
	// "@bot check" re-checks open findings: a verification pass.
	{Kind: JobCheck, ClosesThread: true, Usage: UsageRerun, RunKind: runKindVerification, Reacts: true, Label: "перепроверка",
		Commands: []string{"check", "recheck", "проверь", "перепроверь"}, MRCommand: true,
		Help: "- `{mention} check` (`{mention} перепроверь`) — перепроверить открытые замечания бота на текущем коде без нового коммита и закрыть исправленные; в треде замечания — только его\n"},
	// Force review ignores review.max_mr_lines.
	{Kind: JobForceReview, ClosesThread: true, Usage: UsageRerun, RunKind: runKindReview, Reacts: true, Label: "ревью принудительно",
		Commands: []string{"review force", "force review", "ревью принудительно", "принудительное ревью"},
		Help:     "- `{mention} review force` (`{mention} ревью принудительно`) — полное ревью, даже если MR больше порога `review.max_mr_lines`\n"},
	// Replaces a Claude run with an award emoji (no lifecycle of its own;
	// falls back to JobChat, which has one).
	{Kind: JobReact, Usage: UsageRerun, RunKind: runKindOther, Label: "реакция"},
	// The CI explanation: nobody asked for it, so it is charged as automatic.
	{Kind: JobPipeline, Usage: UsageAuto, RunKind: runKindOther, Label: "пайплайн CI"},
}

// kindIndex is jobKinds by kind.
var kindIndex = func() map[JobKind]KindInfo {
	m := make(map[JobKind]KindInfo, len(jobKinds))
	for _, i := range jobKinds {
		m[i.Kind] = i
	}
	return m
}()

// AllJobKinds returns every JobKind, in display order. It is the one list
// UIs and CLIs derive their kind options from (the /runs filter, `reviewbot
// runs -kind`); a test checks it against the constants above.
func AllJobKinds() []JobKind {
	out := make([]JobKind, len(jobKinds))
	for i, info := range jobKinds {
		out[i] = info.Kind
	}
	return out
}

// Info returns the kind's entry in the kind table; ok is false for a kind
// that is not a JobKind constant (e.g. read back from an old database row).
func (k JobKind) Info() (info KindInfo, ok bool) {
	info, ok = kindIndex[k]
	return info, ok
}

// Manual reports whether the job was requested by a person via a command.
// It is true for every kind but JobAutoReview (unknown kinds included).
func (k JobKind) Manual() bool { return !kindIndex[k].Auto }

// Help is the kind's part of the "@bot help" answer for the given mention
// ("" for kinds without one).
func (k JobKind) Help(mention string) string {
	return strings.ReplaceAll(kindIndex[k].Help, "{mention}", mention)
}

// Job is one unit of work for the review worker.
type Job struct {
	Kind        JobKind
	ProjectID   int64
	ProjectPath string // "group/sub/project"
	MRIID       int64
	// HeadSHA is the MR head at event time (auto jobs). Workers always re-read
	// the MR, so this is only a hint.
	HeadSHA string

	// Who asked (manual jobs). Zero for auto jobs.
	UserID   int64
	Username string

	// Where to answer (manual jobs). DiscussionID is the thread the command
	// was posted in; NoteID is the command note itself.
	DiscussionID string
	NoteID       int64
	// Question is the text after the mention for JobChat.
	Question string

	// ForceFull is set for auto reviews that must be full (Draft -> Ready).
	ForceFull bool

	// Pipeline is the finished pipeline of a JobPipeline (nil otherwise).
	Pipeline *PipelineRef

	EnqueuedAt time.Time

	// TraceParent is the W3C traceparent of the event that produced the job
	// (the webhook request or the poll round; "" when not traced). The job's
	// own trace links to it. Not persisted: a job restored after a restart
	// has no link. Not part of the dedup key.
	TraceParent string
}

// PipelineRef is the pipeline a JobPipeline is about, as the event source
// reported it. Handlers re-read what they act on; Status is a hint (and
// part of the dedup key: a retried job turns the same pipeline from failed
// to success).
type PipelineRef struct {
	ID     int64
	Ref    string // branch (or refs/merge-requests/<iid>/head for MR pipelines)
	SHA    string
	Status string // success, failed, …
	// Main: Ref is the project's main branch (gitlab.MainBranchFor).
	Main bool
	// DefaultBranch of the project, as the event reported it ("" unknown).
	DefaultBranch string
	// Source: GitLab's pipeline source (push, schedule, web, …; "" unknown).
	Source string
	// At: when the pipeline reached Status — finished_at of the Pipeline
	// Hook, updated_at of the API listing (zero: unknown). It tells a
	// pipeline that failed again after a job retry from a duplicate event.
	At time.Time
}

// MRKey identifies an MR across projects, e.g. "42!7".
func (j Job) MRKey() string { return MRKey(j.ProjectID, j.MRIID) }

// MRKey builds the canonical MR key.
func MRKey(projectID, mrIID int64) string { return fmt.Sprintf("%d!%d", projectID, mrIID) }

// RetryLater is returned by a job handler when the job must run again later
// (global rate limit, Claude usage limit). The queue re-schedules the job at At.
type RetryLater struct {
	At     time.Time
	Reason string
}

func (e *RetryLater) Error() string {
	return fmt.Sprintf("retry later at %s: %s", e.At.Format(time.RFC3339), e.Reason)
}

// AsRetryLater unwraps a *RetryLater from err.
func AsRetryLater(err error) (*RetryLater, bool) {
	var r *RetryLater
	ok := errors.As(err, &r)
	return r, ok
}

// Handler processes one job.
type Handler func(ctx context.Context, job Job) error

// Queue accepts jobs and runs them with a Handler.
//
// Semantics the implementation must provide:
//   - At most one job per MR key runs at a time.
//   - Auto jobs are debounced per MR key: a new auto job for the same MR
//     replaces a pending (not yet started) one and restarts the delay.
//   - Manual jobs run without debounce delay.
//   - When ready jobs compete for a worker slot, auto jobs go first.
//   - A handler returning *RetryLater re-schedules the same job at At.
//   - Global concurrency is bounded.
type Queue interface {
	Enqueue(job Job)
	// Run starts workers and blocks until ctx is cancelled and running jobs
	// have finished.
	Run(ctx context.Context, h Handler) error
}

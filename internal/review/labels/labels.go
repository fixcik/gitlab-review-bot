// Package labels sets the automatic MR labels (review.labels,
// .reviewbot.yaml labels).
//
// After every review the bot computes which configured labels apply and
// adds the missing ones; with remove_stale it removes the labels IT added
// earlier that no longer apply. Labels set by people are never removed.
// Rules come in two kinds:
//
//   - deterministic rules (labels/rules.go): changed paths and the facts the
//     review's analyzers established (config.LabelFacts: migrations, API
//     changes, MR size, secrets, tests);
//   - instruction rules (labels/prompt.go): the review model decides through
//     the optional "labels" field of its answer (Prompt).
//
// The changes go to GitLab with the bot's own token first and the service
// token second (labels/gitlab.go); a 403 on the bot's token sends the
// project's labels straight to the service token for an hour. The result
// is one line of the summary (labels/summary.go). Nothing here fails a
// review: errors become verification problems (labels_failed). See
// docs/features/labels.md and docs/DESIGN.md.
//
// The package depends on the shared review packages (env.Env: the log and
// the clock; jobctx: bot problems) and reads the results of the review's
// analyzers (migcheck.Run, compatcheck.Run and APIRun, linters.Run,
// pipeline.TBDStatus) handed over in Run. From the orchestrator it needs a
// Client (GitLab MR labels, one per token) and a Store (the bot's labels
// per MR); the orchestrator builds the model's part with NewPrompt
// (review/review_prompt.go) and calls Feature.Apply once the review is
// published (review/review_summary.go, review/bigmr.go).
package labels

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/compatcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/diffcap"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/linters"
	"github.com/fixcik/gitlab-review-bot/internal/review/migcheck"
	"github.com/fixcik/gitlab-review-bot/internal/review/pipeline"
	"github.com/fixcik/gitlab-review-bot/internal/tracing"
)

// Client reads and changes MR labels (*gitlab.Client: the bot's own
// client and the service-token client).
type Client interface {
	// MRLabels returns the MR's state and current labels.
	MRLabels(ctx context.Context, projectID, mrIID int64) (state string, labels []string, err error)
	// UpdateMRLabels adds and removes labels in one request and returns the
	// MR's labels afterwards.
	UpdateMRLabels(ctx context.Context, projectID, mrIID int64, add, remove []string) ([]string, error)
	// ListProjectLabels returns the labels of the project and its ancestor
	// groups.
	ListProjectLabels(ctx context.Context, projectID int64) ([]string, error)
}

// Store keeps which labels the bot added to an MR and which of them a
// human removed (*store.SQLite).
type Store interface {
	BotLabels(ctx context.Context, projectID, mrIID int64) (added, suppressed []string, err error)
	SaveBotLabels(ctx context.Context, projectID, mrIID int64, added, suppressed []string) error
}

// Feature sets the bot's automatic MR labels from what the review found.
type Feature struct {
	env *env.Env
	// bot is the bot's own client for MR labels, svc the service-token
	// client (used when the bot's token gets 403); both nil = no automatic
	// labels. onLabel counts label changes (action add|remove, result); may
	// be nil.
	bot, svc Client
	onLabel  func(action, result string)
	// store remembers the bot's labels per MR (review.Deps.LabelStore); nil
	// = no automatic labels (nothing may be removed without that memory).
	store Store

	mu sync.Mutex
	// labelDeny: when the bot's token last got 403 on a project's labels.
	labelDeny map[int64]time.Time
	// labelRefusedLogged: project+label pairs whose refused override-label
	// rule was logged (once per process).
	labelRefusedLogged map[string]bool
}

// New returns the labels feature on the shared review env: bot is the
// bot's own label client, svc the service-token one, store the bot's
// label memory and onLabel the label metric (each may be nil).
func New(e *env.Env, bot, svc Client, store Store, onLabel func(action, result string)) *Feature {
	return &Feature{env: e, bot: bot, svc: svc, store: store, onLabel: onLabel}
}

// Run is what the review flow hands to Feature.Apply: the reviewed range
// and the results of the review's analyzers (each nil when off).
type Run struct {
	Checkout    core.Checkout
	Files       []string // the reviewed range's files
	Incremental bool
	Migrations  *migcheck.Run
	Compat      *compatcheck.Run
	API         *compatcheck.APIRun
	Lint        *linters.Run
	TBD         *pipeline.TBDStatus
}

// fullMRFiles: the reviewed files on a full review, else the files of
// merge base .. head (ignore globs applied).
func (lb *Feature) fullMRFiles(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, run Run) ([]string, bool) {
	if !run.Incremental {
		return run.Files, true
	}
	if run.Checkout == nil {
		return nil, false
	}
	files, err := run.Checkout.ChangedFiles(ctx, diffcap.FullDiffBase(mr), eff.Ignore)
	if err != nil {
		lb.env.Log.Warn("review: labels: full mr files", "mr", job.MRKey(), "err", err)
		return nil, false
	}
	return files, true
}

// Apply runs after the review is published and returns the summary line
// ("" when there is nothing to say). Never fails the review.
func (lb *Feature) Apply(ctx context.Context, job core.Job, mr *core.MergeRequest, eff config.Effective, lp *Prompt, run Run) (line string) {
	ctx, tsp := tracing.Start(ctx, "review.labels")
	defer tsp.End()
	if !eff.Labels.On() {
		return ""
	}
	log := lb.env.Log.With("mr", job.MRKey())
	defer func() {
		if p := recover(); p != nil {
			log.Error("review: labels panicked", "panic", p)
			jobctx.BotProblem(ctx, problems.LabelsFailed, "метки MR: внутренняя ошибка", fmt.Sprint(p))
			line = ""
		}
	}()
	lb.logRefusedLabels(job, eff.Labels.Refused)
	if eff.Labels.Dropped > 0 {
		log.Warn("review: labels: too many rules, the rest is ignored", "max", config.MaxLabelRules, "dropped", eff.Labels.Dropped)
	}
	read := lb.Reader()
	st := lb.store
	if read == nil || st == nil {
		log.Info("review: labels configured but no label client or store")
		return ""
	}
	state, current, err := read.MRLabels(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: labels: read mr", "err", err)
		jobctx.BotProblem(ctx, problems.LabelsFailed, "метки MR: не удалось прочитать MR", err.Error())
		return ""
	}
	if state != "opened" {
		log.Info("review: labels: mr not opened, skipping", "state", state)
		return ""
	}
	added, suppressed, err := st.BotLabels(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		log.Warn("review: labels: read state", "err", err)
		return "" // without the bot's own record nothing may be removed
	}

	in := inputs{incremental: run.Incremental}
	in.files, in.filesKnown = lb.fullMRFiles(ctx, job, mr, eff, run)
	in.facts = mrFacts(eff, &in, run)
	if lp.on() {
		in.model, in.modelAnswered = lp.chosen, lp.answered
	}
	plan := planLabels(eff.Labels, in, current, added, suppressed)
	plan.add = withoutOverrideLabel(plan.add, eff.BigMR.OverrideLabel)

	out := labelOutcome{held: plan.held}
	add := plan.add
	if len(add) > 0 && !eff.Labels.CreateMissing {
		add = lb.existingLabels(ctx, job, add, &out)
	}
	final := current
	if len(add) > 0 || len(plan.remove) > 0 {
		final = lb.pushLabels(ctx, job, add, plan.remove, &out)
	}

	// The bot's state: what it added and GitLab kept.
	if final != nil {
		fin := toSet(final)
		for _, a := range add {
			if fin[a] {
				plan.added[a] = true
			}
		}
		for a := range plan.added {
			if !fin[a] {
				delete(plan.added, a) // removed now, or by GitLab (scoped label replaced)
			}
		}
	}
	if err := st.SaveBotLabels(ctx, job.ProjectID, job.MRIID, sortedKeys(plan.added), sortedKeys(plan.suppressed)); err != nil {
		log.Warn("review: labels: save state", "err", err)
	}
	out.current = sortedKeys(plan.added)
	log.Info("review: labels", "added", out.added, "removed", out.removed, "failed", out.failed, "held", out.held)
	return out.summaryLine()
}

// withoutOverrideLabel drops the size-override label
// (review.big_mr.override_label) from the labels to add: the bot never sets
// it, or a label rule would lift the size limit and big_mr.block by itself.
// config.Merge already refuses such rules (EffLabels.Refused); this is the
// safety net.
func withoutOverrideLabel(add []string, override string) []string {
	out := add[:0:0]
	for _, a := range add {
		if !config.IsOverrideLabel(a, override) {
			out = append(out, a)
		}
	}
	return out
}

// logRefusedLabels logs the refused override-label rules once per project
// and label.
func (lb *Feature) logRefusedLabels(job core.Job, refused []string) {
	if len(refused) == 0 {
		return
	}
	lb.mu.Lock()
	defer lb.mu.Unlock()
	if lb.labelRefusedLogged == nil {
		lb.labelRefusedLogged = map[string]bool{}
	}
	for _, name := range refused {
		key := fmt.Sprintf("%d\x00%s", job.ProjectID, strings.ToLower(name))
		if lb.labelRefusedLogged[key] {
			continue
		}
		lb.labelRefusedLogged[key] = true
		lb.env.Log.Warn("review: labels: a rule for the size-override label is ignored (the bot never sets review.big_mr.override_label)",
			"project", job.ProjectPath, "label", name)
	}
}

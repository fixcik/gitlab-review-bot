package pipeline

// Main branch pipelines (review.ci.main_branch): trunk-based development,
// so a red main is urgent. When a pipeline of a project's main branch
// (gitlab.main_branch / main_branches, else the default branch) fails while
// main was green, the bot takes the commits since the last green pipeline,
// finds the merged MRs they came from, builds the digest (+ one explanation
// run naming the likely culprit) and writes ONE note in the culprit MR,
// mentioning its author, whoever merged it and review.ci.notify_main. A
// failure of commits pushed without an MR is the problem
// main_pipeline_failed (+ a comment on the commit mentioning the people to
// notify). When main is green again, the note is edited "✅ main снова
// зелёный". The state of each main branch is in the store (ci_main): the
// last green pipeline and the open incident; one note per incident.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/gitlab"
	"github.com/fixcik/gitlab-review-bot/internal/problems"
	"github.com/fixcik/gitlab-review-bot/internal/review/cijob"
	"github.com/fixcik/gitlab-review-bot/internal/review/citelink"
	"github.com/fixcik/gitlab-review-bot/internal/review/env"
	"github.com/fixcik/gitlab-review-bot/internal/review/jobctx"
	"github.com/fixcik/gitlab-review-bot/internal/review/marker"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
)

// CIMain is implemented by the GitLab client (*gitlab.Client): what the
// main branch scope reads (and what routes a branch pipeline to its MRs).
type CIMain interface {
	ListPipelines(ctx context.Context, projectID int64, q gitlab.PipelineQuery) ([]gitlab.PipelineInfo, error)
	GetPipeline(ctx context.Context, projectID, pipelineID int64) (*gitlab.PipelineInfo, error)
	CompareCommits(ctx context.Context, projectID int64, from, to string) ([]core.Commit, error)
	ListCommitMRs(ctx context.Context, projectID int64, sha string) ([]gitlab.CommitMR, error)
}

// CommitComments is implemented by the bot's GitLab client: a comment on a
// commit (a red main after a direct push has no MR to write in).
type CommitComments interface {
	CreateCommitComment(ctx context.Context, projectID int64, sha, note string) error
}

// CIMainStore keeps the state of each main branch (*store.SQLite).
type CIMainStore interface {
	GetCIMain(ctx context.Context, projectID int64, branch string) (*core.CIMain, error)
	SaveCIMain(ctx context.Context, c core.CIMain) error
}

const (
	// ciMainMaxCommits: commits since the last green pipeline looked at.
	ciMainMaxCommits = 30
	// ciMainMaxCandidates: merged MRs named in the prompt and the note.
	ciMainMaxCandidates = 20
)

func corePipeline(pl gitlab.PipelineInfo) core.Pipeline {
	return core.Pipeline{ID: pl.ID, Status: pl.Status, SHA: pl.SHA, WebURL: pl.WebURL}
}

// handleMainPipeline handles a finished pipeline of a main branch.
func (pf *Feature) handleMainPipeline(ctx context.Context, job core.Job) error {
	ref := strings.TrimSpace(job.Pipeline.Ref)
	log := pf.env.Log.With("project", job.ProjectPath, "branch", ref, "pipeline", job.Pipeline.ID)
	cm, ok := core.GitLabAs[CIMain](pf.env.GitLab)
	st := pf.mainStore
	if !ok || st == nil || ref == "" {
		log.Debug("review: ci main: not supported by the client or the store")
		return nil
	}
	// The repository config of the main branch (with review.require_repo_config
	// a repository without one is not watched).
	eff, ok, err := pf.env.RepoConfig.Auto(ctx, job.ProjectID, &core.MergeRequest{ProjectID: job.ProjectID, TargetBranch: ref})
	if !ok {
		return err
	}
	if !eff.Enabled || !eff.CI.Enabled || !eff.CI.MainBranch {
		return nil
	}
	pl, err := cm.GetPipeline(ctx, job.ProjectID, job.Pipeline.ID)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("get pipeline: %w", err)
	}
	if pl.Ref != ref {
		return nil
	}
	// Only the pipelines of review.ci.main_sources (default: push) say
	// whether main is broken: a nightly schedule or a manual run tests other
	// things, a child pipeline is part of its parent. They do not touch the
	// state either (a child's higher id must not hide its parent's later
	// event as "older").
	source := pl.Source
	if source == "" {
		source = job.Pipeline.Source
	}
	if !pf.env.Cfg.Review.CI.MainSourceAllowed(source) {
		log.Info("review: ci main: pipeline source not in review.ci.main_sources, ignored", "source", source)
		return nil
	}
	state, err := st.GetCIMain(ctx, job.ProjectID, ref)
	if err != nil {
		return err
	}
	if state == nil {
		state = &core.CIMain{ProjectID: job.ProjectID, Branch: ref}
	}
	if pl.ID < state.LastPipeline {
		log.Info("review: ci main: an older pipeline finished, ignored", "last", state.LastPipeline)
		return nil
	}
	switch pl.Status {
	case "success":
		pf.mainGreen(ctx, job, *pl, state)
	case "failed":
		if err := pf.mainRed(ctx, job, eff, cm, *pl, state); err != nil {
			return err
		}
	default:
		return nil
	}
	state.UpdatedAt = pf.env.Now()
	return st.SaveCIMain(ctx, *state)
}

// mainGreen: main passed; an open incident ends and its note is edited.
func (pf *Feature) mainGreen(ctx context.Context, job core.Job, pl gitlab.PipelineInfo, state *core.CIMain) {
	if state.RedPipeline != 0 && state.NoteID != 0 && state.CulpritMR != 0 {
		pf.editMainNoteGreen(ctx, job, state, pl)
	}
	if state.RedPipeline != 0 {
		pf.env.Log.Info("review: ci main: green again", "project", job.ProjectPath, "branch", state.Branch, "pipeline", pl.ID, "red_since", state.RedPipeline)
	}
	state.RedPipeline, state.RedSHA, state.CulpritMR, state.NoteID = 0, "", 0, 0
	state.GreenPipeline, state.GreenSHA, state.LastPipeline = pl.ID, pl.SHA, pl.ID
}

// editMainNoteGreen marks the note in the culprit MR "main is green again".
func (pf *Feature) editMainNoteGreen(ctx context.Context, job core.Job, state *core.CIMain, pl gitlab.PipelineInfo) {
	log := pf.env.Log.With("project", job.ProjectPath, "mr", state.CulpritMR)
	ds, err := pf.env.GitLab.ListDiscussions(ctx, job.ProjectID, state.CulpritMR)
	if err != nil {
		log.Warn("review: ci main: read the culprit MR notes", "err", err)
		return
	}
	for _, d := range ds {
		for _, n := range d.Notes {
			if n.ID != state.NoteID || !pf.env.IsBotNote(n) {
				continue
			}
			body := renderMainGreen(n.Body, state, pl)
			if err := pf.env.GitLab.UpdateNote(ctx, job.ProjectID, state.CulpritMR, n.ID, mdtext.NeutralizeQuickActions(body)); err != nil {
				log.Warn("review: ci main: edit the note", "err", err)
			}
			return
		}
	}
	log.Info("review: ci main: the note is gone, nothing to edit", "note", state.NoteID)
}

func renderMainGreen(old string, state *core.CIMain, pl gitlab.PipelineInfo) string {
	rest := old
	if loc := marker.CIMainNoteRe.FindStringIndex(rest); loc != nil {
		rest = rest[loc[1]:]
	}
	// The old text keeps its words, not its pings.
	rest = mdtext.DefuseComments(strings.ReplaceAll(strings.TrimSpace(rest), "@", "@​"))
	return fmt.Sprintf("<!-- %s pipeline=%d state=green -->\n✅ %s снова зелёный (пайплайн %s).\n\n<details>\n<summary>Что было: пайплайн #%d</summary>\n\n%s\n\n</details>\n",
		marker.CIMainNotePrefix, pl.ID, mdtext.InlineCode(state.Branch), cijob.PipelineLink(corePipeline(pl)), state.RedPipeline, rest)
}

// mainRed: main failed. Only the first failure of an incident is reported.
func (pf *Feature) mainRed(ctx context.Context, job core.Job, eff config.Effective, cm CIMain, pl gitlab.PipelineInfo, state *core.CIMain) error {
	log := pf.env.Log.With("project", job.ProjectPath, "branch", state.Branch, "pipeline", pl.ID)
	state.LastPipeline = pl.ID
	if state.RedPipeline != 0 {
		log.Info("review: ci main: still red, already reported", "red_since", state.RedPipeline, "mr", state.CulpritMR)
		return nil
	}
	d, err := pf.buildCIDigest(ctx, job.ProjectID, corePipeline(pl), eff)
	if err != nil {
		log.Warn("review: ci main: digest", "err", err)
		return nil // problem recorded; the next failure tries again
	}
	if len(d.Failed) == 0 {
		log.Info("review: ci main: only ignored jobs failed", "ignored", d.Ignored)
		return nil
	}
	state.RedPipeline, state.RedSHA = pl.ID, pl.SHA

	greenSHA := state.GreenSHA
	if greenSHA == "" {
		greenSHA = pf.lastGreenSHA(ctx, job.ProjectID, state.Branch, cm, pl.ID)
	}
	shas, err := commitsSinceGreen(ctx, job.ProjectID, cm, greenSHA, pl.SHA)
	if err != nil {
		log.Warn("review: ci main: commits since green", "err", err)
	}
	if len(shas) == 0 {
		shas = []string{pl.SHA}
	}
	cands, direct := pf.mainCandidates(ctx, job.ProjectID, state.Branch, cm, shas)

	var expl *ciExplanation
	if eff.CI.Explain && len(cands) > 0 && len(explainableJobs(d)) > 0 {
		expl = pf.explainMain(ctx, job, eff, d, cands, greenSHA, state.Branch)
	}
	culprit := pickCulprit(cands, expl)
	pf.observeCIFailures("main", d, expl)

	if culprit == nil {
		pf.mainDirectPush(ctx, job, eff, d, pl, state.Branch, direct)
		return nil
	}
	body := pf.renderMainNote(job, d, expl, culprit, cands, state.Branch, eff)
	id, err := pf.env.GitLab.CreateNote(ctx, job.ProjectID, culprit.IID, mdtext.NeutralizeQuickActions(body))
	if err != nil {
		log.Warn("review: ci main: note in the culprit MR", "mr", culprit.IID, "err", err)
		jobctx.BotProblem(ctx, problems.MainPipelineFailed, fmt.Sprintf("ветка %s: пайплайн упал, заметку в !%d написать не удалось", state.Branch, culprit.IID), err.Error())
		return nil
	}
	state.CulpritMR, state.NoteID = culprit.IID, id
	log.Info("review: ci main: reported in the culprit MR", "mr", culprit.IID, "candidates", len(cands))
	return nil
}

// lastGreenSHA is the commit of the newest green pipeline of a counted
// source before pipeline before ("" when unknown): a green nightly schedule
// says nothing about the merges.
func (pf *Feature) lastGreenSHA(ctx context.Context, projectID int64, branch string, cm CIMain, before int64) string {
	ps, err := cm.ListPipelines(ctx, projectID, gitlab.PipelineQuery{Ref: branch, Status: "success", Limit: 20})
	if err != nil {
		return ""
	}
	for _, g := range ps {
		if g.ID < before && pf.env.Cfg.Review.CI.MainSourceAllowed(g.Source) {
			return g.SHA
		}
	}
	return ""
}

// commitsSinceGreen returns the commits greenSHA..redSHA, newest first, at
// most ciMainMaxCommits (none when greenSHA is unknown).
func commitsSinceGreen(ctx context.Context, projectID int64, cm CIMain, greenSHA, redSHA string) ([]string, error) {
	if greenSHA == "" || greenSHA == redSHA {
		return nil, nil
	}
	cs, err := cm.CompareCommits(ctx, projectID, greenSHA, redSHA)
	if err != nil {
		return nil, err
	}
	var shas []string
	for i := len(cs) - 1; i >= 0 && len(shas) < ciMainMaxCommits; i-- {
		shas = append(shas, cs[i].SHA)
	}
	return shas, nil
}

// pickCulprit is the merged MR the explanation names, else the newest
// merged one (nil without candidates).
func pickCulprit(cands []gitlab.CommitMR, expl *ciExplanation) *gitlab.CommitMR {
	if len(cands) == 0 {
		return nil
	}
	if expl != nil && expl.CulpritMR != 0 {
		for i := range cands {
			if cands[i].IID == expl.CulpritMR {
				return &cands[i]
			}
		}
	}
	return &cands[0]
}

// mainCandidates returns the merged MRs (into branch) of the commits,
// newest merge first, and the commits that came without an MR.
func (pf *Feature) mainCandidates(ctx context.Context, projectID int64, branch string, cm CIMain, shas []string) (cands []gitlab.CommitMR, direct []string) {
	seen := map[int64]bool{}
	for _, sha := range shas {
		mrs, err := cm.ListCommitMRs(ctx, projectID, sha)
		if err != nil {
			pf.env.Log.Warn("review: ci main: merge requests of a commit", "sha", sha, "err", err)
			continue
		}
		found := false
		for _, m := range mrs {
			if m.State != "merged" || m.TargetBranch != branch {
				continue
			}
			found = true
			if !seen[m.IID] {
				seen[m.IID] = true
				cands = append(cands, m)
			}
		}
		if !found {
			direct = append(direct, sha)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].MergedAt.After(cands[j].MergedAt) })
	if len(cands) > ciMainMaxCandidates {
		cands = cands[:ciMainMaxCandidates]
	}
	return cands, direct
}

// explainMain runs the explanation over the commits since the last green
// pipeline (the checkout fetches the main branch and the newest candidate's
// MR ref). A Claude usage limit does not postpone the report: a red main
// is reported right away, without the explanation.
func (pf *Feature) explainMain(ctx context.Context, job core.Job, eff config.Effective, d *ciDigest, cands []gitlab.CommitMR, greenSHA, branch string) *ciExplanation {
	mr := &core.MergeRequest{ProjectID: job.ProjectID, IID: cands[0].IID, TargetBranch: branch,
		DiffRefs: core.DiffRefs{BaseSHA: greenSHA, HeadSHA: d.Pipeline.SHA}}
	co, err := pf.env.Repos.Checkout(ctx, job.ProjectPath, mr)
	if err != nil {
		pf.env.Log.Warn("review: ci main: checkout", "err", err)
		pf.observeCIExplain(ciExplainError)
		return nil
	}
	defer CloseCheckout(pf.env.Log, co)
	var bs *env.Base
	if greenSHA != "" {
		bs = pf.env.PrepareBase(ctx, job, co, mr)
		defer bs.Release()
	}
	diff, cut := pf.ciDiff(ctx, co, greenSHA, eff.Ignore)
	var targets []ciCandidate
	for _, c := range cands {
		targets = append(targets, ciCandidate{IID: c.IID, Title: mdtext.OneLine(c.Title), Author: c.Author, MergedBy: c.MergedBy})
	}
	expl, _, _ := pf.explainCI(ctx, job, ciExplainInput{dir: co.Dir(), base: bs, digest: d, diff: diff, cut: cut,
		what: "main branch " + branch + " (after merges)", lang: prompts.LanguageName(eff.Language), targets: targets})
	return expl
}

// mentionList renders @user for valid, distinct usernames (not the bot).
func (pf *Feature) mentionList(users ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range users {
		u = strings.TrimPrefix(strings.TrimSpace(u), "@")
		if u == "" || !mdtext.UsernameRe.MatchString(u) || seen[strings.ToLower(u)] || strings.EqualFold(u, pf.env.BotUsername) {
			continue
		}
		seen[strings.ToLower(u)] = true
		out = append(out, "@"+u)
	}
	return out
}

// renderMainNote is the note in the culprit MR (everything after the
// marker is defused: branch, job and test names, logs are untrusted).
func (pf *Feature) renderMainNote(job core.Job, d *ciDigest, expl *ciExplanation, culprit *gitlab.CommitMR, cands []gitlab.CommitMR, branch string, eff config.Effective) string {
	var b strings.Builder
	if expl.onlyInfra() {
		fmt.Fprintf(&b, "⚠️ %s красный после мержа этого MR (пайплайн %s, коммит `%s`), но, похоже, причина не в коде: инфраструктура CI или нестабильный тест.",
			mdtext.InlineCode(branch), cijob.PipelineLink(d.Pipeline), mdtext.ShortSHA(d.Pipeline.SHA))
	} else {
		fmt.Fprintf(&b, "🔴 %s сломан после мержа этого MR (пайплайн %s, коммит `%s`).", mdtext.InlineCode(branch), cijob.PipelineLink(d.Pipeline), mdtext.ShortSHA(d.Pipeline.SHA))
		if people := pf.mentionList(culprit.Author, culprit.MergedBy); len(people) > 0 {
			b.WriteString(" " + strings.Join(people, " ") + ", посмотрите, пожалуйста: в trunk-based main должен оставаться зелёным — почините или откатите.")
		}
	}
	if m := pf.mentionList(pf.env.Cfg.Review.CI.NotifyUsers()...); len(m) > 0 {
		b.WriteString(" cc " + strings.Join(m, " "))
	}
	b.WriteString("\n\n")
	var others []string
	for _, c := range cands {
		if c.IID != culprit.IID {
			others = append(others, fmt.Sprintf("!%d", c.IID))
		}
	}
	if len(others) > 0 {
		b.WriteString("Другие MR, смерженные после последнего зелёного пайплайна: " + strings.Join(mdtext.CapList(others, 10), ", ") + ".")
		if expl == nil || expl.CulpritMR == 0 {
			b.WriteString(" Этот MR выбран как последний смерженный.")
		}
		b.WriteString("\n\n")
	}
	if expl != nil && len(expl.Jobs) > 0 {
		links := citelink.New(pf.env.Cfg.GitLab.URL)
		links.Add(job.ProjectPath, d.Pipeline.SHA)
		b.WriteString("#### Почему упал\n\n" + renderCIExplanation(expl, job.ProjectPath, links))
	}
	b.WriteString("#### Что упало\n\n" + renderCIDigest(d, true) + "\n")
	b.WriteString("<sub>Когда " + mdtext.InlineCode(branch) + " снова станет зелёным, бот отметит это здесь. Объяснение — модель по логам CI и коду; логи — недоверенные данные.</sub>")
	return fmt.Sprintf("<!-- %s pipeline=%d state=red -->\n", marker.CIMainNotePrefix, d.Pipeline.ID) + mdtext.DefuseComments(b.String())
}

// mainDirectPush: no merged MR to blame. The problem main_pipeline_failed
// is recorded; the people of review.problems.notify (when the problem is
// new or due for a reminder) and review.ci.notify_main get a comment on
// the commit.
func (pf *Feature) mainDirectPush(ctx context.Context, job core.Job, eff config.Effective, d *ciDigest, pl gitlab.PipelineInfo, branch string, direct []string) {
	var names []string
	for _, jd := range d.Failed {
		names = append(names, jd.Job.Name)
	}
	jobctx.BotProblem(ctx, problems.MainPipelineFailed, "ветка "+branch+": пайплайн упал после прямого пуша (без MR)",
		fmt.Sprintf("пайплайн #%d (%s), коммит %s; упали: %s", pl.ID, pl.WebURL, mdtext.ShortSHA(pl.SHA), strings.Join(mdtext.CapList(names, 10), ", ")))
	outs := problems.Commit(ctx)
	var users []string
	var notified []problems.Outcome
	svc := problems.ServiceOf(ctx)
	for _, o := range outs {
		if o.Kind == problems.MainPipelineFailed && o.Notify {
			users = append(users, svc.Notify()...)
			notified = append(notified, o)
		}
	}
	users = append(users, pf.env.Cfg.Review.CI.NotifyUsers()...)
	people := pf.mentionList(users...)
	log := pf.env.Log.With("project", job.ProjectPath, "branch", branch, "pipeline", pl.ID)
	cc, ok := core.GitLabAs[CommitComments](pf.env.GitLab)
	if len(people) == 0 || !ok {
		log.Info("review: ci main: red after a direct push, nobody to notify", "commits", len(direct))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "🔴 %s сломан (пайплайн %s) после прямого пуша без MR. %s, посмотрите, пожалуйста.\n\n", mdtext.InlineCode(branch),
		cijob.PipelineLink(corePipeline(pl)), strings.Join(people, " "))
	b.WriteString(renderCIDigest(d, true))
	if err := cc.CreateCommitComment(ctx, job.ProjectID, pl.SHA, mdtext.NeutralizeQuickActions(mdtext.DefuseComments(b.String()))); err != nil {
		log.Warn("review: ci main: commit comment", "err", err)
		return
	}
	svc.MarkNotified(ctx, notified)
	log.Info("review: ci main: red after a direct push, commit commented", "notified", len(people))
}

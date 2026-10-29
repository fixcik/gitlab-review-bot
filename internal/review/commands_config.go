package review

// "@bot config": the effective configuration of the MR as a table, each
// value with its source (service, .reviewbot.yaml, default).

import (
	"context"
	"errors"
	"fmt"
	"github.com/fixcik/gitlab-review-bot/internal/reactions"
	"reflect"
	"strings"

	"github.com/fixcik/gitlab-review-bot/internal/config"
	"github.com/fixcik/gitlab-review-bot/internal/core"
	"github.com/fixcik/gitlab-review-bot/internal/learn"
	"github.com/fixcik/gitlab-review-bot/internal/review/mdtext"
	"github.com/fixcik/gitlab-review-bot/internal/review/prompts"
	"github.com/fixcik/gitlab-review-bot/internal/skills"
)

const (
	srcService = "сервис"
	srcRepo    = "`.reviewbot.yaml`"
	srcDefault = "по умолчанию"
	// maxConfigListItems caps lists (globs, tools) in the config table.
	maxConfigListItems = 12
)

// configRow is one row of the "@bot config" table.
type configRow struct{ name, value, source string }

// handleConfig answers "@bot config" with the effective configuration of
// the MR; an invalid or missing (not opted in) .reviewbot.yaml gets the
// repository config reply instead.
func (r *Reviewer) handleConfig(ctx context.Context, job core.Job) error {
	mr, err := r.env.GitLab.GetMR(ctx, job.ProjectID, job.MRIID)
	if err != nil {
		return fmt.Errorf("get mr: %w", err)
	}
	// .reviewbot.yaml of the target branch (nil when absent).
	repo, err := r.targetRepoConfig(ctx, job.ProjectID, mr)
	if err != nil {
		var rc *repoConfigError
		if errors.As(err, &rc) {
			return r.denyRepoConfig(ctx, job, mr, rc)
		}
		return err
	}
	eff := config.Merge(r.env.Cfg, repo)
	var b strings.Builder
	b.WriteString("### ⚙️ Настройки бота для этого MR\n\n")
	b.WriteString("| Настройка | Значение | Источник |\n|---|---|---|\n")
	for _, row := range r.configRows(ctx, job, mr, repo, eff) {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", row.name, row.value, row.source)
	}
	if repo == nil {
		// The branch name is the user's: one code span it cannot close (mdtext.Code).
		fmt.Fprintf(&b, "\n_В целевой ветке %s нет `%s` — действуют настройки сервиса._\n", mdtext.Code(mr.TargetBranch), config.RepoConfigPath)
	} else {
		fmt.Fprintf(&b, "\n_`%s` читается из целевой ветки %s._\n", config.RepoConfigPath, mdtext.Code(mr.TargetBranch))
	}
	return r.env.Reply(ctx, job, b.String())
}

// configRows builds the table. Values never include tokens or URLs: every
// value is scrubbed with learn.Scrub as well.
func (r *Reviewer) configRows(ctx context.Context, job core.Job, mr *core.MergeRequest, repo *config.Repo, eff config.Effective) []configRow {
	def := config.Defaults()
	svc := r.env.Cfg
	var rows []configRow
	add := func(name, value, source string) {
		rows = append(rows, configRow{name: name, value: learn.Scrub(value), source: source})
	}
	svcOr := func(changed bool) string {
		if changed {
			return srcService
		}
		return srcDefault
	}
	has := repo != nil

	// Enabled / language / auto review.
	src := srcDefault
	if has && repo.Enabled != nil {
		src = srcRepo
	}
	add("`enabled`", onOff(eff.Enabled), src)
	src = svcOr(svc.Review.DefaultLang != def.Review.DefaultLang)
	if has && repo.Language != "" {
		src = srcRepo
	}
	add("`language`", mdtext.CodeCell(eff.Language)+" ("+prompts.LanguageName(eff.Language)+")", src)
	src = srcDefault
	if has && repo.AutoReview != nil {
		src = srcRepo
	}
	add("`auto_review`", onOff(eff.AutoReview), src)

	// Pause and ignore marker.
	pausedVal := "нет"
	if f := r.flags(); f != nil {
		if paused, by, at, err := f.GetMRPause(ctx, job.ProjectID, job.MRIID); err != nil {
			pausedVal = "неизвестно"
		} else if paused {
			pausedVal = "⏸ да"
			if by != "" {
				pausedVal += ", поставил " + quietUser(by)
			}
			if !at.IsZero() {
				pausedVal += ", " + r.env.Clock(at)
			}
		}
	}
	add("пауза авторевью", pausedVal, "команда `pause` / `resume`")
	ign := "нет"
	if r.hasIgnoreMarker(mr.Description) {
		ign = "да — авторевью не выполняется"
	}
	add("маркер `ignore`", ign, "описание MR")

	// Limits (effective).
	lim := eff.Limits
	limSrc := func(svcVal, defVal, effVal, repoVal any, repoSet bool) string {
		if repoSet && reflect.DeepEqual(effVal, repoVal) {
			return srcRepo
		}
		return svcOr(!reflect.DeepEqual(svcVal, defVal))
	}
	var rl config.RepoLimits
	if has && repo.Limits != nil {
		rl = *repo.Limits
	}
	add("`limits.user_reruns_per_hour`", limitText(lim.UserRerunsPerHour),
		limSrc(svc.Limits.UserRerunsPerHour, def.Limits.UserRerunsPerHour, lim.UserRerunsPerHour, rl.UserRerunsPerHour, rl.UserRerunsPerHour > 0))
	add("`limits.user_chat_per_hour`", limitText(lim.UserChatPerHour),
		limSrc(svc.Limits.UserChatPerHour, def.Limits.UserChatPerHour, lim.UserChatPerHour, rl.UserChatPerHour, rl.UserChatPerHour > 0))
	add("`limits.mr_rerun_cooldown`", lim.MRRerunCooldown.D().String(),
		limSrc(svc.Limits.MRRerunCooldown, def.Limits.MRRerunCooldown, lim.MRRerunCooldown, rl.MRRerunCooldown, rl.MRRerunCooldown > 0))
	add("`limits.global_runs_per_hour`", limitText(svc.Limits.GlobalRunsPerHour), svcOr(svc.Limits.GlobalRunsPerHour != def.Limits.GlobalRunsPerHour))

	// Review gate.
	add("`block_min_severity`", mdtext.CodeCell(svc.Review.BlockMinSeverity), svcOr(svc.Review.BlockMinSeverity != def.Review.BlockMinSeverity))

	// Compat.
	src = svcOr(svc.Review.CompatCheck != def.Review.CompatCheck)
	if svc.Review.CompatCheck && has && repo.Compat != nil && repo.Compat.Enabled != nil {
		src = srcRepo
	}
	add("обратная совместимость (`compat`)", onOff(eff.Compat.Enabled), src)
	var extra []string
	for _, p := range []struct {
		name  string
		items []string
	}{{"openapi", eff.Compat.OpenAPI}, {"proto", eff.Compat.Proto}, {"graphql", eff.Compat.GraphQL}} {
		if len(p.items) > 0 {
			extra = append(extra, p.name+": "+codeList(p.items))
		}
	}
	if len(extra) > 0 {
		add("`compat`: доп. пути", strings.Join(extra, "; "), srcRepo)
	}

	// Context repositories.
	ctxOn := svc.ContextEnabled() && r.crossrepo.HasContextRepos() && eff.ContextEnabled()
	src = svcOr(svc.Review.Context.Enabled != def.Review.Context.Enabled || !svc.ContextEnabled())
	if svc.ContextEnabled() && has && repo.Context != nil && repo.Context.Enabled != nil {
		src = srcRepo
	}
	val := onOff(ctxOn)
	if !svc.ContextEnabled() {
		val += " (нет `gitlab.groups` или выключено в сервисе)"
	}
	add("контекст других репозиториев", val, src)
	if eff.Context != nil && len(eff.Context.Exclude) > 0 {
		add("`context.exclude`", codeList(eff.Context.Exclude), srcRepo)
	}
	if rel := eff.ContextRelated(); len(rel) > 0 {
		var names []string
		for _, rr := range rel {
			names = append(names, rr.Repo)
		}
		add("связанные репозитории (`context.related`)", codeList(names), srcRepo)
	}

	// Learning.
	learnOn := r.learning.RulesOn(eff)
	src = svcOr(svc.Review.Learn.Enabled != def.Review.Learn.Enabled)
	if svc.Review.Learn.Enabled && has && repo.Learn != nil && repo.Learn.Enabled != nil {
		src = srcRepo
	}
	add("обучение (`learn`)", onOff(learnOn), src)

	// Team skills.
	src = svcOr(svc.Review.Skills.On())
	if svc.Review.Skills.On() && has && repo.Skills != nil && repo.Skills.Enabled != nil {
		src = srcRepo
	}
	skVal := onOff(eff.SkillsEnabled)
	if !svc.Review.Skills.On() {
		skVal += " (не задан `review.skills.dir`)"
	} else if eff.SkillsEnabled {
		if list, _, err := skills.Load(svc.Review.Skills.Dir); err == nil {
			var names []string
			for _, s := range skills.Select(list, eff.SkillsDisable) {
				names = append(names, s.Name)
			}
			if len(names) > 0 {
				skVal += ": " + codeList(names)
			} else {
				skVal += ": скиллов нет"
			}
		}
	}
	add("скиллы команды (`skills`)", skVal, src)

	// What the model may do (fixed) and ignore globs.
	add("инструменты модели", "только чтение файлов (`Read`, `Grep`, `Glob`), код до MR — в `/base`; команды не запускаются", srcDefault)
	ignVal := "—"
	if len(eff.Ignore) > 0 {
		ignVal = codeList(eff.Ignore)
	}
	add("`ignore`", ignVal, combinedSourceDef(!reflect.DeepEqual(svc.Review.DefaultIgnore, def.Review.DefaultIgnore), has && len(repo.Ignore) > 0))
	src = srcDefault
	if has && repo.MaxFiles > 0 {
		src = srcRepo
	}
	add("`max_files`", fmt.Sprint(eff.MaxFiles), src)
	instr := "нет"
	if n := len([]rune(strings.TrimSpace(eff.Instructions))); n > 0 {
		instr = fmt.Sprintf("заданы (%d симв.)", n)
	}
	add("`instructions`", instr, srcRepo)

	// Jira, title, diagram.
	jiraVal := "не подключена"
	if r.task.Jira != nil {
		jiraVal = "подключена"
	}
	add("Jira", jiraVal, svcOr(svc.Jira.Enabled()))
	keys := "любые"
	if len(eff.JiraProjectKeys) > 0 {
		keys = codeList(eff.JiraProjectKeys)
	}
	src = svcOr(len(svc.Jira.ProjectKeys) > 0)
	if has && repo.Jira != nil && len(repo.Jira.ProjectKeys) > 0 {
		src = srcRepo
	}
	add("ключи Jira (`project_keys`)", keys, src)
	add("проверка заголовка (`title_check`)", onOff(svc.Jira.TitleCheck), svcOr(svc.Jira.TitleCheck != def.Jira.TitleCheck))
	src = svcOr(svc.Review.Diagram != def.Review.Diagram)
	if svc.Review.Diagram && has && repo.Diagram != nil {
		src = srcRepo
	}
	add("схема изменений (`diagram`)", onOff(eff.Diagram), src)
	add(reactionsConfigRow(svc, repo, eff))
	// Automatic labels (review/labels).
	src = svcOr(svc.Review.Labels.Enabled != def.Review.Labels.Enabled || len(svc.Review.Labels.Rules) > 0)
	if has && repo.Labels != nil {
		src = srcRepo
	}
	labelsVal := onOff(eff.Labels.On())
	if eff.Labels.On() {
		names := make([]string, 0, len(eff.Labels.Rules))
		for _, lr := range eff.Labels.Rules {
			names = append(names, lr.Name)
		}
		labelsVal += ": " + codeList(names)
	}
	add("метки MR (`labels`)", labelsVal, src)
	// Size limit of the review (bigmr.go).
	src = svcOr(svc.Review.MaxMRLines != def.Review.MaxMRLines)
	if has && repo.MaxMRLines > 0 && eff.BigMR.MaxLines == repo.MaxMRLines {
		src = srcRepo
	}
	add("порог размера MR (`max_mr_lines`)", bigMRConfigValue(eff.BigMR), src)
	return rows
}

// onOff renders a switch.
func onOff(b bool) string {
	if b {
		return "✅ вкл"
	}
	return "⛔ выкл"
}

// limitText renders a limit (0 or less = no limit).
func limitText(n int) string {
	if n <= 0 {
		return "без лимита"
	}
	return fmt.Sprint(n)
}

// codeList renders items as inline code, at most maxConfigListItems.
func codeList(items []string) string {
	var out []string
	for i, it := range items {
		if i == maxConfigListItems {
			out = append(out, fmt.Sprintf("и ещё %d", len(items)-maxConfigListItems))
			break
		}
		out = append(out, mdtext.CodeCell(mdtext.Shorten(it, 80)))
	}
	return strings.Join(out, ", ")
}

// combinedSourceDef: lists merged from the service (with built-in defaults)
// and the repo.
func combinedSourceDef(svcChanged, repoSet bool) string {
	base := srcDefault
	if svcChanged {
		base = srcService
	}
	if repoSet {
		return base + " + " + srcRepo
	}
	return base
}

// reactionsConfigRow is the "@bot config" row of review.reactions.
func reactionsConfigRow(svc *config.Service, repo *config.Repo, eff config.Effective) (name, value, source string) {
	name, value, source = "реакции (`reactions`)", onOff(eff.Reactions.Enabled), srcDefault
	if svc.Review.Reactions.Enabled != config.Defaults().Review.Reactions.Enabled {
		source = srcService
	}
	if repo != nil && repo.Reactions != nil && repo.Reactions.Enabled != nil {
		source = srcRepo
	}
	if eff.Reactions.Enabled {
		var on []string
		for _, ev := range reactions.Events() {
			if n := eff.Reactions.Emoji(string(ev)); n != "" {
				on = append(on, string(ev)+" "+mdtext.CodeCell(":"+n+":"))
			}
		}
		if len(on) > 0 {
			value += ": " + strings.Join(on, ", ")
		}
	}
	return name, value, source
}

// bigMRConfigValue is the "@bot config" value of the size limit.
func bigMRConfigValue(b config.EffBigMR) string {
	if !b.On() {
		return "без лимита"
	}
	v := fmt.Sprintf("%d строк", b.MaxLines)
	if b.OverrideLabel != "" {
		v += ", метка-исключение " + mdtext.CodeCell(b.OverrideLabel)
	}
	if b.Block {
		v += ", блокирует мерж"
	}
	return v
}

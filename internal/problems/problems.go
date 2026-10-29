// Package problems records what the bot itself could not check during a
// review ("verification problems"): a linter that did not run, a coverage
// report that was not found, a related repository without access, a diff
// that was cut, an answer that could not be parsed, and what the review
// model reports it could not verify. Problems are collected per job in the
// context (like the meter attribution and the diagnostics run), persisted
// at most once per job and fingerprint, listed on /problems and shown in
// the MR summary of the run that hit them.
//
// Recording never fails a review: every error here is logged and dropped.
package problems

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Kind is the type of a problem (a fixed set, see Kinds).
type Kind string

// Bot-side kinds: what the bot's own steps could not do.
const (
	LintFailed         Kind = "lint_failed"
	BaseUnavailable    Kind = "base_unavailable"
	CompatFailed       Kind = "compat_failed"
	CoverageMissing    Kind = "coverage_missing"
	ContextUnavailable Kind = "context_unavailable"
	JiraFailed         Kind = "jira_failed"
	AdvisorUnavailable Kind = "advisor_unavailable"
	// DescriptionSummaryFailed: the summary block could not be written into
	// the MR description (no right, GitLab error).
	DescriptionSummaryFailed Kind = "description_summary_failed"
	// DeepReviewUnavailable: the deep-review subagent call failed; the
	// review finished without the second opinion.
	DeepReviewUnavailable Kind = "deep_review_unavailable"
	SysmapFailed          Kind = "sysmap_failed"
	DiffTruncated         Kind = "diff_truncated"
	InvalidOutput         Kind = "invalid_output"
	ClaudeAuthFailed      Kind = "claude_auth_failed"
	EvidenceFailed        Kind = "evidence_failed"
	MCPFailed             Kind = "mcp_failed"
	ReactionsFailed       Kind = "reactions_failed"
	LabelsFailed          Kind = "labels_failed"
	VerifyFailed          Kind = "verify_failed"
	// CIFetchFailed: the CI failure digest could not read a pipeline's
	// jobs, a job log or the test report (review.ci).
	CIFetchFailed Kind = "ci_fetch_failed"
	// MainPipelineFailed: a pipeline of a main branch failed and no merged
	// MR could be blamed (a direct push), see review.ci.
	MainPipelineFailed Kind = "main_pipeline_failed"
	// RepoConfigInvalid: the .reviewbot.yaml of the target branch is
	// invalid, so the repository is not reviewed (one fingerprint per
	// project: the subject is always the file name).
	RepoConfigInvalid Kind = "repo_config_invalid"
)

// Model-reported kinds: what the review model says it could not verify
// (report_problem over MCP, or the optional "problems" array of the answer).
const (
	ModelToolFailed        Kind = "model_tool_failed"
	ModelMissingDependency Kind = "model_missing_dependency"
	ModelNoAccess          Kind = "model_no_access"
	ModelTooLarge          Kind = "model_too_large"
	ModelConfigUnclear     Kind = "model_config_unclear"
	ModelOther             Kind = "model_other"
)

// Source says who reported a problem.
type Source string

const (
	SourceBot   Source = "bot"
	SourceModel Source = "model"
)

// Severity of a kind.
type Severity string

const (
	SevInfo  Severity = "info"
	SevWarn  Severity = "warn"
	SevError Severity = "error"
)

// KindInfo describes a kind for people: a Russian title (what went wrong,
// shown in the MR summary), an imperative Action (what to do, the heading of
// the fix on /problems), a severity, the impact on the review, a one-line
// "how to fix" Hint and, for most kinds, step-by-step instructions (Steps,
// see steps.go; nil = the page shows the Hint). ModelName is the short name
// the model uses ("" for bot-side kinds).
type KindInfo struct {
	Kind      Kind
	Title     string
	Action    string
	Severity  Severity
	Impact    Impact
	Hint      string
	Steps     []Step
	ModelName string
}

// Model reports a model kind.
func (k KindInfo) Model() bool { return k.ModelName != "" }

var kinds = []KindInfo{
	{Kind: LintFailed, Title: "Линтер или сканер не отработал", Action: "Почините линтер или его отчёт в CI", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "gitleaks: проверьте, что он есть в образе песочницы (Dockerfile.sandbox). Линтер из CI (ci:<tool>): проверьте `lint.ci` в `.reviewbot.yaml` (имя job, путь артефакта) и job в `.gitlab-ci.yml` (`artifacts: when: always`, отчёт в `paths` или `reports:codequality`); причина — в саммари и в записи запуска (/runs)."},
	{Kind: BaseUnavailable, Title: "Код до MR (/base) недоступен модели", Action: "Верните модели код до MR (/base)", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "Снимок merge base для `/base` не создан: репозиторий больше `review.context.max_repo_mb` или выгрузка упала (причина — в теме и в логах бота). Модель сравнивала изменения только по диффу."},
	{Kind: CompatFailed, Title: "Проверка обратной совместимости не выполнена", Action: "Почините проверку совместимости API", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "Инструмент (oasdiff, buf, graphql-inspector) недоступен или упал: проверьте образ песочницы и саму спецификацию; детали — в /runs."},
	{Kind: CoverageMissing, Title: "Нет данных о покрытии", Action: "Настройте отчёт о покрытии", Severity: SevInfo, Impact: ImpactPartial,
		Hint: "Проверьте `coverage` в `.reviewbot.yaml` (имя джобы, путь к отчёту), что джоба публикует артефакт и что отчёт включает изменённые файлы."},
	{Kind: ContextUnavailable, Title: "Связанный репозиторий недоступен", Action: "Откройте боту связанный репозиторий", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "Проверьте путь в `context.related`, доступ бота к проекту (Reporter) и `review.context.max_repo_mb`."},
	{Kind: JiraFailed, Title: "Задача Jira не получена", Action: "Проверьте доступ бота к Jira", Severity: SevInfo, Impact: ImpactPartial,
		Hint: "Проверьте ключ задачи в MR, токен `JIRA_TOKEN` и права его пользователя на проект Jira."},
	{Kind: AdvisorUnavailable, Title: "Советник (advisor) недоступен", Action: "Смените модель советника или выключите его", Severity: SevInfo, Impact: ImpactNone,
		Hint: "Аккаунт или модель не поддерживают advisor: смените `claude.advisor_model` или оставьте его пустым; ревью идёт без советника."},
	{Kind: DeepReviewUnavailable, Title: "Второе мнение (deep-review) не получено", Action: "Смените модель deep-review или выключите его", Severity: SevInfo, Impact: ImpactNone,
		Hint: "Вызов сабагента `deep-review` завершился ошибкой (модель недоступна аккаунту, перегрузка, лимит): ревью закончено без второго мнения. Если повторяется — смените `claude.deep_review_model` или оставьте его пустым."},
	{Kind: DescriptionSummaryFailed, Title: "Саммари в описании MR не записано", Action: "Дайте боту право менять описание или выключите саммари", Severity: SevInfo, Impact: ImpactNone,
		Hint: "Токен бота получил 403/404 на изменение описания MR (нужна роль Developer или автор MR): задайте GITLAB_SERVICE_TOKEN или выключите `review.description_summary`; ревью идёт без блока в описании."},
	{Kind: SysmapFailed, Title: "Карта системы проверена не полностью", Action: "Дайте карте системы больше бюджета", Severity: SevInfo, Impact: ImpactNone,
		Hint: "Бюджет извлечения исчерпан или сканирование упало: см. /map, `review.sysmap.max_runs_per_hour` и `deps_max_runs_per_hour`."},
	{Kind: DiffTruncated, Title: "Дифф обрезан — ревью частичное", Action: "Разбейте MR или поднимите лимит диффа", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "MR больше `review.max_diff_bytes` / `max_files`: разбейте MR или поднимите лимиты; часть изменений не просмотрена."},
	{Kind: InvalidOutput, Title: "Ответ модели не разобран", Action: "Разберите ответ модели в транскрипте", Severity: SevError, Impact: ImpactBreaks,
		Hint: "Модель дважды ответила не по схеме, ревью не опубликовано: смотрите транскрипт в /runs; при повторах — проверьте версию Claude Code."},
	{Kind: ClaudeAuthFailed, Title: "Claude отклонил токен", Action: "Замените токен Claude", Severity: SevError, Impact: ImpactBreaks,
		Hint: "Токен `CLAUDE_CODE_OAUTH_TOKEN` / ключ `ANTHROPIC_API_KEY` недействителен, истёк или отозван: выпустите новый, обновите окружение бота и перезапустите его. Пока токен не заменён, ревью и ответы не работают."},
	{Kind: EvidenceFailed, Title: "Проверка миграций не выполнена", Action: "Разберите сбой проверки миграций", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "Анализ миграций или проверка `deployed_usage` упали: блокирующие замечания на миграциях понижены. Детали — в логах бота и /runs."},
	{Kind: MCPFailed, Title: "Инструменты MCP недоступны", Action: "Верните инструменты MCP в песочницу", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "Проверьте `review.mcp.url` / `listen` и форвард порта в egress-контейнере; ревью прошло без инструментов бота."},
	{Kind: ReactionsFailed, Title: "Реакции (award emoji) не ставятся", Action: "Дайте боту ставить реакции или выключите их", Severity: SevInfo, Impact: ImpactNone,
		Hint: "GitLab ответил 403 на award emoji: проверьте, что аккаунт бота (`GITLAB_TOKEN`, scope `api`) — участник проекта с ролью Reporter или выше и не заблокирован; либо выключите реакции (`review.reactions.enabled: false` или `reactions.enabled: false` в `.reviewbot.yaml`). Ревью и ответы это не затрагивает."},
	{Kind: LabelsFailed, Title: "Метки MR не проставлены", Action: "Создайте метку или дайте боту права на метки", Severity: SevWarn, Impact: ImpactNone,
		Hint: "Меткам MR нужна роль Developer: задайте `GITLAB_SERVICE_TOKEN` (Maintainer) или дайте боту Developer; метку, которой нет в проекте и группах, создайте или включите `labels.create_missing`. Ревью от этого не страдает."},
	{Kind: VerifyFailed, Title: "Перепроверка замечаний не выполнена", Action: "Устраните причину, по которой перепроверка не запустилась", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "Замечания и breaking changes опубликованы без перепроверки: общий лимит запусков (`limits.global_runs_per_hour`), лимит Claude, ответ не по схеме или ошибка запуска — причина в теме, транскрипт в /runs. Выключить: `review.verify.enabled: false`."},
	{Kind: CIFetchFailed, Title: "Данные CI не прочитаны", Action: "Откройте боту логи и отчёты CI", Severity: SevWarn, Impact: ImpactPartial,
		Hint: "Бот не смог прочитать джобы пайплайна, лог job или отчёт о тестах: при 403 у Reporter нет доступа к логам (проект с закрытыми пайплайнами) — задайте `GITLAB_SERVICE_TOKEN` (бот повторит запрос им) или откройте пайплайны участникам; причина — в теме и описании. Разбор упавшего пайплайна вышел неполным."},
	{Kind: MainPipelineFailed, Title: "Сломан main после прямого пуша", Action: "Почините main или откатите прямой пуш", Severity: SevError, Impact: ImpactBreaks,
		Hint: "Пайплайн main-ветки упал, а смерженного MR, на который можно указать, нет (прямой пуш). Почините main или откатите коммит; бот написал комментарий к коммиту, если задан `review.ci.notify_main` или `review.problems.notify`."},
	{Kind: RepoConfigInvalid, Title: "Конфиг .reviewbot.yaml невалиден — ревью не выполняется", Action: "Исправьте .reviewbot.yaml в целевой ветке", Severity: SevError, Impact: ImpactBreaks,
		Hint: "Исправьте `.reviewbot.yaml` в целевой ветке (ошибки и строки — в заметке бота в MR и в описании проблемы); проверить локально: `reviewbot validate-repo-config .reviewbot.yaml`; справочник ключей — docs/configuration.md."},
	{Kind: ModelToolFailed, Title: "Модель: инструмент не сработал", Action: "Разберите ошибку инструмента в транскрипте", Severity: SevWarn, Impact: ImpactPartial,
		Hint:      "Инструмент чтения (Read/Grep/Glob) или инструмент бота (MCP) вернул ошибку: детали — в транскрипте запуска (/runs).",
		ModelName: "tool_failed"},
	{Kind: ModelMissingDependency, Title: "Модель: не хватает зависимостей", Action: "Опишите модели недостающие зависимости", Severity: SevWarn, Impact: ImpactPartial,
		Hint:      "Модель не запускает команды и видит только файлы репозитория: зависимостей (vendor, node_modules) и сгенерированного кода, которых нет в git, у неё нет. Опишите их в `instructions` `.reviewbot.yaml` или добавьте нужный репозиторий в `context.related`.",
		ModelName: "missing_dependency"},
	{Kind: ModelNoAccess, Title: "Модель: нет доступа к нужному", Action: "Откройте модели нужный репозиторий", Severity: SevWarn, Impact: ImpactPartial,
		Hint:      "Добавьте репозиторий в `context.related` или выдайте боту доступ к проекту.",
		ModelName: "no_access"},
	{Kind: ModelTooLarge, Title: "Модель: файл слишком большой", Action: "Исключите или разбейте большой файл", Severity: SevInfo, Impact: ImpactPartial,
		Hint:      "Исключите сгенерированные файлы через `ignore` или разбейте файл; модель не прочитала его целиком.",
		ModelName: "too_large"},
	{Kind: ModelConfigUnclear, Title: "Модель: непонятная конфигурация", Action: "Опишите модели устройство проекта", Severity: SevInfo, Impact: ImpactPartial,
		Hint:      "Опишите устройство проекта в `instructions` `.reviewbot.yaml` или командой `@bot память запомни`.",
		ModelName: "config_unclear"},
	{Kind: ModelOther, Title: "Модель: не удалось проверить", Action: "Разберите сообщение модели", Severity: SevInfo, Impact: ImpactPartial,
		Hint:      "Подробности — в описании проблемы и в транскрипте запуска (/runs).",
		ModelName: "other"},
}

var byKind = func() map[Kind]KindInfo {
	m := make(map[Kind]KindInfo, len(kinds))
	for _, k := range kinds {
		k.Steps = kindSteps[k.Kind]
		m[k.Kind] = k
	}
	return m
}()

// Kinds returns every kind in display order (Steps are shared: read only).
func Kinds() []KindInfo {
	out := make([]KindInfo, len(kinds))
	for i, k := range kinds {
		out[i] = byKind[k.Kind]
	}
	return out
}

// Info describes a kind (ok=false for an unknown one).
func Info(k Kind) (KindInfo, bool) {
	i, ok := byKind[k]
	return i, ok
}

// InfoOf is Info with a neutral fallback for unknown kinds (old rows).
func InfoOf(k Kind) KindInfo {
	if i, ok := byKind[k]; ok {
		return i
	}
	return KindInfo{Kind: k, Title: string(k), Action: string(k), Severity: SevInfo, Impact: ImpactPartial}
}

// ModelNames are the short kind names the model may use, in order.
func ModelNames() []string {
	var out []string
	for _, k := range kinds {
		if k.ModelName != "" {
			out = append(out, k.ModelName)
		}
	}
	return out
}

// ModelKind maps a model's short name ("tool_failed") or a full model kind
// ("model_tool_failed") to the kind.
func ModelKind(name string) (Kind, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, k := range kinds {
		if k.ModelName != "" && (name == k.ModelName || name == string(k.Kind)) {
			return k.Kind, true
		}
	}
	return "", false
}

// Problem is one thing the bot could not check.
type Problem struct {
	Kind   Kind
	Source Source
	// Subject is what could not be checked ("golangci-lint", a repository
	// path, "команды setup"); it is part of the fingerprint after
	// NormalizeSubject.
	Subject string
	// Detail is the last error text (scrubbed and capped when recorded).
	Detail      string
	ProjectPath string
	MRIID       int64
	RunID       string
	At          time.Time
}

// Fingerprint identifies the same problem across runs: kind + project +
// normalized subject.
func (p Problem) Fingerprint() string {
	return Fingerprint(p.Kind, p.ProjectPath, p.Subject)
}

// Fingerprint is the first 32 hex chars of sha256(kind \n project \n
// NormalizeSubject(subject)); the project path is compared lower-case.
func Fingerprint(kind Kind, project, subject string) string {
	s := sha256.Sum256([]byte(string(kind) + "\n" + strings.ToLower(strings.Trim(strings.TrimSpace(project), "/")) + "\n" + NormalizeSubject(subject)))
	return hex.EncodeToString(s[:16])
}

var (
	// tmpPathRe: paths of temporary directories (they differ every run).
	tmpPathRe = regexp.MustCompile(`(?:/private)?/(?:tmp|var/tmp|var/folders|run/user|dev/shm)(?:/[^\s"'` + "`" + `),;:]*)?`)
	// tmpNameRe: names of the bot's own temp dirs and files (".tmp-123",
	// "reviewbot-mcp-4711", "go-build123").
	tmpNameRe = regexp.MustCompile(`\.tmp-[^\s/"']*|\b(?:reviewbot|go-build|claude)-[a-z0-9-]*[0-9][a-z0-9-]*`)
	hexRe     = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	digitsRe  = regexp.MustCompile(`[0-9]+`)
)

// NormalizeSubject lower-cases s and strips what changes from run to run:
// temp directory paths, hex ids (commit SHAs, container ids) and numbers.
func NormalizeSubject(s string) string {
	s = strings.ToLower(s)
	s = tmpPathRe.ReplaceAllString(s, "<tmp>")
	s = tmpNameRe.ReplaceAllString(s, "<tmp>")
	s = hexRe.ReplaceAllStringFunc(s, func(m string) string {
		if strings.ContainsAny(m, "0123456789") {
			return "<hex>"
		}
		return m
	})
	s = digitsRe.ReplaceAllString(s, "n")
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// Caps.
const (
	MaxSubjectRunes      = 200 // bot-side subjects
	MaxModelSubjectRunes = 120 // model-reported subjects
	MaxDetailRunes       = 500
	// MaxModelPerRun caps the problems the model may report in one run
	// (MCP tool and JSON answer together).
	MaxModelPerRun = 5
)

var (
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->|<!--.*$`)
	htmlTagRe     = regexp.MustCompile(`</?[A-Za-z][^<>]*>`)
)

// CleanText makes untrusted text (the model's, a tool's) safe to store and
// show: HTML comments and tags removed, control characters dropped, one
// line, capped at n runes ("…" appended when cut).
func CleanText(s string, n int) string {
	s = htmlCommentRe.ReplaceAllString(s, " ")
	s = htmlTagRe.ReplaceAllString(s, " ")
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			return ' '
		case unicode.IsControl(r), r == 0x200b, r == 0x2028, r == 0x2029, r == 0xfeff:
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); n > 0 && len(r) > n {
		s = strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

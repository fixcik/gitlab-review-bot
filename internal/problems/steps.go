package problems

import (
	"cmp"
	"strings"
)

// Impact says what a problem does to the review (the order "сначала
// важное" on /problems).
type Impact string

const (
	// ImpactBreaks: the review is not done (or not published) or main is
	// broken.
	ImpactBreaks Impact = "breaks"
	// ImpactPartial: the review ran, but did not check everything.
	ImpactPartial Impact = "partial"
	// ImpactNone: the review itself is not affected (labels, reactions,
	// the advisor, the system map).
	ImpactNone Impact = "none"
)

// Impacts lists the impacts, most important first.
func Impacts() []Impact { return []Impact{ImpactBreaks, ImpactPartial, ImpactNone} }

// Label is the Russian name of the impact ("" for an unknown one).
func (i Impact) Label() string {
	switch i {
	case ImpactBreaks:
		return "ломает ревью или main"
	case ImpactPartial:
		return "ревью проверило не всё"
	case ImpactNone:
		return "не влияет на ревью"
	}
	return ""
}

// Rank orders impacts: 0 is the most important; unknown ones sort last.
func (i Impact) Rank() int {
	switch i {
	case ImpactBreaks:
		return 0
	case ImpactPartial:
		return 1
	case ImpactNone:
		return 2
	}
	return 3
}

// Valid reports a known impact.
func (i Impact) Valid() bool { return i.Rank() < 3 }

// Step is one step of the instructions "как исправить". Text may hold
// `code` spans (backticks). Or marks an alternative to the previous step
// (shown as "или" before it). Snippet is an optional fragment to copy.
type Step struct {
	Text    string
	Or      bool
	Snippet *Snippet
}

// Snippet is a ready fragment of a file (or a command). Example marks
// values that must be adapted (job names, paths, logins). When Placeholder
// is set and the problem's subject is a safe project path, the page puts
// the subject in its place (and the snippet is no longer an example).
type Snippet struct {
	// File is where it goes: ".reviewbot.yaml", "config.yaml",
	// ".gitlab-ci.yml", "терминал", a spec file.
	File        string
	Where       string
	Code        string
	Example     bool
	Placeholder string
}

// Snippet files the page and the tests know.
const (
	FileRepoConfig    = ".reviewbot.yaml"
	FileServiceConfig = "config.yaml"
	FileCI            = ".gitlab-ci.yml"
	FileShell         = "терминал"
)

const (
	whereRepo    = "корень репозитория, целевая ветка MR"
	whereService = "конфиг бота"
)

func repoSnip(code string, example bool) *Snippet {
	return &Snippet{File: FileRepoConfig, Where: whereRepo, Code: code, Example: example}
}

func serviceSnip(code string, example bool) *Snippet {
	return &Snippet{File: FileServiceConfig, Where: whereService, Code: code, Example: example}
}

// kindSteps: the instructions per kind (kinds without an entry show their
// Hint). Every .reviewbot.yaml and config.yaml snippet is checked against
// internal/config by TestStepSnippetsAreValidConfig.
var kindSteps = map[Kind][]Step{
	LintFailed: {
		{Text: "Причина — в теме проблемы, в саммари MR и в записи запуска (/runs)."},
		{Text: "gitleaks: проверьте, что он есть в образе песочницы (`Dockerfile.sandbox`)."},
		{Text: "Линтер из CI (`ci:<tool>`): job должна сохранять отчёт и при падении —",
			Snippet: &Snippet{File: FileCI, Where: "пайплайн репозитория", Example: true,
				Code: "lint:\n  script: make lint-report   # пишет gl-code-quality.json\n  artifacts:\n    when: always\n    paths: [gl-code-quality.json]"}},
		{Text: "…а `lint.ci` должен называть эту job и путь к отчёту:",
			Snippet: repoSnip("lint:\n  ci:\n    - job: lint\n      artifact: gl-code-quality.json", true)},
	},
	BaseUnavailable: {
		{Text: "Причина — в теме проблемы и в логах бота: репозиторий больше лимита или выгрузка снимка упала."},
		{Text: "Если репозиторий больше лимита — поднимите его:",
			Snippet: serviceSnip("review:\n  context:\n    max_repo_mb: 1000   # по умолчанию 500", true)},
		{Text: "Пока снимка нет, модель сравнивает изменения только по диффу."},
	},
	CompatFailed: {
		{Text: "Вывод инструмента (oasdiff, buf, graphql-inspector) — в записи запуска (/runs)."},
		{Text: "Спецификация ссылается на внешний `$ref` по URL или абсолютному пути — положите схему в репозиторий и сошлитесь относительным путём:",
			Snippet: &Snippet{File: "спецификация API", Where: "в репозитории", Example: true, Code: "$ref: \"./common/money.yaml\""}},
		{Text: "Инструмента нет или он падает в песочнице — проверьте образ (`Dockerfile.sandbox`)."},
	},
	CoverageMissing: {
		{Text: "Укажите джобу и путь к отчёту (Cobertura, Clover, Go cover profile или LCOV):",
			Snippet: repoSnip("coverage:\n  job: tests\n  path: build/coverage.xml", true)},
		{Text: "Джоба должна сохранять отчёт как артефакт:",
			Snippet: &Snippet{File: FileCI, Where: "пайплайн репозитория", Example: true,
				Code: "tests:\n  artifacts:\n    when: always\n    paths: [build/coverage.xml]"}},
		{Text: "Отчёт должен включать файлы, которые меняет MR."},
	},
	ContextUnavailable: {
		{Text: "Проверьте, что путь в `context.related` совпадает с полным путём проекта в GitLab:",
			Snippet: &Snippet{File: FileRepoConfig, Where: whereRepo, Example: true, Placeholder: "group/project",
				Code: "context:\n  related:\n    - repo: group/project"}},
		{Text: "Дайте аккаунту бота роль Reporter в этом проекте."},
		{Text: "Проект больше `review.context.max_repo_mb` (по умолчанию 500 МБ) — поднимите лимит в конфиге бота."},
	},
	JiraFailed: {
		{Text: "Проверьте ключ задачи в MR: задача с таким ключом должна существовать."},
		{Text: "Проверьте токен `JIRA_TOKEN` в окружении бота и права его пользователя на проект Jira."},
	},
	DeepReviewUnavailable: {
		{Text: "Ошибка вызова сабагента — в теме проблемы. Разовая (перегрузка, лимит) пройдёт сама; если повторяется, укажите другую модель или выключите его пустой строкой:",
			Snippet: serviceSnip("claude:\n  deep_review_model: \"\"", false)},
		{Text: "Ревью при этом идёт, только без второго мнения."},
	},
	DescriptionSummaryFailed: {
		{Text: "Бот пишет саммари в описание MR от своего токена, а при 403 — от служебного. Дайте право менять описание (роль Developer) или задайте служебный токен `GITLAB_SERVICE_TOKEN`."},
		{Text: "Или выключите саммари в описании:", Snippet: serviceSnip("review:\n  description_summary: false", false)},
	},
	AdvisorUnavailable: {
		{Text: "Аккаунт или модель не поддерживают советника: укажите другую модель или выключите его пустой строкой:",
			Snippet: serviceSnip("claude:\n  advisor_model: \"\"", false)},
		{Text: "Ревью при этом идёт, только без советника."},
	},
	SysmapFailed: {
		{Text: "Причина — в теме проблемы; что уже есть на карте — на /map."},
		{Text: "Бюджет извлечения исчерпан — поднимите часовые лимиты:",
			Snippet: serviceSnip("review:\n  sysmap:\n    max_runs_per_hour: 40        # по умолчанию 20\n    deps_max_runs_per_hour: 120  # по умолчанию 60", true)},
	},
	DiffTruncated: {
		{Text: "Разбейте MR на части поменьше — так модель увидит все изменения."},
		{Or: true, Text: "Поднимите лимит размера диффа для всех репозиториев:",
			Snippet: serviceSnip("review:\n  max_diff_bytes: 800000   # по умолчанию 400000", true)},
		{Text: "Упёрлись в число файлов — поднимите его для этого репозитория:",
			Snippet: repoSnip("max_files: 400   # по умолчанию 200", true)},
	},
	InvalidOutput: {
		{Text: "Откройте запись запуска (/runs): в транскрипте видно, что модель ответила вместо JSON."},
		{Text: "Повторите ревью командой `@<бот> review` в MR."},
		{Text: "Повторяется — проверьте версию Claude Code в образе песочницы."},
	},
	ClaudeAuthFailed: {
		{Text: "Выпустите новый токен подписки (или новый ключ `ANTHROPIC_API_KEY` в консоли Anthropic):",
			Snippet: &Snippet{File: FileShell, Where: "на своей машине", Code: "claude setup-token"}},
		{Text: "Запишите его в окружение бота (`CLAUDE_CODE_OAUTH_TOKEN` или `ANTHROPIC_API_KEY`) и перезапустите бота."},
		{Text: "Пока токен не заменён, ревью и ответы не работают."},
	},
	EvidenceFailed: {
		{Text: "Детали — в логах бота и в записи запуска (/runs)."},
		{Text: "Пока проверка падает, блокирующие замечания на миграциях понижаются. Если анализ миграций не нужен, его можно выключить:",
			Snippet: serviceSnip("review:\n  migrations:\n    enabled: false", false)},
	},
	MCPFailed: {
		{Text: "Проверьте адрес MCP, как его видит Claude CLI в песочнице (`url` пусто — вычисляется из `sandbox.egress_proxy` и порта `listen`):",
			Snippet: serviceSnip("review:\n  mcp:\n    listen: \":8091\"\n    url: \"\"", false)},
		{Text: "Проверьте, что egress-контейнер пробрасывает порт `listen`."},
		{Text: "Бот уже повторил ревью без MCP: ревью прошло, но без инструментов бота."},
	},
	ReactionsFailed: {
		{Text: "Добавьте аккаунт бота (`GITLAB_TOKEN`, scope `api`) в проект с ролью Reporter или выше и проверьте, что он не заблокирован."},
		{Or: true, Text: "Выключите реакции в этом репозитории:",
			Snippet: repoSnip("reactions:\n  enabled: false", false)},
		{Text: "…или во всём сервисе:",
			Snippet: serviceSnip("review:\n  reactions:\n    enabled: false", false)},
	},
	LabelsFailed: {
		{Text: "Нет прав: меткам MR нужна роль Developer — задайте `GITLAB_SERVICE_TOKEN` (Maintainer) или дайте боту Developer."},
		{Text: "Метки нет в проекте и его группах — создайте её в GitLab."},
		{Or: true, Text: "Разрешите боту ставить отсутствующие метки (GitLab создаст их сам) в этом репозитории:",
			Snippet: repoSnip("labels:\n  create_missing: true", false)},
		{Text: "…или во всём сервисе:",
			Snippet: serviceSnip("review:\n  labels:\n    create_missing: true", false)},
	},
	VerifyFailed: {
		{Text: "Причина — в теме проблемы, транскрипт — в записи запуска (/runs)."},
		{Text: "Упёрлись в общий лимит запусков Claude — поднимите его:",
			Snippet: serviceSnip("limits:\n  global_runs_per_hour: 30   # по умолчанию 15", true)},
		{Or: true, Text: "Перепроверка не нужна — выключите её:",
			Snippet: serviceSnip("review:\n  verify:\n    enabled: false", false)},
	},
	CIFetchFailed: {
		{Text: "Причина — в теме и описании проблемы."},
		{Text: "403: у бота (Reporter) нет доступа к логам job — задайте `GITLAB_SERVICE_TOKEN` (бот повторит запрос им) или откройте пайплайны участникам проекта."},
	},
	MainPipelineFailed: {
		{Text: "Откройте упавший пайплайн (ссылка — в описании проблемы) и посмотрите, какие job упали."},
		{Text: "Почините main или откатите прямой пуш:",
			Snippet: &Snippet{File: FileShell, Where: "в клоне репозитория", Example: true, Code: "git revert <коммит>"}},
		{Text: "Чтобы в следующий раз бот позвал людей, задайте `review.ci.notify_main`:",
			Snippet: serviceSnip("review:\n  ci:\n    notify_main: [alice]", true)},
	},
	RepoConfigInvalid: {
		{Text: "Ошибки с номерами строк — в описании проблемы и в заметке бота в MR. Исправьте `.reviewbot.yaml` в целевой ветке."},
		{Text: "Проверьте файл локально перед пушем:",
			Snippet: &Snippet{File: FileShell, Where: "в корне репозитория", Code: "reviewbot validate-repo-config .reviewbot.yaml"}},
		{Text: "Справочник ключей — docs/configuration.md."},
	},
	ModelToolFailed: {
		{Text: "Откройте транскрипт запуска (/runs): какой инструмент (Read/Grep/Glob или MCP бота) вернул ошибку."},
		{Text: "Упал инструмент MCP — проверьте и проблему «Инструменты MCP недоступны»."},
	},
	ModelMissingDependency: {
		{Text: "Модель не запускает команды и видит только файлы из git: vendor, node_modules и сгенерированного кода у неё нет. Опишите, откуда они берутся:",
			Snippet: repoSnip("instructions: |\n  Код в internal/gen генерируется из api/*.proto (make gen) и не хранится в git.", true)},
		{Or: true, Text: "Добавьте репозиторий с этим кодом в контекст:",
			Snippet: repoSnip("context:\n  related:\n    - repo: group/shared-lib", true)},
	},
	ModelNoAccess: {
		{Text: "Добавьте нужный репозиторий в контекст:",
			Snippet: repoSnip("context:\n  related:\n    - repo: group/project", true)},
		{Text: "Дайте аккаунту бота роль Reporter в этом проекте."},
	},
	ModelTooLarge: {
		{Text: "Исключите сгенерированные файлы из ревью:",
			Snippet: repoSnip("ignore:\n  - \"**/*.pb.go\"\n  - \"**/generated/**\"", true)},
		{Or: true, Text: "Разбейте файл: модель не прочитала его целиком."},
	},
	ModelConfigUnclear: {
		{Text: "Опишите, как устроен проект (сборка, тесты, линтеры):",
			Snippet: repoSnip("instructions: |\n  Сборка — make build, тесты — make test, линтер — make lint.", true)},
		{Or: true, Text: "Или запомните это командой в MR: `@<бот> память запомни <раздел>: <текст>`."},
	},
}

// stateRank: active first, then hidden, then fixed.
func stateRank(r Record) int {
	switch {
	case !r.ResolvedAt.IsZero():
		return 2
	case !r.AckedAt.IsZero():
		return 1
	}
	return 0
}

var sevRank = map[Severity]int{SevError: 0, SevWarn: 1, SevInfo: 2}

// ComparePriority orders problems "сначала важное": active before hidden
// before fixed; then by impact (breaks main or the review → the review
// checked not everything → no impact), severity, repeat count (more first),
// recency (newer first) and fingerprint (stable).
func ComparePriority(a, b Record) int {
	ia, ib := InfoOf(a.Kind), InfoOf(b.Kind)
	if c := cmp.Compare(stateRank(a), stateRank(b)); c != 0 {
		return c
	}
	if c := cmp.Compare(ia.Impact.Rank(), ib.Impact.Rank()); c != 0 {
		return c
	}
	if c := cmp.Compare(sevRank[ia.Severity], sevRank[ib.Severity]); c != 0 {
		return c
	}
	if c := cmp.Compare(b.Count, a.Count); c != 0 {
		return c
	}
	if c := b.LastSeen.Compare(a.LastSeen); c != 0 {
		return c
	}
	return strings.Compare(a.Fingerprint, b.Fingerprint)
}

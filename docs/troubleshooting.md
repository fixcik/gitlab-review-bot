# Решение проблем

## Где смотреть

0. **`reviewbot doctor -config config.yaml`** или страница **`/setup`**: токены, роли
   в группах, режим событий, Docker, образ и сеть песочницы, egress, токен Claude — с
   причиной и действием для каждой проблемы ([deploy.md](deploy.md#проверка-reviewbot-doctor-и-setup)).
   `serve` без токенов пишет «выполните `reviewbot init`» и выходит с кодом 2.
1. **`/runs`** ([usage-and-diagnostics.md](usage-and-diagnostics.md#runs--диагностика-запусков)):
   есть ли задача, её статус (`skipped` с причиной, `postponed` до какого времени,
   `failed` с ошибкой), таймлайн и транскрипт Claude.
2. **`reviewbot runs -config config.yaml -since 6h -status failed`** и
   **`reviewbot runs show <id>`** на машине бота.
3. **Логи бота** (JSON в stderr). Подробнее — с `LOG_LEVEL=debug`: тогда видны и
   отброшенные события вебхука (`webhook: event ignored … reason=…`).
4. **GitLab → проект → Settings → Webhooks → Edit → Recent events**: дошёл ли вебхук и
   что ответил бот (`accepted` или `ignored: <причина>`).
5. **`@reviewbot config`** в MR — действующие настройки и их источник.

## Бот не реагирует

| Симптом / причина | Как проверить | Решение |
|---|---|---|
| вебхука нет или он выключен | Recent events пусты | создайте вебхук ([setup-gitlab.md](setup-gitlab.md#вебхуки)) или `reviewbot sync-hooks -config config.yaml` |
| GitLab не может достучаться до бота | Recent events: ошибка соединения, «Requests to the local network are not allowed» | Admin → Settings → Network → *Allow requests to the local network from web hooks*; проверьте firewall и `GET /healthz` |
| неверный секрет | Recent events: `401 unauthorized` | секрет вебхука = `GITLAB_WEBHOOK_SECRET`; после смены секрета перезапустите бота (`sync_hooks` перезапишет токены) или выполните `sync-hooks` |
| проект вне `gitlab.groups` | ответ `ignored: project … is outside the configured groups` | добавьте группу в `gitlab.groups` |
| нет нужных событий | в Recent events нет Merge request / Note | включите Merge request events и Comments |
| неправильное упоминание | бот не отвечает на `@bot …` | упоминание — `@<username>` бот-аккаунта (в логе при старте `gitlab connected … mention=@…`); `review.bot_mention` игнорируется |
| у автора нет прав | ответ «Недостаточно прав…» | команды доступны с ролью Developer и выше |
| MR — черновик | `/runs`: `skipped`, «draft mr, skipping»; на команду ответ про Draft | снимите Draft: запустится полное ревью |
| авторевью на паузе | в саммари «⏸ Авторевью на паузе» | `@reviewbot resume` |
| маркер в описании | в описании MR `@reviewbot ignore`; лог «ignore marker in mr description» | уберите строку из описания |
| выключено в репозитории | ответ «Бот отключён для этого репозитория…»; лог «auto review disabled for repo» | `enabled`, `auto_review` в `.reviewbot.yaml` целевой ветки |
| репозиторий не подключён | ответ «Репозиторий не подключён к ревью: добавьте `.reviewbot.yaml` …»; лог «repository not opted in»; `reviewbot_skipped_total{reason="no_repo_config"}` | добавьте `.reviewbot.yaml` в целевую ветку MR ([setup-gitlab.md](setup-gitlab.md#подключение-репозитория)) или `review.require_repo_config: false`; после мержа файла — до 5 минут кэша |
| ошибка в `.reviewbot.yaml` | заметка «⚠️ Конфигурация бота невалидна — ревью не выполняется» со всеми ошибками; ответ «Не удалось разобрать `.reviewbot.yaml` в целевой ветке …»; `/problems`: `repo_config_invalid` | исправьте файл в целевой ветке (проверка: `reviewbot validate-repo-config`) |
| новых коммитов нет | лог «head already reviewed»; ответ «Новых коммитов с последнего ревью…» | `@reviewbot full review` |
| все файлы в ignore | ответ «В диапазоне … нет файлов для ревью…» | проверьте `ignore` и `review.default_ignore` |
| MR больше порога | в саммари «📦 MR слишком большой для ревью: N строк (порог M)»; `/runs`: `skipped`, «mr too large, claude review skipped» | разбейте MR; или `@reviewbot review force` / метка `big-mr-ok`; сгенерированные файлы — в `ignore` ([review.md](features/review.md#размер-mr)) |
| изменилось только описание MR | ответ вебхука `ignored: update without new commits` | так и задумано; `@reviewbot review` |
| GitLab не может достучаться до бота никак | бот за NAT, без внешнего адреса | `gitlab.events: poll` ([setup-gitlab.md](setup-gitlab.md#без-вебхуков-опрос-api)) |
| режим опроса: бот молчит | лог `poller: poll failed`; `reviewbot_poll_ticks_total{result="error"}` растёт | проверьте доступ бот-аккаунта к группам `gitlab.groups` (роль Reporter) и сам GitLab; при первом запуске бот только запоминает состояние, ревью начнётся с нового события |
| бот не запустился | `invalid config: …`, `gitlab: cannot read bot user (check GITLAB_TOKEN)`, `claude: neither CLAUDE_CODE_OAUTH_TOKEN nor ANTHROPIC_API_KEY is set` в логе | исправьте конфиг и переменные окружения |

## «⏳ Ревью отложено»

| Текст | Причина | Решение |
|---|---|---|
| «⏳ Ревью отложено до 15:40: Глобальный лимит ревью исчерпан (15 в час)…» | `limits.global_runs_per_hour` | дождитесь — ревью запустится само; или поднимите лимит |
| «⏳ Ревью отложено до 15:40: достигнут лимит использования Claude» | лимит подписки или API Anthropic | дождитесь сброса; при частых случаях снизьте нагрузку (см. ниже) или возьмите тариф выше |
| «⛔ Лимит перезапусков исчерпан…», «⛔ Перезапуск этого MR возможен не чаще…» | пользовательские лимиты | подождите или добавьте пользователя в `limits.exempt_users` |

Отложенные авторевью сохраняются в базе и переживают перезапуск бота; отложенные
ручные команды и вопросы — нет (повторите команду). Подробнее —
[limits.md](features/limits.md).

## «❌ Ревью не удалось»

| Текст | Причина | Решение |
|---|---|---|
| «Claude вернул ответ в неверном формате» | ответ не соответствует схеме дважды подряд | `/runs` → транскрипт; обычно помогает повтор; проверьте версию Claude Code в образе |
| «внутренняя ошибка (подробности в логах бота)» | checkout, docker, таймаут, падение Claude | лог и `/runs` → шаг с ошибкой |

Частые причины внутренней ошибки:

| В логе / `/runs` | Причина | Решение |
|---|---|---|
| `checkout: … fetch …` 401/403 | токен бот-аккаунта истёк или нет доступа к проекту | обновите `GITLAB_TOKEN`, дайте роль Reporter |
| `checkout: …` ошибка TLS | внутренний CA не в системном хранилище | укажите `gitlab.ca_file` (действует на API и `git fetch`) или добавьте CA в систему/образ бота |
| `claude: docker run failed`, `Unable to find image` | нет образа песочницы | `docker compose pull` (приватный пакет — `docker login ghcr.io`, [deploy.md](deploy.md#образы)) или `docker compose build`; проверьте `reviewbot doctor` |
| `network reviewbot-sandbox not found` | не поднята сеть | `docker compose up -d egress` |
| `claude run aborted after 15m0s` | таймаут `claude.timeout` | увеличьте таймаут, уменьшите `max_diff_bytes`/`max_files` |
| в транскрипте Claude ошибки соединения, в логе egress `egress denied` / `egress dial failed` | хост не в allowlist или egress не запущен | `docker compose ps egress`, проверьте `sandbox.egress_allow` |
| `-v` монтирует пустой каталог (бот в compose) | `data_dir` не совпадает с путём на хосте | `data_dir` = `REVIEWBOT_DATA_DIR`, монтирование по одинаковому пути ([deploy.md](deploy.md#всё-в-контейнерах)) |

## Бот не стартует или не ревьюит: «removed: the review model no longer runs commands»

В конфиге остались удалённые ключи (`sandbox.setup_timeout`,
`sandbox.allow_unsandboxed_bash`, `review.default_tools` в `config.yaml`; `setup`,
`tools` в `.reviewbot.yaml`). Модель больше не запускает команд — удалите ключи
([configuration.md](configuration.md#удалённые-ключи)). Для `.reviewbot.yaml`
ошибку видно в заметке бота в MR (каждый ключ со своей строкой) и в ответе на
команды («Не удалось разобрать `.reviewbot.yaml` в целевой ветке …») — ключи убирают в
целевой ветке; проверить заранее — `reviewbot validate-repo-config`.

То же с текстом «compat.commands: removed: the bot no longer runs repository
commands» — свои проверки-команды совместимости удалены; перенесите их в CI и уберите
ключ ([compat.md](features/compat.md)).

## Модели отказано в чтении файла

В транскрипте (`/runs`) `Read`, `Grep` или `Glob` получает отказ по правилам
разрешений. Модель читает только клон (`/work`), `/base`, `/repos`, `/skills` (в
local-режиме — их пути на хосте); всё остальное запрещено намеренно
([security.md](security.md#область-чтения-internalclaudereadscopego)). Если отказ на
файле внутри этих каталогов: в local-режиме путь `data_dir` с пробелом, запятой или
символами `*?[]()` не превращается в правило — переименуйте каталог; если отказ на
`Grep`/`Glob` внутри разрешённых каталогов.

## «Код до MR (/base) недоступен модели»

Проблема `base_unavailable` в саммари и `/problems`, в логе `review: merge base
export failed, Claude gets no /base`. Ревью идёт, но модель сравнивает изменения
только по диффу.

| Причина | Решение |
|---|---|
| рабочее дерево репозитория больше `review.context.max_repo_mb` (500 МБ) | поднимите предел или вынесите крупные файлы из репозитория |
| ошибка `git archive` (диск, права на `<data_dir>/repos/work`) | детали — в логе бота |

## 403 при закрытии тредов

| Симптом | Причина | Решение |
|---|---|---|
| бот пишет «✅ Похоже, исправлено… _Тред можно закрыть._» вместо закрытия | нет служебного токена: Reporter не может закрывать треды | задайте `GITLAB_SERVICE_TOKEN` ([setup-gitlab.md](setup-gitlab.md#служебный-токен)) |
| «ℹ️ По моим данным это исправлено… _Не удалось снять тред автоматически…_» | служебный токен есть, но запрос не прошёл (истёк, нет роли в проекте) | проверьте срок токена и роль Maintainer/Owner в проекте |
| `@reviewbot resolve`: «У бота нет прав закрывать треды…» | нет служебного токена | то же |
| неблокирующие замечания не закрываются и мешают мержу | нет служебного токена | задайте токен или поднимите `review.block_min_severity` до `nit` |
| бот не в ревьюерах | нет служебного токена или `review.assign_reviewer: false` | — |
| `sync-hooks`: «пропущено (нет прав)» | у пользователя служебного токена нет роли Maintainer/Owner в проекте | выдайте роль пользователю служебного токена (не бот-аккаунту: ему достаточно Reporter) или создайте вебхук вручную |

## «Проверка недоступна» (gitleaks, линтеры из CI, совместимость)

| Текст в саммари | Причина | Решение |
|---|---|---|
| «⚠️ Проверка X недоступна: X не установлен в sandbox-образе.» | инструмента нет в образе (свой образ) | соберите образ из `Dockerfile.sandbox` |
| «… превышено время выполнения» | таймаут | `review.lint.timeout` / `review.compat_timeout` |
| «🔧 Линтеры из CI: ⏳ ждём job `X`» | job линтера ещё идёт | ничего: треды появятся после пайплайна (до 3 часов ожидания) |
| «⚠️ ci:X: job `X` упала, файла `…` нет» | линтер упал до записи отчёта или нет `artifacts: when: always` | см. [lint.md](features/lint.md#примеры-gitlab-ciyml) |
| «⚠️ ci:X: job `X` не найдена в пайплайне MR» | job переименована или не запускается по `rules` | поправьте `lint.ci[].job` |
| «… этот GitLab не отдаёт отчёт Code Quality через API» | GitLab старше 19.4 | добавьте файл отчёта в `artifacts:paths` или укажите `artifact:` |
| сервис не стартует: «review.lint.…: removed: linters now come from CI reports» | старые ключи линтеров | [переход](features/lint.md#переход-со-старых-настроек) |
| «⚠️ Проверка oasdiff не выполнена: … спецификация ссылается на внешний $ref» | сети нет, внешние `$ref` не скачиваются | держите спецификацию самодостаточной |
| «ℹ️ buf (`x.proto`): файл вне модулей buf (buf.yaml) — не проверен buf» | нет `buf.yaml` над файлом | добавьте `buf.yaml` в корень модуля |
| «ℹ️ Проверка X выполнена частично: …» | больше 500 файлов или 300 результатов | открытые треды этой проверки не закрываются автоматически |
| «ℹ️ `.reviewbot.yaml` отключает gitleaks, но сервис это не разрешает…» | `review.lint.allow_disable_secrets: false` | так задумано |

## Jira

| Текст в саммари | Причина | Решение |
|---|---|---|
| «⚠️ не удалось получить из Jira: нет доступа (проверьте токен Jira и права на проект)» | 401/403: токен неверный, истёк или у пользователя нет доступа к проекту Jira | новый PAT в `JIRA_TOKEN`; права на проект |
| «⚠️ не удалось получить из Jira: задача не найдена» | 404: задачи нет или ключ опечатан | исправьте ключ в заголовке |
| «⚠️ не удалось получить из Jira: ошибка запроса к Jira» | сеть, TLS, 5xx | доступ с хоста бота к `jira.url`; внутренний CA — `jira.ca_file` |
| раздела «📋 Соответствие задаче» нет | Jira выключена (`jira.url` пуст) или ключи не найдены | проверьте `jira.project_keys` сервиса и `.reviewbot.yaml` |
| подсказка про заголовок не обновилась | изменились и заголовок, и коммиты (тогда будет ревью), саммари ещё нет или `jira.title_check: false` | — |
| бот не стартует: `jira.token (or JIRA_TOKEN) is required when jira.url is set` | задан URL без токена | задайте `JIRA_TOKEN` |

## Другие репозитории не видны

| Причина | Как проверить | Решение |
|---|---|---|
| `gitlab.groups` пуст | лог «context repositories off: gitlab.groups is empty» | задайте группы |
| выключено в сервисе или репозитории | `@reviewbot config` → «контекст других репозиториев» | `review.context.enabled`, `context.enabled` |
| репозиторий крупнее `max_repo_mb` | лог «repository too large, skipped» | увеличьте `review.context.max_repo_mb` |
| больше `max_repos` | лог «max_repos reached, the rest are skipped» | увеличьте `review.context.max_repos` или исключите лишнее |
| исключён | `context.exclude` в `.reviewbot.yaml` | — |
| у бот-аккаунта нет доступа к проекту | проект не в списке | роль Reporter в группе |
| ошибка получения списка группы | лог «context repositories: list group projects» | права и доступность GitLab |
| снимки не успели обновиться | запуск ждёт не больше 3 минут | первый запуск после старта может пройти без части репозиториев |

## Большой расход токенов

Смотрите `/usage`: кто, какие репозитории и MR, какие типы запусков.

| Что видно | Что сделать |
|---|---|
| мало «Кэш, чтение» относительно «Вход» | кэш промптов работает лучше при повторных запусках в пределах нескольких минут; частые одиночные ревью кэш не используют |
| дорогие ревью больших MR | уменьшите `review.max_diff_bytes` и `max_files`, расширьте `ignore` (сгенерированный код, снапшоты, lock-файлы) |
| много шагов в транскрипте (`/runs`) | бот заново ищет, где что лежит: проверьте [память проекта](features/memory.md), опишите устройство проекта в `instructions` |
| много запусков «Ревью» на одном MR | увеличьте `review.debounce`, чтобы серия пушей давала одно ревью |
| много «Обучения» | уменьшите `review.learn.max_per_hour` |
| дорогая основная модель | `claude.model` подешевле; лёгкие задачи уже идут на `claude.light_model` |
| много контекстных репозиториев | уменьшите `review.context.max_repos`, исключите лишнее |
| много «Карты системы» | первое сканирование дорогое, дальше — только изменённые файлы; уменьшите `review.sysmap.max_runs_per_hour` или `max_files_per_repo`, исключите проекты (`review.sysmap.exclude`); `max_runs_per_hour: 0` — только OpenAPI |
| в целом | снизьте `limits.global_runs_per_hour` и пользовательские лимиты |

## Карта системы

| Симптом | Как проверить | Решение |
|---|---|---|
| `/map` отвечает 404 | лог при старте `system map off: it needs context repositories` | нужен непустой `gitlab.groups` и `review.context.enabled`; проверьте `review.sysmap.enabled` |
| репозиторий «частично» | `/map` → «Сканирование», колонка «Ждут» | файлы ждут бюджета `max_runs_per_hour`, дойдут в следующих проходах |
| репозиторий «ошибка» или в логе `sysmap: extraction run failed` | `/map`, лог | лимит Claude, таймаут или сбой песочницы: файлы извлекутся в следующем проходе |
| вызов не связан с сервисом | `/map` → «Неразрешённые вызовы»: причина и «База» | добавьте подсказку `review.sysmap.hints` (имя переменной или хост из «Базы» → проект) и перезапустите бота |
| в саммари нет блока «🔗 Затронуто» | репозиторий MR есть на `/map`? меняет ли MR строки обработчиков (по ветке по умолчанию)? есть ли у контракта потребители? | блок появляется только когда затронутым контрактам кто-то пользуется; ошибки карты ревью не ломают (лог `review: system map`) |
| модель пропускает или путает вызовы | «Контракты репозитория» на `/map` | сообщите разработчикам примеры: правится промпт в `internal/sysmap/extract.go` |

## Разбор упавших пайплайнов

| Симптом | Как проверить | Решение |
|---|---|---|
| пайплайн упал, блока «🔴 Пайплайн упал» нет | `/runs`, задача `пайплайн CI`; метрика `reviewbot_webhook_events_total{event="pipeline"}` | вебхук без Pipeline events — включите галочку или `sync_hooks` (подписывает сам); `review.ci.enabled`, `ci.enabled` в `.reviewbot.yaml`; MR — draft; пайплайн не от последнего коммита; упали только job из `ignore_jobs` |
| «лог job … боту недоступен (нет прав)», проблема `ci_fetch_failed` | `/problems?kind=ci_fetch_failed` | у учётной записи бота нет доступа к логам (закрытые пайплайны проекта, роль): дайте ей доступ или разрешите видимость CI (Settings → CI/CD → General pipelines). Служебный токен не поможет: CI бот читает только своим токеном |
| в блоке нет упавших тестов, только лог | есть ли в пайплайне вкладка Tests | job должна публиковать `artifacts:reports:junit` (pytest `--junitxml`, go-junit-report, …) |
| «Объяснение не построено: исчерпан общий лимит…» | `/usage`, «Разбор CI» | `limits.global_runs_per_hour`; или `review.ci.explain: false` |
| main красный, заметки в MR нет | `/problems?kind=main_pipeline_failed`, лог `ci main` | прямой пуш без MR (тогда — комментарий к коммиту, если есть `review.ci.notify_main` или `review.problems.notify`); main уже был красным (одна заметка на инцидент); `review.ci.main_branch: false` |

## Прочее

| Симптом | Причина |
|---|---|
| замечания в саммари «вне диффа», а не на строках | строка не видна в диффе MR или MR изменился во время ревью (лог «mr changed during review») |
| `suggestion` нет у замечания | тред сдвинут на соседнюю строку — suggestion заменил бы не те строки |
| бот повторно создаёт закрытый тред замечания | закрытый тред с тем же заголовком и файлом не считается открытым; если замечание неверно — ответьте в треде почему, бот выучит правило |
| треды совместимости больше не появляются | человек закрыл тред вручную — изменение принято ([compat.md](features/compat.md#ручное-закрытие)) |
| `database schema version N is newer than supported` | откат бинарника на старую версию после миграции: вернитесь на новую версию или восстановите бэкап |

# Развёртывание

Бот — один бинарник `reviewbot` (Go, cgo для SQLite) с подкомандами:

| Подкоманда | Что делает |
|---|---|
| `reviewbot serve -config config.yaml` | вебхук-сервис (по умолчанию, если подкоманда не указана) |
| `reviewbot egress -listen :3128 [-config config.yaml] [-mcp-listen :8091 -mcp-forward host:port]` | egress-прокси для песочницы; из конфига берёт только `sandbox.egress_allow`; с `-mcp-forward` ещё пробрасывает один порт на MCP-слушатель бота ([mcp.md](features/mcp.md#сеть)) |
| `reviewbot sync-hooks -config config.yaml [-dry-run]` | разовая синхронизация вебхуков, см. [setup-gitlab.md](setup-gitlab.md#автоматически) |
| `reviewbot runs …`, `reviewbot runs show <id>` | диагностика запусков, см. [usage-and-diagnostics.md](usage-and-diagnostics.md#reviewbot-runs) |
| `reviewbot init [-non-interactive]` | первая настройка: спрашивает, проверяет и пишет `config.yaml` и `.env`, см. [ниже](#быстрый-старт-reviewbot-init) |
| `reviewbot doctor [-config config.yaml] [-env .env] [-json]` | проверки настройки (GitLab, Docker, egress, Claude), см. [ниже](#проверка-reviewbot-doctor-и-setup) |

Схема: бот принимает вебхуки GitLab, держит очередь и SQLite, а для каждого запуска
Claude и для каждой проверки бота стартует отдельный контейнер из образа песочницы
через docker daemon хоста. Контейнеры с Claude выходят в интернет только через
egress-прокси. Подробно — в [security.md](security.md).

## Требования

- Linux-хост с Docker (контейнеры песочницы запускаются через `docker` CLI). Для
  пробы подойдёт и macOS с Docker Desktop (`DOCKER_GID` не нужен, каталог данных — в
  домашней папке).
- Доступ с хоста бота к GitLab (API и `git fetch` по HTTPS) и, если включено, к Jira.
- Доступ контейнера egress к `api.anthropic.com` и остальным хостам из
  `sandbox.egress_allow`.
- GitLab должен достучаться до `http(s)://<бот>:8080/webhook`.
- Для сборки бинарника без Docker: Go 1.27 (минимум 1.24.7), gcc (cgo), `git` в `PATH`.

## Образы

Оба образа публикуются в GitHub Container Registry для `linux/amd64` и `linux/arm64`
(workflow [`publish-images.yml`](../.github/workflows/publish-images.yml)):

| Образ | Из чего | Теги |
|---|---|---|
| `ghcr.io/fixcik/gitlab-review-bot` | `Dockerfile` (бот и egress-прокси) | `X.Y.Z`, `X.Y`, `latest` — по тегу `vX.Y.Z`; `edge`, `sha-<7>` — каждый push в `main` |
| `ghcr.io/fixcik/gitlab-review-bot-sandbox` | `Dockerfile.sandbox` | те же |

`docker-compose.yml` берёт их из `REVIEWBOT_IMAGE` и `REVIEWBOT_SANDBOX_IMAGE` (по
умолчанию `:latest`), `sandbox.image` в конфиге должен совпадать с
`REVIEWBOT_SANDBOX_IMAGE` (`reviewbot init` пишет оба). Для продакшена закрепите
версию: `REVIEWBOT_IMAGE=ghcr.io/fixcik/gitlab-review-bot:0.1.0` и так же для песочницы.

**Приватные пакеты.** Новый пакет в ghcr.io по умолчанию приватный, и
`docker compose pull` отвечает `unauthorized` (`reviewbot doctor` говорит то же). Два
варианта:

- войти в реестр: создайте на GitHub classic PAT со scope `read:packages`
  (Settings → Developer settings → Personal access tokens) и выполните
  `echo <PAT> | docker login ghcr.io -u <пользователь GitHub> --password-stdin`;
- сделать пакеты публичными (владелец репозитория): GitHub → профиль → Packages →
  `gitlab-review-bot` → Package settings → Danger Zone → Change visibility → Public;
  то же для `gitlab-review-bot-sandbox`.

Собрать локально вместо скачивания: `docker compose build` (оба образа, теги из тех же
переменных).

### `Dockerfile` — сервис (`reviewbot:latest`)

| Слой | Что внутри |
|---|---|
| сборка | `golang:1.27-bookworm`, `CGO_ENABLED=1`, `go build -trimpath` |
| docker CLI | из `docker:27-cli` (только клиент; daemon — хостовой) |
| runtime | `debian:bookworm-slim` + `ca-certificates`, `git`, `tini` |
| пользователь | uid 1000 (`reviewbot`), `HOME=/home/reviewbot`, порт 8080 |

Тот же образ запускает egress-прокси (`reviewbot egress`).

```bash
docker build -t reviewbot:latest .
```

### `Dockerfile.sandbox` — песочница (`reviewbot-sandbox:latest`)

База `node:22-bookworm-slim`. Контейнеры стартуют с произвольным `--user uid:gid`,
read-only корнем, tmpfs на `/tmp` и `$HOME`, клон MR — в `/work`, код до MR — в
`/base`, другие репозитории — в `/repos`, скиллы — в `/skills` (только чтение).

В образе нет ничего, что запускала бы модель: у Claude только `Read`, `Grep` и
`Glob` ([security.md](security.md#права-claude)). Остальное — проверки самого бота,
которые он запускает в exec-контейнере (`--network none`, без токенов).

| Компонент | Версия (ARG) | Зачем |
|---|---|---|
| Claude Code CLI | `CLAUDE_CODE_VERSION=latest` | ревью; **закрепите версию** для воспроизводимости |
| системное | `ca-certificates`, `git`, `ripgrep` | ripgrep — инструмент `Grep` у Claude (`USE_BUILTIN_RIPGREP=0`), git — Claude Code и gitleaks (коммиты MR) |
| oasdiff | `OASDIFF_VERSION=1.34.1` (проверка sha256 по `checksums.txt`) | совместимость и сводка изменений OpenAPI |
| oas2to3 | собирается из `tools/oas2to3` (стадия `oas2to3`, kin-openapi v0.133.0) | Swagger 2.0 → OpenAPI 3.0 перед oasdiff |
| buf | `BUF_VERSION=1.75.0` (npm) | совместимость proto |
| graphql-inspector | `GRAPHQL_INSPECTOR_VERSION=7.0.2`, `graphql@16.11.0` | совместимость GraphQL |
| gitleaks | `GITLEAKS_VERSION=8.30.0` (sha256) | секреты во всех коммитах MR — единственный сканер, который бот запускает сам |

oasdiff и gitleaks скачиваются и проверяются в отдельной стадии `fetch`: `curl` в
итоговый образ не попадает. Go нужен только стадии сборки `oas2to3`.

Чего в образе нет:

- языковых рантаймов и тулчейнов (Go, PHP, Python, `make`, `gcc`, `libc6-dev`),
  Composer, `jq`, `procps`, `less` — модель ничего не собирает и не запускает,
  бот не запускает код репозитория (`setup`, `tools` и `compat.commands` удалены);
- линтеров: бот читает их отчёты из CI репозитория (`lint.ci`, см.
  [lint.md](features/lint.md)); PHPStan, phpcs, ruff, semgrep (с правилами и venv
  `/opt/pytools`) и golangci-lint удалены;
- `bubblewrap` и `socat` (встроенная изоляция Bash в Claude Code — у модели нет Bash)
  и `diffcov` (покрытие модель получает в промпте или через MCP).

Размер: node + Claude Code + git/ripgrep + проверки совместимости и gitleaks —
ориентировочно 0,5–0,7 ГБ против ~2,5 ГБ с Go, Python/semgrep, PHP и gcc (оценка, не
замер: смотрите `docker image ls` после сборки).

В образе выключены автообновление, телеметрия и отчёты об ошибках Claude Code.
Переменных кэшей тулчейнов (`GOCACHE`, `GOPATH`, `npm_config_cache`, …) нет: в
песочнице ничего не собирается и не устанавливается; временные файлы живут в tmpfs
(`/tmp`, `$HOME`) и пропадают вместе с контейнером.

```bash
docker build -f Dockerfile.sandbox -t reviewbot-sandbox:latest .
# с закреплённой версией Claude Code:
docker build -f Dockerfile.sandbox --build-arg CLAUDE_CODE_VERSION=2.1.310 -t reviewbot-sandbox:latest .
```

Языковые тулчейны не нужны: модель ничего не собирает и не запускает, бот запускает в
песочнице только gitleaks и проверки совместимости из таблицы выше. Свой образ
(`sandbox.image`) имеет смысл только для другой версии этих инструментов.

## Быстрый старт: `reviewbot init`

```bash
# в каталоге с docker-compose.yml (клон репозитория)
reviewbot init            # 6 шагов: GitLab, токен бота, служебный токен, Claude, события, песочница
docker compose up -d      # init предлагает выполнить pull и up сам
```

`init` спрашивает адрес GitLab и группы, токены (ввод скрыт, в историю оболочки не
попадают), способ входа в Claude, режим событий (вебхуки с догоняющим опросом или только
опрос API) и каталог данных. Каждый шаг сразу проверяется (как `reviewbot doctor`): роль
бота в группах, scope токенов, доступ к Docker, наличие образа песочницы. Затем:

- `config.yaml` — только несекретное (`gitlab.url`, `gitlab.groups`, `gitlab.events`,
  `gitlab.webhook_url`, `data_dir`, `sandbox.image`); новый файл — из шаблона со
  значениями [`deploy/config.compose.yaml`](../deploy/config.compose.yaml). В
  существующем остальные ключи и комментарии сохраняются, изменения показываются
  diff'ом (секретоподобные строки скрыты) и записываются только после «y»;
- `.env` — секреты и переменные compose (`GITLAB_TOKEN`, `GITLAB_SERVICE_TOKEN`,
  `CLAUDE_CODE_OAUTH_TOKEN` или `ANTHROPIC_API_KEY`, `GITLAB_WEBHOOK_SECRET` — генерируется,
  `REVIEWBOT_DATA_DIR`, `REVIEWBOT_SANDBOX_IMAGE`, на Linux `DOCKER_GID` — группа
  `/var/run/docker.sock`), права 600, чужие ключи сохраняются. В каталоге под git
  `init` пишет `.env`, только если он в `.gitignore`;
- предлагает `docker compose pull` (если реестр отказал — `docker compose build`) и
  `docker compose up -d`.

Повторный запуск берёт текущие значения как умолчания (Enter — оставить токен).
Секреты не печатаются нигде: только «принят» или причина отказа.

Без вопросов (CI, Ansible): значения из флагов и окружения, при нехватке — выход с кодом 2,
при проваленной проверке ничего не пишется (код 1), изменённый `config.yaml`
перезаписывается только с `-force`:

```bash
GITLAB_TOKEN=... CLAUDE_CODE_OAUTH_TOKEN=... reviewbot init -non-interactive \
  -gitlab-url https://gitlab.example.com -groups backend,platform \
  -webhook-url https://reviewbot.internal/webhook -data-dir /srv/reviewbot -up
```

Флаги: `-config`, `-env`, `-gitlab-url`, `-groups`, `-events webhook|poll|both`,
`-webhook-url`, `-data-dir`, `-sandbox-image`, `-force`, `-up`; `-repo-config` печатает
заготовку `.reviewbot.yaml`. Бот-аккаунт и токены `init` не создаёт
([setup-gitlab.md](setup-gitlab.md)).

Где взять бинарник для `init`: `go build ./cmd/reviewbot` (нужен gcc для cgo) или
образ бота — тогда `docker compose` выполните на хосте сами (в образе нет плагина
compose):

```bash
docker run --rm -it --user "$(id -u):$(id -g)" -e HOME=/tmp \
  --group-add "$(stat -c %g /var/run/docker.sock)" \
  -v "$PWD:/work" -w /work -v /var/run/docker.sock:/var/run/docker.sock \
  ghcr.io/fixcik/gitlab-review-bot:latest reviewbot init
```

`--user` нужен, чтобы `.env` (600) принадлежал вам и `docker compose` на хосте мог его
прочитать; `--group-add` — доступ к сокету docker для проверки образа песочницы;
`HOME=/tmp` — домашний каталог образа не доступен на запись другому uid.

`docker compose up -d` без `config.yaml` останавливается с ошибкой `bind source path does
not exist` (монтирование с `create_host_path: false`), а не создаёт каталог на месте
файла. Без токенов бот пишет «выполните `reviewbot init`» и выходит с кодом 2 (compose
будет его перезапускать — смотрите `docker compose logs reviewbot`).

## Проверка: `reviewbot doctor` и `/setup`

`reviewbot doctor -config config.yaml` повторяет проверки `init` на готовой
конфигурации и читает `.env` рядом (переменные окружения важнее): валидность конфига,
адрес и версия GitLab, токен бота (пользователь, scope `api`, срок), роль бота в каждой
группе (ровно Reporter: выше — предупреждение, ниже — ошибка), служебный токен
(Maintainer+), режим событий и секрет вебхука, доступ к Docker, образ и сеть песочницы,
allowlist и `CONNECT api.anthropic.com:443` через egress-прокси, формат токена Claude
(живой вызов платный и делается на первом ревью). Каждая проблема — с причиной и
действием. Код выхода 1, если есть ошибка; `-json` — тот же отчёт в JSON.

К GitLab — только `GET` (с внутренним CA из `gitlab.ca_file`, как у бота), к Docker — `docker version`, `docker image inspect`,
`docker manifest inspect`, `docker network inspect`. Egress-прокси виден только из
сети compose: с хоста проверка прокси — предупреждение, полная —
`docker compose exec reviewbot reviewbot doctor -config /etc/reviewbot/config.yaml`.

Страница `/setup` работающего бота показывает тот же отчёт (без проверки конфига —
бот уже стартовал с ним) и `/setup/data.json`; доступ — как у `/usage` (`USAGE_TOKEN`).
Только чтение: ни полей ввода, ни записи. Отчёт хранится минуту, «Обновить» — не чаще
раза в 15 секунд.

## Docker Compose

[`docker-compose.yml`](../docker-compose.yml) описывает:

| Объект | Назначение |
|---|---|
| сеть `reviewbot-sandbox` | `internal: true`, без маршрута наружу; имя фиксировано (без префикса проекта), бот передаёт его в `docker run --network` |
| сервис `egress` | `reviewbot egress -listen :3128 -config /etc/reviewbot/config.yaml -mcp-listen :8091 -mcp-forward ${REVIEWBOT_MCP_TARGET:-reviewbot:8091}` (из смонтированного конфига берёт только `sandbox.egress_allow`; проброс MCP нужен для `review.mcp.enabled`, включённого по умолчанию; без проброса ревью уходят в запасной путь без MCP), подключён к обеим сетям, алиас `egress` в сети песочницы, порты не публикуются, read-only, `cap_drop: ALL` |
| сервис `reviewbot` | сам бот в контейнере; запускается по `docker compose up -d` (профиля `full` больше нет) |
| сервис `sandbox-image` | только чтобы `docker compose pull` / `build` скачали или собрали образ песочницы; при `up` сразу завершается (`true`, без сети) |

Значения берутся из `.env` рядом с `docker-compose.yml` (образец —
[`.env.example`](../.env.example)): compose подставляет их в `${...}`, а контейнер бота
получает файл как `env_file`.

### Режим разработки: бот на хосте

```bash
docker compose build sandbox-image   # или docker compose pull sandbox-image
docker compose up -d egress
go run ./cmd/reviewbot serve -config config.yaml
```

Прокси читает `sandbox.egress_allow` из конфига, смонтированного в
`/etc/reviewbot/config.yaml`: по умолчанию это `./config.yaml` (его пишет
`reviewbot init`; комментированный образец —
[`deploy/config.compose.yaml`](../deploy/config.compose.yaml)), свой файл задаётся
переменной `REVIEWBOT_CONFIG` (например,
`REVIEWBOT_CONFIG=./deploy/config.compose.yaml docker compose up -d egress`). Секреты
прокси не нужны. Файл должен быть читаем для uid 1000. После
правки allowlist перезапустите прокси: `docker compose restart egress`.

### Всё в контейнерах

```bash
reviewbot init                       # или: cp .env.example .env && chmod 600 .env, правка руками
docker compose pull                  # или docker compose build
docker compose up -d
```

Раньше бот запускался профилем `full` (`docker compose --profile full up -d`); теперь
профиля нет, `docker compose up -d` поднимает бота, egress и сеть песочницы. Конфиг —
`./config.yaml` (или файл из `REVIEWBOT_CONFIG`), он монтируется в
`/etc/reviewbot/config.yaml` и боту, и прокси; должен существовать до `up`.

**Каталог данных монтируется по одному и тому же абсолютному пути внутри и снаружи.**
Бот создаёт рабочее дерево MR в `data_dir` и запускает `docker run -v <дерево>:/work:ro`. Путь в `-v`
разрешает docker daemon **хоста**, а не контейнер бота, поэтому путь, который видит
бот, должен существовать на хосте. Поэтому:

- `data_dir` в `config.yaml` должен совпадать с `REVIEWBOT_DATA_DIR`
  (по умолчанию `/var/lib/reviewbot`);
- каталог должен быть доступен на запись uid 1000.

Контейнеру бота нужен docker socket (`/var/run/docker.sock`) и группа docker
(`DOCKER_GID`). Доступ к сокету фактически равен root на хосте для процесса бота
(не для песочниц) — запускайте бота на выделенной VM.

Сервис `reviewbot` получает весь `.env` (`env_file`, необязательный), а из окружения
самого `docker compose` по имени (значений в файле нет; незаданные просто не
передаются): `GITLAB_TOKEN`, `GITLAB_WEBHOOK_SECRET`,
`GITLAB_SERVICE_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `JIRA_URL`,
`JIRA_TOKEN`, `USAGE_TOKEN`, `DIAG_TOKEN`, `METRICS_TOKEN`, `MAP_TOKEN`, `LOG_LEVEL`, а для трейсинга —
`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_SERVICE_NAME`
([features/tracing.md](features/tracing.md)). Устаревшее имя
`GITLAB_HOOKS_TOKEN` не прокидывается — используйте `GITLAB_SERVICE_TOKEN`.

Скиллы команды ([skills.md](features/skills.md)) лежат в `./deploy/skills` (или в папке
из `REVIEWBOT_SKILLS`) и монтируются в контейнер бота как `/etc/reviewbot/skills`
(`review.skills.dir` в конфиге). Песочница получает их копию
из `data_dir`, поэтому отдельный путь на хосте для них не нужен.

Если GitLab использует внутренний CA, смонтируйте PEM-файл в контейнер бота и укажите
его в `gitlab.ca_file`: он действует и на API, и на `git fetch` бота. Либо добавьте CA
в образ (`/usr/local/share/ca-certificates` + `update-ca-certificates`).

## Локальный режим (`sandbox.mode: local`)

```yaml
sandbox:
  mode: local
```

Claude и проверки бота запускаются прямо на хосте, `claude` должен быть в `PATH`.
Docker и egress не нужны. Удобно для быстрой проверки на своих репозиториях. Код до
MR модель видит по пути дерева на хосте (`--add-dir <data_dir>/repos/trees/<проект>@<sha>-base`,
без кэша — `<data_dir>/repos/work/<клон>-tree-…`), а не в `/base`.

Риски:

- **никакой изоляции**: ни сети, ни файловой системы, ни процессов. Модель команд
  не запускает, но `Read` с `--add-dir` работает с правами пользователя бота, а
  проверки бота (gitleaks, oasdiff, buf …) идут прямо на хосте;
- из окружения убираются только секреты GitLab/Jira (а у проверок — ещё и токены
  Claude); у проверок сеть не отключается;
- токен Claude передаётся через файловый дескриптор, а не в окружении; если заданы и
  `CLAUDE_CODE_OAUTH_TOKEN`, и `ANTHROPIC_API_KEY`, Claude получает только первый, второй
  из его окружения убирается;
- снимки (`/base`, другие репозитории) защищены от записи только правами 0444;
- при старте в лог пишется предупреждение `!!! sandbox.mode=local …`.

Не используйте local для чужих или недоверенных репозиториев.

## Каталог данных

```
<data_dir>/
├── reviewbot.db (+ -wal, -shm)        SQLite: состояние MR, лимиты, правила, память, учёт токенов, индекс диагностики
├── repos/
│   ├── mirrors/<группа_проект>.git    bare-зеркало каждого проекта (fetch из GitLab)
│   ├── trees/<проект>@<sha>/          кэш рабочих деревьев (review.checkout_cache): файлы head MR, без .git, только чтение;
│   ├── trees/<проект>@<sha>-base/     … и merge base для /base (без .claude/, .mcp.json); общие для задач, хранятся ttl
│   ├── work/<random>/                 каталог задачи: клон истории для gitleaks, файлы compat; удаляется после задачи,
│   │                                  остатки старше 24 ч — при старте (без кэша — одноразовый клон + снимок <random>-tree-…)
│   └── snapshots/<проект>@<sha>/      снимки веток по умолчанию других проектов, без .git (контекст)
├── scratch/                           пустые каталоги для запусков без инструментов
└── diag/<ГГГГММДД>/<id>/              run.json и claude-<n>.jsonl, 48 ч
```

### Размер диска

| Что | Оценка |
|---|---|
| `mirrors` | ≈ размер `.git` всех проектов, которые ревьюит бот или читает как контекст |
| `snapshots` | ≈ размер рабочих деревьев проектов групп (без `.git`), до `max_repos` × `max_repo_mb`; при обновлении старые и новые снимки какое-то время живут вместе |
| `trees` | кэш рабочих деревьев: не больше `review.checkout_cache.max_gb` (10 ГБ) плюс деревья, которые сейчас в работе (по 1–2 на одновременную задачу). Одно дерево — это файлы одного коммита без `.git` (примерно размер checkout проекта; merge base — не больше `review.context.max_repo_mb`); история и объекты общие с `mirrors`. Сколько их живёт: все head и merge base, которые ревьюились за последние `ttl` (2 ч) — при ~20 MR в день это обычно единицы–десятки деревьев. При рестарте каталог очищается |
| `work` | на каждую одновременную задачу (`limits.max_concurrent`): клон истории для gitleaks (объекты — жёсткие ссылки на зеркало, место почти не занимают) и спецификации compat; без кэша — клон с рабочим деревом и снимок merge base |
| `diag` | не больше `diag.max_total_mb` (2 ГБ по умолчанию) |
| `reviewbot.db` | десятки МБ; учёт токенов хранится `usage.retention` (365 дней) |

С включённым контекстом закладывайте **примерно 2× суммарного размера репозиториев
групп** плюс 2–3 ГБ на диагностику и `review.checkout_cache.max_gb` (10 ГБ) на кэш деревьев.

## Обновление и миграции

1. Соберите новые образы (`reviewbot` и `reviewbot-sandbox`).
2. Перезапустите бота (`docker compose up -d` / rollout). Бот останавливается
   мягко и ничего не теряет — см. [ниже](#перезапуск-без-потерь). Задачи, прерванные
   на ходу, в `/runs` получают статус `interrupted`, после старта выполняются снова.
3. Схема SQLite обновляется автоматически при старте: сначала последовательные
   миграции (`schema_version`), затем именованные (`schema_migrations`:
   `learnings_v1`, `mr_flags_v1`, `finding_outcomes_v1`, `project_memory_v1`,
   `diag_runs_v1`, `queue_pending_v1`, `queue_jobs_v1`, `queue_done_v1`), таблицы учёта токенов создаются при первом обращении. Ручных шагов
   нет. Откат на версию со старой схемой не поддерживается (`database schema version
   N is newer than supported`) — делайте бэкап перед обновлением.

Перед обновлением прочитайте изменения в ключах конфига: устаревший
`review.bot_mention` теперь игнорируется; `sandbox.setup_timeout`,
`sandbox.allow_unsandboxed_bash`, `review.default_tools` и ключи `setup` / `tools` /
`compat.commands` в `.reviewbot.yaml` **удалены** — с ними сервис не стартует, а ревью репозитория
отвечает ошибкой конфигурации, пока ключи не убраны из целевой ветки
([configuration.md](configuration.md#удалённые-ключи)). Пересоберите
`reviewbot-sandbox`: старый образ работает, но тащит лишнее (bubblewrap, Composer,
diffcov).

## Перезапуск без потерь

**Очередь в SQLite.** Каждая задача очереди сохраняется в `reviewbot.db` и после старта
выполняется снова:

- авторевью — одна запись на MR (`queue_pending`): debounce, отсрочка по лимиту и
  «полное ревью» сохраняются как были;
- все остальные задачи — по записи на задачу (`queue_jobs`): `review`, `full review`,
  `review force`, `check`, вопросы боту, ответы в тредах бота, команды правил и памяти,
  `pause`/`resume`/`ignore`/`resolve`/`config`, `help`, реакции, обновления сводки и
  заголовка, разбор пайплайнов. Порядок задач одного MR сохраняется; новые события
  после старта встают за восстановленными.

Задачи не-авторевью, поставленные раньше `queue.manual_max_age` (по умолчанию 6 ч),
после старта отбрасываются (строка в логе, `reviewbot_queue_restored_total{result="expired"}`).

**Прерванная задача.** Если задача выполнялась в момент остановки, после старта она:

| Задача | После перезапуска |
|---|---|
| авторевью | выполняется заново (ревью идемпотентно: замечания сверяются с открытыми тредами по маркерам, сводка обновляется по маркеру, черновики прерванного запуска удаляются) |
| `review`, `full review`, `review force`, `check` | заново, если итоговый ответ команды ещё не опубликован; если ответ (✅/❌/⛔ …) уже есть — пропускается. Промежуточные заметки (ответ «исправлено» в треде замечания, даже если команда написана в этом треде) ответом не считаются |
| вопрос боту | заново, если ответа в треде ещё нет; иначе пропускается (ответ не дублируется) |
| `help`, `config`, `ignore`, `pause`, `resume`, `resolve`, правила, память | так же: заново, пока в треде нет ответа бота (флаги паузы, правила и память при повторе не дублируются — повтор ответит «уже на паузе» / «уже знаю») |
| ответ в треде бота (обучение) | заново, если правило ещё не подтверждено ответом (заметка помечается обработанной до запуска Claude, повтор её не учит дважды) |
| реакция (`@bot спасибо`) | заново (реакция ставится идемпотентно) |
| обновление сводки и заголовка, разбор пайплайна | заново (перечитывают MR и обновляют свои блоки по маркерам) |

Задача, отложенная лимитом Claude («⏳ … отвечу после …»), ждёт своего времени и после
перезапуска (не дольше суток; следующие команды того же MR тем временем выполняются).
Отброшенная при старте задача (ответ уже был или она старше `queue.manual_max_age`)
теряет 👀/⏳ бота. Выполненные задачи помнятся сутки (`queue_done`): при
`events: both` опрос после старта не ставит их комментарии в очередь повторно. Если запуск Claude прерванной команды уже был учтён в лимитах, повтор
не проверяется пользовательскими лимитами и паузой между перезапусками MR и не
учитывается пользователю второй раз (общий часовой лимит считает каждый запуск).

**Мягкая остановка.** По SIGTERM/SIGINT бот перестаёт запускать новые задачи и даёт
выполняющимся `queue.shutdown_grace` (по умолчанию 5 минут) закончить — начатое ревью
не выбрасывается вместе с потраченными токенами. Вебхуки (и опрос при `poll`/`both`)
всё это время принимаются и сохраняются в очередь. Когда время вышло, оставшиеся задачи
прерываются и повторяются после старта; второй сигнал (второй Ctrl-C) прерывает сразу.
`queue.shutdown_grace: 0` — прежнее поведение: прерывать сразу.

**Таймаут остановки контейнера должен быть больше `queue.shutdown_grace`** (плюс ~15 с
на остановку HTTP и опроса), иначе процесс убьют посреди ожидания:

- Docker Compose: `stop_grace_period` у сервиса бота — в
  [`docker-compose.yml`](../docker-compose.yml) стоит `6m` (у Docker по умолчанию 10 с);
  `docker stop -t 360` для голого Docker.
- Kubernetes:

  ```yaml
  spec:
    terminationGracePeriodSeconds: 360   # > queue.shutdown_grace (5m)
    containers:
      - name: reviewbot
        args: ["serve", "-config", "/etc/reviewbot/config.yaml"]
  ```

  Бот — один экземпляр (SQLite): при rollout используйте `strategy: Recreate`, чтобы
  новый под не стартовал на той же базе, пока старый дорабатывает.
- systemd: `TimeoutStopSec=6min`.

**События во время простоя.** GitLab не повторяет неудачную доставку вебхука: при
`gitlab.events: webhook` всё, что пришло, пока бот перезапускался или лежал, теряется
(бот предупреждает об этом в логе при старте). В продакшене используйте
`gitlab.events: both` (нужен `gitlab.groups`): опрос после старта догоняет пропущенное
от курсоров в базе, а события, уже сохранённые в очереди, повторно не ставятся.
`config.yaml` от `reviewbot init` (по умолчанию) и
[`deploy/config.compose.yaml`](../deploy/config.compose.yaml) так и настроены.

## Бэкапы

Ценное — только `reviewbot.db`: правила команды, память проектов, исходы замечаний,
состояние MR, учёт токенов. `repos/` восстанавливается сам, `diag/` живёт 48 часов.

```bash
sqlite3 /var/lib/reviewbot/reviewbot.db ".backup '/backup/reviewbot-$(date +%F).db'"
```

`.backup` безопасен на работающем боте (WAL). Копировать файл `cp` без `-wal` нельзя.
Если состояние MR потеряно, следующее ревью каждого MR будет полным.

## Мониторинг

Бот отдаёт метрики Prometheus на `/metrics` и пробу готовности `/readyz` на
порту 8080. Готовые файлы: scrape config
[`deploy/prometheus/scrape.yml`](../deploy/prometheus/scrape.yml), правила
алертов [`deploy/prometheus/reviewbot-alerts.yml`](../deploy/prometheus/reviewbot-alerts.yml),
дашборд Grafana [`deploy/grafana/reviewbot-dashboard.json`](../deploy/grafana/reviewbot-dashboard.json).
Как подключить и что показывает каждый ряд дашборда —
[metrics.md](metrics.md#дашборд-и-алерты). Если задан `METRICS_TOKEN`, тот же
токен нужен Prometheus (`authorization` в scrape config).

## Переменные окружения

| Переменная | Кто читает | Назначение |
|---|---|---|
| `GITLAB_URL` | бот | `gitlab.url` |
| `GITLAB_TOKEN` | бот | токен бот-аккаунта (обязателен) |
| `GITLAB_WEBHOOK_SECRET` | бот | секрет вебхука (обязателен при `gitlab.events: webhook` и `both`) |
| `GITLAB_SERVICE_TOKEN` | бот | служебный токен |
| `GITLAB_HOOKS_TOKEN` | бот | устаревшее имя служебного токена |
| `CLAUDE_CODE_OAUTH_TOKEN` | бот → песочница | токен подписки Claude (`claude setup-token`) |
| `ANTHROPIC_API_KEY` | бот → песочница | ключ API Anthropic (вместо подписки) |
| `JIRA_URL` | бот | `jira.url` |
| `JIRA_TOKEN` | бот | токен Jira |
| `USAGE_TOKEN` | бот | доступ к `/usage` |
| `DIAG_TOKEN` | бот | доступ к `/runs` |
| `MAP_TOKEN` | бот | доступ к `/map` (карта системы) |
| `LOG_LEVEL` | бот | `debug` включает подробные логи |
| `REVIEWBOT_DATA_DIR` | docker compose | путь каталога данных на хосте и в контейнере (по умолчанию `/var/lib/reviewbot`) |
| `DOCKER_GID` | docker compose | gid группы сокета docker на хосте (по умолчанию 999; `reviewbot init` на Linux подставляет сам) |
| `REVIEWBOT_CONFIG` | docker compose | путь конфига на хосте для бота и egress (по умолчанию `./config.yaml`) |
| `REVIEWBOT_IMAGE` | docker compose | образ бота и egress (по умолчанию `ghcr.io/fixcik/gitlab-review-bot:latest`) |
| `REVIEWBOT_SANDBOX_IMAGE` | docker compose | образ песочницы для `pull`/`build` (по умолчанию `ghcr.io/fixcik/gitlab-review-bot-sandbox:latest`; = `sandbox.image`) |
| `REVIEWBOT_SKILLS` | docker compose | папка со скиллами команды на хосте (по умолчанию `./deploy/skills`), монтируется в бота как `/etc/reviewbot/skills` |

Логи — JSON в stderr (у `sync-hooks` — текст).

Проверка живости: `GET /healthz` → `ok`.

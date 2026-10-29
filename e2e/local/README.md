# Локальный стенд: GitLab CE в Docker + тестовые проекты

Стенд для проверки бота без боевого GitLab:
GitLab CE и GitLab Runner в Docker, тестовые пользователи и токены, группы
`reviewbot-sandbox` / `reviewbot-sandbox-ext`, тестовые проекты с заготовленными
MR. Какой MR к какому сценарию — [SCENARIOS.md](SCENARIOS.md).

## Требования

- Docker с `docker compose` v2. **macOS (Docker Desktop):** выделить VM 8–12 ГБ RAM
  (Settings → Resources); сам GitLab занимает 4–6 ГБ, плюс CI-джобы и песочницы бота.
  Машина от 16 ГБ RAM. Диск: 20+ ГБ (образ GitLab ~3,5 ГБ, данные, образы CI и песочницы).
- `bash`, `curl`, `jq`, `git`, `openssl`, `iconv`, `awk` (на macOS: `brew install jq`,
  остальное есть). Go 1.24.7+ — для запуска бота через `go run`.
- Образ: официальный `gitlab/gitlab-ce` мультиархитектурный (amd64 и arm64), на Apple
  Silicon работает нативно. По умолчанию `18.11.13-ce.0`; **лучше задать версию боевого
  GitLab**: `export GITLAB_VERSION=<версия>-ce.0`. `GITLAB_IMAGE` задаёт образ целиком
  (другая редакция или зеркало, например `gitlab/gitlab-ee:<версия>-ee.0`).
  `RUNNER_VERSION` — тег `gitlab/gitlab-runner` (по умолчанию `latest`).
- Строка в `/etc/hosts`, чтобы бот на хосте и браузер видели GitLab по тому же адресу,
  что CI и контейнеры:

  ```bash
  sudo sh -c 'echo "127.0.0.1 gitlab" >> /etc/hosts'
  ```

  Скрипты `up.sh` и `seed.sh` ходят на `http://localhost:8929` и без неё.

## Запуск

```bash
e2e/local/up.sh        # 5–10 минут при первом старте; повторный запуск безопасен
e2e/local/seed.sh      # группы уже есть (up.sh), создаёт проекты, ветки, MR (~5 минут)

# образы бота (один раз, нужен интернет)
docker build -t reviewbot:latest .
docker build -f Dockerfile.sandbox -t reviewbot-sandbox:latest .

# токен Claude — в своём .env (по умолчанию .env в корне репозитория, chmod 600):
#   CLAUDE_CODE_OAUTH_TOKEN=...   или   ANTHROPIC_API_KEY=...
e2e/local/run-bot.sh               # бот на хосте: egress + go run ... serve
e2e/local/run-bot.sh --container   # или бот в контейнере (профиль full) в сети reviewbot-local
```

- `up.sh`: поднимает `gitlab` и `gitlab-runner`, ждёт healthy, через
  `gitlab-rails runner` создаёт пользователей `reviewbot` (Reporter), `rb-dev` (Developer),
  `rb-guest` (Guest), группы, токены (scope `api`, 90 дней) и пароль root; отключает
  регистрацию и Auto DevOps, разрешает вебхуки на локальные адреса; регистрирует
  instance runner (docker executor, `alpine:3.20`, сеть `reviewbot-local`,
  `clone_url = http://gitlab:8929`). Секреты пишет только в `e2e/local/.env.local`
  (chmod 600, в `.gitignore`), в вывод — только имена переменных.
- `seed.sh`: каждый проект — `fixtures/<проект>/base/` как `main`, затем ветки из
  `fixtures/<проект>/mr/*/` (оверлей + `.mr.json`) и MR от `rb-dev`. Существующие проекты
  пропускаются; `--reset` пересоздаёт, `--only a,b` — выборочно, `--no-mrs` — только
  ветки, `--open <проект>:<ветка>` — открыть MR позже (для B2). `RB_BIG_MB` — размер
  `rb-big` (по умолчанию 24 МБ при `max_repo_mb: 10`).
- Бот: конфиг [config.yaml](config.yaml) (опрос, Jira выключена, скиллы из
  [skills/](skills/), карта системы, миграции по умолчанию). Страницы:
  `http://localhost:8080/runs`, `/usage`, `/map`, `/metrics`.
- MR, открытые до первого старта бота, только запоминаются (B1): ревью —
  комментарием `@reviewbot review` от `rb-dev` (см. SCENARIOS.md).

## Вход и инструменты

- GitLab: <http://gitlab:8929> (или <http://localhost:8929>), пользователь `root`, пароль —
  `GITLAB_ROOT_PASSWORD` в `e2e/local/.env.local` (смотреть локально, в чат не вставлять).
- Переменные в текущий шелл: `set -a; . e2e/local/.env.local; set +a`.
- `curl`: `curl -s -H "PRIVATE-TOKEN: $RB_DEV_TOKEN" http://localhost:8929/api/v4/projects?search=rb-`.
- `glab` по http: `glab config set -h gitlab:8929 api_protocol http`, затем
  `GITLAB_HOST=gitlab:8929 GITLAB_TOKEN=$RB_DEV_TOKEN glab mr list -R reviewbot-sandbox/rb-backend`
  (не проверено на стенде; `curl` работает всегда).

## Вебхуки (B8–B11)

`up.sh` уже разрешил исходящие запросы GitLab на локальные адреса. В `config.yaml`:
`gitlab.events: webhook` или `both` и `gitlab.webhook_url`:

- бот на хосте — `http://host.docker.internal:8080/webhook` (на Linux имя даёт
  `extra_hosts` в compose);
- бот в контейнере — `http://reviewbot:8080/webhook`.

Затем `go run ./cmd/reviewbot sync-hooks -config e2e/local/config.yaml` (с переменными из
`.env.local`) или `gitlab.sync_hooks: true`.

## CI и runner

Джобы (`rb-py`: pytest + Cobertura, `rb-php`: PHPUnit + Postgres + Clover) идут в сети
`reviewbot-local`. Образы джоб (`python:3.12-slim`, `php:8.3-cli`, `postgres:16-alpine`,
`alpine:3.20`) мультиархитектурные. Каждый пуш ветки `rb-py`/`rb-php` (и `seed.sh`)
запускает пайплайн: `rb-py` — 1–2 минуты, `rb-php` — 3–6 минут (apt, pcov, composer),
runner берёт до 8 джоб одновременно (`RUNNER_CONCURRENCY`, по умолчанию 8). Если в джобе `rb-php` не резолвится сервис
`postgres` (service-контейнеры и `network_mode` своей сети), перерегистрировать runner
с сетью на каждую джобу:

```bash
docker compose -f e2e/local/docker-compose.yml exec gitlab-runner gitlab-runner unregister --name reviewbot-local
RUNNER_NETWORKING=per-build e2e/local/up.sh
```

## Трейсинг (Tempo и Grafana)

Необязательный профиль compose `tracing` — приёмник трейсов бота
([docs/features/tracing.md](../../docs/features/tracing.md)):

```bash
docker compose -f e2e/local/docker-compose.yml --profile tracing up -d tempo grafana
# e2e/local/config.yaml: tracing.enabled: true (endpoint http://localhost:4318 — бот на хосте)
e2e/local/run-bot.sh
# бот в контейнере стенда ходит в Tempo по имени:
OTEL_EXPORTER_OTLP_ENDPOINT=http://tempo:4318 e2e/local/run-bot.sh --container
```

- Tempo (`grafana/tempo:3.0.5`, `TEMPO_VERSION`) — один процесс, OTLP на 4317/4318,
  блоки на томе `tempo-data`, API на `localhost:3200`. Конфиг — `tracing/tempo.yaml`.
- Grafana (`grafana/grafana:13.1.3`, `GRAFANA_VERSION`) — `http://localhost:3000`
  (`GRAFANA_PORT`), анонимный админ без логина, источник Tempo (uid `tempo`) из
  `tracing/grafana-datasources.yaml`. Ссылка «Трейс» на `/runs/<id>` открывает трассу в
  Explore (`tracing.ui_url` в `config.yaml`).
- Проверка приёмника без бота:
  `REVIEWBOT_OTLP_ENDPOINT=http://localhost:4318 REVIEWBOT_TEMPO_URL=http://localhost:3200 go test ./internal/tracing -run TestLiveTempo -v`.
- Профиль выключен — трейсинг в `config.yaml` тоже держите выключенным (иначе бот будет
  писать раз в минуту, что приёмник недоступен; на работу это не влияет).
- `down.sh` останавливает и эти контейнеры (`--purge` удаляет и том Tempo).

## Файлы

| Файл | Что это |
|---|---|
| `docker-compose.yml` | `gitlab` (порт 8929, урезанная память), `gitlab-runner`, сеть `reviewbot-local`, тома; профиль `tracing` — `tempo` и `grafana` |
| `tracing/` | конфиг Tempo и источник данных Grafana для профиля `tracing` |
| `up.sh`, `bootstrap.rb` | запуск и начальная настройка; лог бутстрапа — `.state/bootstrap.log` |
| `seed.sh`, `fixtures/` | тестовые проекты и MR; `fixtures/go.mod` исключает их из модуля бота |
| `check-fixtures.sh` | офлайн-проверка фикстур (gofmt, go vet, php -l, python, TS, YAML/JSON) и генерация таблицы SCENARIOS.md |
| `config.yaml`, `skills/` | конфиг бота и скиллы; проверяются тестом `TestLocalStandConfigs` |
| `run-bot.sh`, `compose.bot.yml` | запуск бота на хосте или в контейнере в сети стенда |
| `down.sh` | остановка; `--purge` — удалить тома, `.env.local`, `.state` |
| `lib.sh`, `askpass.sh` | общие функции; токены передаются curl/git не через argv |

## Что локально не проверяется

- Jira (раздел I, общие стори/эпики в U14–U15) — только на бою.
- Боевая версия и редакция GitLab, её особенности (если `GITLAB_VERSION` не совпадает).
- Реальная сеть: VPN, внутренний CA, прокси, доступность бота из боевого GitLab,
  вебхуки с боевого GitLab.
- Права и токены боевых аккаунтов (здесь root — служебный токен и Owner группы).

## Уборка

```bash
e2e/local/run-bot.sh --stop      # если бот запускался в контейнере
e2e/local/down.sh                # остановить, данные сохранятся
e2e/local/down.sh --purge        # удалить всё: тома GitLab, .env.local, .state
sudo sed -i '' '/[[:space:]]gitlab$/d' /etc/hosts   # macOS (на Linux: sed -i без '')
```

Токены существуют только внутри этого GitLab: `--purge` уничтожает их вместе с ним.

## Если что-то не так

- GitLab не становится healthy: `docker compose -f e2e/local/docker-compose.yml logs --tail 200 gitlab`;
  чаще всего мало памяти у VM Docker. Таймаут ожидания — `UP_TIMEOUT` (по умолчанию 1500 с).
- Бутстрап упал: `.state/bootstrap.log` (секретов в нём нет). Скрипт `bootstrap.rb`
  проверен только синтаксически: API внутренних сервисов GitLab меняется между версиями.
- Токены перестали работать: `NEW_TOKENS=1 e2e/local/up.sh`.
- `seed.sh --reset` ждёт удаления проекта: в GitLab 18 удаление отложенное, скрипт
  удаляет проект повторно с `permanently_remove`.

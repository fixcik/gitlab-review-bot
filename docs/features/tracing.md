# Трейсинг (OpenTelemetry)

Бот отдаёт трейсы OpenTelemetry по OTLP/HTTP (JSON): **одна трасса на задачу очереди**
(ревью, команда, вопрос, разбор пайплайна) с дочерними спанами на каждый вызов GitLab,
git, решение лимитов, анализатор, запуск Claude, MCP-вызов, перепроверку и публикацию.
Запросы вебхука и раунды опроса GitLab — отдельные короткие трассы; трасса задачи
ссылается на событие, которое её породило (span link).

Трейсы дополняют, а не заменяют [`/runs`](../usage-and-diagnostics.md): на `/runs` —
полный транскрипт, промпты и вывод за 48 часов; в трейсах — только тайминги, id, размеры
и статусы, зато в общем бэкенде (Tempo, Grafana Cloud) рядом с трейсами других сервисов,
с поиском по атрибутам и без срока в 48 часов. Трейсинг выключен по умолчанию
(`tracing.enabled: false`) и не зависит от `diag.enabled`: работает и с выключенной
диагностикой.

Реализация своя, без OpenTelemetry Go SDK (маленький трейсер и экспортёр в
`internal/tracing`), формат — стандартный OTLP, его принимают Tempo, OpenTelemetry
Collector, Grafana Alloy и Grafana Cloud. Проверено вживую: Tempo 3.0.5 и
OpenTelemetry Collector 0.163.2 принимают экспорт бота и отдают трассу обратно
(`TestLiveTempo`, см. [ниже](#проверка-приёмника)).

## Включение

```yaml
tracing:
  enabled: true
  endpoint: "http://tempo:4318"     # OTLP/HTTP; /v1/traces добавляется сам
  environment: "prod"               # deployment.environment
  ui_url: "https://grafana.example.com/explore?schemaVersion=1&orgId=1&panes=…{trace_id}…"
```

Все ключи — в [configuration.md](../configuration.md#tracing). Переменные окружения
OpenTelemetry переопределяют файл (но не включают трейсинг сами по себе):

| Переменная | Что задаёт |
|---|---|
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | `tracing.endpoint` (приоритет выше следующей) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `tracing.endpoint` |
| `OTEL_EXPORTER_OTLP_HEADERS` | `tracing.headers`, `k=v,k2=v2` (значения URL-кодированы), по ключу поверх файла |
| `OTEL_SERVICE_NAME` | `tracing.service_name` (по умолчанию `gitlab-review-bot`) |
| `OTEL_SDK_DISABLED=true` | выключает трейсинг |

В `docker-compose.yml` бота первые три уже прокидываются по имени.

### Куда отправлять

**Tempo напрямую.** `endpoint: "http://tempo:4318"`. В Tempo должен быть включён
OTLP-приёмник по HTTP (`distributor.receivers.otlp.protocols.http`, порт 4318).
Многотенантный Tempo: `headers: {X-Scope-OrgID: "<tenant>"}`.

**OpenTelemetry Collector / Grafana Alloy.** `endpoint: "http://otel-collector:4318"`;
в коллекторе — приёмник `otlp` с `protocols.http`, дальше любой экспорт (Tempo, Jaeger,
облако). Через коллектор удобно делать tail sampling (например, «всегда хранить трассы
с ошибками»), которого в самом боте нет.

**Grafana Cloud.** OTLP-шлюз стека и Basic-аутентификация (instance id и токен с правом
`traces:write`):

```yaml
tracing:
  enabled: true
  endpoint: "https://otlp-gateway-prod-eu-west-2.grafana.net/otlp"   # из карточки стека → OpenTelemetry
  headers:
    Authorization: "Basic ${GRAFANA_OTLP_AUTH}"   # base64("<instance id>:<token>")
```

Значение заголовка ссылается на переменную окружения `${NAME}` — так секрет не лежит в
файле. Неустановленная переменная — ошибка конфига при старте. Значения заголовков и
сами переменные: не пишутся в лог (там только имена заголовков), вычищаются из
диагностики и спанов, не попадают ни в один дочерний процесс (git, проверки, Claude).
Внутренний CA у приёмника — `tracing.ca_file`.

### Локальный стенд

На стенде (`e2e/local`) есть профиль compose `tracing`: Tempo и Grafana с готовым
источником данных Tempo ([e2e/local/README.md](../../e2e/local/README.md#трейсинг-tempo-и-grafana)).

## Что трассируется

```text
review.job  (consumer)  gitlab.project=group/app gitlab.mr.iid=7 job.kind=auto_review
│                       reviewbot.job.result=ok  vcs.head_sha=…  link → POST /webhook
├─ gitlab.get_mr
│  └─ GET /projects/{id}/merge_requests/{id}            (client) http.response.status_code=200
├─ gitlab.get_raw_file
│  └─ GET /projects/{id}/repository/files/{path}/raw    (client) 404
├─ git.checkout
├─ git.changed_files
├─ git.diff                                             reviewbot.diff_bytes=…
├─ limiter.reserve                                      reviewbot.limiter.allowed=true
├─ review.threads
│  └─ gitlab.list_discussions → GET …/discussions
├─ review.base
├─ review.compat
│  └─ exec oasdiff                                      process.exit.code=0
├─ review.lint
│  └─ exec gitleaks
├─ review.task · review.context · review.coverage · review.sysmap · review.sysmap.deps · review.migrations
├─ review.learn.rules
├─ claude review  (client)                              gen_ai.request.model=sonnet gen_ai.usage.input_tokens=…
│                                                       gen_ai.usage.output_tokens=… reviewbot.claude.cost_usd=…
├─ mcp tools/call submit_finding  (server)              (MCP включён: на каждый вызов инструмента)
├─ review.verify
│  └─ claude verification
├─ review.diff_index → gitlab.get_mr_diffs
├─ review.publish                                       reviewbot.published=3 reviewbot.blocking=1
│  └─ review.publish.drafts                             reviewbot.drafts=3 reviewbot.drafts_created=3
│     ├─ gitlab.delete_draft_notes
│     ├─ gitlab.create_draft_note ×3                    reviewbot.marker=finding
│     └─ gitlab.publish_draft_notes
├─ review.publish.compat · review.publish.lint
├─ review.tbd · review.labels · review.problems
└─ review.summary → gitlab.update_note
```

Спан HTTP-запроса получает статус «ошибка» при коде 400 и выше, кроме одного случая: ответ 404 на
чтение файла репозитория (`…/repository/files/…`) — это «файла нет» (необязательные файлы вроде
`.reviewbot.yaml`), а не сбой. Код ответа в спане остаётся, красным он не отмечается.

Отдельные трассы:

- `POST /webhook` (server) — каждый запрос вебхука: `gitlab.event`,
  `reviewbot.webhook.result` (`accepted` / `ignored`), `http.response.status_code`. Входящий
  заголовок `traceparent` (если его ставит прокси перед ботом) не делает спан дочерним: запрос
  ещё не аутентифицирован (секрет проверяется внутри), поэтому спан — корень новой трассы,
  сэмплированной по `tracing.sample_ratio`, а входящий контекст — span link («Links» в Grafana).
  Флаг sampled из заголовка не обходит `sample_ratio`.
- `poller.tick` и `poller.pipelines.tick` — раунд опроса GitLab с HTTP-спанами внутри и
  `reviewbot.poll.jobs`.

Трасса задачи начинается заново (новый trace id) и ссылается на спан события через
span link: в Grafana это ссылка «Links» у корня. Связь живёт в памяти: задача, которая
пережила перезапуск бота (отложенное авторевью), ссылки не имеет.

Откуда берутся спаны:

- **Шаги диагностики.** Декораторы `internal/diag` (GitLab, лимиты, checkout/git, exec,
  запуски Claude, `diag.Step`) — те же, что пишут таймлайн `/runs`, — создают и спан.
  Таймлайн и спаны независимы: с `diag.enabled: false` спаны всё равно есть.
- **HTTP-запросы к GitLab** — клиентские спаны самого клиента, по одному на попытку
  (повтор после 5xx — `http.request.resend_count`).
- **Анализаторы, перепроверка, публикация, саммари** — явные спаны в `internal/review`.
- **MCP** — серверный спан на каждый `tools/call`. Его родитель — спан задачи (сессия MCP
  открывается до запуска Claude), по времени он внутри спана `claude review`.

Вне задачи (фоновое обновление карты системы и контекстных репозиториев, синхронизация
вебхуков) спанов нет.

### Атрибуты

| Спан | Атрибуты |
|---|---|
| `review.job` | `job.kind`, `job.manual`, `gitlab.project`, `gitlab.project.id`, `gitlab.mr.iid`, `vcs.head_sha`, `gitlab.mr.state`, `gitlab.mr.draft`, `gitlab.note.id`, `gitlab.pipeline.id/status`, `reviewbot.user_id`, `reviewbot.queue.wait_ms`, `reviewbot.job.result` (`ok`, `skipped`, `postponed`, `failed`, `interrupted` — как на `/runs`), `reviewbot.job.reason` (причина пропуска или отсрочки), `reviewbot.job.retry_at`, `reviewbot.run_id` (id записи на `/runs`), `reviewbot.job.attempt` |
| GitLab HTTP | `http.request.method`, `url.template` = `url.path` (шаблон: `{id}`, `{sha}`, `{path}` вместо значений), `server.address`, `server.port`, `http.response.status_code`, `http.request.resend_count`, `error.type` |
| `gitlab.*`, `git.*`, `limiter.*`, `exec …` | `reviewbot.step.kind`, `reviewbot.step.status` (`ok`, `error`, `warn`, `denied`, `info`) и поля шага: только числа, флаги и короткие id (`reviewbot.diff_bytes`, `reviewbot.files`, `reviewbot.notes`, `reviewbot.head_sha`, `reviewbot.marker`, `reviewbot.discussion_id`, `reviewbot.reason`, `reviewbot.limiter.allowed`, `process.executable.name`, `process.exit.code`) |
| `claude <вид>` | `gen_ai.system=anthropic`, `gen_ai.provider.name`, `gen_ai.operation.name=invoke_agent`, `gen_ai.agent.name=claude-code`, `gen_ai.request.model`, `gen_ai.response.model` (модели из отчёта CLI), `gen_ai.usage.input_tokens` (вход вместе с кэшем, как в семантических соглашениях GenAI), `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read.input_tokens`, `gen_ai.usage.cache_creation.input_tokens`, `reviewbot.claude.kind` (вид учёта: `review`, `verification`, `chat`, `ci`, `map`, `learning`, `memory`, `other`), `reviewbot.claude.cost_usd`, `reviewbot.claude.turns`, `reviewbot.claude.cli_duration_ms`, `reviewbot.claude.timeout_s`, `reviewbot.claude.prompt_bytes`, `reviewbot.claude.mcp`, `reviewbot.claude.context_repos`, `process.exit.code`, `error.type` (`rate_limited`, `timeout`, …) |
| `mcp tools/call <tool>` | `mcp.method.name`, `gen_ai.tool.name`, `reviewbot.mcp.status` (`ok`, `error`, `limit`), `reviewbot.mcp.args_bytes`, `reviewbot.mcp.result_bytes`, `reviewbot.mcp.call` |
| `review.*` | `review.publish`: `reviewbot.findings`, `published`, `outside`, `skipped`, `blocking`; `review.publish.drafts`: `drafts`, `drafts_created`; `review.summary`: `update`, `body_bytes` |

Ресурс: `service.name`, `service.version` (версия сборки), `service.instance.id` (имя
хоста — id контейнера в docker), `deployment.environment` и
`deployment.environment.name` (если задан `tracing.environment`), `telemetry.sdk.*`.

Ошибка спана: статус `ERROR`, событие `exception` (`exception.type` — короткий класс,
`exception.message` — вычищенный и обрезанный до 256 байт текст), `error.type`.

## Приватность: что уходит в трейсы

В спаны **никогда** не попадают: промпты и системные промпты, код и дифф, тексты заметок
и ответов Claude, логи CI, аргументы и ответы MCP-инструментов, пути файлов и их списки,
аргументы команд, имена веток и пользователей, токены. Только id (проект, MR, заметка,
тред, коммит), размеры, счётчики, длительности, статусы и короткие классы ошибок.

Как это обеспечено:

- в атрибуты спана из полей шага диагностики проходят только числа, флаги и строки из
  короткого списка ключей (SHA, состояния, маркер заметки, id треда, причина отказа
  лимита); всё остальное остаётся только на `/runs`;
- у HTTP-спанов GitLab путь — шаблон (`/projects/{id}/repository/files/{path}/raw`):
  ни имени файла, ни query (`ref`, `private_token`) нет; заголовки не записываются;
- каждая строка спана (атрибут, событие, сообщение статуса) проходит через тот же
  скрабер, что и `/runs` (известные секреты из конфига и окружения, форматы токенов,
  учётные данные в URL), затем из всех URL вырезается query и фрагмент, затем длина
  обрезается до 256 байт;
- имена спанов — константы или шаблоны, без данных.

Что видит приёмник: названия проектов (`gitlab.project`), номера MR, SHA коммитов, модели
и расход токенов. Если это чувствительно для вашего бэкенда — держите приёмник внутри
контура (Tempo / коллектор рядом с ботом). Подробнее про границы — в
[security.md](../security.md#трейсинг).

## Корреляция: логи ↔ трейсы ↔ /runs

- **Логи.** Строки JSON-лога, записанные во время задачи, получают `trace_id` и `span_id`
  (корня задачи или текущего спана). Большая часть кода бота пишет лог без контекста,
  поэтому строка привязывается к задаче по атрибуту `mr` (очередь выполняет не больше
  одной задачи на MR). В Loki: `{app="reviewbot"} | json | trace_id="…"`; в Grafana
  удобно настроить derived field `trace_id` → источник Tempo. Строки вне задачи и
  несэмплированных трасс без `trace_id`.
- **`/runs`.** Запись задачи хранит `trace_id`; на странице `/runs/<id>` — строка
  «Трейс» со ссылкой из `tracing.ui_url` (`{trace_id}` подставляется), `reviewbot runs show
  <id>` печатает `Trace:`. Обратно: атрибут `reviewbot.run_id` корня трассы — id записи
  на `/runs`.
- **Метрики.** Разбивка по исходу (`reviewbot.job.result`) та же, что у
  `reviewbot_jobs_total` и `/runs`.

Пример `ui_url` для Grafana Explore с источником Tempo (uid `tempo`): JSON-параметр
`panes` с запросом TraceQL `{trace_id}`, URL-кодированный, — готовая строка в
`e2e/local/config.yaml`.

## Сэмплирование

Head sampling по доле: `tracing.sample_ratio` (по умолчанию `1` — все трассы). Решение
принимается при создании корня по trace id (как `TraceIdRatioBased`), дочерние спаны
следуют решению корня — трасса либо целиком есть, либо её нет. Трассы задач, вебхуков и
опроса сэмплируются независимо (ссылка задачи на несэмплированное событие не
ставится). «Всегда хранить ошибки» бот сам не умеет: решение принимается до того, как
исход известен; для этого — tail sampling в OpenTelemetry Collector или Alloy
(`tail_sampling` с политикой `status_code: ERROR`) при `sample_ratio: 1` у бота.
У несэмплированной задачи нет `trace_id` ни в логе, ни на `/runs` (сама запись на `/runs`
есть, как обычно).

## Надёжность экспорта

Экспорт никогда не тормозит бота:

- законченный спан кладётся в очередь (2048 спанов) без ожидания; очередь полна — спан
  отбрасывается и считается в `reviewbot_tracing_spans_total{result="dropped"}`;
- отдельная горутина отправляет пачки до 512 спанов раз в 5 секунд (или сразу, если
  пачка набралась), `POST {endpoint}/v1/traces`, `Content-Type: application/json`,
  `Content-Encoding: gzip`, таймаут `tracing.timeout`;
- повторы с экспоненциальной задержкой (до 4 повторов, `Retry-After` учитывается) — на
  429, 502, 503, 504 и сетевые ошибки; прочие 4xx и 5xx не повторяются (спаны — `failed`);
  ответ `partialSuccess` с `rejectedSpans` — часть спанов `failed`;
- в лог — не чаще раза в минуту («tracing: export failed, spans dropped» с адресом без
  учётных данных);
- на SIGTERM бот дожидается очереди задач и до 5 секунд досылает накопленные спаны;
- экспортёр не стартовал (например, нечитаемый `tracing.ca_file`) — ошибка в логе, бот
  работает без трейсинга.

Метрики — в [metrics.md](../metrics.md): `reviewbot_tracing_spans_total{result}`,
`reviewbot_tracing_export_errors_total{reason}`; на дашборде — две панели в разделе
«Инфраструктура».

## Телеметрия Claude Code CLI (экспериментально)

`tracing.claude_cli: true` включает собственные трейсы Claude Code внутри песочницы.
Claude Code документирует ([Monitoring](https://code.claude.com/docs/en/monitoring-usage)):
трассировку (бета) включают `CLAUDE_CODE_ENABLE_TELEMETRY=1` и
`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`, экспорт — `OTEL_TRACES_EXPORTER=otlp` с
`OTEL_EXPORTER_OTLP_*`, а в сессиях `-p` CLI читает `TRACEPARENT` из окружения. Бот
передаёт CLI:

| Переменная | Значение |
|---|---|
| `CLAUDE_CODE_ENABLE_TELEMETRY`, `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA` | `1` |
| `OTEL_TRACES_EXPORTER` | `otlp` |
| `OTEL_METRICS_EXPORTER`, `OTEL_LOGS_EXPORTER` | `none` (только трейсы) |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | `http/json` |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | `tracing.claude_cli_endpoint` (или `endpoint`) + `/v1/traces`, без `user:password@` и query (значение видно в argv `docker run`) |
| `OTEL_RESOURCE_ATTRIBUTES` | `service.namespace=gitlab-review-bot`, `deployment.environment` |
| `TRACEPARENT` | спан `claude <вид>` этого запуска |
| `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | только при `tracing.claude_cli_headers: true`: `headers`, а `user:password@` из адреса приёмника — как `Authorization: Basic …` (если в `headers` нет своего `Authorization`); передаётся по имени, значения нет в argv `docker run` |

Тогда спаны CLI (`claude_code.interaction` → `claude_code.llm_request`,
`claude_code.tool`) становятся дочерними для спана запуска Claude в трассе задачи.
CLI по умолчанию скрывает в спанах текст промпта и детали инструментов; переключатели
`OTEL_LOG_USER_PROMPTS` / `OTEL_LOG_TOOL_DETAILS` / `OTEL_LOG_TOOL_CONTENT` бот не
ставит и не пропускает. Команды проверок (gitleaks, oasdiff, …) телеметрию не получают.

Сеть — главное последствие: **песочница начинает сама ходить в приёмник**.

- `https`-приёмник на порту 443 — через egress-прокси: его хост добавляется в
  разрешённые (`sandbox.egress_allow`) только при `claude_cli: true`. Egress-прокси
  читает это из того же конфига (`reviewbot egress -config …`), то есть блок `tracing`
  должен быть в файле, который видит egress.
- `http`-приёмник (или https на другом порту) — напрямую: хост добавляется в `NO_PROXY`
  CLI, а сам приёмник должен быть в сети песочницы (`sandbox.network`, internal-сеть без
  выхода наружу) — например, отдельный коллектор, подключённый к этой сети. Адрес, как его
  видит песочница, — `tracing.claude_cli_endpoint`; `localhost` в docker-режиме — ошибка
  конфига.
- Учётные данные экспорта (`headers` и `user:password@` в адресе приёмника) в песочницу
  не попадают, пока явно не задан `tracing.claude_cli_headers: true`. Лучше дать песочнице отдельный коллектор без
  аутентификации внутри контура, а тот уже отправит дальше с секретом.
- Канал из песочницы наружу расширяется: модель, которая прочитала prompt injection,
  теоретически может пытаться вынести данные в атрибутах спанов CLI на этот приёмник
  (как и сейчас — на `api.anthropic.com`). Держите приёмник своим.

**Не проверено вживую:** передача `TRACEPARENT` и экспорт CLI в этом репозитории
проверены только на уровне окружения (`internal/claude/telemetry_test.go`), запуска
настоящего Claude Code с трассировкой в тестах нет. Флаг бета, поведение может
измениться между версиями CLI.

## Проверка приёмника

Тест `TestLiveTempo` отправляет маленькую трассу в настоящий приёмник и читает её из Tempo:

```bash
REVIEWBOT_OTLP_ENDPOINT=http://localhost:4318 REVIEWBOT_TEMPO_URL=http://localhost:3200 \
  go test ./internal/tracing -run TestLiveTempo -v
```

Без переменных тест пропускается.

## Как устроено

- `internal/tracing` — трейсер (`Tracer.Start`, `tracing.Start` для дочерних спанов,
  `Span.SetAttr/AddEvent/RecordError/SetStatus/End`, W3C `traceparent`, сэмплер),
  экспортёр (`BatchExporter`), кодирование OTLP/JSON (`otlp.go`, golden-тест),
  `LogHandler` для slog, HTTP-обёртка серверных спанов. Нулевой `*Tracer` и `*Span` — no-op;
  `tracing.Start` без спана в контексте ничего не создаёт.
- `internal/diag/trace.go` — мост: `startStep` превращает шаг таймлайна в спан,
  `TraceJobs` — корень задачи (снаружи `diag.Recorder.Handler`, чтобы запись `/runs`
  видела спан и передавала ему исход), спаны Claude. `diag.Hooks{Rec, Trace}` ставит
  декораторы, если включена диагностика или трейсинг.
- `internal/gitlab/trace.go` — клиентские спаны HTTP; `internal/review` — спаны
  анализаторов; `internal/mcpserver` — спаны инструментов; `internal/webhook`,
  `internal/poller` — `core.Job.TraceParent`; `internal/claude/telemetry.go` — окружение CLI;
  `cmd/reviewbot/tracing.go` — сборка.

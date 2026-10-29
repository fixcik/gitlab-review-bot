# Метрики и проверки готовности

Бот отдаёт метрики в формате Prometheus (`GET /metrics`, text exposition 0.0.4)
и проверку готовности `GET /readyz`. `GET /healthz` не изменился: это
проверка живости (процесс отвечает), без обращений к GitLab, базе и Docker.

Все метрики бота начинаются с `reviewbot_`. В метках **нет пользователей,
проектов и MR** — так число временных рядов не растёт вместе с командой и
репозиториями. Разбивка по людям, репозиториям и MR есть на странице
`/usage` (токены и стоимость), подробности конкретной задачи — на `/runs`
(таймлайн, транскрипт Claude, ошибки).

## Доступ

```yaml
metrics:
  enabled: true   # false — /metrics отвечает 404
  token: ""       # лучше через env METRICS_TOKEN
```

- Без токена `/metrics` открыт всем, кто достучится до порта бота (при
  старте в лог пишется строка об этом). Чувствительных данных в метриках нет,
  но лучше закрыть порт сетью или задать токен.
- С токеном нужен заголовок `Authorization: Bearer <токен>`. Токен в URL
  (`?token=`) и cookie **не принимаются**: эндпоинт для скрейперов, а не для
  браузера.
- `/readyz` доступен всегда (это проба для оркестратора) и не зависит от
  `metrics.enabled`.

Версия сборки берётся из `-ldflags "-X main.version=1.2.3"`
(`docker build --build-arg VERSION=1.2.3 .`), по умолчанию `dev`.

## Список метрик

Типы: counter — растущий счётчик (смотрите `rate()`/`increase()`), gauge —
текущее значение, histogram — распределение (`_bucket`, `_sum`, `_count`).

### Задачи очереди

| Метрика | Тип | Метки | Смысл |
|---|---|---|---|
| `reviewbot_jobs_total` | counter | `kind`, `status` | Обработанные задачи. `kind`: `review` (авто-ревью, `@bot review`, `full review`, `review force`), `chat`, `learning` (ответы в тредах, команды правил), `other` (help, pause, memory и прочие команды, задачи пайплайнов CI — [ci.md](features/ci.md)). `status` — как на `/runs`: `ok`, `skipped` (задача ничего не сделала: черновик, уже проверено, выключено в репозитории…), `postponed` (отложена до сброса лимита), `failed`, `interrupted` (остановка сервиса). |
| `reviewbot_job_duration_seconds` | histogram | `kind` | Время выполнения обработчика задачи. Корзины: 1, 5, 15, 30, 60, 120, 300, 600, 1200 с. |
| `reviewbot_skipped_total` | counter | `reason` | Ревью, в которых бот сознательно не запускал Claude. `reason`: `too_big` — проверяемый диапазон больше `review.max_mr_lines` (выполнены только быстрые проверки без модели, [review.md](features/review.md#размер-mr)); `trivial_increment` — автоматическое инкрементальное ревью, в новых коммитах только форматирование, комментарии или документация (`review.skip_trivial_increments`; выполнены gitleaks, TODO и отчёты CI-линтеров, [review.md](features/review.md#тривиальные-пуши)); `no_repo_config` — репозиторий не подключён: нет `.reviewbot.yaml` в целевой ветке при `review.require_repo_config: true` (ни одной проверки); `repo_config_invalid` — `.reviewbot.yaml` целевой ветки невалиден (ни одной проверки, заметка в MR, проблема `repo_config_invalid`; [review.md](features/review.md#какие-репозитории-ревьюятся)). Считаются и авто-, и ручные ревью. Такие задачи в `reviewbot_jobs_total` — `status="skipped"`. |
| `reviewbot_review_route_total` | counter | `route` | Ревью по маршруту модели (`review.routing`, [review.md](features/review.md#выбор-модели-по-риску)): `light` — диапазон с низким риском, лёгкая модель без советника и MCP; `heavy` — большой или рискованный диапазон (`review.routing.heavy_model`, без советника и `deep-review`); `normal` — `claude.model` (в том числе все ревью при выключенной маршрутизации). Доля `light` — сколько ревью идут на дешёвую модель. |
| `reviewbot_queue_jobs` | gauge | `state` | Задачи в очереди на момент скрейпа: `pending` — готовы к запуску и ждут только воркера или окончания задачи своего MR (плюс авторевью в паузе `review.debounce`), `running` — выполняются, `retry_wait` — отложены до своего времени (лимит Claude или бота, цепочка `refresh_status` пока идёт пайплайн) и другие задачи MR не держат. Очередь хранится в SQLite: после рестарта задачи восстанавливаются (`reviewbot_queue_restored_total`). |
| `reviewbot_queue_restored_total` | counter | `kind`, `result` | Задачи из сохранённой очереди, найденные при старте. `kind` — как в `reviewbot_jobs_total`. `result`: `queued` — не успела начаться (или ждала повтора по лимиту), поставлена снова; `replayed` — прервана остановкой, ещё не ответив, выполняется заново; `expired` — старше `queue.manual_max_age`, отброшена; `answered` — прервана остановкой, но ответ в треде уже опубликован, отброшена (иначе ответ появился бы дважды); `invalid` — нечитаемая запись, отброшена. Рост `expired` — бот долго лежал или очередь стояла. |
| `reviewbot_queue_draining` | gauge | — | `1`, пока бот останавливается и дожидается выполняющихся задач (`queue.shutdown_grace`): новые задачи не запускаются, события принимаются в очередь. |

### Claude

| Метрика | Тип | Метки | Смысл |
|---|---|---|---|
| `reviewbot_claude_runs_total` | counter | `kind`, `model`, `result` | Запуски `claude -p`. `kind` — вид запуска как на `/usage`: `review`, `chat`, `verification` (перепроверка замечаний и breaking changes, [verify.md](features/verify.md); команда `check`), `learning`, `memory`, `map` (фоновое извлечение контрактов для карты системы), `ci` (объяснение упавшего пайплайна, [ci.md](features/ci.md)), `other`. `result`: `ok`, `error` (сбой CLI, max turns, отмена), `rate_limited` (исчерпан лимит подписки/API), `timeout`. |
| `reviewbot_claude_tokens_total` | counter | `kind`, `model`, `type` | Токены по данным CLI. `type`: `input`, `output`, `cache_read`, `cache_write`. Учитываются и неудачные запуски, если CLI успел сообщить расход. |
| `reviewbot_claude_cost_usd_total` | counter | `kind`, `model` | Условная стоимость в USD по ценам API (оценка самого CLI; при подписке это не реальный счёт). |
| `reviewbot_claude_run_duration_seconds` | histogram | `kind` | Время одного запуска вместе со стартом sandbox-контейнера. Корзины: 5, 15, 30, 60, 120, 300, 600, 900, 1800 с. |
| `reviewbot_claude_rate_limited_total` | counter | — | Запуски, упёршиеся в лимит Claude. |

Метка `model` — идентификатор модели, который сообщил CLI (`modelUsage`,
например `claude-sonnet-4-5-20250929`), в нижнем регистре, без суффиксов вида
`[1m]`. Токены и стоимость раскладываются по всем моделям запуска (включая
субагентов); в `claude_runs_total` запуск относится к модели с наибольшей
долей стоимости. Если CLI не сообщил модель — берётся запрошенная из конфига
(`sonnet`, `opus` …), если пусто и её — `default`.

### Лимиты, инструменты, вебхуки, опрос, карта системы, GitLab

| Метрика | Тип | Метки | Смысл |
|---|---|---|---|
| `reviewbot_limit_denials_total` | counter | `scope` | Отказы собственных лимитов бота. `global` — `limits.global_runs_per_hour`; `user_rerun` — `reruns_per_hour` пользователя; `mr_cooldown` — перезапуск этого MR чаще `mr_rerun_cooldown`; `user_chat` — `chat_per_hour`; `learn` — запуск обучения или сжатия памяти: глобальный лимит или `review.learn.max_per_hour` (оба проверяет лимитер одной бронью). |
| `reviewbot_tool_runs_total` | counter | `tool`, `result` | Детерминированные проверки. `tool`: `oasdiff`, `buf`, `graphql-inspector`, `custom` (compat), `gitleaks` (lint; линтеры бот не запускает — их отчёты из CI, `lint.ci`, сюда не попадают; прежние значения `semgrep`, `golangci-lint`, `ruff`, `eslint`, `phpstan`, `phpcs`, `custom` для lint больше не появляются). `result`: `ok` — отработал, замечаний нет; `findings` — нашёл breaking changes / замечания на изменённых строках; `unavailable` — не установлен в sandbox-образе; `failed` — запустился, но не смог проверить; `partial` — проверена только часть (лимит файлов/результатов, часть спецификаций упала). Инструмент, которому нечего проверять в MR (нет изменённых спецификаций), не считается. Compat-инструменты считаются один раз за проверку: oasdiff/graphql-inspector — по всем спецификациям сразу, `custom` — по всем командам сразу. |
| `reviewbot_webhook_events_total` | counter | `event`, `result` | Доставки вебхуков. `event`: `merge_request`, `note`, `pipeline` (разбор упавших пайплайнов, [ci.md](features/ci.md)), `other`. `result`: `accepted` — поставлена задача; `ignored` — событие не требует действий (черновик, бот не упомянут, проект вне `gitlab.groups`, неподдерживаемое событие); `unauthorized` — неверный `X-Gitlab-Token`; `bad_request` — битый JSON, слишком большое тело; `method_not_allowed` — не POST. |
| `reviewbot_poll_ticks_total` | counter | `result` | Раунды опроса API (`gitlab.events: poll`/`both`). `ok` — все группы опрошены; `error` — хотя бы одна группа не опросилась (MR с ошибкой повторяется в следующем раунде). |
| `reviewbot_poll_jobs_total` | counter | — | Задачи, найденные опросом (до отбрасывания дублей вебхука в режиме `both`). |
| `reviewbot_poll_last_success_timestamp_seconds` | gauge | — | Unix-время последнего раунда опроса без ошибок; `0` — ещё не было. Для алерта `ReviewbotPollingFailing`. |
| `reviewbot_poll_last_round_timestamp_seconds` | gauge | — | Unix-время последнего раунда опроса (с ошибками или без); `0` — ещё не было. Для алерта `ReviewbotPollerStalled`. |
| `reviewbot_poll_interval_seconds` | gauge | — | Настроенный `gitlab.poll_interval` в секундах; `0` — опрос выключен (`events: webhook`). Окна алертов опроса считаются от него. |
| `reviewbot_sysmap_scans_total` | counter | `status` | Обновления репозиториев карты системы ([sysmap.md](features/sysmap.md)) по итоговому статусу: `ok` — все файлы актуальны, `partial` — часть файлов ждёт бюджета запусков (`review.sysmap.max_runs_per_hour`), `error` — снимок не разобран. |
| `reviewbot_sysmap_repos` | gauge | `status` | Репозитории в карте по статусу сканирования (`ok`, `partial`, `error`), на момент запроса метрик. |
| `reviewbot_sysmap_entries` | gauge | `side` | Записи контрактов в карте: `provides` (эндпоинты, приём вебхуков, события сервера) и `consumes` (вызовы, отправка вебхуков, подписки). |
| `reviewbot_sysmap_unresolved` | gauge | — | Вызовы, которые не сопоставились ровно с одним провайдером (список — на `/map`). Рост после деплоя — повод добавить подсказку в `review.sysmap.hints`. |
| `reviewbot_mcp_tool_calls_total` | counter | `tool`, `status` | Вызовы MCP-инструментов бота моделью во время ревью ([mcp.md](features/mcp.md)): `ok` — инструмент ответил, `error` — ответил ошибкой (неверные аргументы, нет данных), `limit` — исчерпан `review.mcp.max_calls`. Вердикты `submit_finding` (accepted/adjusted/rejected) — в логе задачи на `/runs`. |
| `reviewbot_mcp_fallbacks_total` | counter | `reason` | Запуски ревью, в которых CLI не воспользовался MCP-сервером бота, и ревью повторено без MCP ([запасной путь](features/mcp.md#запасной-путь)): `not_connected` — CLI сообщил статус сервера не `connected`, `no_request` — статуса нет и до сервера не дошёл ни один запрос, `config_dropped` — конфиг MCP не записан. Рост — проверить проброс egress (`REVIEWBOT_MCP_TARGET`). |
| `reviewbot_mcp_degraded` | gauge | — | `1`, пока новые ревью идут без MCP после запасного пути (пауза 30 минут с последнего), иначе `0`; снимается при каждом сборе. |
| `reviewbot_problems_total` | counter | `kind`, `source` | Проблемы проверки ([problems.md](features/problems.md)): что бот не смог проверить. `kind` — вид из фиксированного списка (`lint_failed`, `coverage_missing`, `context_unavailable`, `diff_truncated`, `invalid_output`, `model_*`, …), `source`: `bot` — шаг бота, `model` — сообщила модель. Считается одна запись на задачу и отпечаток (одна и та же проблема дважды в одном ревью — одна). |
| `reviewbot_verify_runs_total` | counter | `result` | Запуски перепроверки перед публикацией ([verify.md](features/verify.md)): один на ревью, в котором есть что перепроверять (critical/major замечания и/или breaking changes Claude). `result`: `ok`, `limit` — не запущена, исчерпан общий лимит запусков, `rate_limited` — лимит Claude, `invalid_output` — ответ дважды не по схеме, `error` — другая ошибка. Всё, кроме `ok`, — замечания опубликованы без перепроверки. |
| `reviewbot_verify_verdicts_total` | counter | `verdict`, `severity` | Вердикты перепроверки. `verdict`: `confirm`, `downgrade`, `reject`, `missing` (вердикта нет — считается подтверждением), `unverified` (запуск не удался), `proven` (в запуск не отправлялось: бот доказал замечание детерминированно — подтверждённый `deployed_usage` миграции, gitleaks, детерминированная проверка совместимости; [verify.md](features/verify.md#что-не-перепроверяется)). `severity` — исходный уровень замечания (`critical`, `major`, `minor`, `nit`) или `breaking` для пунктов обратной совместимости (у них только `confirm`/`reject`/`missing`/`unverified`). Доля `reject` — сколько ложных срабатываний ревью ловит перепроверка. |
| `reviewbot_ci_failures_total` | counter | `scope`, `cause` | Упавшие job упавших пайплайнов ([ci.md](features/ci.md)), один раз на пайплайн. `scope`: `mr` — head-пайплайн MR, `main` — пайплайн main-ветки. `cause` — по объяснению модели: `mr` (изменения; job из `lint.ci` — всегда `mr`), `infra`, `flaky`, `unknown` (объяснения не было или причина не ясна). Рост `scope="main"` — main часто красный; рост `cause="flaky"` — пора чинить нестабильные тесты. |
| `reviewbot_ci_explain_runs_total` | counter | `result` | Объяснения упавших пайплайнов (`review.ci.explain`): `ok`, `limit` — не запущено, исчерпан общий лимит запусков, `rate_limited` — лимит Claude (дайджест без объяснения, задача не откладывается), `invalid_output` — ответ дважды не по схеме, `error` — другая ошибка (в т.ч. checkout), `duplicate` — тот же сбой уже объяснён, запуска нет. |
| `reviewbot_problems_new_total` | counter | `kind` | Проблемы, увиденные **впервые** или вернувшиеся после «Скрыть» на `/problems`. Для алерта «появилась новая проблема». |
| `reviewbot_reactions_total` | counter | `event`, `result` | Реакции бота (award emoji, [reactions.md](features/reactions.md)). `event`: `picked_up` (👀), `done`, `failed`, `postponed`, `denied`, `thanks`, `ack`, `learned`, `clean_review`. `result`: `added` — реакция поставлена (или снята, если событие только снимает 👀), `exists` — уже стояла, ничего не меняли, `disabled` — выключено в `.reviewbot.yaml` репозитория, `error` — ошибка GitLab (в логе), `forbidden` — GitLab ответил 403 (заодно проблема `reactions_failed`), `dropped` — очередь реакций переполнена. Рост `forbidden`/`error` — повод проверить права бот-аккаунта. |
| `reviewbot_labels_total` | counter | `action`, `result` | Изменения [автоматических меток MR](features/labels.md), по одной на метку: `action` — `add` или `remove`; `result`: `ok`, `forbidden` — ни одному токену нельзя менять метки MR (Reporter-бот без служебного токена), `missing` — метки нет в проекте и группах, а `create_missing` выключен, `ignored` — GitLab ответил 200, но метку не применил, `error` — другая ошибка API. Всё, кроме `ok`, — ещё и проблема `labels_failed`. |
| `reviewbot_tracing_spans_total` | counter | `result` | Спаны трейсинга ([tracing.md](features/tracing.md); только при `tracing.enabled`), переданные экспортёру OTLP: `exported` — приняты приёмником; `dropped` — очередь экспорта (2048) полна или экспортёр остановлен, спан не отправлялся (бот при этом не ждёт); `failed` — приёмник отклонил (4xx, `partialSuccess`) или повторы исчерпаны. Рост `dropped`/`failed` — приёмник недоступен или не успевает. |
| `reviewbot_tracing_export_errors_total` | counter | `reason` | Неудачные запросы экспорта трейсов, каждая попытка: `network` — нет ответа (адрес, DNS, TLS, таймаут), `throttled` — 429, `server` — 5xx, `client` — прочие 4xx (чаще всего неверные `tracing.headers`; не повторяются), `partial` — приёмник отклонил часть спанов. |
| `reviewbot_gitlab_requests_total` | counter | `method`, `code` | HTTP-запросы к GitLab API обоими токенами (бота и сервисным), **каждая попытка, включая ретраи**. `code`: класс `2xx`/`3xx`/`4xx`, точный код для `403`, `404`, `429` и всех `5xx`, `error` — ответа не было (сеть, таймаут, отмена). |
| `reviewbot_gitlab_reads_saved_total` | counter | `op` | Чтения GitLab API, которые задача **не сделала**: тот же MR (`op="get_mr"`), его дифф (`get_mr_diffs`) или его треды (`list_discussions`) уже были прочитаны в этой же задаче (память на одну задачу, [review.md](features/review.md#скорость-ревью)). Треды перечитываются после каждой записи бота в MR (заметки, черновики, ответы, закрытие тредов); проверка «MR изменился во время ревью» всегда читает MR заново. Сравнивайте с `reviewbot_gitlab_requests_total{method="GET"}`. |

### Сборка, готовность, рантайм

| Метрика | Тип | Метки | Смысл |
|---|---|---|---|
| `reviewbot_build_info` | gauge | `version`, `go_version` | Всегда 1. |
| `reviewbot_readiness_check_ok` | gauge | `check` | 1/0 по каждой проверке `/readyz` (`gitlab`, `store`, `docker`) на момент скрейпа, с тем же кэшем. |
| `go_goroutines` | gauge | — | Число горутин. |
| `go_memstats_alloc_bytes` | gauge | — | Занятая куча. |
| `go_memstats_sys_bytes` | gauge | — | Память, полученная от ОС. |
| `process_start_time_seconds` | gauge | — | Время старта процесса (unix). |

Защита от взрыва кардинальности: у каждой метрики не больше 1000 сочетаний
меток; новые сочетания сверх лимита складываются в один ряд, где все метки
равны `other`.

## /readyz

```json
{"status":"ok","checks":[
  {"name":"gitlab","ok":true,"latency_ms":41},
  {"name":"store","ok":true,"latency_ms":0},
  {"name":"docker","ok":true,"latency_ms":63}
]}
```

200, если все проверки прошли, иначе 503 и `"status":"fail"` с текстом
ошибки в `error` (токены из текста вырезаются).

- `gitlab` — `GET /user` токеном бота; результат кэшируется на 30 с.
- `store` — ping SQLite и `SELECT 1`; без кэша.
- `docker` — только при `sandbox.mode: docker`:
  `docker version --format '{{.Server.Version}}'` с таймаутом 5 с, кэш 30 с.
  Падает, если демон недоступен (не смонтирован сокет, нет прав).

Проверка, прерванная потому, что вызывающий ушёл раньше её собственного
таймаута (проба оборвала соединение, скрейп `/metrics` ограничен 4 с),
отвечает этому вызывающему ошибкой, но в кэш не попадает: следующий запрос
выполнит её заново.

## Дашборд и алерты

Готовые файлы в `deploy/` (нужны только Prometheus и Grafana):

| Файл | Что это |
|---|---|
| [`deploy/prometheus/scrape.yml`](../deploy/prometheus/scrape.yml) | job `reviewbot` для `scrape_configs` |
| [`deploy/prometheus/reviewbot-alerts.yml`](../deploy/prometheus/reviewbot-alerts.yml) | правила алертов (группы, аннотации на русском со ссылками на `/runs`, `/usage`, `/problems`) |
| [`deploy/grafana/reviewbot-dashboard.json`](../deploy/grafana/reviewbot-dashboard.json) | дашборд «GitLab Review Bot» |

Тест `deploy/assets_test.go` (часть `go test ./...`) строит реестр
`internal/telemetry` и проверяет, что каждая метрика, метка (в селекторах,
`by (...)`, `label_values`) и значение метки из этих файлов существует в коде.
Переименовали метрику или метку — тест укажет, какие панели и алерты
поправить.

### Prometheus: скрейп

```yaml
scrape_configs:
  - job_name: reviewbot
    scrape_interval: 30s
    scrape_timeout: 10s
    metrics_path: /metrics
    # Если задан METRICS_TOKEN (без токена — удалите блок):
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/secrets/reviewbot-metrics-token
    static_configs:
      - targets: ["reviewbot:8080"]
```

Это содержимое `deploy/prometheus/scrape.yml`: вставьте job в свой
`prometheus.yml` или подключите файл через `scrape_config_files`
(Prometheus ≥ 2.43). Токен — только заголовком `Authorization: Bearer`
(см. [Доступ](#доступ)); неверный токен — ответ 401 и `up == 0`. Имя job
`reviewbot` ожидают алерты `ReviewbotDown`/`ReviewbotAbsent`; другое имя —
поправьте их `expr`.

Kubernetes: `readinessProbe` → `httpGet: {path: /readyz, port: 8080}`
(`periodSeconds: 30`, `timeoutSeconds: 15`), `livenessProbe` → `/healthz`.
Не используйте `/readyz` как liveness: при недоступном GitLab перезапуск бота
не поможет.

### Prometheus: алерты

```yaml
# prometheus.yml
rule_files:
  - /etc/prometheus/rules/reviewbot-alerts.yml
```

Проверка перед загрузкой: `promtool check rules reviewbot-alerts.yml`, затем
перезагрузка Prometheus (`SIGHUP` или `POST /-/reload` при
`--web.enable-lifecycle`). Маршрутизация по `severity` (`critical`,
`warning`, `info`) — в Alertmanager.

| Алерт | Уровень | Когда | Куда смотреть |
|---|---|---|---|
| `ReviewbotDown` | critical | `up{job="reviewbot"} == 0` 5 мин | процесс, логи, `METRICS_TOKEN` |
| `ReviewbotAbsent` | warning | таргета `job="reviewbot"` нет 15 мин | scrape config, service discovery |
| `ReviewbotNotReady` | critical | проверка `/readyz` (`gitlab`, `store`, `docker`) падает 5 мин | ответ `/readyz` |
| `ReviewbotQueueBacklog` | warning | `pending` > 10 дольше 15 мин | `/runs`, `limits.max_concurrent` |
| `ReviewbotQueueStuck` | warning | есть готовые к запуску (`pending`), нет `running` 15 мин, очередь не дренируется; отложенные до срока (`retry_wait`) не считаются | `/runs`, рестарт (очередь устойчивая — задачи не теряются) |
| `ReviewbotJobFailureRatioHigh` | warning | > 20% задач `failed` за 15 мин (от 5 задач) | `/runs?status=failed` |
| `ReviewbotClaudeRateLimited` | warning | ≥ 3 упора в лимит Claude за час | `/usage`, `/runs?status=postponed` |
| `ReviewbotClaudeFailureRatioHigh` | warning | > 20% запусков Claude `error`/`timeout` за 30 мин | `/runs` |
| `ReviewbotInvalidOutputSpike` | warning | > 3 ответов не по схеме за час (ревью, перепроверка, CI) | `/problems?kind=invalid_output` |
| `ReviewbotClaudeCostHigh` | warning | стоимость за сутки > порога (**50 USD**, число в `expr`) | `/usage` |
| `ReviewbotGlobalLimitHit` | info | > 5 отказов `limits.global_runs_per_hour` за час | `/usage` |
| `ReviewbotNewVerificationProblem` | info | рост `reviewbot_problems_new_total` | `/problems?kind=…` |
| `ReviewbotVerificationProblemsFrequent` | warning | > 20 проблем одного вида за час | `/problems?kind=…` |
| `ReviewbotVerifyFailing` | info | > 3 невыполненных перепроверок за час | `/problems?kind=verify_failed` |
| `ReviewbotSecretsFound` | critical | gitleaks нашёл секреты | `/runs` |
| `ReviewbotSandboxToolsUnavailable` | warning | > 5 `unavailable` инструментов за 30 мин | `sandbox.image`, `/readyz` |
| `ReviewbotRepoConfigInvalid` | info | ревью пропущены из-за невалидного `.reviewbot.yaml` | `/problems?kind=repo_config_invalid` |
| `ReviewbotMainBroken` | info | упал пайплайн main (`reviewbot_ci_failures_total{scope="main"}`) | заметка в MR-виновнике, `/problems?kind=main_pipeline_failed` |
| `ReviewbotGitLabErrors` | warning | > 10% запросов к GitLab 5xx/429/без ответа 10 мин | GitLab, сеть, `/readyz` |
| `ReviewbotWebhookUnauthorized` | warning | > 5 вебхуков с неверным `X-Gitlab-Token` за 15 мин | `GITLAB_WEBHOOK_SECRET` |
| `ReviewbotPollerStalled` | warning | опрос (`poll`/`both`) не сделал ни одного раунда дольше max(3 × `gitlab.poll_interval`, 10 мин) + 5 мин | лог `poller`, рестарт |
| `ReviewbotPollingFailing` | warning | раунды идут, успешных нет дольше max(3 × `gitlab.poll_interval`, 5 мин) | лог `poller: poll failed`, права токена |

Пороги рассчитаны на команду в десятки MR в день — правьте их прямо в файле. Окна алертов
опроса растут с `gitlab.poll_interval` сами (`reviewbot_poll_interval_seconds`). Если
`review.debounce` длиннее 15 минут, увеличьте `for` у `ReviewbotQueueStuck`. Юнит-тесты
алертов — `deploy/prometheus/tests/reviewbot-alerts.test.yml` (`promtool test rules`; тест
`deploy` запускает их, если `promtool` есть в `PATH` или в `PROMTOOL`).

### Grafana: дашборд

Импорт: **Dashboards → New → Import → Upload JSON file** →
`deploy/grafana/reviewbot-dashboard.json` → выберите источник Prometheus
(вход `DS_PROMETHEUS`). Нужна Grafana 10+ (`schemaVersion` 39). При
provisioning из файла вход не подставляется — выберите источник в переменной
«Источник данных» и сохраните дашборд.

Переменные: источник данных, `job` и `instance` (из
`reviewbot_build_info`; несколько ботов на одном Prometheus выбираются
здесь). Переменной проекта нет: в метриках нет меток проектов и
пользователей — разбивка по ним на `/usage`. Аннотация «Рестарт» отмечает
перезапуски процесса.

| Ряд | Что показывает |
|---|---|
| Обзор | `up`, проверки `/readyz`, версия, аптайм, стоимость за период; ревью в час по статусу, все задачи по видам; очередь (`pending`/`running`/`retry_wait`), длительность задач p50/p95, ревью без Claude по причинам |
| Claude | запуски по виду и результату, доля неудачных, упоры в лимит; длительность p50/p95; токены по типу и по моделям, доля кэша; стоимость в час и за сутки по видам; отказы лимитов бота по scope; ответы не по схеме; советник недоступен (`advisor_unavailable`); вызовы MCP-инструментов |
| Качество ревью | вердикты перепроверки, подтверждённые замечания по уровню, доля отклонённых за период, невыполненные перепроверки, детерминированные проверки (compat, gitleaks), реакции и автоматические метки по результату |
| CI | упавшие job по scope (`mr`/`main`) и причине, main за период, объяснения пайплайнов по результату, сбои чтения отчётов CI (`ci_fetch_failed`) |
| Карта системы | обновления репозиториев по статусу, репозитории и контракты, неразрешённые вызовы, запуски и стоимость модели карты (`kind="map"`) |
| Проблемы проверки | новые проблемы по виду, все проблемы за период по виду и источнику, проблемы в час |
| Инфраструктура | запросы к GitLab по коду и методу, доля ошибок; вебхуки по событию и результату; раунды опроса и отставание; `/readyz` во времени; горутины и память Go; спаны трейсинга по результату и ошибки экспорта по причине (пусто, если трейсинг выключен) |

Чего на дашборде нет, потому что бот этого не отдаёт: время ожидания в
очереди, лимит `max_concurrent` как метрика, задержка запросов к GitLab,
опубликованные замечания по уровню и блокирующие замечания (видно только
подтверждённые перепроверкой), закрытые/исправленные треды, фаза B карты
отдельно от фонового сканирования (обе — `kind="map"`), остаток бюджета
Claude, чтения отчётов линтеров из CI, egress-прокси, `process_resident_memory_bytes`
(есть `go_memstats_sys_bytes`).

### Полезные запросы


- стоимость за сутки по видам запусков:
  `sum by (kind) (increase(reviewbot_claude_cost_usd_total[1d]))`;
- доля кэша во входных токенах:
  `sum(rate(reviewbot_claude_tokens_total{type="cache_read"}[1h])) / sum(rate(reviewbot_claude_tokens_total{type=~"input|cache_read|cache_write"}[1h]))`;
  по видам запусков — то же с `sum by (kind) (...)` в числителе и знаменателе (вторая
  серия панели «Доля кэша во входных токенах»);
- доля замечаний, отклонённых перепроверкой, за неделю:
  `sum(increase(reviewbot_verify_verdicts_total{verdict="reject",severity!="breaking"}[7d])) / sum(increase(reviewbot_verify_verdicts_total{severity!="breaking"}[7d]))`;
- 95-й перцентиль длительности ревью:
  `histogram_quantile(0.95, sum by (le) (rate(reviewbot_job_duration_seconds_bucket{kind="review"}[1h])))`.

## Как это устроено

Пакет `internal/metrics` — небольшая реализация формата Prometheus без
внешних зависимостей (клиентская библиотека Prometheus не используется).
`internal/telemetry` описывает метрики и навешивает их декораторами, не
меняя логику ревью: обёртка обработчика очереди (статус берётся из
`internal/diag`, поэтому совпадает с `/runs`), `core.Runner` (запуски Claude),
`core.Limiter` (отказы), compat/lint-чекеры (результаты инструментов),
HTTP-обработчик вебхука (по коду и телу ответа) и наблюдатель запросов
`gitlab.Client.SetObserver` (каждая попытка). Длина очереди читается при
скрейпе через `queue.Queue.Stats()`.

# Документация

## Запуск и эксплуатация

| Документ | О чём |
|---|---|
| [setup-gitlab.md](setup-gitlab.md) | бот-аккаунт и служебный токен, роли, группы, вебхуки или опрос API, подключение репозитория (`.reviewbot.yaml`), настройки проектов |
| [deploy.md](deploy.md) | быстрый старт (`reviewbot init`), проверка (`reviewbot doctor`, `/setup`), образы в ghcr.io, docker compose, локальный режим, каталог данных, обновления, бэкапы, переменные окружения |
| [configuration.md](configuration.md) | все ключи `config.yaml` и `.reviewbot.yaml`; проверка `.reviewbot.yaml` (заметка в MR, `reviewbot validate-repo-config`, JSON Schema [reviewbot.schema.json](reviewbot.schema.json)) |
| [security.md](security.md) | модель изоляции, что никуда не попадает, защита от prompt injection, остаточные риски |
| [usage-and-diagnostics.md](usage-and-diagnostics.md) | страницы `/usage`, `/runs` и `/map`, команда `reviewbot runs`; страница `/problems` — в [features/problems.md](features/problems.md) |
| [design-review.md](design-review.md) | дизайн-ревью веб-страниц (`/`, `/usage`, `/runs`, `/problems`, `/knowledge`, `/map`): что исправлено и что осталось |
| [metrics.md](metrics.md) | метрики Prometheus (`/metrics`), проверка готовности `/readyz`, примеры алертов |
| [features/tracing.md](features/tracing.md) | трейсы OpenTelemetry (OTLP/HTTP): трасса на задачу со спанами GitLab, анализаторов, Claude и публикации; Tempo, Collector, Alloy, Grafana Cloud; корреляция с логами и `/runs`; приватность |
| [troubleshooting.md](troubleshooting.md) | симптом → причина → решение |

## Возможности

| Документ | О чём |
|---|---|
| [features/review.md](features/review.md) | какие репозитории ревьюятся (opt-in), саммари, схема, inline-замечания, инкрементальное ревью, черновики, блокировка мержа |
| [features/verify.md](features/verify.md) | перепроверка critical/major замечаний перед публикацией: отдельный запуск пытается опровергнуть каждое (подтвердить, понизить, отклонить), блок «🧪 Перепроверка» в саммари |
| [features/compat.md](features/compat.md) | проверка обратной совместимости (trunk-based development) |
| [features/coverage.md](features/coverage.md) | покрытие добавленных строк из артефакта CI, непокрытые строки для модели |
| [features/tbd.md](features/tbd.md) | блок «🌿 Trunk-based»: размер MR, возраст ветки, отставание, пайплайн; правило про флаги |
| [features/migrations.md](features/migrations.md) | безопасность миграций БД для trunk-based: подсказки Claude (опасные операции, кандидаты использования в задеплоенном коде), политика severity, проверка `deployed_usage` |
| [features/ci.md](features/ci.md) | разбор упавших пайплайнов: дайджест упавших job и тестов в саммари MR, объяснение причины от Claude в треде, «кто сломал main» — заметка в MR-виновнике |
| [features/lint.md](features/lint.md) | секреты (gitleaks), линтеры из отчётов CI (`lint.ci`: Code Quality, SARIF, Checkstyle), «TODO без задачи» |
| [features/jira.md](features/jira.md) | связь с задачей Jira и формат заголовка MR |
| [features/context.md](features/context.md) | чтение других репозиториев групп |
| [features/sysmap.md](features/sysmap.md) | карта системы: кто какие эндпоинты, вебхуки, WebSocket/SSE предоставляет и потребляет; затронутые потребители в ревью |
| [features/description-summary.md](features/description-summary.md) | саммари изменений в описании MR («Саммари от ревью-бота»): свой блок между маркерами, отказ автора, права и токен |
| [features/labels.md](features/labels.md) | автоматические метки MR: по путям, фактам бота и условиям для модели; scoped-метки, снятие только своих меток, права и токен |
| [features/problems.md](features/problems.md) | проблемы проверки: что бот сам не смог проверить (инструменты, отчёты, доступы, обрезанный дифф, сообщения модели) — блок в саммари, упоминание ответственных, страница `/problems` |
| [features/mcp.md](features/mcp.md) | MCP-инструменты бота в ревью: данные по запросу вместо больших разделов промпта и проверка каждого замечания на ходу теми же правилами, что при публикации (включено по умолчанию; если CLI не подключился — повтор без MCP) |
| [features/learning.md](features/learning.md) | правила команды из ответов и действий |
| [features/memory.md](features/memory.md) | память проекта о сборке, тестах и структуре |
| [features/skills.md](features/skills.md) | скиллы команды (`SKILL.md`) на ревью и в ответах |
| [features/commands.md](features/commands.md) | все команды, синонимы и права |
| [features/reactions.md](features/reactions.md) | реакции бота: 👀 команда принята, ✅ / ❌ / ⏳ / 🚫 итог, ❤️ / 👍 на «спасибо» и «ок» без запуска Claude, 🧠 правило, 🎉 чистое ревью |
| [features/limits.md](features/limits.md) | лимиты, отложенные ревью, повторы |

## Для разработчиков

- [e2e/local/README.md](../e2e/local/README.md) — локальный стенд: GitLab CE и runner в Docker, тестовые пользователи, проекты и MR для проверочных сценариев ([SCENARIOS.md](../e2e/local/SCENARIOS.md))

| Документ | О чём |
|---|---|
| [DESIGN.md](DESIGN.md) | архитектура, пакеты, маркеры заметок, потоки (на английском) |
| [SANDBOX.md](SANDBOX.md) | ссылка на [security.md](security.md) |

Термины: **бот-аккаунт** — пользователь GitLab, от имени которого бот пишет
(`GITLAB_TOKEN`, Reporter); **служебный токен** — токен Maintainer/Owner для
закрытия тредов, назначения ревьюера, меток MR и вебхуков (`GITLAB_SERVICE_TOKEN`);
**песочница** — одноразовый docker-контейнер, в котором работает Claude;
**саммари** — заметка бота «🤖 Ревью» в MR; **блокирующий тред** — открытый тред
бота, который при настройке «All threads must be resolved» не даёт смержить MR.

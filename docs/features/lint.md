# Секреты и линтеры

Кроме Claude, бот публикует детерминированные результаты — **только по строкам,
которые MR добавляет или меняет** (результаты по старому коду и удалённым строкам
отбрасываются):

| Источник | Что ищет | Кто запускает | Важность |
|---|---|---|---|
| **gitleaks** | секреты (ключи, токены, пароли) | бот, в песочнице; сканируются все коммиты MR (`merge base..head`) | 🔴 critical, **всегда блокирует** |
| **линтеры из CI** (`lint.ci`) | всё, что умеют линтеры команды: phpstan, phpcs, ruff, eslint, golangci-lint, semgrep, архитектурные линтеры… | **CI репозитория**; бот читает их отчёты из пайплайна MR | из отчёта; по умолчанию **не блокируют** |
| **todo** | `TODO`/`FIXME`/`XXX`/`HACK` в комментариях без ссылки на задачу ([ниже](#todo-без-задачи)) | всегда (встроен в бота, без песочницы) | `review.todo.severity` (minor); не блокирует, если не `blocking: true` |

**Бот сам линтеры не запускает.** Раньше он гонял phpstan, phpcs, ruff, eslint,
golangci-lint, semgrep и команды `lint.custom` в своей песочнице; теперь их
результаты берутся только из отчётов CI (см. [переход](#переход-со-старых-настроек)).
Так линтеры работают с теми же версиями, конфигами, зависимостями и сервисами, что и
в CI команды, а бот не исполняет код репозитория ради линтеров.

Claude получает результаты как данные и не дублирует их; его замечание, которое
повторяет результат линтера (тот же файл и строка ±1, называет правило или линтер
целым словом), отбрасывается. Общие имена инструментов (`lint`, `linter`, `todo`,
`check`, `sast`, … — например, имя job по умолчанию) не считаются названием линтера.
Замечание никогда не отбрасывается ради более слабого результата: если его
severity выше, чем у результата линтера, или оно блокирует мерж, а результат нет,
публикуются оба.

## Линтеры из CI

Нужны две вещи: job в `.gitlab-ci.yml`, которая сохраняет отчёт линтера, и запись в
`.reviewbot.yaml`, где этот отчёт лежит.

```yaml
# .reviewbot.yaml
lint:
  ci:
    - job: "phpstan"            # имя job (можно glob) в пайплайне последнего коммита MR
      artifact: "phpstan.json"  # путь к отчёту в артефактах job, ИЛИ:
      # report: codequality     # отчёт GitLab Code Quality этой job
      format: auto              # codeclimate | sarif | checkstyle | auto
      tool: "phpstan"           # имя в MR: ci:phpstan (по умолчанию — имя job)
      severity: ""              # уровень для всех результатов; пусто — из отчёта
      blocking: false           # true — блокирует по review.block_min_severity
```

| Ключ | Что значит |
|---|---|
| `job` | Имя job в **пайплайне последнего коммита MR** (head pipeline). Можно glob: `lint:*`, `phpstan*`. Параллельные job (`phpstan 1/2`, `phpstan 2/2`) подходят под имя без номера — их отчёты объединяются. |
| `artifact` | Путь к файлу отчёта в артефактах job (`artifacts:paths`). |
| `report: codequality` | Взять отчёт GitLab Code Quality этой job (`artifacts:reports:codequality`). Нужен GitLab 19.4+ (скачивание отчёта по типу через API); на более старом GitLab добавьте тот же файл в `artifacts:paths` — бот найдёт его в архиве. |
| `format` | `auto` (по умолчанию) определяет по содержимому: JSON-массив — CodeClimate, JSON с `runs` — SARIF, XML — Checkstyle. |
| `tool` | Имя линтера в тредах и саммари (`ci:<tool>`), ключ для `severity_overrides`. Для job с glob задайте явно. |
| `severity` | Перекрыть уровень всех результатов (`critical`/`major`/`minor`/`nit`). |
| `blocking` | По умолчанию линтеры мерж не блокируют. С `blocking: true` результат блокирует, если его уровень не ниже `review.block_min_severity` (например, архитектурный линтер с `severity: critical`). |

До 10 записей на репозиторий. `lint.enabled: false` выключает и их.

### Форматы

| Формат | Что читается | Уровни |
|---|---|---|
| **CodeClimate / GitLab Code Quality** (JSON-массив; поток объектов через NUL тоже) | `description`, `check_name` (правило), `severity`, `location.path`, `location.lines.begin` или `location.positions.begin.line` (`end` — для многострочных) | `blocker`, `critical` → critical; `major` → major; `minor` и без уровня → minor; `info` → nit |
| **SARIF 2.1.0** | `runs[].results[]`: `ruleId`, `level`, `message.text`, первая `physicalLocation` (`artifactLocation.uri`, `region.startLine/endLine`); ссылка на правило — `tool.driver.rules[].helpUri` | `error` → major, `warning` (и без уровня) → minor, `note`/`none` → nit |
| **Checkstyle XML** | `<file name>` / `<error line severity message source>` | `error` → major, `warning` → minor, `info`/`ignore` → nit |

Пути в отчётах могут быть относительными, с `./` или абсолютными путями раннера
(`/builds/group/project/src/a.php`) — бот сопоставляет их с файлами MR. Отчёт больше
20 МБ (в том числе после распаковки gzip) не читается. Из отчёта берутся не больше
300 результатов на изменённых строках.

### Примеры `.gitlab-ci.yml`

Каждый пример показывает оба способа: `artifacts:reports:codequality` (виджет Code
Quality в самом GitLab, в `.reviewbot.yaml` — `report: codequality`) и обычный
артефакт (`artifact: <путь>`). Достаточно одного; `paths` вместе с `reports` — самый
совместимый вариант (работает и на GitLab старше 19.4).

`allow_failure: true` и `artifacts: when: always` — чтобы найденные замечания не
роняли пайплайн и отчёт сохранялся, даже когда линтер завершился с ошибкой. Бот
читает отчёт и у упавшей job.

**PHPStan** (`--error-format=gitlab` — формат Code Quality):

```yaml
phpstan:
  stage: test
  script:
    - composer install --no-interaction --no-progress
    - vendor/bin/phpstan analyse --no-progress --error-format=gitlab > phpstan.json
  allow_failure: true
  artifacts:
    when: always
    paths: [phpstan.json]
    reports:
      codequality: phpstan.json
```

```yaml
# .reviewbot.yaml
lint:
  ci:
    - job: phpstan
      report: codequality        # или artifact: phpstan.json
```

**PHP_CodeSniffer** — отчёт Checkstyle (встроенный) или Code Quality через
сторонний репортер (например, `micheh/phpcs-gitlab`):

```yaml
phpcs:
  stage: test
  script:
    - composer install --no-interaction --no-progress
    - vendor/bin/phpcs --report=checkstyle --report-file=phpcs.xml
  allow_failure: true
  artifacts:
    when: always
    paths: [phpcs.xml]
```

```yaml
lint:
  ci:
    - job: phpcs
      artifact: phpcs.xml        # format: auto → checkstyle
```

**ruff** (`--output-format=gitlab`):

```yaml
ruff:
  stage: test
  image: python:3.12-slim
  script:
    - pip install ruff
    - ruff check --output-format=gitlab --output-file=ruff.json . || true
  artifacts:
    when: always
    paths: [ruff.json]
    reports:
      codequality: ruff.json
```

**ESLint** — форматтер Code Quality (`eslint-formatter-gitlab`) или Checkstyle:

```yaml
eslint:
  stage: test
  script:
    - npm ci                                    # eslint-formatter-gitlab в devDependencies
    - npx eslint --format gitlab . || true      # пишет отчёт в ESLINT_CODE_QUALITY_REPORT
  variables:
    ESLINT_CODE_QUALITY_REPORT: eslint.json
  artifacts:
    when: always
    paths: [eslint.json]
    reports:
      codequality: eslint.json
```

**golangci-lint** — Code Climate: v1 `--out-format code-climate`, v2
`--output.code-climate.path`:

```yaml
golangci-lint:
  stage: test
  image: golangci/golangci-lint:v2.15.0
  script:
    - golangci-lint run --output.code-climate.path=gl-code-quality.json --output.text.path=stdout ./...
    # v1: golangci-lint run --out-format code-climate:gl-code-quality.json,line-number
  allow_failure: true
  artifacts:
    when: always
    paths: [gl-code-quality.json]
    reports:
      codequality: gl-code-quality.json
```

**semgrep** (`--sarif`):

```yaml
semgrep:
  stage: test
  image: semgrep/semgrep
  script:
    - semgrep scan --config p/default --sarif --output semgrep.sarif || true
  artifacts:
    when: always
    paths: [semgrep.sarif]
```

```yaml
lint:
  ci:
    - job: semgrep
      artifact: semgrep.sarif
      blocking: true             # SAST: ERROR → major, блокирует при block_min_severity: major
```

Любой другой линтер подключается так же, если умеет писать CodeClimate, SARIF или
Checkstyle (go-arch-lint, deptrac, import-linter через конвертер, ktlint, …).

Линтер, который в CI просто падает с логом без отчёта, бот не разбирает построчно —
но упавшую job объясняет функция разбора падений CI (отдельная возможность бота).
Здесь такая запись даёт строку «⚠️ … job упала, файла … нет» и проблему проверки
`lint_failed`.

### Когда появляются треды

- **Job уже завершилась** (успешно или с ошибкой) — отчёт читается сразу, треды
  публикуются вместе с ревью.
- **Job ещё идёт** (или пайплайн последнего коммита ещё не создан) — остальное ревью
  публикуется сразу, в саммари строка
  `🔧 Линтеры из CI: ⏳ ждём job `phpstan``. Когда пайплайн завершится, бот (задача
  `refresh_status`, как у блока покрытия: проверка раз в 2 минуты, до 3 часов)
  перечитывает отчёты, публикует и закрывает треды линтеров, обновляет блок и строку
  блокировки мержа — **без нового запуска Claude** (Claude этих результатов не видел,
  поэтому дубль его открытого замечания на той же строке не публикуется).
- **Job упала без отчёта** — строка в блоке и проблема проверки `lint_failed`.
- **Job нет в пайплайне** (переименовали, не запустилась по `rules`) или у MR нет
  пайплайна — строка в блоке и проблема `lint_failed`. Ручная (`manual`), пропущенная
  или отменённая job — «не выполнялась».

### Как выглядит в MR

Inline-тред по результату линтера:

```markdown
**🔧 ci:phpstan** · `variable.undefined` · 🟠 Major

_Не блокирует мерж._

Variable $undefined might not be defined.

_Результат ci:phpstan (автоматическая проверка, без участия Claude)._
```

Неблокирующие треды сразу закрываются служебным токеном. Неблокирующий результат на
строке вне диффа попадает в саммари.

В саммари — свёрнутый блок:

```markdown
<details><summary>🔧 Линтеры из CI: 3 замечания на изменённых строках, нет отчёта: 1</summary>

| Линтер | Job | Результат |
|---|---|---|
| ci:phpstan | phpstan | 2 на изменённых строках (всего в отчёте: 41) |
| ci:ruff | `ruff 1/2`, `ruff 2/2` | 1 на изменённых строках |
| ci:eslint | `eslint` | ⚠️ нет отчёта |

новых тредов: 2, уже обсуждаются: 1.

⚠️ ci:eslint: в артефактах job `eslint` нет файла `eslint.json` (или артефакты истекли).
</details>
```

- Тот же результат на следующем ревью не публикуется повторно (id = линтер +
  правило + файл + сообщение без номеров строк).
- Тред `ci:` закрывается, когда отчёт **для нового коммита прочитан** и этого
  результата в нём больше нет. Пока job идёт, упала без отчёта, отчёт не разобран
  или прочитан частично — треды остаются.
- Тред линтера, которого больше нет в `lint.ci`, остаётся как есть.
- Блокирующий тред, закрытый человеком, больше не блокирует и не публикуется заново.

Уровни переопределяются в конфиге сервиса: `review.lint.severity_overrides` с
ключами `ci:<tool>` и `ci:<tool>:<правило>`.

## Секреты (gitleaks)

gitleaks бот по-прежнему запускает сам — это не линтер, а проверка истории: секрет
нужно найти во **всех коммитах MR**, включая добавленный и тут же удалённый, а CI
обычно смотрит только на итоговое дерево. Запуск — отдельный контейнер песочницы:
`--network none`, без токенов, argv без shell, таймаут `review.lint.timeout`;
сканируется только каталог `.git` клона.

Каждый найденный секрет — отдельный блокирующий тред. Значение секрета нигде не
показывается: ни в треде, ни в саммари, ни в промпте Claude, ни в логах (бот даже не
читает эти поля отчёта gitleaks).

```markdown
**🔑 🔧 gitleaks** · `aws-access-token` · 🔴 Critical

⛔ **Блокирует мерж: в изменениях найден секрет.** Его секрет нужно отозвать и удалить
из истории: удаления в новом коммите недостаточно, значение остаётся в истории git.
Отзовите (смените) секрет, перепишите историю ветки без него и закройте тред.

_Файл `config/dev.env`, строка 3, коммит `1a2b3c4d5e6f`._

_Значение секрета намеренно не показывается._
```

- Секрет, добавленный и удалённый внутри MR, всё равно находится: он остаётся в
  истории («в истории коммитов MR, не в итоговом диффе»).
- Комментарии `gitleaks:allow` игнорируются, а `.gitleaksignore` и `.gitleaks.toml`
  берутся из **целевой ветки**: MR не может сам разрешить свой секрет.
- Тред закрывается автоматически, когда gitleaks отработал полностью и больше не
  находит секрет в коммитах MR (например, после переписывания истории), с
  напоминанием, что секрет всё равно нужно отозвать.

В саммари сразу после строки статуса — `### 🔑 Найдены секреты: N` (если есть), а
после раздела совместимости — свёрнутый блок «🔧 Линтеры и сканеры» со статусом
gitleaks и встроенной проверки «TODO без задачи».

Репозиторий не может выключить gitleaks, пока сервис не разрешил
(`review.lint.allow_disable_secrets: true`); попытка отмечается в саммари: «ℹ️
`.reviewbot.yaml` отключает gitleaks, но сервис это не разрешает…».

## TODO без задачи

Отложенная работа без задачи теряется: через полгода никто не помнит, что значит
`// TODO: переделать`. Поэтому маркер в комментарии, **добавленном MR**, должен
ссылаться на задачу; иначе бот открывает тред и просит её завести. Проверка
встроена в бота: без песочницы, без внешних программ и без Claude, работает и при
`review.lint.enabled: false`.

**Что считается маркером.** Слова из `review.todo.keywords` (по умолчанию `TODO`,
`FIXME`, `XXX`, `HACK`) — заглавными, целым словом, **внутри комментария**:
`// TODO`, `# FIXME`, `/* HACK */`, строка докблока ` * XXX`, `-- TODO` (SQL),
`<!-- TODO -->`, `{# TODO #}` (Twig/Jinja), `; TODO` (INI, Lisp), `% TODO` (TeX,
Erlang), docstring Python. Не маркеры: `todo`, `TODOs`, `MY_TODO`,
`context.TODO()`, `TODO()`, текст вне комментариев (`"TODO list"` в строке кода,
`TODO` в тексте Markdown). В строке берётся первый маркер.

**Что считается ссылкой на задачу** — в том же комментарии или на следующей
строке, если она тоже только комментарий (продолжение):

| Форма | Пример |
|---|---|
| ключ Jira проекта из `jira.project_keys` (если проекты не заданы — любой `ABC-123`) | `// TODO(PROJ-123): убрать после миграции`, `# TODO PROJ-123 …` |
| задача GitLab | `// TODO #42`, `// FIXME(group/project#42)` |
| URL задачи | `// FIXME: см. https://jira.example.com/browse/PROJ-5`, `…/-/issues/7`, `github.com/o/r/issues/7` |

```go
// TODO(PROJ-123): убрать после переезда на v2     ← ок
// TODO: переделать кэш                             ← тред «TODO без задачи»
// FIXME: гонка при инвалидации,
// см. PROJ-77                                      ← ок (строка-продолжение)
var hint = "TODO list"                              ← не комментарий, не проверяется
```

Без заданных `jira.project_keys` ключами не считаются `UTF-8`, `SHA-256`,
`ISO-8601`, `RFC-7231` и подобные, а также `TODO-1` (префикс — сам маркер). Ключ
чужого проекта при заданных `project_keys` (`OPS-1`, а проекты `PROJ`) — не
ссылка. Ключ задачи MR бот **не подставляет**: TODO — отложенная работа, и задача
MR для неё почти всегда не та.

**Тред:**

```markdown
**🔧 todo** · TODO без задачи · 🟡 Minor

_Не блокирует мерж._

> TODO: переделать кэш

Отложенная работа без задачи теряется. Заведите задачу и укажите её ключ в комментарии:

    TODO(PROJ-123): …

Формат ключа: `PROJ-123` (проекты Jira: `PROJ`). Подойдёт и ссылка на задачу GitLab (`#123`, `group/project#123`) или URL задачи.
```

- Треды — как у остальных инструментов: id = файл + маркер + текст комментария
  (пробелы и числа нормализуются), поэтому сдвиг строки не создаёт новый тред.
  Тред закрывается, когда в следующем пуше у комментария появилась задача или он
  удалён. Одинаковые комментарии в одном файле — один тред.
- Проверяются только **добавленные** строки: старые TODO файла не всплывают, но
  строка со старым TODO, которую MR изменил, проверяется.
- Замечание Claude о том же TODO (та же строка и слово «todo») отбрасывается как
  дубликат — только если оно не серьёзнее треда todo (critical-замечание рядом с
  TODO публикуется, см. выше).
- В саммари — строка `todo` в «🔧 Линтеры и сканеры».

**Что не проверяется.** Файлы из `review.todo.ignore` (по умолчанию `vendor/**`,
`node_modules/**`, `**/*.min.js`, `**/*.min.css`; glob без `/` сравнивается и с
именем файла: `*.pb.go`), сгенерированные файлы (в первых 20 строках `// Code
generated … DO NOT EDIT.`, `@generated`, `<auto-generated>`, «This file was
automatically generated»), бинарные файлы, JSON и `.txt`. Тесты проверяются;
чтобы исключить — `todo.ignore: ["**/*_test.go"]`.

**Как определяется комментарий** (эвристика по строкам, язык — по имени файла):
строковые литералы (`"…"`, `'…'`, `` `…` ``) пропускаются; блочные комментарии,
многострочные строки Go/JS (обратные кавычки) и тройные кавычки Python
отслеживаются между строками (строка в тройных кавычках, с которой начинается
строка кода, — docstring, то есть комментарий); кавычка, не закрытая в той же
строке, считается апострофом. Ограничения: heredoc, raw-строки других языков
(`r"…"`, тройные кавычки в Kotlin/Scala), регулярные выражения с кавычками или
`//` и вложенные блочные комментарии не разбираются — маркер может быть
пропущен или, редко, найден в строке. `#` в shell/YAML — комментарий только в
начале строки или после пробела (`${#arr}`, `url#frag` — не комментарии). В HTML
комментарии JS внутри `<script>` не ищутся (во Vue/Svelte — ищутся).
Неизвестное расширение: `//`, `#`, `--` после пробела, `/* */`, `<!-- -->`. Если
файл не читается на голове MR, проверяются только строки диффа.

**Проверка ключей в Jira** (`review.todo.verify_keys: true`, Jira настроена):
ключи из комментариев ищутся в Jira (только чтение). Ключа нет или у бота нет
доступа → тред «задача PROJ-999 не найдена» (🟡 Minor, не блокирует). Если в
комментарии есть хоть одна существующая задача, ссылка на GitLab или URL —
претензий нет (ссылки не проверяются). Не больше 10 запросов к Jira за ревью
(остальные ключи не проверяются, в саммари примечание), ответы кэшируются на час.
Ошибка Jira не роняет ревью: ключ считается правильным, в «Не удалось проверить»
— «Задача Jira не получена», проверка todo — «выполнена частично» (её треды в
этом ревью не закрываются).

**Блокировка.** По умолчанию не блокирует. `blocking: true` (в сервисе или
репозитории) — замечание блокирует, если его уровень не ниже
`review.block_min_severity` (например, `severity: major` при пороге `major`).
Репозиторий может включить блокировку, но не выключить включённую в сервисе;
выключить проверку целиком (`todo.enabled: false`) — может.

```yaml
# config.yaml
review:
  todo:
    keywords: [TODO, FIXME, XXX, HACK]
    severity: minor
    blocking: false
    ignore: ["vendor/**", "node_modules/**", "**/*.min.js", "**/*.min.css"]
    verify_keys: true

# .reviewbot.yaml
todo:
  enabled: true                 # false — не проверять репозиторий
  keywords: [TODO, FIXME]       # заменяет список сервиса
  severity: nit
  ignore: ["legacy/**"]         # добавляется к списку сервиса
```

## Настройка

```yaml
# config.yaml
review:
  lint:
    enabled: true              # false — ни gitleaks, ни lint.ci
    timeout: "3m"              # на один запуск gitleaks
    tools: [gitleaks]
    severity_overrides:
      "ci:ruff": nit
      "ci:semgrep:python.lang.security.audit.formatted-sql-query": critical
    allow_disable_secrets: false
```

```yaml
# .reviewbot.yaml
lint:
  enabled: true                # false — всё, кроме gitleaks
  disable: []                  # [gitleaks] — только если сервис разрешил
  ci:
    - job: phpstan
      report: codequality
```

## Переход со старых настроек

Ключи прежней схемы (бот запускал линтеры сам) теперь — **ошибка конфигурации** с
текстом `removed: linters now come from CI reports — configure `lint.ci`, see
docs/features/lint.md`:

| Где | Ключ | Что делать |
|---|---|---|
| `config.yaml` | `review.lint.semgrep_rules`, `review.lint.allow_custom` | удалить (сервис не стартует, пока они есть) |
| `config.yaml` | `review.lint.tools` с `semgrep`, `golangci-lint`, `ruff`, `eslint`, `phpstan`, `phpcs` | оставить `[gitleaks]` (или удалить ключ) |
| `config.yaml` | `review.lint.severity_overrides` с ключами `ruff`, `phpstan:…`, `custom:…` | переименовать в `ci:<tool>` / `ci:<tool>:<правило>` |
| `.reviewbot.yaml` | `lint.custom`, `lint.semgrep_configs`, бывшие линтеры в `lint.disable` | перенести линтеры в CI и описать отчёты в `lint.ci` (пока файл с этими ключами, ревью в репозитории не идёт — бот отвечает на команды ошибкой разбора) |

Ключа `setup` больше нет вовсе (модель не запускает команд, бот не готовит окружение;
[удалённые ключи](../configuration.md#удалённые-ключи)): если он стоял ради линтеров
(`composer install`, `npm ci`, `go mod download` для golangci-lint), удалите его —
`.reviewbot.yaml` с `setup` не разбирается. Зависимости линтерам ставит сам CI. Открытые
треды прежних линтеров (`semgrep`, `phpstan`, …, `custom:<имя>`) бот больше не
закрывает: неблокирующие уже закрыты, блокирующие (`semgrep`, `custom` с
`blocking: true`) закройте вручную.

## Ограничения

| Ограничение | Значение |
|---|---|
| записей `lint.ci` | 10 |
| отчёт | 20 МБ (и после распаковки gzip) |
| результатов на линтер | первые 300 на изменённых строках («показаны первые 300 результатов»; старые треды тогда не закрываются) |
| `report: codequality` | GitLab 19.4+ или тот же файл в `artifacts:paths` |
| ожидание job | пока цепочка `refresh_status` жива: до 3 часов после ревью |
| gitleaks | только `.git` клона; конфиг и ignore — из целевой ветки |
| todo | первые 100 результатов; файлы до 4 МБ (больше — только строки диффа); `verify_keys` — 10 запросов к Jira за ревью |

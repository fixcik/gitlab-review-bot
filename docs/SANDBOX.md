# Модель изоляции

Документ перенесён в [security.md](security.md):

- [модель изоляции](security.md#модель-изоляции): сеть, файлы, процесс, права Claude
  (только чтение, без Bash), безопасный git;
- [что никуда не попадает](security.md#что-никуда-не-попадает);
- [что разрешено писать](security.md#что-разрешено-писать);
- [prompt injection](security.md#prompt-injection);
- [остаточные риски](security.md#остаточные-риски);
- [чек-лист усиления](security.md#чек-лист-усиления).

Локальный режим без изоляции — в [deploy.md](deploy.md#локальный-режим-sandboxmode-local),
линтеры и проверки совместимости в песочнице — в [features/lint.md](features/lint.md) и
[features/compat.md](features/compat.md). Устройство для разработчиков — в
[DESIGN.md](DESIGN.md#sandbox).

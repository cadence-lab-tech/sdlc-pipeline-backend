# CLAUDE.md — sdlc-pipeline-backend

Go-сервис SDLC Pipeline: REST для UI, MCP-сервер для локальных агентов, воркеры
индексации репозиториев и детектора дрейфа.

## Прежде чем писать код

Домен описан не здесь, а в [sdlc-pipeline-docs](https://github.com/stepaKaziatko/sdlc-pipeline-docs).
Локально — соседняя директория `../docs/`:

- `../docs/ARCHITECTURE.md` — модель домена, гейты, дрейф, план по фазам
- `../docs/decisions/README.md` — принятые решения
- `../docs/MCP_CONTRACT.md` — контракт агента, если задача касается MCP

Самая точная спецификация домена — `migrations/`. При расхождении с документом
права миграция.

## Стек

Go 1.23, Gin, pgx (без ORM), PostgreSQL 17 + pgvector, эмбеддинги Voyage
`voyage-code-3` размерности 1024 ([решение 0008](../docs/decisions/0008-voyage-embeddings.md)).

## Структура

```
cmd/api/            HTTP + MCP сервер
cmd/worker/         индексация, детектор дрейфа, протухшие аренды, эмбеддинги
cmd/sdlc/           CLI для агентов и CI
internal/
  config/
  httpapi/          роутер Gin, хендлеры, middleware (в т.ч. резолвер актора)
  mcp/              MCP-сервер: тулы поверх тех же сервисов, что и REST
  domain/           сущности и бизнес-правила
  workflow/         движок: переходы, прогон гейтов, аренды
  drift/            базовые линии, детектор, разрешение сигналов
  store/postgres/   репозитории поверх pgx
  blobstore/        inline + S3-совместимый
  github/           чтение репозиториев
  indexer/          обход, чанкинг, tree-sitter, дельты
  embed/            Voyage
migrations/
docs/               референсы реализации (не описания решений)
```

MCP-тулы и REST-хендлеры вызывают одни и те же сервисы. Логика не дублируется —
это два транспорта над одним ядром.

## База данных

```bash
docker compose up -d
for f in migrations/*.up.sql; do
  docker exec -i sdlc-postgres psql -v ON_ERROR_STOP=1 -U sdlc -d sdlc < "$f"
done
docker exec -i sdlc-postgres psql -U sdlc -d sdlc < migrations/seed/default_workflow.sql
docker exec -i sdlc-postgres psql -U sdlc -d sdlc < migrations/seed/drift_demo.sql  # опционально
```

Postgres на `localhost:55432`, база/пользователь/пароль — `sdlc`.

### Конвенции миграций

- Нумерация `NNNNNN_name.up.sql` / `.down.sql`, шесть цифр, пара обязательна.
- **Применённая миграция не редактируется.** Изменение схемы — новая миграция.
- Каждая миграция прогоняется на живой базе перед коммитом: up → down → up.
  Ограничения проверяются негативным тестом, а не на глаз.
- Enum'ы — `text` + `CHECK`, не типы Postgres: расширять проще. Down-миграция,
  сужающая `CHECK`, обязана сначала удалить строки отменяемого типа — иначе
  `ADD CONSTRAINT` не пройдёт валидацию на засиженной базе.
- Комментарии в SQL объясняют **почему** поле такое, а не что оно хранит.

### Инварианты схемы

- `workflow_statuses` несёт `UNIQUE (workflow_id, id)` ради композитных FK.
  `tasks (workflow_id, status_id)` ссылается составным ключом — база физически
  не даёт поставить задаче статус из чужого цикла. Не ломать это при рефакторинге.
- Частичные уникальные индексы держат «один активный workflow на проект», «один
  начальный статус», «одна основная роль на статус».
- `task_drift_signals_open_uniq` делает детектор дрейфа идемпотентным.
  Детектор пишется через `ON CONFLICT DO NOTHING`.
- `enforce_task_repo_scope()` — отложенный constraint-триггер. Отложенность
  принципиальна: без неё нельзя одной транзакцией включить `multi_repo` и
  расширить область задачи. Не менять на обычный триггер.
- `workflow_statuses.position` — `DEFERRABLE INITIALLY DEFERRED`;
  переупорядочивание идёт одной транзакцией.

## Что нельзя делать

- **Хардкодить роли и статусы.** Никаких `if status == "analysis"`. Движок
  работает с переходами и гейтами, поведение роли — данные в `role_versions`.
- **Писать в репозитории пользователя.** Доступ только на чтение
  ([`repositories.access_mode`](migrations/000001_core.up.sql)).
- **Класть секреты в БД.** `repositories.credential_ref` хранит имя секрета во
  внешнем хранилище, не токен.
- **Обходить `principals` в аудите.** Все поля актора — внешние ключи.

## Референсы реализации

`docs/drift_detection.sql` — запросы детектора дрейфа, проверенные на сценарии
из `migrations/seed/drift_demo.sql`. Воркер должен исполнять именно их; при
изменении — перепроверить на том же сценарии.

## Статус

Фаза 0: схема и контракт зафиксированы, кода нет. Следующий шаг — `cmd/api`,
`internal/store/postgres`, движок переходов.

# SDLC Pipeline — Backend

Go-сервис, ведущий задачи по настраиваемому циклу разработки: REST API для UI,
MCP-сервер для локальных агентов, воркеры индексации репозиториев и детектора
дрейфа.

Документация по домену вынесена в отдельный репозиторий
[sdlc-pipeline-docs](https://github.com/stepaKaziatko/sdlc-pipeline-docs):

- [Архитектура](https://github.com/stepaKaziatko/sdlc-pipeline-docs/blob/main/ARCHITECTURE.md) — модель домена, гейты, дрейф, схема БД, план по фазам
- [Контракт агента](https://github.com/stepaKaziatko/sdlc-pipeline-docs/blob/main/MCP_CONTRACT.md) — MCP-тулы для локального агента
- [Принятые решения](https://github.com/stepaKaziatko/sdlc-pipeline-docs/tree/main/decisions) — почему устроено именно так

Здесь остаются референсы реализации: [drift_detection.sql](docs/drift_detection.sql) —
запросы детектора дрейфа, которые исполняет воркер.

Фронтенд: [sdlc-pipeline-frontend](https://github.com/stepaKaziatko/sdlc-pipeline-frontend).

## Стек

Go 1.23, Gin, pgx (без ORM), PostgreSQL 17 + pgvector, эмбеддинги Voyage
(`voyage-code-3`, 1024).

## Запуск

```bash
make up      # база + приложение в контейнерах
make logs
make reset   # снести вместе с данными
```

Подробности — [dev/README.md](dev/README.md). API поднимается на
`localhost:8088`, Postgres на `localhost:55432`.

Схему приложение накатывает само: миграции встроены в бинарник, поэтому образ
самодостаточен и отдельный шаг с миграциями в compose не нужен. На пустой базе
дополнительно применяется сид с демонстрационным проектом.

Без контейнеров:

```bash
export SDLC_DATABASE_URL='postgres://sdlc:sdlc@localhost:55432/sdlc?sslmode=disable'
export SDLC_SEED=default_workflow
make build && ./bin/api
```

## API

Аутентификации нет ([решение 0010](https://github.com/stepaKaziatko/sdlc-pipeline-docs/blob/main/decisions/0010-auth-deferred.md)):
действующее лицо приезжает заголовком `X-SDLC-Actor` и заводится в `principals`
при первом обращении.

| Метод | Путь | Что делает |
|---|---|---|
| `GET` | `/healthz`, `/readyz` | пробы; `readyz` проверяет базу |
| `GET`/`POST` | `/v1/projects` | список и создание проектов |
| `GET` | `/v1/projects/{key}` | проект, репозитории, определения свойств задач |
| `GET` | `/v1/projects/{key}/workflow` | активный цикл: статусы с ролями, переходы с гейтами |
| `GET`/`POST` | `/v1/projects/{key}/roles` | роли проекта; создание вместе с первой версией |
| `GET`/`PATCH`/`DELETE` | `/v1/projects/{key}/roles/{role}` | роль, вывеска, архивирование |
| `GET`/`POST` | `.../roles/{role}/versions` | версии инструкции; правка — новая версия |
| `GET`/`POST` | `/v1/projects/{key}/workflows` | версии цикла; создание черновика копией активного |
| `GET`/`DELETE` | `/v1/workflows/{id}` | версия цикла целиком; удаление — только черновика |
| `GET` | `/v1/workflows/{id}/validation` | проверка цикла как графа, без изменений |
| `POST` | `/v1/workflows/{id}/activate` | сделать черновик действующим циклом |
| `POST`/`PUT` | `/v1/workflows/{id}/statuses` | добавить статус; `PUT` — порядок колонок целиком |
| `PATCH`/`DELETE` | `/v1/workflows/{id}/statuses/{status}` | правка и удаление статуса черновика |
| `PUT` | `.../statuses/{status}/roles` | исполнитель статуса и проверяющие |
| `POST`/`PATCH`/`DELETE` | `.../transitions[/{transition}]` | переходы черновика |
| `POST`/`PATCH`/`DELETE` | `.../transitions/{transition}/gates[/{gate}]` | гейты черновика |
| `GET`/`POST` | `/v1/projects/{key}/tasks` | список и создание задач |
| `GET` | `/v1/tasks/{key}` | карточка: задача, попытки этапов, артефакты, вердикт гейтов |
| `GET` | `/v1/tasks/{key}/timeline` | лента событий |
| `PATCH` | `/v1/tasks/{key}/scope` | свойства и область задачи по репозиториям |
| `GET`/`POST` | `/v1/tasks/{key}/artifacts` | артефакты текущей попытки этапа |
| `GET` | `/v1/tasks/{key}/transitions` | доступные переходы с прогоном гейтов |
| `POST` | `/v1/tasks/{key}/transitions/{transition}` | выполнить переход |
| `POST` | `.../transitions/{transition}/gates/{gate}/decision` | решение по гейту |
| `GET`/`POST` | `/v1/artifacts/{id}/content`, `/approve` | содержимое и апрув артефакта |

Заблокированный гейтами переход — это `409` с разбором по каждому гейту: что
именно не сдано и что с этим делать.

Роли и цикл правятся версиями, а не на месте: версия роли иммутабельна, а
редактируется только черновик цикла ([решение 0016](https://github.com/stepaKaziatko/sdlc-pipeline-docs/blob/main/decisions/0016-workflow-versions-immutable.md)).
Попытка изменить активный цикл — `409`.

## Структура

```
cmd/api/            HTTP-сервер; в фазе 2 сюда встанет MCP
internal/
  config/           настройки из окружения
  apperr/           ошибки домена с кодом, общим для REST и MCP
  domain/           сущности схемы в Go
  workflow/         движок: переходы, прогон гейтов, артефакты, область задачи;
                    отдельно authoring — сборка ролей и версий цикла
  store/postgres/   запросы, миграции, контент-адресуемые blobs
  httpapi/          роутер Gin, middleware, хендлеры
migrations/         схема и сиды, встроены в бинарник через embed.go
dev/                docker-compose для локального запуска
docs/               референсы реализации
```

## Статус

Фаза 1 на бэкенде закрыта: проекты, задачи, артефакты, движок переходов с
гейтами `artifacts_required`, `checklist`, `human_approval`, `repo_ref_required`,
`repo_scope`, `drift_clear`, редактор ролей и цикла.

Дальше: MCP-сервер и аренды (фаза 2), индексация репозиториев и детектор дрейфа
(фаза 3). Гейты `agent_review`, `schema_validation` и `external_check` до тех
пор держатся флагом `SDLC_STRICT_GATES`.

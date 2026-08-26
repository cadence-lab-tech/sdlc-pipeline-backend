# dev — локальное окружение

Здесь лежит всё, что нужно, чтобы поднять бэкенд целиком на своей машине:
Postgres с pgvector, само приложение и инструменты для разглядывания базы.

## Запуск

```bash
make up        # из корня backend/
make logs      # логи приложения
make down      # остановить
make reset     # остановить и снести данные
```

Или напрямую:

```bash
docker compose -f dev/docker-compose.yml up -d --build
```

| Что | Адрес |
|---|---|
| API | http://localhost:8088 |
| Postgres | `localhost:55432`, база/пользователь/пароль — `sdlc` |
| Adminer | http://localhost:8089 (только с `--profile tools`) |

Порты нестандартные намеренно: 8080 и 5432 на машине разработчика обычно уже
заняты. Переопределяются через `dev/.env` — образец в `.env.example`.

## Что происходит на старте

Приложение накатывает схему само: миграции встроены в бинарник через
`migrations/embed.go`, поэтому отдельного шага с миграциями в compose нет и
разъехаться с версией сервиса он не может.

Следом применяется сид `default_workflow` — **только если база пуста**. Он
разворачивает демонстрационный проект: цикл из восьми статусов, пять ролей с
инструкциями и контрактами, гейты на каждом переходе, три репозитория и эпик с
тремя задачами. Сиды не идемпотентны, поэтому пересобрать демо можно только
через `make reset`.

Чтобы добавить сценарий для детектора дрейфа:

```bash
SDLC_SEED=default_workflow,drift_demo make reset up
```

## Первые запросы

```bash
curl localhost:8088/v1/projects
curl localhost:8088/v1/projects/SDLC/workflow
curl localhost:8088/v1/tasks/SDLC-1

# запустить задачу в цикл
curl -X POST localhost:8088/v1/tasks/SDLC-1/transitions/start \
     -H 'X-SDLC-Actor: analyst-anna'

# посмотреть, что держит следующий переход
curl -s localhost:8088/v1/tasks/SDLC-1/transitions | jq '.transitions[] | select(.key=="analysis_done")'
```

Заголовок `X-SDLC-Actor` — это действующее лицо. Аутентификации пока нет
([решение 0010](https://github.com/stepaKaziatko/sdlc-pipeline-docs/blob/main/decisions/0010-auth-deferred.md)),
заголовку никто не мешает соврать; без него берётся `SDLC_DEFAULT_ACTOR`.

## Переменные окружения

| Переменная | По умолчанию | Зачем |
|---|---|---|
| `SDLC_DATABASE_URL` | — | обязательна |
| `SDLC_HTTP_ADDR` | `:8080` | внутри контейнера |
| `SDLC_AUTO_MIGRATE` | `true` | накатывать схему на старте |
| `SDLC_SEED` | пусто | сиды через запятую, только на пустой базе |
| `SDLC_STRICT_GATES` | `false` | см. ниже |
| `SDLC_DEFAULT_ACTOR` | `anonymous` | актор без заголовка |
| `SDLC_LOG_LEVEL` | `info` | `debug`, `warn`, `error` |
| `SDLC_ENV` | `development` | не-development включает release-режим Gin и JSON-логи |

**`SDLC_STRICT_GATES`** решает, что делать с гейтом типа, который ещё не
реализован (`agent_review`, `schema_validation`, `external_check` приезжают в
фазах 2-3). `false` — такой гейт отдаёт `skipped` и переход пропускает, и цикл
можно прогнать насквозь. `true` — валит переход. В продакшене значение
обязано быть `true`, иначе задача проезжает мимо непроверенных условий.

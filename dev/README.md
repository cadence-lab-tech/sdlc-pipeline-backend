# dev — локальное окружение

Здесь лежит всё, что нужно, чтобы поднять бэкенд целиком на своей машине:
Postgres с pgvector, приложение, воркер фоновых заданий и инструменты для
разглядывания базы.

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
| MCP для агентов | http://localhost:8088/mcp/projects/SDLC |
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

## Сквозной сценарий

`e2e.sh` прогоняет задачу по всему циклу глазами агентов: захват с арендой,
heartbeat, отказ второй машине на занятой задаче, сдача артефактов,
перепроверка ревьюером с возвратом на доработку и повторным ревью, апрувы,
проверка содержимого вердикта схемой и переходы до терминального статуса.

```bash
make reset up && dev/e2e.sh
```

Скрипту нужна чистая база: демо-задача `SDLC-1` должна стоять на статусе `new`.
Он же — самый быстрый способ увидеть, что именно происходит в цикле.

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

# очередь работы: что можно забрать и кем
curl -s localhost:8088/v1/projects/SDLC/queue | jq '.queue[] | {task: .task.key, role: .role_key, claimable}'

# забрать задачу агентом и получить контекст этапа
curl -s -X POST localhost:8088/v1/tasks/SDLC-1/claim \
     -H 'X-SDLC-Actor: agent:analyst-bot' -H 'Content-Type: application/json' \
     -d '{"session_ref":"macbook/claude-code/8f21"}'
```

Подключить локального агента:

```bash
claude mcp add --transport http sdlc http://localhost:8088/mcp/projects/SDLC
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
| `SDLC_MCP_ENABLED` | `true` | поднимать ли MCP-сервер в этом же процессе |
| `SDLC_LEASE_TTL` | `30m` | сколько живёт аренда без heartbeat |
| `SDLC_HEARTBEAT_INTERVAL` | `5m` | как часто агент обязан её продлевать |
| `SDLC_LEASE_SWEEP_INTERVAL` | `1m` | как часто воркер ищет протухшие аренды |
| `SDLC_LOG_LEVEL` | `info` | `debug`, `warn`, `error` |
| `SDLC_ENV` | `development` | не-development включает release-режим Gin и JSON-логи |

`SDLC_HEARTBEAT_INTERVAL` обязан быть меньше `SDLC_LEASE_TTL` — иначе аренда
протухнет раньше первого продления, и сервис откажется стартовать. Обе величины
задаются и приложению, и воркеру: он решает, что аренда истекла, по тем же
правилам, по которым приложение её выдаёт.

**`SDLC_STRICT_GATES`** решает, что делать с гейтом типа, который движок ещё не
умеет исполнять — сейчас это только `external_check`, он приезжает с чтением
GitHub в фазе 3. `false` — такой гейт отдаёт `skipped` и переход пропускает, и
цикл можно прогнать насквозь. `true` — валит переход. В продакшене значение
обязано быть `true`, иначе задача проезжает мимо непроверенных условий.

## Воркер

Отдельный процесс, а не горутина в приложении: реплик приложения может быть
несколько, и каждая гоняла бы свой проход по одним и тем же арендам. Схему
воркер не катит — она уже накачена приложением.

```bash
docker compose -f dev/docker-compose.yml logs -f worker
```

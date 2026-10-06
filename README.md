# Cadence Lab — Backend

Go-сервис, ведущий задачи по настраиваемому циклу разработки: REST API для UI,
MCP-сервер для локальных агентов, воркеры индексации репозиториев и детектора
дрейфа.

Документация по домену вынесена в отдельный репозиторий
[sdlc-pipeline-docs](https://github.com/cadence-lab-tech/sdlc-pipeline-docs):

- [Архитектура](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/ARCHITECTURE.md) — модель домена, гейты, дрейф, схема БД, план по фазам
- [Контракт агента](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/MCP_CONTRACT.md) — MCP-тулы для локального агента
- [Принятые решения](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/tree/main/decisions) — почему устроено именно так

Здесь остаются референсы реализации: [drift_detection.sql](docs/drift_detection.sql) —
запросы детектора дрейфа, которые исполняет воркер.

Фронтенд: [sdlc-pipeline-frontend](https://github.com/cadence-lab-tech/sdlc-pipeline-frontend).

## Стек

Go 1.25, Gin, pgx (без ORM), PostgreSQL 17 + pgvector, официальный MCP SDK,
эмбеддинги Voyage (`voyage-code-3`, 1024). Чтение GitHub — на стандартной
библиотеке: индексатору нужны четыре ручки, и клиентская библиотека ради них
не окупается.

## Запуск

```bash
make up      # база, приложение и воркер в контейнерах
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
make build && ./bin/api      # рядом собираются bin/worker и bin/clab
```

Сквозной проход задачи по циклу глазами агентов — `dev/e2e.sh`: захват,
аренда, сдача артефактов, перепроверка ревьюером с возвратом на доработку,
апрувы и переходы до терминального статуса.

```bash
make reset up && dev/e2e.sh
```

Инкрементальность индексации, детектор дрейфа, гибридный поиск, перенос
связки, токены и вебхуки проверяются интеграционными тестами по настоящей
схеме: `make test-integration` (нужна выделенная база — тест сносит в ней
схему).

## Аутентификация и перенос правил

`SDLC_AUTH_MODE=token` включает токены и права по участию в проекте; первый
администратор берётся из `SDLC_ADMIN_TOKEN`. Правила команды переносятся в
проект связкой: `clab export`, `clab import --file bundle.json`, из агента —
промпт `/mcp__sdlc__import_rules`. Подробности — [dev/README.md](dev/README.md).

## API

Аутентификации нет ([решение 0010](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0010-auth-deferred.md)):
действующее лицо приезжает заголовком `X-SDLC-Actor` и заводится в `principals`
при первом обращении.

| Метод | Путь | Что делает |
|---|---|---|
| `GET` | `/healthz`, `/readyz` | пробы; `readyz` проверяет базу |
| `GET` | `/v1/instance` | открыто: версия сборки, профиль `tenancy`, `project_creation`, способы входа; версия ещё и в заголовке `X-Clab-Version` каждого ответа |
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
| `GET` | `/v1/projects/{key}/queue` | очередь работы: незакрытые прогоны с ролью и состоянием аренды |
| `GET` | `/v1/tasks/{key}/state` | задача глазами агента: роль, вход этапа, дрейф, гейты впереди |
| `POST` | `/v1/tasks/{key}/claim`, `/heartbeat`, `/release` | аренда задачи |
| `GET`/`POST` | `/v1/tasks/{key}/repo-refs` | привязанные PR, ветки и коммиты |
| `GET` | `/v1/tasks/{key}/drift` | базовые линии и открытые расхождения |
| `POST` | `/v1/drift-signals/{id}/resolve` | закрыть сигнал разбором или отклонить |
| `GET`/`POST` | `/v1/projects/{key}/search` | гибридный поиск по коду, артефактам и документам |
| `GET` | `/v1/projects/{key}/index` | состояние индекса репозиториев проекта |
| `POST` | `/mcp/projects/{key}` | MCP-сервер для локальных агентов (streamable HTTP) |

Заблокированный гейтами переход — это `409` с разбором по каждому гейту: что
именно не сдано и что с этим делать.

Роли и цикл правятся версиями, а не на месте: версия роли иммутабельна, а
редактируется только черновик цикла ([решение 0016](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0016-workflow-versions-immutable.md)).
Попытка изменить активный цикл — `409`.

## Агенты

Локальный агент подключается к MCP-серверу, привязанному к проекту путём:

```bash
claude mcp add --transport http sdlc http://localhost:8088/mcp/projects/SDLC
```

Инструкция роли, результаты предыдущих этапов, область репозиториев и требования
гейтов приезжают в ответе на `sdlc_claim_task` — синхронизировать в локальный
репозиторий нечего. Полный контракт — в [MCP_CONTRACT.md](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/MCP_CONTRACT.md).

Задача выдаётся под аренду с heartbeat: агент локальный и может умереть, не
отпустив её. Протухшие аренды возвращает в общий доступ `cmd/worker`.

Поиск по знанию проекта — `sdlc_search_context`: код из актуального индекса
репозиториев, артефакты прошлых задач и проектные документы. Гибридный, вектор
плюс полнотекст; без ключа Voyage остаётся полнотекстовым, и агент видит это в
поле `mode`.

Те же операции для людей, CI и не-MCP клиентов даёт CLI `clab`. Человек
входит через браузер, как в `gh`, и дальше работает из каталога
репозитория — проект находится по `origin`:

```bash
clab auth login --host https://sdlc.example.com   # код в терминале, подтверждение в браузере
clab auth status
cd ~/src/api                                      # репозиторий привязан к проекту
clab tasks                                        # таблица; --json — ответ сервера как есть
clab show SDLC-1
clab mcp install                                  # claude mcp add с тем же токеном
```

Подключение своего проекта с нуля — установка, вход, проект, репозитории,
стартовые роли и цикл, агент, первая задача — расписано в
[QUICKSTART.md](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/QUICKSTART.md);
подробности каждого шага — в [INTRODUCTION.md](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/INTRODUCTION.md).

Скрипты и CI обходятся без конфига — переменные окружения перекрывают его:

```bash
export SDLC_API_URL=http://localhost:8088/v1 SDLC_PROJECT=SDLC SDLC_ACTOR=agent:analyst-bot
./bin/clab --json queue --role analyst
./bin/clab claim SDLC-1
./bin/clab submit SDLC-1 --kind analysis.brief --title "Разбор" --file brief.md
./bin/clab gates SDLC-1 --transition analysis_done
./bin/clab transition SDLC-1 --to analysis_done --summary "Требования разобраны"
./bin/clab search "как устроена аренда" --scope code
./bin/clab index
```

Поставить `clab` на свою машину можно тремя способами. Раздача публичная
([решение 0038](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0038-public-cli-distribution.md)):
ни токена, ни участия в организации не нужно.

```bash
# macOS и Linux с Homebrew
brew trust cadence-lab-tech/tap && brew install cadence-lab-tech/tap/clab

# без Homebrew
curl -fsSL https://raw.githubusercontent.com/cadence-lab-tech/homebrew-tap/main/install.sh | sh

# Windows и всё остальное — архив из релиза
gh release download --repo cadence-lab-tech/homebrew-tap \
    --pattern 'clab_*_windows_amd64.zip'
```

Релизы собирает `.github/workflows/release.yml` по тегу `v*`: архивы уходят
релизом в публичный тап
[homebrew-tap](https://github.com/cadence-lab-tech/homebrew-tap), где живут
формула и `install.sh`, а здесь у релиза остаются только заметки. Код
сервиса закрыт, наружу выходят только бинарники CLI.

## Структура

```
cmd/api/            HTTP-сервер: REST и MCP на одних сервисах
cmd/worker/         аренды, индексация, детектор дрейфа, состояние PR
cmd/clab/           CLI для людей, агентов и CI
internal/
  config/           настройки из окружения
  apperr/           ошибки домена с кодом, общим для REST и MCP
  domain/           сущности схемы в Go
  workflow/         движок: переходы, прогон гейтов, артефакты, область задачи,
                    аренды, перепроверка, дрейф и поиск; отдельно authoring —
                    роли и цикл
  store/postgres/   запросы, миграции, контент-адресуемые blobs
  github/           чтение репозиториев: дерево, файлы, PR, проверки
  indexer/          инкрементальный прогон, чанкинг, символы, дельты
  embed/            Voyage и заглушка для локального прогона
  mcp/              MCP-сервер поверх тех же сервисов
  httpapi/          роутер Gin, middleware, хендлеры
  testdb/           выделенная база для интеграционных проверок
migrations/         схема и сиды, встроены в бинарник через embed.go
dev/                docker-compose и сквозной сценарий e2e.sh
docs/               референсы реализации
```

## Статус

Фазы 1-3 на бэкенде закрыты: проекты, задачи, артефакты, движок переходов со
всеми девятью типами гейтов, редактор ролей и цикла, MCP-сервер, аренды с
heartbeat, CLI, инкрементальная индексация репозиториев с кэшем эмбеддингов,
гибридный поиск и детектор дрейфа.

Дальше — фаза 4: символьный граф через tree-sitter, вынос blobs в объектное
хранилище, аутентификация и метрики цикла. Из мелкого: вебхуки GitHub вместо
опроса и политика вычистки старых прогонов индексации.

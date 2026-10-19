# Cadence Lab — Backend

Go-сервис, ведущий задачи по настраиваемому циклу разработки: REST API для UI,
MCP-сервер для локальных агентов, воркеры индексации репозиториев и детектора
дрейфа.

Поставить Cadence Lab у себя — репозиторий
[cadence-lab-tech/self-hosted](https://github.com/cadence-lab-tech/self-hosted):
стек Docker Compose, скрипты установки, обновления и резервной копии,
руководство администратора. Этот репозиторий — исходники сервиса и CLI,
распространяются под FSL-1.1-ALv2 (раздел [«Лицензия»](#лицензия)).

Здесь же референсы реализации: [drift_detection.sql](docs/drift_detection.sql) —
запросы детектора дрейфа, которые исполняет воркер.

Фронтенд: [sdlc-pipeline-frontend](https://github.com/cadence-lab-tech/sdlc-pipeline-frontend).

## Стек

Go 1.26, Gin, pgx (без ORM), PostgreSQL 17 + pgvector, официальный MCP SDK,
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

Режим доступа задаёт `SDLC_AUTH_MODE`. В `token` (по умолчанию вне
development) каждый запрос несёт `Authorization: Bearer <token>`, права
проверяются по участию в проекте и команде; люди входят через GitHub или
корпоративного провайдера OIDC, CLI — через браузер. В `header` (локальная
отладка цикла) действующее лицо приезжает заголовком `X-SDLC-Actor` без
проверки прав. В обоих режимах принципал заводится в `principals` при первом
обращении. Подробности — [dev/README.md](dev/README.md).

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
| `PATCH` | `/v1/tasks/{key}/scope` | свойства и область задачи: `add_repositories`, `remove_repositories` (сопровождающий), `note` |
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
редактируется только черновик цикла.
Попытка изменить активный цикл — `409`.

## Агенты

Локальный агент подключается к MCP-серверу, привязанному к проекту путём:

```bash
claude mcp add --transport http sdlc http://localhost:8088/mcp/projects/SDLC
```

Инструкция роли, результаты предыдущих этапов, область репозиториев и требования
гейтов приезжают в ответе на `sdlc_claim_task` — синхронизировать в локальный
репозиторий нечего.

Контракт агента коротко. Агент работает с проектом, к которому привязан путь
MCP, и проходит этап так:

1. **Захват.** `sdlc_claim_task` выдаёт этап задачи под аренду: роль и её
   инструкцию, контракт артефактов, входные артефакты, область репозиториев,
   переходы с гейтами и интервал heartbeat. `sdlc_list_tasks` и
   `sdlc_get_task` — посмотреть очередь и карточку без аренды.
2. **Работа.** `sdlc_search_context` ищет по коду, артефактам и документам
   проекта; `sdlc_get_artifact`, `sdlc_get_document`, `sdlc_get_drift` —
   прочитать нужное. Долгий этап держится `sdlc_heartbeat`.
3. **Сдача.** `sdlc_submit_artifact` кладёт артефакт того `kind`, что в
   контракте роли; правка — новая версия, сданное не меняется.
   `sdlc_link_repo_ref` привязывает PR, ветку или коммит.
4. **Переход.** `sdlc_check_gates` показывает, что держит переход;
   `sdlc_request_transition` выполняет его или возвращает разбор гейтов.
   Гейт апрува закрывает человек. Найдена проблема — артефакт с её описанием
   и переход-возврат. Бросить этап — `sdlc_release`.

Стоимость прогона агент сообщает `sdlc_report_usage`. Остальные тулы — для
заведения и настройки: `sdlc_create_task`, `sdlc_update_task`,
`sdlc_link_tasks`, `sdlc_assign_task`, `sdlc_update_task_scope`,
`sdlc_put_role`, `sdlc_put_document`, `sdlc_add_repository`, перенос правил
`sdlc_export_bundle` и `sdlc_import_bundle`.

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

Область задачи меняет `clab scope`. Расширить её вправе и агент
(`sdlc_update_task_scope` умеет только это), сузить — решение человека
уровня `maintainer` и выше, поэтому `--remove` есть только в CLI и REST:

```bash
clab scope SDLC-42 --add acme/web --reason "поле надо показать на фронте"
clab scope SDLC-42 --remove acme/api --reason "бэкенд был только справочником"
clab scope SDLC-42 --remove acme/api --add acme/web --reason "..."  # замена одним вызовом
```

Убрать нельзя последний репозиторий области и репозиторий, к которому у
задачи привязаны PR, ветки или коммиты: в нём уже идёт работа задачи. Ушёл
основной — основным становится первый из оставшихся по `owner/name`. В ленте
это то же событие `task.scope_updated` с `removed_repositories`.

Установка своего инстанса, вход, первый проект и подключение агента —
в [cadence-lab-tech/self-hosted](https://github.com/cadence-lab-tech/self-hosted):
README и руководство администратора. Установкой на своей машине `clab`
управляет сам, без входа и без API: `clab server upgrade|backup|restore|status`
повторяет скрипты пакета (`upgrade.sh`, `backup.sh`, `restore.sh`) и
находит каталог по `--dir`, `CADENCE_DIR` или вверх от текущего; вывод
по-английски, как у пакета. Код — `cmd/clab/server*.go`, правило версий —
стек от v0.6.0 и не новее `clab`.

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

Поставить `clab` на свою машину можно тремя способами. Раздача публичная:
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
формула и `install.sh`, а здесь у релиза остаются только заметки.

### Выпуск версии

Версия — один тег `vX.Y.Z` на `main` обоих репозиториев, backend и
[frontend](https://github.com/cadence-lab-tech/sdlc-pipeline-frontend), в любом
порядке. По тегу каждый `release.yml` публикует свой образ:

| Образ | Что внутри |
|---|---|
| `ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:vX.Y.Z` | api, worker, clab, собранные с `VERSION=vX.Y.Z`; `/LICENSE` |
| `ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:vX.Y.Z` | статика интерфейса в Caddy; `/LICENSE` |

Тег образа совпадает с git-тегом, буква `v` остаётся. `/LICENSE` — файл
`LICENSE` из корня репозитория на коммите тега; без него задание `image`
падает до сборки, и образ версии не выходит. Сборки по SHA из `deploy.yml`
отсутствие файла терпят, а тег их не запускает: наш инстанс выкатывается
только по push в `main`. Архивы CLI и событие `clab-release` в тап — как
раньше.

Цепочка поставки ([решение 0046](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0046-supply-chain-signatures-pins.md),
с v0.9.0): у каждого образа в индексе SBOM (SPDX) и provenance (SLSA v1,
`mode=max`) обеих платформ; digest индекса подписан cosign без ключей —
сертификатом на OIDC-токен прогона, identity
`https://github.com/cadence-lab-tech/<repo>/.github/workflows/release.yml@refs/tags/vX.Y.Z`;
к образу backend приложен подписанный OpenVEX из
`.github/release/openvex.json` (известные ложные находки сканеров, правится
PR). Там же `govulncheck -mode=binary` по `/api`, `/worker`, `/clab`
опубликованного образа: находка вне VEX — предупреждение в сводке прогона,
релиз не падает (`.github/release/govulncheck-binaries.sh`, проверка без
сети — `bash .github/release/govulncheck-binaries.test.sh`). `checksums.txt`
архивов CLI подписан так же, бандл `checksums.txt.sigstore.json` уходит в
релиз тапа. Секретов для этого не нужно — только `id-token: write` у
заданий `image` и `release`. Команды проверки для клиента —
[«Verifying a release»](https://github.com/cadence-lab-tech/self-hosted/blob/main/docs/admin.md#verifying-a-release)
в admin.md пакета.

Когда в реестре есть оба образа версии и оба подписаны, в
[`cadence-lab-tech/self-hosted`](https://github.com/cadence-lab-tech/self-hosted)
уходит `repository_dispatch`, образы — по тегу и digest индекса:

```json
{"event_type": "cadence-release",
 "client_payload": {"version": "vX.Y.Z",
                    "backend_image": "ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:vX.Y.Z@sha256:…",
                    "frontend_image": "ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:vX.Y.Z@sha256:…"}}
```

Шлёт его тот выпуск, который опубликовал и подписал свой образ вторым:
после подписи шаг «оповестить self-hosted»
(`.github/release/notify-self-hosted.sh`, такой же во frontend) смотрит в
реестр, есть ли образ второй половины, и проверяет его подпись (`cosign
verify` с identity `release.yml` соседа на теге). Образ есть, а подписи ещё
нет — событие не уходит, прогон зелёный: сосед подпишет свой образ и
отправит событие сам. Если оба
выпуска закончат одновременно, событие может прийти дважды — обработчик в
`self-hosted` идемпотентен. Проверка скрипта без сети —
`bash .github/release/notify-self-hosted.test.sh`.

В `self-hosted` событие открывает PR «Release vX.Y.Z»; его слияние ставит
тег и публикует GitHub Release `vX.Y.Z` с пакетом поставки —
`cadence-self-hosted-vX.Y.Z.tar.gz`, `checksums.txt` и `get-cadence.sh`.
Из него клиенты ставят инстанс одной строкой и обновляются `./upgrade.sh`
без git, поэтому выпуск закончен, когда у этого релиза есть все три ассета.

Что нужно завести:

- секрет `SELF_HOSTED_DISPATCH_TOKEN` в backend и frontend (или секрет
  организации на оба): fine-grained PAT владельца `cadence-lab-tech` с
  доступом только к `cadence-lab-tech/self-hosted`, право Contents: Read and
  write. Без секрета прогон зелёный, а шаг печатает команду для отправки
  руками;
- пакеты GHCR `sdlc-pipeline-backend` и `sdlc-pipeline-frontend` публичные
  (CLAB-94): образ соседа прогон читает своим `GITHUB_TOKEN` без настроек.
  Если пакет снова станет приватным, чтение чужого пакета выдаётся в
  настройках пакета: `sdlc-pipeline-frontend` → Manage Actions access →
  репозиторий `sdlc-pipeline-backend` с ролью Read, и наоборот.

Пришла только половина выпуска — в логе шага «оповестить self-hosted»
написано, какого образа нет, и ответ реестра. Поставить тег во втором
репозитории или перезапустить упавший прогон (`gh run rerun <id> --failed`):
событие отправит он. Шаг отличает «образа ещё нет» (404, прогон зелёный)
от отказа реестра и молчания сети (401/403, таймаут): это поломка доступа,
и такой прогон красный — иначе событие не отправил бы ни один из двух
выпусков (CLAB-99). Отправить руками — команда из лога шага: `gh api
repos/cadence-lab-tech/self-hosted/dispatches -f event_type=cadence-release
-f 'client_payload[version]=vX.Y.Z'` плюс `backend_image` и
`frontend_image`.

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

## Лицензия

Cadence Lab распространяется с открытыми исходниками под
[Functional Source License 1.1, ALv2 Future License](LICENSE) (FSL-1.1-ALv2):

- пользоваться, изменять и разворачивать у себя для своих нужд можно
  бесплатно, в том числе в компании;
- предлагать продукт третьим лицам как конкурирующий продукт или сервис
  нельзя;
- через два года после выхода каждая версия становится доступна под
  Apache 2.0.

Готовый к установке пакет и руководство —
[cadence-lab-tech/self-hosted](https://github.com/cadence-lab-tech/self-hosted).

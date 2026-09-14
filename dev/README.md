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
| MinIO | http://localhost:9001 (только с `--profile s3`) |

Порты нестандартные намеренно: 8080 и 5432 на машине разработчика обычно уже
заняты. Переопределяются через `dev/.env` — образец в `.env.example`.

### Стек на задачу

Параллельно идут несколько задач, и у каждой свой агент. Общий стек они делят
вместе с тестовыми данными: один разворачивает демо, другой сносит базу, третий
ловит чужие задачи в очереди — и проверки перестают что-либо доказывать. Стек на
задачу разводит агентов так же, как `git worktree` разводит рабочие копии:

```bash
dev/stack.sh up CLAB-59       # поднять и дождаться готовности
dev/stack.sh ls               # какие стеки заняты и на каких портах
dev/stack.sh logs CLAB-59     # логи api (или другого сервиса аргументом)
dev/stack.sh psql CLAB-59     # psql в базу стека
dev/stack.sh e2e CLAB-59      # сквозной проход в своём стеке
dev/stack.sh down CLAB-59     # снести вместе с данными
```

То же из корня backend: `make task-up KEY=CLAB-59`, `make task-down KEY=CLAB-59`,
`make task-ls`.

Порты руками не выбираются — агент их не угадает. Набор выдаётся один раз и
запоминается в реестре вне репозитория (`CLAB_STACKS_DIR`, по умолчанию
`~/.clab/stacks`), поэтому повторный `up` той же задачи поднимает стек на тех же
адресах, а соседняя задача получает свободные: API `18080+n`, Postgres `55440+n`,
интерфейс `5180+n`. Наборов `CLAB_STACKS_SLOTS` (восемь) — упереться в потолок
лучше, чем занять порт соседа.

Интерфейс в контейнер не заворачивается: в рабочей копии задачи он поднимается
своим `vite`, и обе переменные печатает `up`.

```bash
cd ../frontend
SDLC_API_URL=http://localhost:18080 SDLC_UI_PORT=5180 npm run dev
```

`SDLC_API_URL` передаётся префиксом одной команды и не экспортируется в
оболочку: его читает и `clab`, а он должен ходить в рабочий инстанс, а не в
локальный стек. `dev/stack.sh env` по той же причине печатает адреса под
именами `CLAB_STACK_*` — их можно смело `eval`.

Фоновые задания в задачном стеке выключены намеренно: индексация приватных
репозиториев и эмбеддинги стоят денег и лимитов GitHub, а проверяют в нём обычно
не их. Нужны — `dev/stack.sh up CLAB-59 --index`, и тогда стек берёт настройки
из `dev/.env` как обычно.

База у стека своя и пустая, поэтому сиды применяются: демо-проект на месте, и
`dev/e2e.sh` с `make test-integration` идут, не трогая основной стек.

Стек — объект тестирования, а не источник задач: за задачей агент по-прежнему
ходит в рабочий инстанс через MCP и `clab`.

Снести стек — часть уборки за задачей, вместе с `git worktree`.

### Второй стек вручную

Без реестра и обвязки то же самое делается руками — префикс имён контейнеров
обязателен, одного `-p` мало:

```bash
DEV_STACK_PREFIX=clabqa POSTGRES_PORT=55433 API_PORT=18081 \
  docker compose -f dev/docker-compose.yml -p clabqa up -d --build
DEV_STACK_PREFIX=clabqa docker compose -f dev/docker-compose.yml -p clabqa down -v
```

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

## Индексация репозиториев

Воркер раз в `SDLC_INDEX_INTERVAL` сверяет головы отслеживаемых веток с тем, что
уже проиндексировано, и ставит недостающие коммиты в очередь. Прогон
инкрементальный: листинг дерева сравнивается с предыдущим прогоном, и сеть с
моделью тратятся только на изменившиеся файлы.

Репозитории демо-проекта настоящие и приватные, поэтому без токена воркер честно
скажет в логе, что ветка не прочитана, и продолжит остальные задания. С токеном
(классический PAT или fine-grained с правом на чтение содержимого) индексация
работает по-настоящему:

```bash
echo 'SDLC_GITHUB_TOKEN=ghp_…' >> dev/.env
docker compose -f dev/docker-compose.yml up -d worker
docker compose -f dev/docker-compose.yml logs -f worker
```

Состояние индекса — `GET /v1/projects/SDLC/index` или `clab index`:

```bash
curl -s localhost:8088/v1/projects/SDLC/index | jq '.repositories[]
    | {repository, state, kind, files_indexed, files_reused, chunks_embedded}'
```

`files_reused` против `files_indexed` показывает, во что обошлась
инкрементальность: на втором прогоне подряд первое число должно быть в разы
больше второго. Если это не так — прогон почему-то не нашёл родителя, и стоит
смотреть в логи, а не в счета за эмбеддинги.

**Эмбеддинги.** Без `SDLC_VOYAGE_API_KEY` векторы не считаются, и поиск
остаётся полнотекстовым — это рабочий режим, а не поломка: индексация и детектор
дрейфа от векторов не зависят. Поле `mode` в ответе поиска показывает, какой
режим сейчас. Ключ, добавленный позже, не требует переиндексации: отдельный
проход воркера досчитает векторы уже разложенным кускам.

`SDLC_EMBEDDINGS_PROVIDER=hash` — детерминированная заглушка для локальной
проверки проводки. Это мешок слов, а не семантика: близкими окажутся тексты с
общими словами, и только.

## Поиск

```bash
curl -s 'localhost:8088/v1/projects/SDLC/search?q=как+устроена+аренда&top_k=5' | jq
./bin/clab search "как устроена аренда" --scope code --repo cadence-lab-tech/sdlc-pipeline-backend
```

Ищет по коду из актуального индекса, по артефактам задач и по проектным
документам. Гибридно: вектор плюс полнотекст, слияние по рангам.

## Интеграционные проверки

Инкрементальность прогона, детектор дрейфа и гибридный поиск живут в связке с
базой и на моках не проверяются. Для них есть тесты по настоящей схеме:

```bash
make test-integration
```

База под них выделенная (`sdlc_test`): тест сносит в ней схему целиком и
накатывает миграции заново. Без `SDLC_TEST_DATABASE_URL` такие тесты
пропускаются, и `make test` остаётся быстрым.

## Сквозной сценарий

`e2e.sh` прогоняет задачу по всему циклу глазами агентов: захват с арендой,
heartbeat, отказ второй машине на занятой задаче, сдача артефактов,
перепроверка ревьюером с возвратом на доработку и повторным ревью, апрувы,
проверка содержимого вердикта схемой, ручное снятие гейта CI и переходы до
терминального статуса.

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

Заголовок `X-SDLC-Actor` — это действующее лицо в режиме
`SDLC_AUTH_MODE=header`, в котором compose поднимает окружение по умолчанию:
заголовку никто не мешает соврать, права не проверяются — это режим отладки
цикла. Без заголовка берётся `SDLC_DEFAULT_ACTOR`.

## Аутентификация и права

Режим `SDLC_AUTH_MODE=token` ([решение 0021](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0021-token-auth-and-project-members.md)):
запрос без `Authorization: Bearer <token>` отбивается, права проверяются по
участию в проекте (`viewer`, `agent`, `maintainer`, `admin`). Первый
администратор заводится из окружения — в пустой базе некому выдать первый
токен:

```bash
printf 'SDLC_AUTH_MODE=token\nSDLC_ADMIN_TOKEN=sdlc_замените_на_длинный_секрет\n' >> dev/.env
make up

export SDLC_TOKEN=sdlc_замените_на_длинный_секрет SDLC_PROJECT=SDLC
./bin/clab me                                     # admin, is_admin=true
./bin/clab members set user:alice maintainer      # добавить человека
./bin/clab token create --name alice --for user:alice   # выдать ей токен
./bin/clab token create --name ci-reviewer --for agent:reviewer-bot --roles reviewer
```

Какие роли специалистов участник может забирать, задаёт администратор проекта
на участии ([решение 0030](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0030-member-roles-github-login-open-projects.md)):
`clab members set user:alice agent --roles developer,reviewer`; без `--roles` —
любые. Команде роли выдаются так же: `clab team grant platform --project SDLC
--level agent --roles reviewer`. Токен может только сузить эти рамки: `--roles`
у токена пересекается с ролями участника, и токен ревьюера не заберёт этап
разработчика. Сам токен показывается один раз при выдаче; в базе лежит хеш.
Локальный агент подключается с ним же:

```bash
claude mcp add --transport http sdlc http://localhost:8088/mcp/projects/SDLC \
    --header "Authorization: Bearer $SDLC_TOKEN"
```

Гейт `human_approval` в любом режиме закрывает только принципал вида `user`:
агент, даже сопровождающий, поставить апрув не может.

**Приглашения ссылкой** (решение 0035). Лид зовёт в организацию, администратор
проекта — в проект; права, срок (1-30 дней, по умолчанию неделя), уровни и
роли проверяет сервер:

```bash
clab invite create --team acme --role lead --days 3
clab invite create --project SDLC --level agent --roles developer   # без --project — SDLC_PROJECT или git remote
clab invite list --team acme          # статус, роль или уровень, кто выдал, срок, кто принял
clab invite revoke <id>               # полный ID из списка
```

Ссылка `<адрес хоста>/invite/clab_inv_…` печатается один раз; `clab --json
invite create …` отдаёт ответ сервера и поле `url`. Выдаёт и принимает
приглашение только человек, в любом режиме: в режиме заголовка актор по
умолчанию `agent:$USER`, поэтому локально — `SDLC_ACTOR=user:<хэндл>`.
Ссылка строится от адреса хоста clab, а на `make up` это API `:8088`, не
интерфейс. Чтобы ссылка открывалась в интерфейсе на vite, выдавайте с
`--host localhost:5173`: dev-сервер проксирует `/v1`, и запрос уходит туда же.

**Вход CLI через браузер** ([решение 0032](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0032-cli-login-device-flow.md)).
Человеку токен руками не нужен: `clab auth login --host http://localhost:8088`
печатает код, открывает `/login/cli`, и после «Разрешить» в интерфейсе
кладёт долгоживущий токен в `~/.config/clab/config.json` (`CLAB_CONFIG_DIR`
переопределяет каталог). Дальше `clab auth status`, `clab auth logout`,
`clab auth token` для скриптов рядом; `--host` выбирает хост из конфига,
`SDLC_API_URL` и `SDLC_TOKEN` перекрывают конфиг. Проект без `--project` и
`SDLC_PROJECT` берётся по `origin` текущего каталога
(`GET /v1/projects?repo=owner/name`). Агента к MCP проекта подключает
`clab mcp install` — это `claude mcp add` с тем же токеном; `clab mcp config`
печатает JSON для любого другого агента. Команды чтения показывают таблицы,
`--json` в любом месте строки отдаёт ответ сервера как есть — так CLI зовут
`dev/e2e.sh` и раннер.

**Вход через GitHub.** У GitHub App в настройках есть раздел OAuth: Client ID
и Client secret, callback URL — `https://<адрес интерфейса>/login/github`;
второй callback URL и Setup URL (с «Redirect on update») —
`https://<адрес интерфейса>/settings/github/callback`, по нему возвращается
подключение установки.
С `SDLC_GITHUB_APP_CLIENT_ID` и `SDLC_GITHUB_APP_CLIENT_SECRET` на странице
входа появляется кнопка «Войти через GitHub»: браузер уходит за кодом
авторизации, бэкенд меняет его на профиль (`POST /v1/auth/github/login`) и
выдаёт ту же сессию, что и провайдер OIDC. Принципал ключуется идентификатором
GitHub (`principals.idp_subject = github:<id>`), хэндл — логин. Токен
пользователя нигде не хранится: репозитории читаются установкой приложения,
а не от имени вошедшего. Вошедший впервые ни в одном проекте не состоит —
доступ ему выдаёт администратор проекта.

**GitHub по кнопке.** С `SDLC_GITHUB_APP_SLUG` в настройках проекта и на
странице команды появляется «Подключить GitHub»: человек ставит приложение
на свой аккаунт или организацию, возвращается на
`/settings/github/callback`, страница уводит его за кодом повторной
авторизации (для входившего — без вопросов) и отдаёт код бэкенду
(`POST /v1/teams/{key}/github/connect`). Тот записывает установку на
команду, если она видна вошедшему, и возвращает репозитории установок
команды, которые человек видит сам; отмеченные привязываются пачкой
(`POST /v1/projects/{key}/github/repositories`). Установки команды —
`clab team github`, привязка по координатам подбирает установку сама
(`clab repo add`, `--installation` задаёт явно).

**Создание проектов.** По умолчанию (`SDLC_PROJECT_CREATION=open`) проект
заводит любой вошедший — из интерфейса или `clab project create`; без
явной команды он уходит в личную команду создателя (решение 0033), создатель
становится его администратором и сам добавляет коллег — среди тех, кто уже
входил. `teams` возвращает правило фазы 6: администратор системы и лиды
команд с правом на это.

**Арендатор — команда.** Личная команда заводится при первом входе;
организацию заводит любой вошедший (`clab team create`) и зовёт в неё
коллег, на тарифе `free` — до трёх. Личный аккаунт превращается в
организацию `clab team convert`, тариф назначает администратор инстанса
`clab team plan <команда> --set business`.

## Перенос правил в проект

Существующие правила команды — CLAUDE.md, AGENTS.md, ADR, описания ролей —
переносятся в проект связкой ([решение 0022](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0022-bundle-import-export.md)).
Агенту достаточно промпта `/mcp__sdlc__import_rules` или тула
`sdlc_import_guide`; из CLI:

```bash
./bin/clab import-guide                          # что и куда переносить
./bin/clab export > bundle.json                  # выгрузить проект связкой
./bin/clab import --file bundle.json --dry-run   # отчёт без изменений
./bin/clab import --file bundle.json
./bin/clab repo add acme/api --hint backend
./bin/clab role put --key writer --name Техписатель --file docs/roles/writer.md
./bin/clab doc put --kind guideline --title Соглашения --file CLAUDE.md
./bin/clab workflow activate <id>                # цикл из связки приходит черновиком
```

Импорт идемпотентен: роль с той же инструкцией не трогается, с другой —
получает новую версию; документ с теми же `kind` и `title` обновляется.

## Вебхуки GitHub

С секретом `SDLC_GITHUB_WEBHOOK_SECRET` сервис принимает события на
`POST /webhooks/github` ([решение 0023](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0023-github-webhooks-write-to-queue.md)):
`push` ставит коммит в очередь индексации, `pull_request` и `check_run`
помечают привязанные PR к пересинхронизации, `installation` и
`installation_repositories` отмечают снятие, приостановку и убранные из
установки репозитории — индексация по ним останавливается, индекс остаётся.
Доставка ищет репозиторий сначала по установке, потом по координатам:
без `installation.id` она не обслуживается ([решение 0034](https://github.com/cadence-lab-tech/sdlc-pipeline-docs/blob/main/decisions/0034-github-installation-on-tenant.md)).
В сеть обработчик не ходит — воркер забирает очередь раз в
`SDLC_INDEX_POLL_INTERVAL`. Локально без публичного адреса индексация
по-прежнему идёт по таймеру.

## Объектное хранилище вместо тома

Крупное содержимое по умолчанию уходит на том `blobs`, общий у api и воркера.
Когда реплик api несколько и общего тома между ними нет, вместо каталога
задаётся бакет — интерфейс `blobstore.Store` один, меняется реализация.

Локально это проверяется профилем `s3`: он поднимает MinIO и заводит в нём
бакет. Настройки — в `dev/.env` (образец в `.env.example`), они же задают
пользователя MinIO:

```bash
cat >> dev/.env <<'EOF'
SDLC_BLOB_S3_BUCKET=sdlc-blobs
SDLC_BLOB_S3_ENDPOINT=http://minio:9000
SDLC_BLOB_S3_REGION=us-east-1
SDLC_BLOB_S3_ACCESS_KEY_ID=sdlc
SDLC_BLOB_S3_SECRET_ACCESS_KEY=sdlcsdlc
EOF

docker compose -f dev/docker-compose.yml --profile s3 up -d --build
```

В логе api появится `внешнее хранилище содержимого store=s3://sdlc-blobs/`.
Дальше документ больше 256 КБ ляжет в бакет, а в базе останется адрес:

```bash
curl -s localhost:8088/v1/projects/SDLC/documents -H 'X-SDLC-Actor: local-dev' \
  -H 'Content-Type: application/json' -d @big-document.json
docker exec sdlc-postgres psql -U sdlc -d sdlc -tAc \
  "SELECT storage, external_uri FROM blobs WHERE storage='external'"
```

Убрать бакет из `dev/.env` и пересобрать — вернётся том. Адреса `s3://`,
записанные раньше, файловое хранилище читать откажется: это разные хранилища,
и молчаливой подмены здесь быть не должно.

## Метрики цикла

`GET /v1/projects/SDLC/metrics?period=30d` или `clab metrics`: длительность
этапов по статусам, откаты и повторные попытки, вердикты по гейтам, сигналы
дрейфа, пропускная способность. Считается по накопленным таблицам.

## Переменные окружения

| Переменная | По умолчанию | Зачем |
|---|---|---|
| `SDLC_DATABASE_URL` | — | обязательна |
| `SDLC_HTTP_ADDR` | `:8080` | внутри контейнера |
| `SDLC_AUTO_MIGRATE` | `true` | накатывать схему на старте |
| `SDLC_SEED` | пусто | сиды через запятую, только на пустой базе |
| `SDLC_STRICT_GATES` | `false` | см. ниже |
| `SDLC_AUTH_MODE` | `header` в development, иначе `token` | `header` — актор из заголовка без проверки прав; `token` — только токены |
| `SDLC_ADMIN_HANDLE` | `admin` | хэндл первого администратора |
| `SDLC_ADMIN_TOKEN` | пусто | его токен; заводится на старте, при рестарте не дублируется |
| `SDLC_BOOTSTRAP_TEAM` | пусто | ключ первой команды: администратор становится её лидом, право заводить проекты у неё есть сразу |
| `SDLC_BOOTSTRAP_TEAM_NAME` | ключ команды | её название |
| `SDLC_PROJECT_CREATION` | `open` | кто заводит проекты: `open` — любой вошедший, проект уходит в его личную команду; `teams` — администратор системы и лиды команд с правом на это |
| `SDLC_TENANCY` | `shared` | профиль поставки: `shared` — общий инстанс, пределы тарифа проверяются; `single` — выделенный, квот нет |
| `SDLC_GITHUB_APP_CLIENT_ID` | пусто | Client ID GitHub App для входа людей через GitHub; задаётся вместе с секретом |
| `SDLC_GITHUB_APP_CLIENT_SECRET` | пусто | Client secret того же приложения — обмен кода авторизации делает бэкенд |
| `SDLC_GITHUB_APP_SLUG` | пусто | слаг приложения (`github.com/apps/<slug>`): адрес кнопки «Подключить GitHub»; без него кнопка выключена |
| `SDLC_GITHUB_URL` | `https://github.com` | адрес сайта GitHub: там живёт `/login/oauth`; у GitHub Enterprise отличается от адреса API |
| `SDLC_OIDC_ISSUER` | пусто | адрес корпоративного провайдера; пусто — вход только по выданным токенам |
| `SDLC_OIDC_CLIENT_ID` | пусто | идентификатор приложения у провайдера; им же проверяется аудитория ID-токена |
| `SDLC_OIDC_GROUPS_CLAIM` | `groups` | клейм с группами: по нему синхронизируется состав команд |
| `SDLC_SESSION_TTL` | `12h` | срок жизни сессии, выданной входом через провайдера |
| `SDLC_GITHUB_APP_ID` | пусто | идентификатор GitHub App; без него доступ идёт общим токеном |
| `SDLC_GITHUB_APP_KEY_FILE` | пусто | файл с приватным ключом приложения — единственный секрет доступа к GitHub |
| `SDLC_GITHUB_TOKEN_<NAME>` | пусто | токен для запасного пути: имя `<NAME>` лежит в `projects.github_credential_ref` или `repositories.credential_ref` |
| `SDLC_DEFAULT_ACTOR` | `anonymous` | актор без заголовка (режим `header`) |
| `SDLC_BLOB_DIR` | пусто | каталог для вынесенного содержимого; пусто — всё inline |
| `SDLC_BLOB_INLINE_MAX_BYTES` | `262144` | до этого размера содержимое лежит в базе |
| `SDLC_BLOB_MAX_BYTES` | `67108864` | предел принимаемого содержимого |
| `SDLC_BLOB_S3_BUCKET` | пусто | бакет объектного хранилища; заданный перекрывает `SDLC_BLOB_DIR` |
| `SDLC_BLOB_S3_ENDPOINT` | по региону | адрес хранилища; для AWS достаточно региона |
| `SDLC_BLOB_S3_REGION` | `AWS_REGION` | регион, он же участвует в подписи |
| `SDLC_BLOB_S3_PREFIX` | пусто | префикс ключей внутри бакета |
| `SDLC_BLOB_S3_ACCESS_KEY_ID` | `AWS_ACCESS_KEY_ID` | ключ доступа |
| `SDLC_BLOB_S3_SECRET_ACCESS_KEY` | `AWS_SECRET_ACCESS_KEY` | секрет доступа |
| `SDLC_BLOB_S3_PATH_STYLE` | `true` | адресация `endpoint/bucket/key`; `false` — `bucket.endpoint` |
| `SDLC_GITHUB_WEBHOOK_SECRET` | пусто | секрет подписи вебхуков; без него ручка выключена |
| `SDLC_INDEX_POLL_INTERVAL` | `15s` | как часто воркер смотрит в очередь прогонов между опросами |
| `SDLC_INDEX_RETAIN_RUNS` | `5` | сколько завершённых прогонов хранить на ветку |
| `SDLC_BLOB_GC_ENABLED` | `true` | собирать ли мусор в блобах: блоб без ссылок из артефактов, файлов индекса и документов |
| `SDLC_BLOB_GC_INTERVAL` | `1h` | как часто воркер гоняет сборку (в compose — `5m`) |
| `SDLC_BLOB_GC_LIMIT` | `500` | сколько блобов удалять за проход |
| `SDLC_BLOB_GC_MIN_AGE` | `24h` | насколько старым должен быть кандидат (в compose — `1h`) |
| `SDLC_BLOB_GC_DRY_RUN` | `false` | считать и не удалять: посмотреть в лог перед включением |
| `SDLC_EMBEDDING_CACHE_GC_ENABLED` | `true` | вытеснять ли кэш эмбеддингов: ключ по содержимому, владельца нет, уменьшать его больше некому |
| `SDLC_EMBEDDING_CACHE_GC_INTERVAL` | `6h` | как часто воркер гоняет вытеснение (в compose — `5m`) |
| `SDLC_EMBEDDING_CACHE_TTL` | `720h` | сколько строка живёт без использования (в compose — `24h`); `0` — не вытеснять по сроку |
| `SDLC_EMBEDDING_CACHE_MAX_ROWS` | `1000000` | верхняя граница размера в строках (~4 ГБ при `vector(1024)`); `0` — без границы |
| `SDLC_EMBEDDING_CACHE_GC_LIMIT` | `10000` | сколько строк удалять за проход |
| `SDLC_EMBEDDING_CACHE_GC_DRY_RUN` | `false` | считать и не удалять |
| `SDLC_EMBEDDING_CACHE_TOUCH_AFTER` | `1h` | не переписывать отметку использования чаще этого |
| `SDLC_MCP_ENABLED` | `true` | поднимать ли MCP-сервер в этом же процессе |
| `SDLC_LEASE_TTL` | `30m` | сколько живёт аренда без heartbeat |
| `SDLC_HEARTBEAT_INTERVAL` | `5m` | как часто агент обязан её продлевать |
| `SDLC_LEASE_SWEEP_INTERVAL` | `1m` | как часто воркер ищет протухшие аренды |
| `SDLC_INDEX_ENABLED` | `true` | индексировать ли репозитории |
| `SDLC_INDEX_INTERVAL` | `5m` | как часто сверять головы веток (в compose — `1m`) |
| `SDLC_INDEX_STUCK_AFTER` | `30m` | когда вернуть в очередь прогон, застрявший в `running` |
| `SDLC_INDEX_MAX_FILE_BYTES` | `524288` | предел размера индексируемого файла |
| `SDLC_INDEX_MAX_FILES` | `20000` | предел числа файлов в прогоне |
| `SDLC_INDEX_CHUNK_CHARS` | `1600` | верхняя граница куска |
| `SDLC_GITHUB_TOKEN` | пусто | токен на чтение репозиториев |
| `SDLC_GITHUB_API_URL` | `https://api.github.com` | адрес API (GitHub Enterprise) |
| `SDLC_EMBEDDINGS_PROVIDER` | по ключу | `voyage`, `hash` или `none` |
| `SDLC_VOYAGE_API_KEY` | пусто | ключ Voyage; без него провайдер — `none` |
| `SDLC_EMBEDDINGS_MODEL` | `voyage-code-3` | модель; размерность фиксирована схемой |
| `SDLC_EMBEDDINGS_BATCH` | `96` | сколько кусков уходит в модель за раз |
| `SDLC_LOG_LEVEL` | `info` | `debug`, `warn`, `error` |
| `SDLC_ENV` | `development` | не-development включает release-режим Gin и JSON-логи |

`SDLC_HEARTBEAT_INTERVAL` обязан быть меньше `SDLC_LEASE_TTL` — иначе аренда
протухнет раньше первого продления, и сервис откажется стартовать. Обе величины
задаются и приложению, и воркеру: он решает, что аренда истекла, по тем же
правилам, по которым приложение её выдаёт.

`SDLC_EMBEDDINGS_PROVIDER` выводится из наличия ключа: с ключом — `voyage`, без
— `none`. Задавать вручную нужно только ради `hash`.

**`SDLC_STRICT_GATES`** решает, что делать с гейтом типа, который движок не
умеет исполнять. Все девять типов из схемы он умеет, так что флаг работает
только на гейт неизвестного типа: `false` — такой гейт отдаёт `skipped` и
переход пропускает, `true` — валит переход. В продакшене значение обязано быть
`true`, иначе задача проезжает мимо непроверенных условий.

## Воркер

Отдельный процесс, а не горутина в приложении: реплик приложения может быть
несколько, и каждая гоняла бы свой проход по одним и тем же арендам. Схему
воркер не катит — она уже накачена приложением.

Заданий пять, и идут они последовательно: возврат протухших аренд;
индексация репозиториев с детектором дрейфа сразу после каждого прогона;
вычистка старых прогонов без ссылающихся базовых линий; синхронизация
состояния привязанных PR (её читает гейт `external_check`); разбор артефактов
на куски и досчёт векторов. Между полными проходами воркер раз в
`SDLC_INDEX_POLL_INTERVAL` забирает из очереди то, что положил вебхук.

```bash
docker compose -f dev/docker-compose.yml logs -f worker
```

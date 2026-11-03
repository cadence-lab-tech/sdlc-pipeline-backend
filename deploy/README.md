# Деплой

Одна машина, один домен, четыре контейнера за Caddy. Что и куда
разворачивается — здесь; чем машина создаётся и как чинится — Terraform и
скрипты машины в локальной папке `devops/` владельца инстанса, вне
репозитория.

## Что где крутится

```
                        ┌─ caddy (80/443, TLS сам) ─┐
  https://<домен>/ ─────┤  /v1 /mcp /webhooks ──────┼── api      Go :8080 ──┐
                        │  остальное ───────────────┼── ui       статика    │
                        └───────────────────────────┘   worker   Go        ├── postgres 18 + pgvector
                                                                            │
                                                             S3-хранилище ◄─┘  содержимое артефактов
```

Наружу торчит только Caddy. Один домен на всё: интерфейс ходит в `/v1` того
же origin, агенты — в `/mcp/projects/<KEY>`, GitHub — в `/webhooks/github`.
Второго списка адресов нет — нет и CORS.

## Файлы

| | что это |
|---|---|
| `deploy/docker-compose.yml` | стек целиком; лежит на сервере в `/opt/sdlc` |
| `deploy/Caddyfile` | TLS и маршрутизация; правки доезжают выкладкой — `deploy.sh` делает `caddy reload` |
| `deploy/deploy.sh` | выкладка на сервере, она же откат: `backend <тег>` или `frontend <тег>` |
| `deploy/backup.sh` | дамп базы с ротацией, cron 03:00 |
| `deploy/postgres/major-upgrade.sh` | переезд базы на новую мажорную версию PostgreSQL перед `postgres` (раздел 7); копия файла пакета self-hosted |
| `deploy/.env.example` | шаблон `/opt/sdlc/.env`; собирает `devops/bin/bootstrap-env.sh` |
| `Dockerfile` | образ api, воркера и CLI |
| `.github/workflows/deploy.yml` | сборка образа в GHCR и выкладка `backend` |
| `Dockerfile` во frontend | образ интерфейса: сборка Vite, статика в Caddy |
| `.github/workflows/deploy.yml` во frontend | то же для образа `frontend` |

Два репозитория — два образа — две выкладки. Каждый workflow подменяет
только свой образ; на сервере они выстраиваются в очередь через `flock`.

## 1. Машина

Создаётся Terraform, руками не настраивается: он и скрипты машины лежат в
локальной папке `devops/`, вне репозитория.
`cloud-init` ставит Docker, заводит пользователя `deploy`, включает ufw и
cron дампа. Каталог `/opt/sdlc` уже существует и принадлежит `deploy`.

**Проверьте результат, а не статус.** `cloud-init status` говорит `done`,
даже когда половина шагов провалилась:

```bash
ssh -i ~/.ssh/sdlc-deploy deploy@<ip> 'id; docker --version; ls -la /opt/sdlc'
```

Нет Docker — смотреть `/var/log/cloud-init-output.log` и догонять руками.

## 2. Конфигурация на сервере

```bash
ACME_EMAIL=you@example.ru ./devops/bin/bootstrap-env.sh
```

Скрипт кладёт `/opt/sdlc/.env` с правами 600: домен и ключи S3 — из
Terraform, пароль базы, токен администратора и секрет вебхука — сгенерированы
на машине. Второй раз он не запустится: переписать `.env` — сменить пароль
базы под живым томом. Дальше правки только руками, по
[`.env.example`](.env.example).

Что стоит вписать руками, когда появится:

- `SDLC_GITHUB_APP_CLIENT_ID` / `_SECRET` — вход коллег через GitHub;
  callback в настройках приложения — `https://<домен>/login/github`;
- `SDLC_GITHUB_APP_ID` и ключ в `/opt/sdlc/secrets/github-app.pem`
  (`SDLC_GITHUB_APP_KEY_FILE=/secrets/github-app.pem`) — чтение
  репозиториев установкой приложения; вебхук — `https://<домен>/webhooks/github`
  с `SDLC_GITHUB_WEBHOOK_SECRET` из `.env`. Ключ читает контейнер от
  пользователя `nonroot` (uid 65532), поэтому после `scp` его надо отдать
  этому uid — shell в образе нет, делается одноразовым контейнером:

  ```bash
  docker run --rm -v /opt/sdlc/secrets:/s alpine:3 \
    sh -c 'chown 65532:65532 /s/github-app.pem && chmod 400 /s/github-app.pem && chmod 711 /s'
  ```

  Каталог остаётся за `deploy`: заменить ключ можно тем же `scp` и повтором
  команды. Приложению нужны ещё и верные часы: утверждение, выписанное «в
  будущем», GitHub отбивает как `Bad credentials`, и отказ не отличить от
  отозванного ключа — часы проверяет `./devops/bin/diagnose.sh`, раздел
  «часы»;
- `SDLC_VOYAGE_API_KEY` — семантический поиск; без него полнотекстовый.

После правки `.env` — `docker compose up -d`: compose перечитает окружение
и перезапустит только те контейнеры, у которых оно изменилось.

## 3. Секреты в GitHub

В **обоих** репозиториях, бэкенда и интерфейса. Секреты ssh лежат в
окружении `production` (`--env production`), а не в секретах репозитория:
окружение доступно только job'у `deploy` и только из ветки `main` (CLAB-142),
иначе любой участник с правом записи прочитал бы ключ выкладки через workflow
в своей ветке. Окружение и ограничение веткой — Settings → Environments →
`production` → Deployment branches: только `main`.

| | откуда |
|---|---|
| секрет окружения `SSH_KEY` | `~/.ssh/sdlc-deploy` целиком |
| секрет окружения `SSH_HOST` | `./devops/bin/tf.sh output -raw server_ip` |
| секрет окружения `SSH_USER` | `deploy` |
| секрет окружения `SSH_KNOWN_HOSTS` | `ssh-keyscan <ip>` |
| переменная репозитория `SDLC_DOMAIN` | домен — по нему workflow проверяет выкладку снаружи |

```bash
for repo in sdlc-pipeline-backend sdlc-pipeline-frontend; do
  gh secret set SSH_KEY -R cadence-lab-tech/$repo --env production < ~/.ssh/sdlc-deploy
  gh secret set SSH_HOST -R cadence-lab-tech/$repo --env production --body "$(./devops/bin/tf.sh output -raw server_ip)"
  gh secret set SSH_USER -R cadence-lab-tech/$repo --env production --body deploy
  ssh-keyscan "$(./devops/bin/tf.sh output -raw server_ip)" 2>/dev/null | gh secret set SSH_KNOWN_HOSTS -R cadence-lab-tech/$repo --env production
  gh variable set SDLC_DOMAIN -R cadence-lab-tech/$repo --body "$(./devops/bin/tf.sh output -raw domain)"
done
```

Пакеты в GHCR приватные. Workflow логинится на машине токеном своего
прогона (`GITHUB_TOKEN` читает пакет, привязанный к репозиторию), поэтому
personal access token не нужен — пока выкладка идёт через Actions.

## 4. Первая выкладка

Пуш в `main` любого из репозиториев собирает его образ и выкладывает. Порядок
первого раза важен: сначала бэкенд — `deploy.sh backend` поднимает базу, api
и воркер; Caddy при этом ждёт контейнер `ui`, которого ещё нет, и стек
целиком встанет после выкладки интерфейса. Проверка снаружи в workflow
бэкенда поэтому может покраснеть на первом прогоне — это ожидаемо, второй
образ её закроет.

```bash
gh run watch -R cadence-lab-tech/sdlc-pipeline-backend
gh run watch -R cadence-lab-tech/sdlc-pipeline-frontend
curl https://<домен>/readyz          # {"status":"ok","database":"ok"}
```

Первый вход — токеном администратора: `./devops/bin/remote-run.sh 'grep
^SDLC_ADMIN_TOKEN= .env'` (в переписку с агентом не выносить). Дальше —
`clab members set`, проекты заводят сами коллеги.

Локальный агент коллеги:

```bash
claude mcp add --transport http sdlc https://<домен>/mcp/projects/<KEY> \
    --header "Authorization: Bearer $SDLC_TOKEN"
```

## 5. Откат

```bash
./devops/bin/remote-read.sh version       # что выложено и что было до
gh workflow run deploy.yml -R cadence-lab-tech/sdlc-pipeline-backend -f tag=<sha>
```

Или руками на машине: `./deploy.sh backend $(cat .last-ok-backend)`. Миграции
назад не катятся: откат кода работает, пока новая миграция совместима со
старым — конвенция миграций это требует.

### Долгая миграция

Api катит миграции при старте и не отвечает на `/readyz`, пока они идут;
воркер ждёт здоровья api (`depends_on`). `deploy.sh` ждёт готовности api
**до 10 минут** от запуска стека (`DEPLOY_READY_TIMEOUT`, секунды, по
умолчанию 600), а `start_period` healthcheck'а api в `docker-compose.yml` —
не меньше этого предела, чтобы `up -d` не объявил api больным раньше.
Если `up -d` всё же упал, пока api стартует (например, предел поднят выше
`start_period`), скрипт ждёт api до предела и запускает стек повторно:
воркер не остаётся пересозданным, но не запущенным. Лог выкладки говорит,
что ждёт api и до какого предела, и раз в минуту — сколько осталось.

Предел вышел — job `deploy` красный, а в логе «выложить»:

- `api не готов за N с` — возможно, ещё идёт миграция. Смотреть
  `docker compose logs -f api` на машине;
- **не откатываться**, пока идёт миграция: `.last-ok-backend` не тронут,
  откат нужен, только если api упал (в логах — паника или ошибка миграции);
- воркер не запущен. Когда api станет `healthy` — довести стек:

  ```bash
  cd /opt/sdlc && docker compose up -d && docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile
  ```

  или повторить job `deploy` (тот же `./deploy.sh backend <тег>`);
- миграция заведомо долгая — повтор с большим пределом на машине:
  `DEPLOY_READY_TIMEOUT=1800 ./deploy.sh backend <тег>`. Тогда `up -d`
  упадёт на `start_period`, а скрипт дождётся api и доведёт стек сам.

В конце отчёта — `docker compose ps -a` и хвосты логов api и воркера.

## 6. Бэкапы

`backup.sh` по cron в 03:00 кладёт `pg_dump | gzip` в `/opt/sdlc/backups`,
хранит две недели. Содержимое артефактов в дамп не входит — оно в S3, в базе
адреса. Дампы лежат на той же машине: защищают от «уронил миграцией», не от
потери машины. Забирать наружу — следующий шаг, когда данных станет жалко.

Восстановление:

```bash
gunzip -c backups/sdlc-<дата>.sql.gz | docker compose exec -T postgres psql -U sdlc -d sdlc
```

## 7. Переезд на PostgreSQL 18

С CLAB-237 база — `pgvector/pgvector:pg18-trixie` (PostgreSQL 18, Debian 13),
данные в томе `sdlc_postgres` (`/var/lib/postgresql/18/docker`). Прежний
кластер 17 лежит в томе `sdlc_pgdata`; первая выкладка `main` с этим
compose переносит его тем же механизмом, что пакет self-hosted (раздел
«PostgreSQL 18» в `docs/admin.md` пакета): `up -d` в `deploy.sh` ждёт
одноразовые `pg-upgrade-dump` (`pg_dumpall` образом 17) и `pg-upgrade-load`
(загрузка в новый кластер 18), и только потом поднимает `postgres`, api и
воркер. Простой — на время дампа и загрузки, место — размер базы плюс
сжатый дамп.

Перед слиянием — ручной бэкап и проверка места:

```bash
cd /opt/sdlc && ./backup.sh
docker compose exec -T postgres psql -U sdlc -d postgres -Atc "SELECT pg_size_pretty(sum(pg_database_size(oid))) FROM pg_database"
df -h "$(docker info -f '{{.DockerRootDir}}')"
```

После выкладки:

```bash
docker compose exec -T postgres psql -U sdlc -d sdlc -Atc 'SHOW server_version'   # 18.x
docker compose logs pg-upgrade-dump pg-upgrade-load
docker compose logs postgres api worker | grep -i 'collation version mismatch'   # пусто
```

Переезд упал — выкладка красная, `postgres` на половине данных не
поднимается, `sdlc_pgdata` не тронут; следующий `up -d` начинает переезд
заново, а кластер 18, если он уже стоял на месте, откладывает в
`cadence-upgrade/replaced-<время>` тома `sdlc_postgres`. Откат на 17: прежний `docker-compose.yml` (revert и выкладка или руками
на машине) и `docker compose up -d` — 17 поднимается на `sdlc_pgdata` с
данными на момент переезда; записанное после — из бэкапа. Когда откат больше
не нужен: `docker compose rm -f pg-upgrade-dump && docker volume rm
sdlc_pgdata`.

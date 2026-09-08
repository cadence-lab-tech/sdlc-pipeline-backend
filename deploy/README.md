# Деплой

Одна машина в Timeweb, один домен, четыре контейнера за Caddy. Что и куда
разворачивается — здесь; чем машина создаётся и как чинится —
[devops/README.md](../devops/README.md). Почему именно так —
[решение 0031](https://github.com/stepaKaziatko/sdlc-pipeline-docs/blob/main/decisions/0031-deploy-timeweb-single-vps.md).

## Что где крутится

```
                        ┌─ caddy (80/443, TLS сам) ─┐
  https://<домен>/ ─────┤  /v1 /mcp /webhooks ──────┼── api      Go :8080 ──┐
                        │  остальное ───────────────┼── ui       статика    │
                        └───────────────────────────┘   worker   Go        ├── postgres 17 + pgvector
                                                                            │
                                                              S3 Timeweb ◄──┘  содержимое артефактов
```

Наружу торчит только Caddy. Один домен на всё: интерфейс ходит в `/v1` того
же origin, агенты — в `/mcp/projects/<KEY>`, GitHub — в `/webhooks/github`.
Второго списка адресов нет — нет и CORS.

## Файлы

| | что это |
|---|---|
| `deploy/docker-compose.yml` | стек целиком; лежит на сервере в `/opt/sdlc` |
| `deploy/Caddyfile` | TLS и маршрутизация |
| `deploy/deploy.sh` | выкладка на сервере, она же откат: `backend <тег>` или `frontend <тег>` |
| `deploy/backup.sh` | дамп базы с ротацией, cron 03:00 |
| `deploy/.env.example` | шаблон `/opt/sdlc/.env`; собирает `devops/bin/bootstrap-env.sh` |
| `Dockerfile` | образ api, воркера и CLI |
| `.github/workflows/deploy.yml` | сборка образа в GHCR и выкладка `backend` |
| `Dockerfile` во frontend | образ интерфейса: сборка Vite, статика в Caddy |
| `.github/workflows/deploy.yml` во frontend | то же для образа `frontend` |

Два репозитория — два образа — две выкладки. Каждый workflow подменяет
только свой образ; на сервере они выстраиваются в очередь через `flock`.

## 1. Машина

Создаётся Terraform, руками не настраивается: [devops/README.md](../devops/README.md).
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
- `SDLC_GITHUB_APP_ID` и ключ в `/opt/sdlc/secrets/github-app.pem` (права
  600, `SDLC_GITHUB_APP_KEY_FILE=/secrets/github-app.pem`) — чтение
  репозиториев установкой приложения; вебхук — `https://<домен>/webhooks/github`
  с `SDLC_GITHUB_WEBHOOK_SECRET` из `.env`;
- `SDLC_VOYAGE_API_KEY` — семантический поиск; без него полнотекстовый.

После правки `.env` — `docker compose up -d`: compose перечитает окружение
и перезапустит только те контейнеры, у которых оно изменилось.

## 3. Секреты в GitHub

В **обоих** репозиториях, бэкенда и интерфейса:

| | откуда |
|---|---|
| секрет `SSH_KEY` | `~/.ssh/sdlc-deploy` целиком |
| секрет `SSH_HOST` | `./devops/bin/tf.sh output -raw server_ip` |
| секрет `SSH_USER` | `deploy` |
| секрет `SSH_KNOWN_HOSTS` | `ssh-keyscan <ip>` |
| переменная `SDLC_DOMAIN` | домен — по нему workflow проверяет выкладку снаружи |

```bash
for repo in sdlc-pipeline-backend sdlc-pipeline-frontend; do
  gh secret set SSH_KEY -R stepaKaziatko/$repo < ~/.ssh/sdlc-deploy
  gh secret set SSH_HOST -R stepaKaziatko/$repo --body "$(./devops/bin/tf.sh output -raw server_ip)"
  gh secret set SSH_USER -R stepaKaziatko/$repo --body deploy
  ssh-keyscan "$(./devops/bin/tf.sh output -raw server_ip)" 2>/dev/null | gh secret set SSH_KNOWN_HOSTS -R stepaKaziatko/$repo
  gh variable set SDLC_DOMAIN -R stepaKaziatko/$repo --body "$(./devops/bin/tf.sh output -raw domain)"
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
gh run watch -R stepaKaziatko/sdlc-pipeline-backend
gh run watch -R stepaKaziatko/sdlc-pipeline-frontend
curl https://<домен>/readyz          # {"status":"ok","database":"ok"}
```

Первый вход — токеном администратора: `./devops/bin/remote-run.sh 'grep
^SDLC_ADMIN_TOKEN= .env'` (в переписку с агентом не выносить). Дальше —
`sdlc members set`, проекты заводят сами коллеги.

Локальный агент коллеги:

```bash
claude mcp add --transport http sdlc https://<домен>/mcp/projects/<KEY> \
    --header "Authorization: Bearer $SDLC_TOKEN"
```

## 5. Откат

```bash
./devops/bin/remote-read.sh version       # что выложено и что было до
gh workflow run deploy.yml -R stepaKaziatko/sdlc-pipeline-backend -f tag=<sha>
```

Или руками на машине: `./deploy.sh backend $(cat .last-ok-backend)`. Миграции
назад не катятся: откат кода работает, пока новая миграция совместима со
старым — конвенция миграций это требует.

## 6. Бэкапы

`backup.sh` по cron в 03:00 кладёт `pg_dump | gzip` в `/opt/sdlc/backups`,
хранит две недели. Содержимое артефактов в дамп не входит — оно в S3, в базе
адреса. Дампы лежат на той же машине: защищают от «уронил миграцией», не от
потери машины. Забирать наружу — следующий шаг, когда данных станет жалко.

Восстановление:

```bash
gunzip -c backups/sdlc-<дата>.sql.gz | docker compose exec -T postgres psql -U sdlc -d sdlc
```

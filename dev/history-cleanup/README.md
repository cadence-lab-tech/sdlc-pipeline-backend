# Чистка истории backend от `devops/` и открытие (CLAB-94)

Порядок для владельца. Действия необратимы и внешние (force-push, видимость
репозиториев и пакетов, секреты), поэтому агент их не исполняет: он подготовил
скрипт [`cleanup.sh`](cleanup.sh), опись и этот порядок, а точку невозврата
проходит человек. Цель: в опубликованной истории backend нет `devops/`, а
backend, frontend, `self-hosted` и образы GHCR читаются без входа; `docs`
остаётся приватным.

Каталог разовый: после проведения его можно убрать отдельным коммитом.

## Что проверено репетицией

Весь порядок — `snapshot`, `rewrite`, `verify`, `push`, `verify-remote`, `map`,
`rollback` — прогнан на зеркале реального репозитория (состояние `2fe35aa`)
против локального bare-репозитория вместо GitHub. На GitHub ничего не
отправлялось.

| Факт | До | После |
|---|---|---|
| Коммитов в `main` | 72 | 71 (вычеркнут только `785963f`: он правил один файл в `devops/`) |
| Дерево `main` (`main^{tree}`) | `c8c8f012…` | то же |
| Коммитов с прежним SHA | | 16 (всё до `89ad235`); у остальных SHA новые |
| Теги | `v0.1.0` … `v0.5.1`, аннотированные | те же шесть, автор, дата и сообщение те же; деревья отличаются только путями `devops/` |
| `rev-list --all --objects \| grep ' devops/'` | 37 объектов | 0 |
| Ссылки задач и baseline'ы | 22 коммита + 29 baseline'ов | все 51 меняют SHA, ни один не вычеркнут |

Та же последовательность на синтетическом репозитории без сети и `clab` —
`cleanup_test.sh` (нужен `git-filter-repo` в PATH); он же регрессия на правки
скрипта.

Негативные проверки `verify` тоже пройдены: потерянный тег, фильтр по чужому
пути (`deploy`) и неприменённый фильтр дают FAIL.

Чего репетиция не покрывает: настоящий push на GitHub (он принимает только
явные ссылки, `--mirror` не годится из-за `refs/pull/*`, поэтому скрипт
пушит `main` и теги поимённо); gitleaks (не был установлен); право `push -f`
на `main` у вас на GitHub (защиты ветки у приватного репозитория на этом
тарифе нет, `gh api …/branches/main/protection` отвечает 403 «Upgrade»).

## Нужно решить до начала

Рекомендации аналитика (`analysis.open_questions` задачи) — в скобках.

1. Способ: `git filter-repo --path devops --invert-paths` на зеркале (рекомендован)
   или новая история. Скрипт написан под filter-repo.
2. `refs/pull/*`: 26 PR (25 влитых и #26 этой задачи) держат старые коммиты с
   `devops/`, force-push их не убирает. Секретов там нет. Не ждать GitHub
   Support, обращение отправить параллельно, если важно, чтобы шаблоны
   Terraform не были достижимы по SHA.
3. Старые версии образов в пакетах: видимость переключается на весь пакет, и
   публичными станут все теги, в том числе сборки по SHA до CLAB-93 с
   упоминанием домена и хостинга нашего инстанса в строках бинарника.
   Удаление версий необратимо. Рекомендация: оставить `vX.Y.Z` и последние
   сборки для отката инстанса, остальное удалить до открытия.
4. Упоминания домена и хостинга в 4 коммитах `main` вне `devops/` и личный
   адрес автора в 47 коммитах: оставить (рекомендация) или переписать тем же
   прогоном (`--mailmap`, второй случай переписать SHA не будет). Решать до
   `rewrite`.
5. Область задачи: уборка в `self-hosted` (шаг 9) вне области CLAB-94
   (`backend`, `frontend`). Либо `clab scope CLAB-94 --add
   cadence-lab-tech/self-hosted --reason "…"`, либо обычный коммит владельца.
6. Окно проведения: вечер без задач в работе, одним заходом.

## Порядок

Точка невозврата — шаг 5. До неё всё обратимо; после неё откат возможен, пока
никто не склонировал новую историю и не запушил поверх.

| # | Шаг | Команда |
|---|---|---|
| 0 | Условия начала | `cleanup.sh preflight` — все `ok` |
| 1 | Слить PR этой задачи, убрать рабочие копии и стек CLAB-94 | PR открыт — П1 не пройти |
| 2 | Заморозка выкладок, лишние ветки | см. ниже |
| 3 | Снимок | `cleanup.sh snapshot` |
| 4 | Переписать копию и проверить | `cleanup.sh rewrite && cleanup.sh verify` |
| 5 | **Точка невозврата** | `cleanup.sh push` |
| 6 | Проверка с сервера, опись, выкладки обратно | `cleanup.sh verify-remote`, `map`, `gh workflow enable` |
| 7 | Видимость репозиториев | `gh repo edit … --visibility public` |
| 8 | Пакеты GHCR | в вебе, см. ниже |
| 9 | Уборка `self-hosted` | см. ниже |
| 10 | Локальная уборка, сообщение коллегам | см. ниже |

Скрипт читает `CLEANUP_DIR` (по умолчанию `~/clab-history-cleanup`, вне
репозиториев) и `REPO_URL`; запускается из любого каталога. Нужны `git`,
`git-filter-repo` (`pipx install git-filter-repo`), `python3`, `gh`, `clab`.

### 1. Слияние PR задачи

Задача идёт по циклу до `release`; слияние и снятие рабочих копий делает роль
`developer` по её инструкции. Пока PR открыт или `.worktrees/` не пуст,
`preflight` не пройдёт. Слияние пересоберёт и выложит `main`
(`deploy.yml` не игнорирует `dev/`): это ожидаемо и безвредно.

### 2. Заморозка

Без неё push `main` выложит инстанс, а шесть тегов шесть раз запустят
`release.yml`: пересоберут и перезапишут образы `vX.Y.Z`, `gh release upload
--clobber` в тапе, шесть событий тапу и `self-hosted`.

```bash
gh workflow disable deploy  -R cadence-lab-tech/sdlc-pipeline-backend
gh workflow disable release -R cadence-lab-tech/sdlc-pipeline-backend
git push origin --delete clab/CLAB-9-repo-detach   # слита PR #6; в ней остаётся devops/
```

Выключенный workflow события не копит: после `enable` старые push'и не
перезапускаются.

### 3. Снимок

`cleanup.sh snapshot` кладёт в `CLEANUP_DIR`: `backup.git` (зеркало, включая
`refs/pull/*`), `backend.bundle` (проверенный), `ls-remote-before.txt`,
`state-before.txt`, `log-main-before.txt`, `tasks-refs-before.tsv` (опись П6),
`devops-copy/` и `devops.sha256` (П4). `devops-copy/` содержит `.env` и
`terraform.tfstate` — единственную копию состояния Terraform, у которой нет
удалённого backend. Унесите её с этой машины (шифрованный носитель), прежде чем
идти дальше: `git clean -fdx` или замена каталога клоном её уничтожат.

Повторный `snapshot` в тот же каталог отказывается: бэкап «до» делается один
раз.

### 4. Переписать и проверить

`rewrite` клонирует зеркало, убирает `refs/pull/*` (сервер их не принимает) и
запускает `git filter-repo --path devops --invert-paths`. `verify` проверяет
Р1–Р5 против бэкапа, сервер не трогается:

- нет `devops/` ни в `git log --all -- devops`, ни среди объектов (Р1);
- дерево `main` совпало (Р2). Значение считается в момент снимка, а не берётся
  из аналитики: после слияния PR задачи дерево другое;
- число коммитов `main` = до минус коммиты, правившие только `devops/`
  (считается независимо по деревьям, а не по `commit-map`); автор, дата и тема
  сообщения остальных совпали строка в строку (Р3). Тело сообщения
  `filter-repo` переписывает только там, где в нём назван прежний сокращённый
  SHA коммита: на 2026-10-11 это один коммит, `c8b19f0` (`4beec24` →
  `2a5e1b9`, новый SHA того же коммита); `verify` тела не сверяет;
- те же теги, у каждого та же подпись и сообщение, дерево отличается только
  путями `devops/` (Р4);
- `git fsck`; gitleaks по полной истории, если установлен (Р5) — иначе
  `brew install gitleaks` и `gitleaks detect --source "$CLEANUP_DIR/work.git"
  --log-opts=--all` руками.

Любой FAIL — в GitHub не пушится ничего, `work.git` удаляется и строится
заново из `backup.git`.

### 5. Точка невозврата

```bash
cleanup.sh push
```

Перед отправкой скрипт повторяет `verify`, сверяет, что ветки и теги на сервере
всё ещё равны снимку (иначе чужой push затёрся бы) и что других веток там нет,
показывает, что уйдёт, и просит набрать фразу подтверждения. Уходит
`+refs/heads/main` и `+refs/tags/vX` по именам с `--force-with-lease` по
`main`.

Откат: `cleanup.sh rollback` (спросит подтверждение) возвращает `main` и теги из
бэкапа. Годится, пока никто не клонировал новую историю.

### 6. После push

```bash
cleanup.sh verify-remote   # свежий клон: Р1-Р4, SHA на сервере = отправленные, одна ветка
cleanup.sh map             # Р9: tasks-refs-after.tsv и relink.sh (сам не запускается)
gh workflow enable deploy  -R cadence-lab-tech/sdlc-pipeline-backend
gh workflow enable release -R cadence-lab-tech/sdlc-pipeline-backend
```

Затем:

- Р7: `workflow_dispatch` на `deploy` (поле `tag` пустое) или любой коммит
  `main` собирает образ и выкладывает; `https://<домен инстанса>/readyz` — 200 с
  `database: ok`, `GET /v1/instance` отвечает версией.
- Р8: после push на инстанс придёт вебхук `push`, `clab index` покажет прогон
  на новом HEAD в `done`, поиск по коду инстанса находит файл backend.
  Индексатор прежних SHA не читает: файлы переиспользуются по blob SHA, а
  прежнее содержимое берётся из базы, не с GitHub
  (`internal/indexer/indexer.go:375`, `:504-508`); детектор дрейфа
  сравнивает SHA строками (`internal/store/postgres/drift.go:45`, `:53`), поэтому
  задачи в полёте получают сигнал дрейфа, а не ошибку. В полёте сейчас задач
  нет, у задач в `new` baseline'ов ещё нет (ставятся при захвате).
- Р9: ссылки задач на коммиты backend, потерявшие SHA, — таблица
  `tasks-refs-after.tsv`; приложить к CLAB-94. Для результатов работы
  (`kind=commit`) в `relink.sh` уже собраны `clab link … --ref <новый SHA>`:
  прочитать и запустить. Номера PR и теги сохранились.
- Р6: `git ls-remote origin | grep -c refs/pull` — 26 после слияния #26 (по
  одному `refs/pull/N/head` на PR; в критериях 25 было до #26). Через них старые
  коммиты достижимы по SHA, пока Support не вычистит.

### 7. Видимость репозиториев

Только после шага 6: пока репозиторий приватен, остаток старой истории виден
членам организации. Открытое назад не отозвать, копии останутся.

```bash
gh repo edit cadence-lab-tech/sdlc-pipeline-backend  --visibility public --accept-visibility-change-consequences
gh repo edit cadence-lab-tech/sdlc-pipeline-frontend --visibility public --accept-visibility-change-consequences
gh repo view cadence-lab-tech/sdlc-pipeline-docs --json visibility -q .visibility   # PRIVATE
```

Форков нет и `allow_forking=false`; после открытия решите, оставлять ли его.
Защиту `main` на публичном репозитории включить можно, на приватном тарифе
нет.

### 8. Пакеты GHCR

Видимость пакета не следует за репозиторием, и через REST она не
переключается, только в вебе: организация → Packages → `sdlc-pipeline-backend`
→ Package settings → Change package visibility → Public; то же для
`sdlc-pipeline-frontend`. Список версий и удаление через `gh api` требуют
`gh auth refresh -s read:packages,delete:packages` (сейчас у токена этих прав
нет, пакеты из-под `gh` не видны).

```bash
docker logout ghcr.io
docker pull ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.5.1    # без входа
docker pull ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v0.5.1
c=$(docker create ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.5.1)
docker export "$c" | tar -t | grep -E '(^|/)(devops|\.git|docs)(/|$)'    # пусто
docker export "$c" | tar -xO LICENSE | head -3                           # FSL-1.1-ALv2
docker rm "$c"
```

Версии до CLAB-93 (решение 3) проверяются `strings` по бинарнику на домен и
хостинг; лишние версии удаляются до открытия, а не после: удалённая версия
образа не возвращается.

Машина инстанса образы не ломает: `deploy.sh` делает `docker compose pull`
без своего входа, полагаясь на `docker login` из workflow (`deploy.yml:122-123`),
и после открытия работает и без него.

### 9. Уборка `self-hosted` (вне области задачи, решение 5)

Образы публичны, поэтому `check.yml` зелёный и без токена:

```bash
gh secret delete GHCR_READ_TOKEN -R cadence-lab-tech/self-hosted
# и отозвать сам PAT (read:packages) в настройках GitHub
```

Тексты, которые остаются неверными (проверено на `eb9f37f`): `README.md:89-90`,
`install.sh:227`, `upgrade.sh:118`, `docs/admin.md:60-61`,
`.github/workflows/check.yml:10-11` и шаги с `GHCR_READ_TOKEN` на `:58-96`.
Проверка: `git grep -n -i -E 'GHCR_READ_TOKEN|private during|are private|docker
login ghcr|may need a registry login|While images are private'` пуст.

### 10. Локальная уборка

- Все рабочие копии backend (владельца и других машин, раннера) заменить
  свежими клонами. Оставляемые: `git fetch origin && git reset --hard
  origin/main`, затем `git reflog expire --expire=now --all && git gc
  --prune=now`; `git log --all -- devops` пуст в каждой.
- Локальные ветки без запушенной работы переносить патчем, не `git pull`:
  история разошлась.
- В `backend/.git` remote `container-use` и каталог
  `~/.config/container-use/repos/github.com/cadence-lab-tech/sdlc-pipeline-backend`
  несут копию старой истории; решение 0041 отказалось от container-use,
  убрать их (`git remote remove container-use`, затем каталог).
- `backend/devops/` на месте, не в git (`.gitignore`), совпадает с
  `devops.sha256`: `cd backend/devops && shasum -a 256 -c "$CLEANUP_DIR/devops.sha256"`.

## Критерии приёмки и чем проверяются

| Критерий | Проверка |
|---|---|
| П1, П2, П5 | `cleanup.sh preflight` |
| П3, П4, П6 | `cleanup.sh snapshot` (bundle verify, `diff -r` копии, опись) |
| Р1–Р5 до push | `cleanup.sh verify` |
| Р1–Р4 на сервере, одна ветка | `cleanup.sh verify-remote` |
| Р6 | `git ls-remote origin \| grep -c refs/pull` (26: 25 влитых PR и #26) |
| Р7, Р8 | шаг 6, вручную |
| Р9 | `cleanup.sh map` и `relink.sh` |
| О1 | `gh repo view … --json visibility` |
| О2, О3, О4 | шаг 8, вручную |
| У1, У2 | шаг 9 |
| У3 | шаг 10 |
| У4 | следующий тег проходит `release.yml` до конца |

## Опись на 2026-10-11

Снято только чтением.

- **Видимость**: backend, frontend, docs — PRIVATE; `self-hosted`, `homebrew-tap` — PUBLIC.
- **backend на сервере**: ветки `main` и `clab/CLAB-9-repo-detach`; теги
  `v0.1.0` … `v0.5.1`; открытых PR нет, форков нет. frontend: ветка `main`, теги
  `v0.5.0`, `v0.5.1` (переписывать не нужно: путей `devops/` там нет).
- **Workflows backend** (оба `active`): `deploy` (push в `main`, кроме
  `docs/**`, `devops/**`, `README.md`, `CLAUDE.md`, `.claude/**`; `workflow_dispatch`),
  `release` (push тега `v*`). У frontend свои `deploy` и `release`, их трогать
  не нужно.
- **Секреты**: backend — `SELF_HOSTED_DISPATCH_TOKEN`, `SSH_HOST`, `SSH_KEY`,
  `SSH_KNOWN_HOSTS`, `SSH_USER`, `TAP_DISPATCH_TOKEN`; frontend — те же без
  `TAP_DISPATCH_TOKEN`; `self-hosted` — `GHCR_READ_TOKEN`, `RELEASE_PR_TOKEN`.
  Ничего из этого чистка не меняет, кроме `GHCR_READ_TOKEN` на шаге 9.
- **Пакеты GHCR**: `sdlc-pipeline-backend`, `sdlc-pipeline-frontend`; список
  версий не снят (нет `read:packages`).
- **Локально**: `backend/devops/` вне git, внутри `.env`, `terraform/terraform.tfvars`,
  `terraform/terraform.tfstate` и `.backup`; remote `container-use` и
  `~/.config/container-use/repos/github.com/cadence-lab-tech/sdlc-pipeline-backend`;
  в основном клоне backend локальные ветки `clab/CLAB-1-…` (на сервере удалена) и
  `fix/task-scope-remove` (того же заголовка, что #25 в `main`); в обеих нет
  коммитов, которых не было бы в `main` по содержанию. Других клонов backend под
  домашним каталогом не найдено (`find ~ -maxdepth 5`).
- **Задачи**: 100 задач проекта, в работе только CLAB-94; ссылок на коммиты
  backend 22, baseline'ов backend 29.

## Что станет неверным после открытия

Не правилось в этой задаче: описание ограничило её чисткой и открытием, а
тексты верны, пока пакеты приватны. После шага 8 они устаревают и просят
отдельной мелкой правки в каждом репозитории (поведение при этом не меняется:
`docker login` в `deploy.yml` на публичном пакете безвреден).

- backend: `.github/release/notify-self-hosted.sh:37-40`,
  `.github/workflows/deploy.yml:111-114`, `deploy/README.md:116-118`,
  `README.md:275-278` («пока пакеты GHCR приватные», Manage Actions access).
- frontend: `.github/release/notify-self-hosted.sh:37-40` (тот же файл),
  `README.md:76-79`.
- В `.claude/settings.json` backend перечислены разрешения на скрипты
  `./devops/bin/…`: путь `devops/` фильтр не затрагивает, и файл станет
  публичным вместе с репозиторием. Это имена локальных скриптов, не секреты.

## Известные риски

- **Метка образов.** `org.opencontainers.image.revision` у `v0.5.0` и
  `v0.5.1` — старый SHA, после чистки коммита с таким SHA нет. Образы не
  пересобираются, приемлемо.
- **Детектор дрейфа** на несуществующем `to_commit` проверен чтением кода, не
  прогоном: если в полёте окажутся задачи, смотрите `clab drift <ключ>` после
  первого прогона индексации и заводите `bug`.
- **Вебхук push.** Force-push придёт на инстанс как обычный push и поставит
  прогон индексации; ожидаемо.

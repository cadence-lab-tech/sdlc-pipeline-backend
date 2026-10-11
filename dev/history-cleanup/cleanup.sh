#!/usr/bin/env bash
# Чистка истории backend от devops/ (CLAB-94): проверки и команды в одном месте.
#
# Скрипт готовит и проверяет, а необратимое делает только `push`, и то после
# набранного вручную подтверждения. Порядок, точка невозврата и откат — в
# README.md рядом; здесь только механика.
#
#   cleanup.sh preflight        условия начала (П1-П2, П5): ничего не меняет
#   cleanup.sh snapshot         бэкап: зеркало, bundle, ls-remote, опись ссылок задач, копия devops/
#   cleanup.sh rewrite          переписать копию зеркала: filter-repo --path devops --invert-paths
#   cleanup.sh verify           проверить переписанное до push (Р1-Р5), сервер не трогается
#   cleanup.sh push             ТОЧКА НЕВОЗВРАТА: force-push main и тегов; спросит подтверждение
#   cleanup.sh verify-remote    свежий клон с сервера и те же проверки после push
#   cleanup.sh map              Р9: старый SHA → новый для ссылок и baseline'ов задач
#   cleanup.sh rollback         вернуть main и теги из бэкапа; спросит подтверждение
#
# Окружение:
#   CLEANUP_DIR  рабочий каталог (по умолчанию ~/clab-history-cleanup); обязан лежать
#                вне репозиториев: в нём зеркало с полной старой историей
#   REPO_URL     откуда клонировать и куда пушить (по умолчанию GitHub backend);
#                путь к локальному bare-репозиторию годится для репетиции
#   CLAB_ROOT    рабочая директория с backend/, frontend/, docs/ (~/sdlc-pipeline)
#   SDLC_PROJECT ключ проекта для clab (CLAB)
#   CLAB_TASK    задача, которую не считать «в работе» в preflight (CLAB-94)
#
# Зависимости: git, git-filter-repo, python3; для preflight, snapshot и map ещё
# gh и clab; gitleaks — если стоит, verify прогонит и его.
set -euo pipefail

OWNER_REPO="cadence-lab-tech/sdlc-pipeline-backend"
FRONTEND_REPO="cadence-lab-tech/sdlc-pipeline-frontend"
REPO_URL="${REPO_URL:-https://github.com/${OWNER_REPO}.git}"
CLEANUP_DIR="${CLEANUP_DIR:-$HOME/clab-history-cleanup}"
CLAB_ROOT="${CLAB_ROOT:-$HOME/sdlc-pipeline}"
CLAB_TASK="${CLAB_TASK:-CLAB-94}"
export SDLC_PROJECT="${SDLC_PROJECT:-CLAB}"

DROP_PATH="devops"
BACKUP="$CLEANUP_DIR/backup.git"
WORK="$CLEANUP_DIR/work.git"
BRANCH="main"

FAILS=0
WARNS=0

say()  { printf '%s\n' "$*"; }
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*"; FAILS=$((FAILS + 1)); }
warn() { printf '  warn  %s\n' "$*"; WARNS=$((WARNS + 1)); }
die()  { printf 'ОШИБКА: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "нужен $1 ($2)"; }

# check "описание" <команда...>: успех команды — ok, иначе FAIL
check() {
  local what="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$what"; else bad "$what"; fi
}

summary() {
  say ""
  if [ "$FAILS" -eq 0 ]; then
    say "Итог: проверки пройдены (предупреждений: $WARNS)."
  else
    say "Итог: НЕ ПРОЙДЕНО проверок: $FAILS (предупреждений: $WARNS). Дальше не идти."
    exit 1
  fi
}

is_github() { case "$REPO_URL" in https://github.com/*) return 0 ;; *) return 1 ;; esac; }

# Рабочий каталог не должен быть внутри репозитория: зеркало несёт всю старую
# историю, и git clean или коммит «заодно» не должны его задеть.
ensure_workdir() {
  mkdir -p "$CLEANUP_DIR"
  if git -C "$CLEANUP_DIR" rev-parse --git-dir >/dev/null 2>&1; then
    die "$CLEANUP_DIR лежит внутри git-репозитория; задайте CLEANUP_DIR вне репозиториев"
  fi
}

need_filter_repo() {
  git filter-repo --version >/dev/null 2>&1 \
    || die "git-filter-repo не найден: pipx install git-filter-repo (или venv и PATH)"
}

# Коммиты зеркала без refs/pull: их GitHub не принимает и силой не удаляет.
non_pull_commits() { git -C "$1" rev-list --exclude='refs/pull/*' --all; }

# Коммиты, которые правят только пути devops/: фильтр по пути вычеркнет их
# целиком, потому что после фильтра у них нет изменений. Считается напрямую по
# деревьям, независимо от commit-map фильтра, чтобы сверка не была круговой.
devops_only_commits() {
  local repo="$1" c files total outside
  non_pull_commits "$repo" | while read -r c; do
    files=$(git -C "$repo" diff-tree --root --no-commit-id --name-only -r "$c")
    [ -n "$files" ] || continue
    total=$(printf '%s\n' "$files" | grep -c . || true)
    outside=$(printf '%s\n' "$files" | grep -v -c "^${DROP_PATH}/" || true)
    [ "$outside" -eq 0 ] && [ "$total" -gt 0 ] && printf '%s\n' "$c"
  done
  return 0
}

# ---------------------------------------------------------------- preflight

cmd_preflight() {
  say "== preflight: условия начала (ничего не меняет)"
  if ! is_github; then
    warn "REPO_URL не GitHub ($REPO_URL): проверки gh пропущены"
  else
    need gh "https://cli.github.com"
    local n
    for repo in "$OWNER_REPO" "$FRONTEND_REPO"; do
      n=$(gh pr list -R "$repo" --state open --json number -q 'length')
      [ "$n" = "0" ] && ok "П1 нет открытых PR в $repo" || bad "П1 открытых PR в $repo: $n"
    done

    local heads
    heads=$(git ls-remote --heads "$REPO_URL" | awk '{print $2}' | tr '\n' ' ')
    [ "$heads" = "refs/heads/$BRANCH " ] \
      && ok "П2 на сервере одна ветка $BRANCH" \
      || bad "П2 на сервере кроме $BRANCH есть ветки (в них остаётся devops/): $heads"

    local wf
    for wf in deploy release; do
      local state
      state=$(gh workflow list -R "$OWNER_REPO" --all | awk -v w="$wf" '$1 == w {print $2}')
      [ "$state" = "disabled_manually" ] \
        && ok "П5 workflow $wf выключен" \
        || bad "П5 workflow $wf: '$state' (нужно disabled_manually; gh workflow disable $wf -R $OWNER_REPO)"
    done
  fi

  if command -v clab >/dev/null 2>&1; then
    local busy
    busy=$(clab tasks --json | python3 -c '
import json, sys
skip = sys.argv[1]
active = {"analysis", "development", "testing", "merge", "release"}
tasks = json.load(sys.stdin)["tasks"]
print(" ".join(t["key"] + ":" + t["status_key"] for t in tasks
               if t["status_key"] in active and t["key"] != skip))
' "$CLAB_TASK")
    [ -z "$busy" ] && ok "П1 нет задач в работе, кроме $CLAB_TASK" || bad "П1 задачи в работе: $busy"
  else
    warn "clab не найден: задачи в работе не проверены"
  fi

  local repo dir count
  for repo in backend frontend docs; do
    dir="$CLAB_ROOT/$repo"
    if [ -d "$dir/.git" ] || [ -f "$dir/.git" ]; then
      count=$(git -C "$dir" worktree list | grep -c .)
      [ "$count" = "1" ] && ok "П2 в $repo одна рабочая копия" \
        || bad "П2 в $repo рабочих копий: $count (git -C $dir worktree list)"
    else
      warn "$dir не найден: рабочие копии не проверены"
    fi
  done
  [ -z "$(ls -A "$CLAB_ROOT/.worktrees" 2>/dev/null)" ] \
    && ok "П2 $CLAB_ROOT/.worktrees пуст" \
    || bad "П2 в $CLAB_ROOT/.worktrees есть каталоги: $(ls "$CLAB_ROOT/.worktrees" | tr '\n' ' ')"

  summary
}

# ----------------------------------------------------------------- snapshot

cmd_snapshot() {
  say "== snapshot: бэкап до первого изменения на сервере"
  ensure_workdir
  need git git
  need python3 python3
  [ ! -e "$BACKUP" ] || die "$BACKUP уже есть; снимок делается один раз, перенесите или удалите каталог осознанно"

  git clone --mirror --no-local "$REPO_URL" "$BACKUP"
  git ls-remote "$REPO_URL" >"$CLEANUP_DIR/ls-remote-before.txt"
  ok "зеркало: $BACKUP"

  git -C "$BACKUP" bundle create "$CLEANUP_DIR/backend.bundle" --all
  git -C "$BACKUP" bundle verify "$CLEANUP_DIR/backend.bundle" >/dev/null
  ok "bundle проверен: $CLEANUP_DIR/backend.bundle"

  {
    say "main $(git -C "$BACKUP" rev-parse "refs/heads/$BRANCH")"
    say "tree $(git -C "$BACKUP" rev-parse "refs/heads/$BRANCH^{tree}")"
    say "commits_main $(git -C "$BACKUP" rev-list --count "refs/heads/$BRANCH")"
    say "commits_all_without_pull $(non_pull_commits "$BACKUP" | grep -c .)"
    say "refs_pull $(git -C "$BACKUP" for-each-ref 'refs/pull/' | grep -c . || true)"
  } >"$CLEANUP_DIR/state-before.txt"
  cat "$CLEANUP_DIR/state-before.txt"

  # Авторы, даты и сообщения main «до»: сверка в verify.
  git -C "$BACKUP" log --format='%H|%an|%ae|%ad|%cn|%ce|%cd|%s' --date=iso-strict "refs/heads/$BRANCH" \
    >"$CLEANUP_DIR/log-main-before.txt"

  # П6: опись ссылок задач на коммиты backend и baseline'ов.
  if command -v clab >/dev/null 2>&1; then
    inventory >"$CLEANUP_DIR/tasks-refs-before.tsv"
    ok "П6 опись ссылок задач: $(($(grep -c . "$CLEANUP_DIR/tasks-refs-before.tsv") - 1)) строк, tasks-refs-before.tsv"
  else
    warn "clab не найден: опись П6 не снята, а без неё Р9 не выполнить"
  fi

  # П4: копия локальной папки devops/ (в git её нет, внутри .env и tfstate).
  local src="$CLAB_ROOT/backend/devops"
  if [ -d "$src" ]; then
    local dst="$CLEANUP_DIR/devops-copy"
    umask 077
    mkdir -p "$dst"
    cp -Rp "$src/." "$dst/"
    if diff -r "$src" "$dst" >/dev/null; then
      ok "П4 копия devops/ совпадает с оригиналом: $dst (унесите на другой носитель)"
    else
      bad "П4 копия devops/ расходится с оригиналом"
    fi
    (cd "$src" && find . -type f -exec shasum -a 256 {} +) | sort -k2 >"$CLEANUP_DIR/devops.sha256"
  else
    warn "П4 $src не найден: копировать нечего"
  fi

  summary
}

# Опись: ключ задачи, тип (commit | baseline), репозиторий, SHA. Берётся из
# `clab show --json`, поля context.repo_refs и context.baselines.
inventory() {
  local keys key
  keys=$(clab tasks --json | python3 -c '
import json, sys
print("\n".join(t["key"] for t in json.load(sys.stdin)["tasks"]))')
  printf 'task\tkind\trepository\tsha\turl\n'
  for key in $keys; do
    clab show "$key" --json | python3 -c '
import json, sys
key, repo = sys.argv[1], sys.argv[2]
ctx = json.load(sys.stdin).get("context") or {}
for r in ctx.get("repo_refs") or []:
    if r.get("ref_type") == "commit" and r.get("repository") == repo:
        print("\t".join([key, "commit", repo, r["ref"], r.get("url") or ""]))
for b in ctx.get("baselines") or []:
    if b.get("repository") == repo:
        print("\t".join([key, "baseline", repo, b["commit"], ""]))
' "$key" "$OWNER_REPO"
  done
}

# ------------------------------------------------------------------ rewrite

cmd_rewrite() {
  say "== rewrite: filter-repo на копии зеркала (сервер не трогается)"
  ensure_workdir
  need_filter_repo
  [ -d "$BACKUP" ] || die "нет $BACKUP: сначала snapshot"
  [ ! -e "$WORK" ] || die "$WORK уже есть: удалите и повторите (бэкап не трогайте)"

  git clone --mirror --no-local "$BACKUP" "$WORK"
  # refs/pull/* GitHub не принимает на push и не даёт удалить; в рабочей копии
  # они только мешают проверке «в истории нет devops/».
  git -C "$WORK" for-each-ref --format='%(refname)' refs/pull/ \
    | while read -r ref; do git -C "$WORK" update-ref -d "$ref"; done

  (cd "$WORK" && git filter-repo --path "$DROP_PATH" --invert-paths --force)
  cp "$WORK/filter-repo/commit-map" "$CLEANUP_DIR/commit-map.txt"
  ok "готово: $WORK, commit-map сохранён в $CLEANUP_DIR/commit-map.txt"
  say "дальше: cleanup.sh verify"
}

# ------------------------------------------------------------------- verify

# Проверки над переписанным репозиторием $1 против бэкапа. Общие для verify
# (до push, WORK) и verify-remote (свежий клон с сервера).
verify_repo() {
  local after="$1" label="$2" t c
  git -C "$after" fsck --no-progress >/dev/null 2>&1 \
    && ok "fsck без ошибок" || bad "fsck нашёл повреждения"

  # Р1: нет devops/ ни в журнале, ни среди объектов.
  [ -z "$(git -C "$after" log --all --oneline -- "$DROP_PATH")" ] \
    && ok "Р1 git log --all -- $DROP_PATH пуст" || bad "Р1 в истории есть $DROP_PATH"
  [ "$(git -C "$after" rev-list --all --objects | grep -c " ${DROP_PATH}/" || true)" = "0" ] \
    && ok "Р1 среди объектов нет ${DROP_PATH}/" || bad "Р1 среди объектов есть ${DROP_PATH}/"

  # Р2: дерево main то же.
  local tb ta
  tb=$(git -C "$BACKUP" rev-parse "refs/heads/$BRANCH^{tree}")
  ta=$(git -C "$after" rev-parse "refs/heads/$BRANCH^{tree}")
  [ "$tb" = "$ta" ] && ok "Р2 дерево $BRANCH совпало: $ta" \
    || bad "Р2 дерево $BRANCH разошлось: было $tb, стало $ta"

  # Р3: число коммитов и их содержание.
  local dropped cb ca
  dropped=$(devops_only_commits "$BACKUP" | sort)
  cb=$(git -C "$BACKUP" rev-list --count "refs/heads/$BRANCH")
  ca=$(git -C "$after" rev-list --count "refs/heads/$BRANCH")
  [ "$ca" -eq $((cb - $(count_in_branch "$dropped"))) ] \
    && ok "Р3 коммитов в $BRANCH: $cb → $ca (вычеркнуты коммиты, правившие только $DROP_PATH/: $(count_in_branch "$dropped"))" \
    || bad "Р3 коммитов в $BRANCH: $cb → $ca, ожидалось $((cb - $(count_in_branch "$dropped")))"

  # Автор, дата, сообщение: после = до минус вычеркнутые, строка в строку.
  local before_log after_log
  before_log=$(git -C "$BACKUP" log --format='%H|%an|%ae|%ad|%cn|%ce|%cd|%s' --date=iso-strict "refs/heads/$BRANCH" \
    | grep -v -F -f <(printf '%s\n' "$dropped" | grep . || echo __none__) | cut -d'|' -f2-)
  after_log=$(git -C "$after" log --format='%H|%an|%ae|%ad|%cn|%ce|%cd|%s' --date=iso-strict "refs/heads/$BRANCH" \
    | cut -d'|' -f2-)
  [ "$before_log" = "$after_log" ] \
    && ok "Р3 авторы, даты и сообщения $BRANCH совпали" \
    || bad "Р3 авторы, даты или сообщения $BRANCH разошлись (разберите до push)"

  # Дерево каждого коммита вне devops/ не изменилось: сравниваем в объединённой копии.
  local scratch="$CLEANUP_DIR/.verify-scratch.git"
  rm -rf "$scratch"
  git clone -q --bare --no-local "$BACKUP" "$scratch"
  git -C "$scratch" fetch -q "$after" '+refs/heads/*:refs/after/heads/*' '+refs/tags/*:refs/after/tags/*'

  # Р4: теги на месте, подписи те же, деревья отличаются только devops/.
  local tags_before tags_after
  tags_before=$(git -C "$BACKUP" for-each-ref --format='%(refname:short)' refs/tags | sort | tr '\n' ' ')
  tags_after=$(git -C "$after" for-each-ref --format='%(refname:short)' refs/tags | sort | tr '\n' ' ')
  [ "$tags_before" = "$tags_after" ] && ok "Р4 теги те же: $tags_after" \
    || bad "Р4 набор тегов разошёлся: было [$tags_before] стало [$tags_after]"
  for t in $tags_before; do
    local tb_msg ta_msg
    tb_msg=$(git -C "$BACKUP" cat-file -p "refs/tags/$t" 2>/dev/null | grep -v '^object ' || true)
    ta_msg=$(git -C "$after" cat-file -p "refs/tags/$t" 2>/dev/null | grep -v '^object ' || true)
    [ "$tb_msg" = "$ta_msg" ] && ok "Р4 $t: тег, автор и сообщение те же" || bad "Р4 $t: тег изменился"
    local changed
    if git -C "$scratch" cat-file -e "refs/after/tags/$t^{commit}" 2>/dev/null; then
      changed=$(git -C "$scratch" diff --name-only "refs/tags/$t^{commit}" "refs/after/tags/$t^{commit}" | grep -v "^${DROP_PATH}/" || true)
      [ -z "$changed" ] && ok "Р4 $t: дерево отличается только путями $DROP_PATH/" \
        || bad "Р4 $t: изменились файлы вне $DROP_PATH/: $(printf '%s' "$changed" | head -3 | tr '\n' ' ')"
    else
      bad "Р4 $t: тега нет после переписывания"
    fi
  done

  # Ветки: набор тот же (в рабочей копии refs/pull уже убраны).
  local hb ha
  hb=$(git -C "$BACKUP" for-each-ref --format='%(refname)' refs/heads | sort | tr '\n' ' ')
  ha=$(git -C "$after" for-each-ref --format='%(refname)' refs/heads | sort | tr '\n' ' ')
  [ "$hb" = "$ha" ] && ok "ветки те же: $ha" || bad "набор веток разошёлся: было [$hb] стало [$ha]"

  # Старые SHA вычеркнутых коммитов ни одному из новых коммитов не предок.
  for c in $dropped; do
    git -C "$after" cat-file -e "$c^{commit}" 2>/dev/null \
      && bad "Р1 вычеркнутый коммит $c всё ещё в репозитории ($label)" || ok "Р1 коммит ${c:0:7} ушёл из истории"
  done

  # Р5: секреты в полной истории.
  if command -v gitleaks >/dev/null 2>&1; then
    gitleaks detect --source "$after" --log-opts="--all" --no-banner >/dev/null 2>&1 \
      && ok "Р5 gitleaks: находок нет" || bad "Р5 gitleaks нашёл утечки: gitleaks detect --source $after --log-opts=--all"
  else
    warn "Р5 gitleaks не установлен: прогоните руками (brew install gitleaks) до push"
  fi
  rm -rf "$scratch"
}

# Сколько из списка SHA лежит в истории main бэкапа (вычеркнутый коммит из
# чужой ветки на счёт main не влияет).
count_in_branch() {
  local list="$1" c n=0
  local in_main
  in_main=$(git -C "$BACKUP" rev-list "refs/heads/$BRANCH")
  for c in $list; do
    printf '%s\n' "$in_main" | grep -q "^$c$" && n=$((n + 1))
  done
  printf '%s' "$n"
}

cmd_verify() {
  say "== verify: переписанная копия против бэкапа (Р1-Р5), сервер не трогается"
  [ -d "$BACKUP" ] || die "нет $BACKUP: сначала snapshot"
  [ -d "$WORK" ] || die "нет $WORK: сначала rewrite"
  verify_repo "$WORK" "work"

  # commit-map: что вычеркнуто, совпадает с независимым подсчётом.
  local cmap="$WORK/filter-repo/commit-map" zeros expect
  if [ -f "$cmap" ]; then
    zeros=$(awk 'NR > 1 && $2 ~ /^0+$/ {print $1}' "$cmap" | sort)
    expect=$(devops_only_commits "$BACKUP" | sort)
    [ "$zeros" = "$expect" ] && ok "commit-map: вычеркнуты ровно коммиты, правившие только ${DROP_PATH}/" \
      || bad "commit-map вычеркнул не то, что ожидалось: $(printf '%s' "$zeros" | tr '\n' ' ')"
    say "  info  прежние SHA у $(awk 'NR > 1 && $1 == $2' "$cmap" | grep -c . || true) коммитов, новые у $(awk 'NR > 1 && $1 != $2 && $2 !~ /^0+$/' "$cmap" | grep -c . || true)"
  else
    bad "нет $cmap: копия переписана не rewrite'ом"
  fi
  summary
}

# --------------------------------------------------------------------- push

confirm() {
  local phrase="$1" answer
  say ""
  say "Цель: $REPO_URL"
  say "Чтобы продолжить, наберите дословно:"
  say "  $phrase"
  printf '> '
  read -r answer
  [ "$answer" = "$phrase" ] || die "подтверждение не совпало, ничего не отправлено"
}

work_tags() { git -C "$1" for-each-ref --format='%(refname:short)' refs/tags; }

cmd_push() {
  say "== push: ТОЧКА НЕВОЗВРАТА"
  [ -d "$BACKUP" ] && [ -d "$WORK" ] || die "нужны snapshot и rewrite"
  say "Повторная проверка переписанной копии:"
  verify_repo "$WORK" "work"
  [ "$FAILS" -eq 0 ] || die "verify не пройден, push запрещён"

  # Никто не успел запушить после бэкапа: иначе force затрёт чужую работу.
  local server_main backup_main
  server_main=$(git ls-remote "$REPO_URL" "refs/heads/$BRANCH" | awk '{print $1}')
  backup_main=$(git -C "$BACKUP" rev-parse "refs/heads/$BRANCH")
  [ "$server_main" = "$backup_main" ] \
    || die "на сервере $BRANCH = $server_main, в бэкапе $backup_main: после снимка был push, повторите snapshot"

  # Любая ветка и любой тег на сервере совпадают со снимком: иначе force затёр бы
  # тег, который кто-то сдвинул или завёл после бэкапа.
  if [ -f "$CLEANUP_DIR/ls-remote-before.txt" ]; then
    local now before
    now=$(git ls-remote --heads --tags "$REPO_URL" | sort)
    before=$(awk '$2 ~ /^refs\/(heads|tags)\//' "$CLEANUP_DIR/ls-remote-before.txt" | sort)
    [ "$now" = "$before" ] || die "ветки или теги на сервере разошлись со снимком (ls-remote-before.txt): повторите snapshot"
  fi

  # Ветки, кроме main, пушем не мы: в них devops/ остался бы достижим. П2 требует
  # удалить их до снимка; здесь страховка на случай, если одна появилась позже.
  local extra
  extra=$(git ls-remote --heads "$REPO_URL" | awk '{print $2}' | grep -v "^refs/heads/$BRANCH$" || true)
  [ -z "$extra" ] || die "на сервере кроме $BRANCH есть ветки: $(printf '%s' "$extra" | tr '\n' ' '); слейте или удалите их и повторите snapshot"

  local tags
  tags=$(work_tags "$WORK")
  say ""
  say "Будет отправлено принудительно:"
  say "  refs/heads/$BRANCH: $backup_main → $(git -C "$WORK" rev-parse "refs/heads/$BRANCH")"
  for t in $tags; do say "  refs/tags/$t"; done
  say "Возврат — cleanup.sh rollback, пока никто не склонировал новую историю."
  confirm "force-push $(basename "$REPO_URL" .git) $BRANCH+$(printf '%s' "$tags" | wc -w | tr -d ' ')tags"

  # Явные ссылки, а не --mirror: refs/pull/* сервер не принимает.
  local specs="+refs/heads/$BRANCH:refs/heads/$BRANCH"
  for t in $tags; do specs="$specs +refs/tags/$t:refs/tags/$t"; done
  # --force-with-lease по main: если сервер сдвинулся с момента проверки, push не пройдёт.
  # shellcheck disable=SC2086
  git -C "$WORK" push --force-with-lease="refs/heads/$BRANCH:$backup_main" "$REPO_URL" $specs
  ok "отправлено; следующий шаг — cleanup.sh verify-remote"
}

# ------------------------------------------------------------ verify-remote

cmd_verify_remote() {
  say "== verify-remote: свежий клон с $REPO_URL"
  ensure_workdir
  [ -d "$BACKUP" ] && [ -d "$WORK" ] || die "нужны snapshot и rewrite"
  local clone="$CLEANUP_DIR/fresh-clone.git"
  rm -rf "$clone"
  git clone -q --bare --no-local "$REPO_URL" "$clone"
  git -C "$clone" fetch -q "$REPO_URL" '+refs/tags/*:refs/tags/*'

  # Свежий клон должен быть тем же, что мы отправляли: те же SHA веток и тегов.
  local r want got
  for r in "refs/heads/$BRANCH" $(git -C "$WORK" for-each-ref --format='%(refname)' refs/tags); do
    want=$(git -C "$WORK" rev-parse "$r")
    got=$(git -C "$clone" rev-parse "$r" 2>/dev/null || echo missing)
    [ "$want" = "$got" ] && ok "$r = ${got:0:12}" || bad "$r: на сервере $got, ожидалось $want"
  done
  verify_repo "$clone" "server"

  say ""
  say "ls-remote:"
  git ls-remote "$REPO_URL" | grep -v 'refs/pull/' | sed 's/^/  /'
  local pulls
  pulls=$(git ls-remote "$REPO_URL" | grep -c 'refs/pull/' || true)
  say "  refs/pull/*: $pulls (Р6: сервер их не удаляет; старые объекты достижимы по SHA через них)"
  local extra
  extra=$(git ls-remote --heads "$REPO_URL" | awk '{print $2}' | grep -v "^refs/heads/$BRANCH$" || true)
  [ -z "$extra" ] && ok "Р1 на сервере одна ветка $BRANCH" || bad "Р1 лишние ветки на сервере: $extra"
  rm -rf "$clone"
  summary
}

# ---------------------------------------------------------------------- map

cmd_map() {
  say "== map: старый SHA → новый (Р9)"
  [ -f "$CLEANUP_DIR/tasks-refs-before.tsv" ] || die "нет tasks-refs-before.tsv: опись снимается в snapshot"
  [ -f "$CLEANUP_DIR/commit-map.txt" ] || die "нет commit-map.txt: сначала rewrite"
  local out="$CLEANUP_DIR/tasks-refs-after.tsv" relink="$CLEANUP_DIR/relink.sh"
  python3 - "$CLEANUP_DIR/commit-map.txt" "$CLEANUP_DIR/tasks-refs-before.tsv" "$out" "$relink" "$OWNER_REPO" <<'PY'
import csv, sys

cmap_path, before_path, out_path, relink_path, repo = sys.argv[1:6]
mapping = {}
with open(cmap_path) as f:
    next(f)
    for line in f:
        old, new = line.split()
        mapping[old] = new

changed = dropped = same = 0
relink = ["#!/usr/bin/env bash",
          "# Сгенерировано cleanup.sh map. НЕ запускалось. Новые ссылки на коммиты вместо",
          "# потерявших SHA; запускать, прочитав, с SDLC_PROJECT=CLAB.",
          "set -euo pipefail"]
with open(before_path) as src, open(out_path, "w") as dst:
    rd = csv.DictReader(src, delimiter="\t")
    wr = csv.writer(dst, delimiter="\t", lineterminator="\n")
    wr.writerow(["task", "kind", "repository", "old_sha", "new_sha", "status"])
    for row in rd:
        old = row["sha"]
        new = mapping.get(old)
        if new is None:
            status, new = "неизвестен карте (коммит не из main-истории или уже новый)", ""
        elif set(new) == {"0"}:
            status, new = "вычеркнут", ""
            dropped += 1
        elif new == old:
            status = "без изменений"
            same += 1
        else:
            status = "новый SHA"
            changed += 1
            if row["kind"] == "commit":
                relink.append(
                    f"clab link {row['task']} --repo {repo} --type commit --ref {new} "
                    f"--url https://github.com/{repo}/commit/{new}")
        wr.writerow([row["task"], row["kind"], row["repository"], old, new, status])
open(relink_path, "w").write("\n".join(relink) + "\n")
print(f"новый SHA: {changed}, без изменений: {same}, вычеркнуто: {dropped}")
PY
  say "таблица: $out"
  say "ссылки на новые SHA: $relink (не запускается автоматически)"
}

# ----------------------------------------------------------------- rollback

cmd_rollback() {
  say "== rollback: вернуть main и теги из бэкапа"
  [ -d "$BACKUP" ] || die "нет $BACKUP"
  local tags backup_main
  tags=$(work_tags "$BACKUP")
  backup_main=$(git -C "$BACKUP" rev-parse "refs/heads/$BRANCH")
  say "Будет возвращено: refs/heads/$BRANCH → $backup_main и теги: $(printf '%s' "$tags" | tr '\n' ' ')"
  say "Выполнять, только пока никто не склонировал новую историю и не запушил поверх неё."
  confirm "rollback $(basename "$REPO_URL" .git)"
  local specs="+refs/heads/$BRANCH:refs/heads/$BRANCH"
  for t in $tags; do specs="$specs +refs/tags/$t:refs/tags/$t"; done
  # shellcheck disable=SC2086
  git -C "$BACKUP" push --force "$REPO_URL" $specs
  ok "возвращено; проверьте: git ls-remote $REPO_URL и сравните с ls-remote-before.txt"
}

# --------------------------------------------------------------------- main

case "${1:-}" in
  preflight)     cmd_preflight ;;
  snapshot)      cmd_snapshot ;;
  rewrite)       cmd_rewrite ;;
  verify)        cmd_verify ;;
  push)          cmd_push ;;
  verify-remote) cmd_verify_remote ;;
  map)           cmd_map ;;
  rollback)      cmd_rollback ;;
  *)
    sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//'
    exit 2
    ;;
esac

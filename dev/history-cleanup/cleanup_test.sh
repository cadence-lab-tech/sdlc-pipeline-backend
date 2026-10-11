#!/usr/bin/env bash
# Самопроверка cleanup.sh на синтетическом репозитории: ни GitHub, ни clab, ни
# сети. Прогоняет весь порядок (snapshot, rewrite, verify, push, verify-remote,
# map, rollback) против локального bare-репозитория и убеждается, что verify
# ловит поломки. Запуск: dev/history-cleanup/cleanup_test.sh
# Нужны git и git-filter-repo в PATH.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok    %s\n' "$*"; }

git filter-repo --version >/dev/null 2>&1 || fail "git-filter-repo не найден в PATH"

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null

# --- синтетический «origin»: devops/ правится вместе с deploy/ и отдельно -----
SRC="$T/src"
git init -q -b main "$SRC"
cd "$SRC"
echo readme >README.md && git add -A && git commit -q -m "первый"
mkdir devops deploy
echo tf >devops/main.tf && echo compose >deploy/compose.yml && git add -A && git commit -q -m "деплой: terraform и compose"
git tag -a v0.1.0 -m "v0.1.0"
echo more >>devops/main.tf && git add -A && git commit -q -m "только devops"          # будет вычеркнут
echo x >>deploy/compose.yml && echo y >>devops/main.tf && git add -A && git commit -q -m "deploy и devops вместе"
git rm -q -r devops && echo finished >finished.txt && git add -A && git commit -q -m "devops вне репозитория"
git tag -a v0.2.0 -m "v0.2.0"
git clone -q --bare "$SRC" "$T/origin.git"

export REPO_URL="$T/origin.git" CLEANUP_DIR="$T/work" CLAB_ROOT="$T/root"
mkdir -p "$CLAB_ROOT/backend/devops"
echo secret >"$CLAB_ROOT/backend/devops/.env"

# clab-заглушка: ровно два вызова, которые делает опись.
mkdir "$T/bin"
OLD1=$(git -C "$T/origin.git" rev-parse 'v0.1.0^{commit}')
cat >"$T/bin/clab" <<EOF
#!/usr/bin/env bash
case "\$1" in
  tasks) echo '{"tasks":[{"key":"T-1","status_key":"done"}]}' ;;
  show)  echo '{"context":{"repo_refs":[{"ref_type":"commit","repository":"cadence-lab-tech/sdlc-pipeline-backend","ref":"$OLD1","url":"u"}],"baselines":[{"repository":"cadence-lab-tech/sdlc-pipeline-backend","commit":"$OLD1"}]}}' ;;
esac
EOF
chmod +x "$T/bin/clab"
export PATH="$T/bin:$PATH"

run() { "$HERE/cleanup.sh" "$@" >"$T/out.txt" 2>&1 || { cat "$T/out.txt" >&2; return 1; }; }

run snapshot;  pass "snapshot: бэкап, bundle, опись, копия devops/"
[ -f "$CLEANUP_DIR/backend.bundle" ] && [ -f "$CLEANUP_DIR/tasks-refs-before.tsv" ] \
  && [ -f "$CLEANUP_DIR/devops-copy/.env" ] || fail "snapshot не оставил артефактов"
run snapshot 2>/dev/null && fail "повторный snapshot должен отказывать" || pass "повторный snapshot отказывает"

run rewrite;   pass "rewrite"
run verify;    pass "verify на корректном результате"
grep -q "вычеркнуты коммиты, правившие только devops/: 1" "$T/out.txt" || fail "ожидался один вычеркнутый коммит"

# --- verify обязан ловить поломки -------------------------------------------
cp -R "$CLEANUP_DIR" "$T/bad"
git -C "$T/bad/work.git" tag -d v0.1.0 >/dev/null
CLEANUP_DIR="$T/bad" "$HERE/cleanup.sh" verify >/dev/null 2>&1 && fail "verify пропустил потерянный тег" || pass "verify ловит потерянный тег"
rm -rf "$T/bad" && mkdir "$T/bad" && cp -R "$CLEANUP_DIR/backup.git" "$T/bad/" \
  && git clone -q --mirror --no-local "$T/bad/backup.git" "$T/bad/work.git"
CLEANUP_DIR="$T/bad" "$HERE/cleanup.sh" verify >/dev/null 2>&1 && fail "verify пропустил неприменённый фильтр" || pass "verify ловит неприменённый фильтр"

# --- push: подтверждение, затем отправка --------------------------------------
echo "нет" | "$HERE/cleanup.sh" push >/dev/null 2>&1 && fail "push прошёл без подтверждения" || pass "push без подтверждения отказывает"
[ "$(git -C "$T/origin.git" rev-parse main)" = "$(git -C "$CLEANUP_DIR/backup.git" rev-parse main)" ] \
  || fail "отказавший push изменил сервер"
# сервер ушёл вперёд после снимка (новый тег): push обязан отказать, ничего не затирая
git -C "$T/origin.git" tag v9.9.9 main
echo "force-push origin main+2tags" | "$HERE/cleanup.sh" push >/dev/null 2>&1 && fail "push прошёл при разошедшихся тегах" || pass "push отказывает, если сервер разошёлся со снимком"
git -C "$T/origin.git" tag -d v9.9.9 >/dev/null
echo "force-push origin main+2tags" | "$HERE/cleanup.sh" push >/dev/null 2>&1 || fail "push с подтверждением не прошёл"
pass "push"
run verify-remote; pass "verify-remote: свежий клон = отправленное"
[ -z "$(git -C "$T/origin.git" log --all --oneline -- devops)" ] || fail "devops/ остался на сервере"

# --- опись и откат ------------------------------------------------------------
run map
awk -F'\t' 'NR > 1 && $6 == "новый SHA" {n++} END {exit n == 2 ? 0 : 1}' "$CLEANUP_DIR/tasks-refs-after.tsv" \
  || fail "map: ожидались две ссылки с новым SHA"
grep -q 'clab link T-1 --repo cadence-lab-tech/sdlc-pipeline-backend --type commit --ref' "$CLEANUP_DIR/relink.sh" \
  || fail "map: нет команды relink для коммита"
pass "map: старый SHA заменён новым, relink собран"

echo "rollback origin" | "$HERE/cleanup.sh" rollback >/dev/null 2>&1 || fail "rollback не прошёл"
[ "$(git -C "$T/origin.git" rev-parse main)" = "$(git -C "$CLEANUP_DIR/backup.git" rev-parse main)" ] \
  || fail "rollback не вернул main"
[ "$(git -C "$T/origin.git" rev-parse v0.1.0)" = "$(git -C "$CLEANUP_DIR/backup.git" rev-parse v0.1.0)" ] \
  || fail "rollback не вернул тег"
pass "rollback возвращает main и теги"

echo "все проверки cleanup.sh пройдены"

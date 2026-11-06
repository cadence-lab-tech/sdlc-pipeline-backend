#!/usr/bin/env bash
# Проверка check-tag.sh без сети: gh подменён заглушкой.
#   bash .github/release/check-tag.test.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/check-tag.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

commit=$(printf 'c%.0s' $(seq 40))
tagobj=$(printf 'd%.0s' $(seq 40))

# gh api: ref тега и объект тега по FAKE_REF и FAKE_OBJ.
#   FAKE_REF  annotated (по умолчанию) | lightweight | missing | error
#   FAKE_OBJ  commit (по умолчанию) | tree | error
cat > "$tmp/gh" <<SH
#!/usr/bin/env bash
printf 'gh' >> "\$FAKE_LOG"; printf ' [%s]' "\$@" >> "\$FAKE_LOG"; echo >> "\$FAKE_LOG"
case "\$2" in
  */git/ref/tags/*)
    case "\${FAKE_REF:-annotated}" in
      annotated) echo "tag $tagobj" ;;
      lightweight) echo "commit $commit" ;;
      missing) echo '{"message":"Not Found","status":"404"}'; echo "gh: Not Found (HTTP 404)" >&2; exit 1 ;;
      *) echo "gh: Server Error (HTTP 502)" >&2; exit 1 ;;
    esac ;;
  */git/tags/$tagobj)
    case "\${FAKE_OBJ:-commit}" in
      commit) echo "commit $commit" ;;
      tree) echo "tree $commit" ;;
      *) echo "gh: Server Error (HTTP 500)" >&2; exit 1 ;;
    esac ;;
  *) echo "неожиданный запрос \$2" >&2; exit 3 ;;
esac
SH
chmod +x "$tmp/gh"

fails=0
run() { # имя, ожидаемый код, затем переменные окружения
  local name=$1 want=$2; shift 2
  : > "$tmp/log"
  : > "$tmp/output"
  set +e
  env -u TAG PATH="$tmp:$PATH" FAKE_LOG="$tmp/log" GITHUB_OUTPUT="$tmp/output" \
    GITHUB_REPOSITORY=cadence-lab-tech/sdlc-pipeline-backend GITHUB_SHA="$commit" \
    GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REF_TYPE=tag GITHUB_REF_NAME=v9.9.9 \
    "$@" bash "$script" > "$tmp/out" 2>&1
  code=$?
  set -e
  if [ "$code" != "$want" ]; then
    echo "FAIL $name: код $code, ждали $want"; cat "$tmp/out"; fails=$((fails + 1))
  fi
}
expect() { # имя, файл, шаблон
  grep -qF -- "$3" "$tmp/$2" || { echo "FAIL $1: в $2 нет «$3»"; cat "$tmp/$2"; fails=$((fails + 1)); }
}
expect_not() {
  ! grep -qF -- "$3" "$tmp/$2" || { echo "FAIL $1: в $2 есть «$3»"; cat "$tmp/$2"; fails=$((fails + 1)); }
}
refused() { # имя: отказ без выходов шага
  expect "$1" out "выпуск не публикуется"
  [ ! -s "$tmp/output" ] || { echo "FAIL $1: при отказе в GITHUB_OUTPUT есть выходы"; cat "$tmp/output"; fails=$((fails + 1)); }
}

# Ручной запуск на аннотированном теге, вход совпадает с ref — успех, коммит
# тега наружу.
run dispatch-ok 0 TAG=v9.9.9
expect dispatch-ok out "тег v9.9.9 аннотированный, коммит $commit"
expect dispatch-ok output "tag=v9.9.9"
expect dispatch-ok output "commit=$commit"
expect dispatch-ok log "[repos/cadence-lab-tech/sdlc-pipeline-backend/git/ref/tags/v9.9.9]"
expect dispatch-ok log "[repos/cadence-lab-tech/sdlc-pipeline-backend/git/tags/$tagobj]"
expect_not dispatch-ok out "::error::"

# Push аннотированного тега — та же проверка, тот же успех.
run push-ok 0 GITHUB_EVENT_NAME=push
expect push-ok output "commit=$commit"

# Запуск на main (ref — ветка) — отказ до запросов к API, в логе команда на
# ref тега и почему.
run branch 1 TAG=v0.25.0 GITHUB_REF_TYPE=branch GITHUB_REF_NAME=main
expect branch out "::error::прогон выпуска запущен на branch «main», а не на теге"
expect branch out "identity на refs/heads/main, а не на теге"
expect branch out "gh workflow run release.yml --repo cadence-lab-tech/sdlc-pipeline-backend --ref v0.25.0 -f tag=v0.25.0"
refused branch
expect_not branch log "gh "

# Вход tag не совпадает с ref прогона — отказ.
run mismatch 1 TAG=v9.9.8
expect mismatch out "::error::вход tag «v9.9.8» не совпадает с ref прогона «v9.9.9»"
refused mismatch
expect_not mismatch log "gh "

# Ручной запуск без входа — отказ.
run no-input 1 TAG=
expect no-input out "::error::ручной запуск без входа tag"
refused no-input

# Имя не похоже на версию — отказ до запросов: имя идёт в путь API.
run bad-name 1 TAG=latest GITHUB_REF_NAME=latest
expect bad-name out "::error::тег «latest» не похож на версию vX.Y.Z"
refused bad-name
expect_not bad-name log "gh "
run bad-name-path 1 GITHUB_EVENT_NAME=push GITHUB_REF_NAME="v1.2.3/../../x"
expect bad-name-path out "не похож на версию"
expect_not bad-name-path log "gh "

# Предварительная версия — имя годится.
run prerelease 0 TAG=v9.9.9-rc.1 GITHUB_REF_NAME=v9.9.9-rc.1
expect prerelease output "tag=v9.9.9-rc.1"

# Тега нет на origin — отказ.
run missing 1 TAG=v9.9.9 FAKE_REF=missing
expect missing out "::error::тега v9.9.9 нет на origin (cadence-lab-tech/sdlc-pipeline-backend)"
refused missing

# Лёгкий тег (ref указывает прямо на коммит) — отказ, и на ручном запуске, и
# на push.
run lightweight 1 TAG=v9.9.9 FAKE_REF=lightweight
expect lightweight out "::error::тег v9.9.9 не аннотированный"
expect lightweight out "git tag -a v9.9.9"
refused lightweight
expect_not lightweight log "git/tags/"
run lightweight-push 1 GITHUB_EVENT_NAME=push FAKE_REF=lightweight
expect lightweight-push out "не аннотированный"
refused lightweight-push

# Аннотированный тег не на коммит (на дерево или другой тег) — отказ.
run not-commit 1 TAG=v9.9.9 FAKE_OBJ=tree
expect not-commit out "::error::аннотированный тег v9.9.9 указывает на объект tree"
refused not-commit

# Тег указывает не на тот коммит, что собирает прогон — отказ.
run other-sha 1 TAG=v9.9.9 GITHUB_SHA="$(printf 'e%.0s' $(seq 40))"
expect other-sha out "::error::тег v9.9.9 на origin указывает на коммит $commit, а прогон собирает eeee"
refused other-sha

# API не ответил — отказ с ответом, а не «тега нет».
run api-error 1 TAG=v9.9.9 FAKE_REF=error
expect api-error out "::error::не удалось прочитать тег v9.9.9 на origin"
expect api-error out "HTTP 502"
refused api-error
run obj-error 1 TAG=v9.9.9 FAKE_OBJ=error
expect obj-error out "::error::не удалось прочитать объект тега v9.9.9"
refused obj-error

# Без обязательных переменных — ошибка, не тишина.
run no-repo 1 TAG=v9.9.9 GITHUB_REPOSITORY=
run no-sha 1 TAG=v9.9.9 GITHUB_SHA=

if [ "$fails" -gt 0 ]; then
  echo "провалено: $fails"; exit 1
fi
echo "ok: check-tag.sh"

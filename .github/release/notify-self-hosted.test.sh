#!/usr/bin/env bash
# Проверка notify-self-hosted.sh без сети: docker и gh подменены заглушками.
#   bash .github/release/notify-self-hosted.test.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/notify-self-hosted.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# docker buildx imagetools inspect: образ есть, если FAKE_OTHER=present.
cat > "$tmp/docker" <<'SH'
#!/usr/bin/env bash
echo "docker $*" >> "$FAKE_LOG"
case "${FAKE_OTHER:-missing}" in
  present) echo "Name: $4"; exit 0 ;;
  denied) echo "ERROR: failed to authorize: 403 Forbidden" >&2; exit 1 ;;
  *) echo "ERROR: $4: not found" >&2; exit 1 ;;
esac
SH
# gh api: пишет аргументы, отвечает 204 или падает при FAKE_GH=fail.
cat > "$tmp/gh" <<'SH'
#!/usr/bin/env bash
printf 'gh' >> "$FAKE_LOG"; printf ' [%s]' "$@" >> "$FAKE_LOG"; echo >> "$FAKE_LOG"
[ "${FAKE_GH:-ok}" = fail ] && { echo "HTTP 401: Bad credentials" >&2; exit 1; }
printf 'HTTP/2.0 204 No Content\r\nDate: x\r\n'
SH
chmod +x "$tmp/docker" "$tmp/gh"

fails=0
run() { # имя, ожидаемый код, затем переменные окружения
  local name=$1 want=$2; shift 2
  : > "$tmp/log"
  set +e
  env PATH="$tmp:$PATH" FAKE_LOG="$tmp/log" VERSION=v9.9.9 \
    REGISTRY=ghcr.io/cadence-lab-tech GH_TOKEN=x "$@" bash "$script" > "$tmp/out" 2>&1
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

# Половина выпуска: второго образа нет — события нет, лог называет образ.
run half-backend 0 SELF=backend HAS_TOKEN=true FAKE_OTHER=missing
expect half-backend out "нет образа ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v9.9.9"
expect half-backend out "not found"
expect_not half-backend log "gh "

run half-frontend 0 SELF=frontend HAS_TOKEN=true FAKE_OTHER=missing
expect half-frontend out "нет образа ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v9.9.9"
expect_not half-frontend log "gh "

# Нет доступа к чужому пакету — тоже не событие, ответ реестра в логе.
run denied 0 SELF=backend HAS_TOKEN=true FAKE_OTHER=denied
expect denied out "403 Forbidden"
expect_not denied log "gh "

# Оба образа есть, секрета нет — прогон зелёный, в логе команда руками.
run no-token 0 SELF=frontend HAS_TOKEN=false FAKE_OTHER=present
expect no-token out "SELF_HOSTED_DISPATCH_TOKEN"
expect no-token out "gh api repos/cadence-lab-tech/self-hosted/dispatches -f event_type=cadence-release"
expect no-token out "client_payload[version]=v9.9.9"
expect_not no-token log "gh "

# Оба образа есть, секрет есть — один запрос с контрактом R5 и 204 в логе.
run send 0 SELF=backend HAS_TOKEN=true FAKE_OTHER=present
expect send out "POST /repos/cadence-lab-tech/self-hosted/dispatches event_type=cadence-release client_payload.version=v9.9.9"
expect send out "HTTP/2.0 204"
expect send log "[repos/cadence-lab-tech/self-hosted/dispatches]"
expect send log "[event_type=cadence-release]"
expect send log "[client_payload[version]=v9.9.9]"
expect send log "[client_payload[backend_image]=ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v9.9.9]"
expect send log "[client_payload[frontend_image]=ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v9.9.9]"
[ "$(grep -c '^gh ' "$tmp/log")" = 1 ] || { echo "FAIL send: запросов не один"; fails=$((fails + 1)); }

# Свой репозиторий поставки через TARGET.
run target 0 SELF=backend HAS_TOKEN=true FAKE_OTHER=present TARGET=acme/delivery
expect target log "[repos/acme/delivery/dispatches]"

# Отказ API — прогон красный: событие не ушло, а секрет есть.
run send-fails 1 SELF=backend HAS_TOKEN=true FAKE_OTHER=present FAKE_GH=fail
expect send-fails out "Bad credentials"

# Без обязательных переменных и с чужим SELF — ошибка, не тишина.
run bad-self 2 SELF=docs HAS_TOKEN=true FAKE_OTHER=present
run no-version 1 SELF=backend VERSION= HAS_TOKEN=true FAKE_OTHER=present

if [ "$fails" -gt 0 ]; then
  echo "провалено: $fails"; exit 1
fi
echo "ok: notify-self-hosted.sh"

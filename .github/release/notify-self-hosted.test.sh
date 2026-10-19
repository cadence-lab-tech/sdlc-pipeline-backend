#!/usr/bin/env bash
# Проверка notify-self-hosted.sh без сети: docker и gh подменены заглушками.
#   bash .github/release/notify-self-hosted.test.sh
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
script="$here/notify-self-hosted.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# docker buildx imagetools inspect: образ есть, если FAKE_OTHER=present
# (nodigest — ответ без строки Digest).
cat > "$tmp/docker" <<'SH'
#!/usr/bin/env bash
echo "docker $*" >> "$FAKE_LOG"
case "${FAKE_OTHER:-missing}" in
  present) printf 'Name:      %s\nMediaType: application/vnd.oci.image.index.v1+json\nDigest:    sha256:%s\n' "$4" "$(printf 'b%.0s' $(seq 64))"; exit 0 ;;
  nodigest) echo "Name: $4"; exit 0 ;;
  denied) echo "ERROR: unexpected status from HEAD request to https://ghcr.io/v2/x/manifests/v9.9.9: 403 Forbidden" >&2; echo "denied" >&2; exit 1 ;;
  unreachable) echo "ERROR: failed to do request: Head \"https://ghcr.io/v2/x/manifests/v9.9.9\": dial tcp: i/o timeout" >&2; exit 1 ;;
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
# cosign verify: пишет аргументы, проходит или падает при FAKE_COSIGN=fail.
cat > "$tmp/cosign" <<'SH'
#!/usr/bin/env bash
printf 'cosign' >> "$FAKE_LOG"; printf ' [%s]' "$@" >> "$FAKE_LOG"; echo >> "$FAKE_LOG"
[ "${FAKE_COSIGN:-ok}" = fail ] && { echo "Error: no signatures found" >&2; exit 1; }
echo '[{"critical":{}}]'
SH
chmod +x "$tmp/docker" "$tmp/gh" "$tmp/cosign"
own="sha256:$(printf 'a%.0s' $(seq 64))"
other_digest="sha256:$(printf 'b%.0s' $(seq 64))"

fails=0
run() { # имя, ожидаемый код, затем переменные окружения
  local name=$1 want=$2; shift 2
  : > "$tmp/log"
  set +e
  env -u GITHUB_REPOSITORY_OWNER PATH="$tmp:$PATH" FAKE_LOG="$tmp/log" VERSION=v9.9.9 \
    REGISTRY=ghcr.io/cadence-lab-tech DIGEST="$own" GH_TOKEN=x "$@" bash "$script" > "$tmp/out" 2>&1
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
expect half-backend out "::notice::в реестре нет образа ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v9.9.9"
expect half-backend out "not found"
expect_not half-backend out "::error::"
expect_not half-backend log "gh "

run half-frontend 0 SELF=frontend HAS_TOKEN=true FAKE_OTHER=missing
expect half-frontend out "нет образа ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v9.9.9"
expect_not half-frontend log "gh "

# Отказ реестра — не «образа нет», а поломка доступа (CLAB-99): прогон
# красный, ответ реестра и команда для отправки руками в логе, события нет.
run denied 1 SELF=backend HAS_TOKEN=true FAKE_OTHER=denied
expect denied out "::error::реестр отказал в чтении ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v9.9.9"
expect denied out "403 Forbidden"
expect denied out "Manage Actions access"
expect denied out "gh api repos/cadence-lab-tech/self-hosted/dispatches -f event_type=cadence-release"
expect_not denied out "::notice::"
expect_not denied log "gh "

# Реестр недоступен — тоже не половина выпуска: красный, команда руками.
run unreachable 1 SELF=frontend HAS_TOKEN=true FAKE_OTHER=unreachable
expect unreachable out "::error::реестр не ответил, есть ли образ ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v9.9.9"
expect unreachable out "i/o timeout"
expect unreachable out "client_payload[version]=v9.9.9"
expect_not unreachable log "gh "

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
# Образы — по тегу и digest индекса (CLAB-130): свой из DIGEST, соседа из
# ответа реестра.
expect send log "[client_payload[backend_image]=ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v9.9.9@$own]"
expect send log "[client_payload[frontend_image]=ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v9.9.9@$other_digest]"
[ "$(grep -c '^gh ' "$tmp/log")" = 1 ] || { echo "FAIL send: запросов не один"; fails=$((fails + 1)); }
expect_not send log "cosign"

# Прогон frontend: свой digest у frontend, соседа — у backend.
run send-frontend 0 SELF=frontend HAS_TOKEN=true FAKE_OTHER=present
expect send-frontend log "[client_payload[backend_image]=ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v9.9.9@$other_digest]"
expect send-frontend log "[client_payload[frontend_image]=ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v9.9.9@$own]"

# Подписанный выпуск: подпись соседа проверяется по digest и identity его
# release.yml на теге; сошлась — событие уходит.
run signed 0 SELF=backend HAS_TOKEN=true FAKE_OTHER=present SIGNED=true
expect signed log "cosign [verify] [--certificate-identity] [https://github.com/cadence-lab-tech/sdlc-pipeline-frontend/.github/workflows/release.yml@refs/tags/v9.9.9] [--certificate-oidc-issuer] [https://token.actions.githubusercontent.com] [ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend@$other_digest]"
expect signed out "подпись ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend@$other_digest проверена"
expect signed log "[event_type=cadence-release]"

# Сосед опубликован, но ещё не подписан — события нет, прогон зелёный:
# его отправит сосед после своей подписи. В логе — почему и команда руками.
run unsigned 0 SELF=frontend HAS_TOKEN=true FAKE_OTHER=present SIGNED=true FAKE_COSIGN=fail
expect unsigned out "::warning::образ ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v9.9.9 есть, но его подпись не проверилась"
expect unsigned out "no signatures found"
expect unsigned out "client_payload[backend_image]=ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v9.9.9@$other_digest"
expect_not unsigned log "gh "

# OWNER задаёт владельца identity (форк, своя организация).
run owner 0 SELF=backend HAS_TOKEN=true FAKE_OTHER=present SIGNED=true OWNER=acme
expect owner log "[https://github.com/acme/sdlc-pipeline-frontend/.github/workflows/release.yml@refs/tags/v9.9.9]"

# Реестр ответил без digest — поломка, не тишина.
run nodigest 1 SELF=backend HAS_TOKEN=true FAKE_OTHER=nodigest
expect nodigest out "::error::в ответе реестра на ghcr.io/cadence-lab-tech/sdlc-pipeline-frontend:v9.9.9 нет digest индекса"
expect_not nodigest log "gh "

# Свой репозиторий поставки через TARGET.
run target 0 SELF=backend HAS_TOKEN=true FAKE_OTHER=present TARGET=acme/delivery
expect target log "[repos/acme/delivery/dispatches]"

# Отказ API — прогон красный: событие не ушло, а секрет есть.
run send-fails 1 SELF=backend HAS_TOKEN=true FAKE_OTHER=present FAKE_GH=fail
expect send-fails out "Bad credentials"

# Без обязательных переменных и с чужим SELF — ошибка, не тишина.
run bad-self 2 SELF=docs HAS_TOKEN=true FAKE_OTHER=present
run no-version 1 SELF=backend VERSION= HAS_TOKEN=true FAKE_OTHER=present
run no-digest 1 SELF=backend DIGEST= HAS_TOKEN=true FAKE_OTHER=present

if [ "$fails" -gt 0 ]; then
  echo "провалено: $fails"; exit 1
fi
echo "ok: notify-self-hosted.sh"

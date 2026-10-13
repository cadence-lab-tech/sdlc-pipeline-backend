#!/usr/bin/env bash
# Оповещение репозитория поставки о выпуске версии (CLAB-91).
#
# Выпуск — это тег vX.Y.Z в двух репозиториях, backend и frontend, и у каждого
# свой прогон release.yml. Событие cadence-release в self-hosted должно уйти,
# когда в реестре есть оба образа версии, поэтому его шлёт тот прогон, который
# опубликовал свой образ вторым: после push он смотрит в реестр, есть ли образ
# второй половины. Нет — событие отправит второй прогон, когда опубликует
# свой. Если оба закончат в одну и ту же минуту, событие уйдёт дважды —
# обработчик в self-hosted идемпотентен (PR на ту же VERSION).
#
# Скрипт лежит одинаковым в .github/release/ обоих репозиториев; правка — в
# обоих сразу.
#
# Окружение:
#   VERSION   тег выпуска, vX.Y.Z
#   REGISTRY  ghcr.io/<владелец в нижнем регистре>
#   SELF      backend | frontend — чей прогон
#   HAS_TOKEN true, если секрет SELF_HOSTED_DISPATCH_TOKEN заведён
#   GH_TOKEN  сам секрет: fine-grained PAT с Contents: write на self-hosted
#   TARGET    репозиторий поставки, по умолчанию cadence-lab-tech/self-hosted
set -euo pipefail

: "${VERSION:?нужен VERSION — тег выпуска}"
: "${REGISTRY:?нужен REGISTRY}"
: "${SELF:?нужен SELF — backend или frontend}"
TARGET="${TARGET:-cadence-lab-tech/self-hosted}"

backend_image="$REGISTRY/sdlc-pipeline-backend:$VERSION"
frontend_image="$REGISTRY/sdlc-pipeline-frontend:$VERSION"
case "$SELF" in
  backend) other="$frontend_image"; other_repo=sdlc-pipeline-frontend ;;
  frontend) other="$backend_image"; other_repo=sdlc-pipeline-backend ;;
  *) echo "SELF должен быть backend или frontend, а не «$SELF»" >&2; exit 2 ;;
esac

manual() {
  echo "Отправить руками:"
  echo "  gh api repos/$TARGET/dispatches -f event_type=cadence-release \\"
  echo "    -f 'client_payload[version]=$VERSION' \\"
  echo "    -f 'client_payload[backend_image]=$backend_image' \\"
  echo "    -f 'client_payload[frontend_image]=$frontend_image'"
}

# Образ соседа читается тем же GITHUB_TOKEN, которым прогон вошёл в реестр.
# Пакеты публичны (CLAB-94), поэтому «образа нет» — это только 404. Отказ
# реестра (401/403: пакет снова приватный, прогону не выдано чтение чужого
# пакета) и молчание сети — не половина выпуска, а поломка: если считать их
# отсутствием образа, событие не уйдёт ни из одного прогона (CLAB-99).
# Такой прогон красный и печатает команду для отправки руками.
if ! out=$(docker buildx imagetools inspect "$other" 2>&1); then
  registry_says() { echo "Ответ реестра:"; printf '%s\n' "$out" | tail -n 3 | sed 's/^/  /'; }
  if printf '%s' "$out" | grep -qiE 'unauthorized|forbidden|denied|\b40[13]\b'; then
    echo "::error::реестр отказал в чтении $other — это не отсутствие образа; событие в $TARGET не отправлено."
    echo "Пакет $other_repo стал приватным, или этому репозиторию не выдано его чтение"
    echo "(Package settings → Manage Actions access), или прогон не вошёл в ghcr.io."
    registry_says
    manual
    exit 1
  fi
  if ! printf '%s' "$out" | grep -qiE 'not found|manifest unknown|name unknown|\b404\b'; then
    echo "::error::реестр не ответил, есть ли образ $other — событие в $TARGET не отправлено."
    registry_says
    manual
    exit 1
  fi
  echo "::notice::в реестре нет образа $other — событие в $TARGET не отправлено."
  echo "Его отправит выпуск $other_repo, когда опубликует свой образ $VERSION:"
  echo "поставьте тег $VERSION там или перезапустите упавший прогон."
  registry_says
  exit 0
fi
echo "оба образа версии в реестре: $backend_image, $frontend_image"

if [ "${HAS_TOKEN:-false}" != true ]; then
  echo "::warning::секрета SELF_HOSTED_DISPATCH_TOKEN нет — событие в $TARGET не отправлено."
  manual
  exit 0
fi

echo "POST /repos/$TARGET/dispatches event_type=cadence-release client_payload.version=$VERSION"
resp=$(gh api --include --method POST "repos/$TARGET/dispatches" \
  -f event_type=cadence-release \
  -f "client_payload[version]=$VERSION" \
  -f "client_payload[backend_image]=$backend_image" \
  -f "client_payload[frontend_image]=$frontend_image")
printf '%s\n' "$resp" | head -n 1

#!/usr/bin/env bash
#
# Выкладка на сервере. Лежит в /opt/sdlc, запускается деплойными workflow'ами
# по ssh — и руками, когда нужен откат:
#
#   ./deploy.sh backend a1b2c3d              # выложить образ сервиса и воркера
#   ./deploy.sh frontend f00ba7              # выложить образ интерфейса
#   ./deploy.sh backend $(cat .last-ok-backend)   # вернуться на предыдущий
#   DEPLOY_READY_TIMEOUT=1800 ./deploy.sh backend a1b2c3d   # заведомо долгая миграция
#
# Откат интерфейса — только на образ не старше 1ccb530 (CLAB-236): Caddy
# ходит в ui на :8080, а образы до него слушают один :80 (CLAB-247).
#
# Два репозитория — два образа, и каждый выкладывается отдельно: сервис не
# пересобирается ради правки интерфейса. Тег пишется прямо в .env, а не
# подставляется переменной: иначе «что сейчас крутится» знал бы только тот,
# кто запускал деплой, а `docker compose up -d` руками поднял бы другое.
set -euo pipefail

cd "$(dirname "$0")"

image="${1:?укажите образ: backend | frontend}"
tag="${2:?укажите тег образа (обычно короткий SHA коммита)}"
registry="${REGISTRY:-ghcr.io/cadence-lab-tech}"

case "$image" in
  backend)  key=IMAGE_BACKEND;  services="api worker" ;;
  frontend) key=IMAGE_FRONTEND; services="ui" ;;
  *) echo "✗ неизвестный образ: $image (backend | frontend)" >&2; exit 2 ;;
esac

# Две выкладки одновременно — гонка за .env: workflow'ы двух репозиториев
# друг о друге не знают, поэтому очередь выстраивается здесь.
exec 9>.deploy.lock
flock 9

# Предел готовности api, секунды от запуска стека (CLAB-369). Api катит
# миграции при старте и молчит на /readyz, пока они не кончатся; миграция
# дольше минуты — обычное дело (000041 шла две с половиной). healthcheck api
# в docker-compose.yml держит start_period не меньше предела по умолчанию,
# иначе `up -d` объявит api больным раньше и бросит воркер пересозданным, но
# не запущенным. Поднять предел — для одной выкладки, переменной.
ready_timeout="${DEPLOY_READY_TIMEOUT:-600}"
case "$ready_timeout" in
  ''|*[!0-9]*) echo "✗ DEPLOY_READY_TIMEOUT — целое число секунд, а не «$ready_timeout»" >&2; exit 2 ;;
esac

previous="$(grep -oE "^${key}=.*" .env | sed 's/.*://')" || previous=""

sed -i "s|^${key}=.*|${key}=${registry}/sdlc-pipeline-${image}:${tag}|" .env

# shellcheck disable=SC2086
docker compose pull $services

started=$(date +%s)
deadline=$((started + ready_timeout))

left() {
  local n=$((deadline - $(date +%s)))
  [ "$n" -gt 0 ] && echo "$n" || echo 0
}

# api_state — «статус здоровье перезапуски» контейнера api или пусто.
api_state() {
  local id
  id="$(docker compose ps -a -q api 2>/dev/null)" || return 0
  [ -n "$id" ] || return 0
  docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}} {{.RestartCount}}' "$id" 2>/dev/null || true
}

# api_phase — фаза api одним docker inspect: healthy; starting — создан,
# работает и ещё не здоров, без перезапусков (похоже на идущую миграцию, а
# не на падение); other — не создан, вышел или перезапускается: такой api
# ждать бессмысленно, это прежняя красная выкладка. Одна проверка — одно
# состояние: api, ставший здоровым между двумя вопросами, не выглядит
# упавшим.
api_phase() {
  local status health restarts
  read -r status health restarts <<<"$(api_state)"
  if [ "${status:-}" = running ] && [ "${health:-}" = healthy ]; then
    echo healthy
  elif [ "${status:-}" = running ] && [ "${restarts:-1}" = 0 ] &&
    { [ "$health" = starting ] || [ "$health" = unhealthy ]; }; then
    echo starting
  else
    echo other
  fi
}

api_healthy() { [ "$(api_phase)" = healthy ]; }
api_starting() { [ "$(api_phase)" = starting ]; }

# compose_up — `up -d` под пределом: сам `up -d` ждёт здоровья api перед
# воркером, и срок от этого не продлевается.
compose_up() {
  local n
  n="$(left)"
  [ "$n" -ge 60 ] || n=60
  timeout "$n" docker compose up -d --remove-orphans
}

# Отчёт, когда предел вышел, а api всё ещё стартует: что с сервисом и что
# делать, одним логом job'а. .last-ok-* не трогается.
not_ready() {
  echo "✗ api не готов за ${ready_timeout} с (DEPLOY_READY_TIMEOUT)."
  echo "  Возможно, ещё идёт миграция базы: docker compose logs -f api"
  echo "  Воркер не запущен. Когда api станет healthy, довести стек:"
  echo "    cd /opt/sdlc && docker compose up -d && docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile"
  echo "  или повторить выкладку: ./deploy.sh $image $tag (повтор job'а deploy)."
  echo "  Откат (.last-ok-$image), пока идёт миграция, не нужен: откатываться — только если api упал."
  echo "  Миграция заведомо долгая — повтор с большим пределом: DEPLOY_READY_TIMEOUT=1800 ./deploy.sh $image $tag"
  docker compose ps -a
  # shellcheck disable=SC2086
  docker compose logs api worker --tail 50
  exit 1
}

echo "▸ запускаем стек; готовности api ждём до ${ready_timeout} с (DEPLOY_READY_TIMEOUT)"
if ! compose_up; then
  # Упавший `up -d` при api, который ещё стартует, — не провал: миграция
  # дольше healthcheck'а. Ждём api до предела и доводим стек повторным
  # `up -d` — воркер не остаётся пересозданным, но не запущенным.
  phase="$(api_phase)"
  [ "$phase" != other ] || {
    echo "✗ docker compose up -d не прошёл, и api не стартует. Логи:"
    docker compose ps -a
    # shellcheck disable=SC2086
    docker compose logs $services --tail 50
    exit 1
  }
  [ "$phase" = healthy ] ||
    echo "▸ api ещё не готов — возможно, идёт миграция базы (docker compose logs -f api); ждём до ${ready_timeout} с от запуска"
  waited=0
  while [ "$phase" != healthy ]; do
    [ "$(left)" -gt 0 ] || not_ready
    sleep 5
    phase="$(api_phase)"
    [ "$phase" != other ] || {
      echo "✗ api перестал стартовать (упал или перезапускается). Логи:"
      docker compose ps -a
      docker compose logs api worker --tail 50
      exit 1
    }
    waited=$((waited + 5))
    # Строка раз в минуту: видно, что выкладка жива и чего ждёт.
    [ $((waited % 60)) -eq 0 ] && echo "  … api ещё не готов, осталось $(left) с"
  done
  echo "▸ api готов — доводим стек"
  compose_up || {
    echo "✗ повторный docker compose up -d не прошёл. Логи:"
    docker compose ps -a
    # shellcheck disable=SC2086
    docker compose logs $services worker --tail 50
    exit 1
  }
fi

# Caddyfile примонтирован в контейнер, и `up -d` его правку не замечает:
# конфигурация не входит в то, по чему compose решает пересоздавать. Reload
# у Caddy бесшовный, поэтому делается всегда, а не по diff'у.
docker compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile

# `up -d` возвращается раньше, чем сервис реально поднялся: у api есть
# healthcheck, ждём его. У интерфейса — статика, достаточно ответа. Ждём до
# того же предела, но не меньше двух минут, как раньше.
echo "▸ ждём $image"
[ "$(left)" -ge 120 ] || deadline=$(($(date +%s) + 120))
while :; do
  case "$image" in
    backend)  api_healthy && docker compose ps --status running --services | grep -qx worker ;;
    frontend) docker compose exec -T caddy wget -q -O /dev/null http://ui:8080/ 2>/dev/null ;;
  esac && {
    echo "  поднялся"
    [ -n "$previous" ] && echo "$previous" > ".last-ok-$image"
    docker compose ps
    docker image prune -f >/dev/null
    exit 0
  }
  [ "$(left)" -gt 0 ] || break
  sleep 2
done

[ "$image" = backend ] && api_starting && not_ready
echo "✗ $image не поднялся. Логи:"
# shellcheck disable=SC2086
docker compose logs $services --tail 50
exit 1

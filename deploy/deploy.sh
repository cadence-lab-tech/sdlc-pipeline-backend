#!/usr/bin/env bash
#
# Выкладка на сервере. Лежит в /opt/sdlc, запускается деплойными workflow'ами
# по ssh — и руками, когда нужен откат:
#
#   ./deploy.sh backend a1b2c3d              # выложить образ сервиса и воркера
#   ./deploy.sh frontend f00ba7              # выложить образ интерфейса
#   ./deploy.sh backend $(cat .last-ok-backend)   # вернуться на предыдущий
#
# Два репозитория — два образа, и каждый выкладывается отдельно: сервис не
# пересобирается ради правки интерфейса. Тег пишется прямо в .env, а не
# подставляется переменной: иначе «что сейчас крутится» знал бы только тот,
# кто запускал деплой, а `docker compose up -d` руками поднял бы другое.
set -euo pipefail

cd "$(dirname "$0")"

image="${1:?укажите образ: backend | frontend}"
tag="${2:?укажите тег образа (обычно короткий SHA коммита)}"
registry="${REGISTRY:-ghcr.io/stepakaziatko}"

case "$image" in
  backend)  key=IMAGE_BACKEND;  services="api worker" ;;
  frontend) key=IMAGE_FRONTEND; services="ui" ;;
  *) echo "✗ неизвестный образ: $image (backend | frontend)" >&2; exit 2 ;;
esac

# Две выкладки одновременно — гонка за .env: workflow'ы двух репозиториев
# друг о друге не знают, поэтому очередь выстраивается здесь.
exec 9>.deploy.lock
flock 9

previous="$(grep -oE "^${key}=.*" .env | sed 's/.*://')" || previous=""

sed -i "s|^${key}=.*|${key}=${registry}/sdlc-pipeline-${image}:${tag}|" .env

# shellcheck disable=SC2086
docker compose pull $services
docker compose up -d --remove-orphans

# `up -d` возвращается раньше, чем сервис реально поднялся: у api есть
# healthcheck, ждём его. У интерфейса — статика, достаточно ответа.
echo "▸ ждём $image"
for _ in $(seq 1 60); do
  case "$image" in
    backend)  ok="$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose ps -q api)" 2>/dev/null || true)"; [ "$ok" = healthy ] ;;
    frontend) docker compose exec -T caddy wget -q -O /dev/null http://ui/ 2>/dev/null ;;
  esac && {
    echo "  поднялся"
    [ -n "$previous" ] && echo "$previous" > ".last-ok-$image"
    docker compose ps
    docker image prune -f >/dev/null
    exit 0
  }
  sleep 2
done

echo "✗ $image не поднялся за две минуты. Логи:"
# shellcheck disable=SC2086
docker compose logs $services --tail 50
exit 1

#!/usr/bin/env bash
#
# Дамп базы с ротацией. Ставится в cron рута на сервере (cloud-init делает
# это сам):
#
#   0 3 * * * /opt/sdlc/backup.sh >> /var/log/sdlc-backup.log 2>&1
#
# Дампы лежат на той же машине, что и база, — то есть защищают от «уронил
# миграцией» и «удалил не то», но не от потери самой машины. Содержимое
# артефактов при этом живёт в S3 и в дамп не входит: в базе лежат адреса.
set -euo pipefail

cd "$(dirname "$0")"

keep_days="${KEEP_DAYS:-14}"
dir="${BACKUP_DIR:-/opt/sdlc/backups}"
mkdir -p "$dir"

# Переменные базы берём из того же .env, что и compose: разойтись они не могут.
set -a
# shellcheck disable=SC1091
source .env
set +a

file="$dir/sdlc-$(date +%Y%m%d-%H%M%S).sql.gz"

# pg_dump запускается ВНУТРИ контейнера с базой: на хосте клиента Postgres
# нет, а версия несовпадающего клиента ломает дамп.
docker compose exec -T postgres \
  pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists \
  | gzip > "$file"

# Пустой дамп — это провал, который иначе заметят только при восстановлении.
if [ "$(stat -c%s "$file")" -lt 1024 ]; then
  echo "✗ дамп подозрительно мал: $file"
  exit 1
fi

find "$dir" -name 'sdlc-*.sql.gz' -mtime "+$keep_days" -delete

echo "✓ $(date +%F' '%T) $file ($(du -h "$file" | cut -f1))"

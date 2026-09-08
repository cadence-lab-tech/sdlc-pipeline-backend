#!/usr/bin/env bash
#
# Читающий запрос к API Timeweb. Только GET — это проверяется здесь, а не
# обещается: метод не параметризуется вовсе.
#
#   ./devops/bin/twc-get.sh projects
#   ./devops/bin/twc-get.sh servers/12345
#   ./devops/bin/twc-get.sh presets/servers | jq '.server_presets[] | select(.location=="ru-1")'
#
# Пути — из https://timeweb.cloud/api-docs, без префикса /api/v1.
# Заменяет twc CLI там, где его нет: тот ставится через pipx, а этого
# достаточно для «посмотреть, что видит облако».
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"

: "${TWC_TOKEN:?не задан TWC_TOKEN (devops/.env — см. .env.example)}"
path="${1:?укажите путь, например servers}"

case "$path" in
  *' '*|*'..'*) die "путь выглядит подозрительно: $path" ;;
esac

curl -fsS \
  -H "Authorization: Bearer $TWC_TOKEN" \
  -H "Accept: application/json" \
  "https://api.timeweb.cloud/api/v1/${path#/}"
echo

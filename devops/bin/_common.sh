# Общая часть скриптов devops/bin. Подключается через source, сама не запускается.

set -euo pipefail

devops_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Настройки подключения лежат в devops/.env и в гит не едут.
# Переменные окружения имеют приоритет — так CI подставляет свои значения.
#
# Читаем построчно, а не через `set -a; source`: source затирает уже заданные
# переменные, то есть делает ровно обратное обещанному.
if [ -f "$devops_dir/.env" ]; then
  while IFS= read -r _line || [ -n "$_line" ]; do
    case "$_line" in '' | '#'*) continue ;; esac
    case "$_line" in *=*) ;; *) continue ;; esac
    _key="${_line%%=*}"
    _val="${_line#*=}"
    # уже задано снаружи — не трогаем
    [ -n "${!_key:-}" ] && continue
    case "$_val" in
      \"*\") _val="${_val#\"}"; _val="${_val%\"}" ;;
      \'*\') _val="${_val#\'}"; _val="${_val%\'}" ;;
    esac
    export "$_key=$_val"
  done < "$devops_dir/.env"
  unset _line _key _val
fi

SDLC_SSH_USER="${SDLC_SSH_USER:-deploy}"
SDLC_SSH_PORT="${SDLC_SSH_PORT:-22}"
SDLC_REMOTE_DIR="${SDLC_REMOTE_DIR:-/opt/sdlc}"

ssh_target="$SDLC_SSH_USER@${SDLC_SSH_HOST:-}"
ssh_opts=(
  -p "$SDLC_SSH_PORT"
  -o BatchMode=yes
  -o ConnectTimeout=10
  # Отпечаток хоста проверяем строго. Автопринятие первого ключа означает
  # доверие любому, кто в этот момент ответит на этот адрес.
  -o StrictHostKeyChecking=yes
)
[ -n "${SDLC_SSH_KEY:-}" ] && ssh_opts+=(-i "$SDLC_SSH_KEY")

die() {
  echo "✗ $*" >&2
  exit 1
}

# Адрес машины проверяется лениво, а не при загрузке: `remote-read.sh --help`
# и любая работа с terraform не должны требовать настроенного ssh.
require_host() {
  [ -n "${SDLC_SSH_HOST:-}" ] ||
    die "не задан SDLC_SSH_HOST (devops/.env — см. .env.example)"
}

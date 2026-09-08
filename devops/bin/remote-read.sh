#!/usr/bin/env bash
#
# Читающие команды на сервере. Ничего не меняет — и это проверяется здесь,
# а не обещается в документации: пропускается только то, что есть в белом
# списке ниже.
#
#   ./devops/bin/remote-read.sh ps
#   ./devops/bin/remote-read.sh logs api 200
#   ./devops/bin/remote-read.sh env-keys
#
# Всё остальное — через remote-run.sh, который спрашивает подтверждение.
# Смысл разделения в том, что агент может свободно смотреть, что сломалось,
# но не может ничего починить молча.
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"

usage() {
  cat <<'USAGE'
Использование: remote-read.sh <команда> [аргументы]

  ps                  состояние контейнеров
  logs <сервис> [N]   последние N строк лога (по умолчанию 100)
  errors [N]          строки с ошибками из логов всех сервисов
  health              живость api, интерфейса и базы изнутри машины
  images              какие образы и теги сейчас запущены
  version             какие теги выложены и на какие откатываться
  env-keys            ИМЕНА переменных в .env, без значений
  disk                место на диске и сколько занял докер
  resources           память, нагрузка, потребление по контейнерам
  certs               сроки сертификатов Caddy
  backups             список дампов базы
  compose             итоговый docker-compose config
  raw <cmd...>        произвольная команда ТОЛЬКО из белого списка
USAGE
}

# Белый список первых слов для `raw`. Всё, что читает и не пишет.
# curl сюда намеренно не входит: он умеет POST, а для проверок живости
# есть отдельная команда health.
readonly ALLOWED_RAW='^(cat|ls|head|tail|wc|grep|find|stat|df|du|free|uptime|uname|date|id|whoami|hostname|ss|ip|ps|top|systemctl status|systemctl is-active|journalctl|ufw status|docker ps|docker images|docker stats|docker inspect|docker system df|docker compose ps|docker compose logs|docker compose config|docker compose images|docker compose top)( |$)'

remote() { require_host; ssh "${ssh_opts[@]}" "$ssh_target" "cd '$SDLC_REMOTE_DIR' && $1"; }

cmd="${1:-}"
shift || true

case "$cmd" in
  ps)
    remote "docker compose ps --format 'table {{.Service}}\t{{.Status}}\t{{.Image}}'"
    ;;

  logs)
    service="${1:?укажите сервис: api | worker | ui | caddy | postgres}"
    lines="${2:-100}"
    [[ "$lines" =~ ^[0-9]+$ ]] || die "число строк должно быть числом"
    remote "docker compose logs --no-color --tail $lines '$service'"
    ;;

  errors)
    lines="${1:-500}"
    [[ "$lines" =~ ^[0-9]+$ ]] || die "число строк должно быть числом"
    # Сервис пишет JSON с "level":"ERROR", Postgres — FATAL. Caddy тоже пишет
    # JSON, и в информационных строках бывает поле "error" — например, при
    # первом выпуске сертификата. Это норма, поэтому level=info отбрасываем.
    remote "docker compose logs --no-color --tail $lines 2>&1 | grep -iE '\"level\":\"(error|warn)\"|fatal|panic|exited with' | grep -v '\"level\":\"info\"' || echo 'ошибок в последних $lines строках нет'"
    ;;

  health)
    # Изнутри машины, мимо Caddy и TLS: так видно, кто именно лежит —
    # приложение или прокси с сертификатом.
    #
    # У api и воркера нет shell — образ distroless. Пробу делает сам бинарник
    # (/api healthcheck), а /readyz спрашиваем из контейнера Caddy: там есть
    # busybox wget, и он ходит по внутренней сети compose.
    #
    # pg_isready — с явными -U и -d, и раскрывать их обязан sh ВНУТРИ
    # контейнера: на хосте этих переменных нет. Без -U pg_isready идёт под
    # root, и Postgres пишет в лог FATAL, который потом попадает в «ошибки».
    remote "
      printf 'api (healthcheck): '; docker compose exec -T api /api healthcheck >/dev/null 2>&1 && echo ok || echo недоступен
      printf 'api /readyz:       '; docker compose exec -T caddy wget -qO- http://api:8080/readyz 2>&1 | tail -1 || echo недоступен
      printf 'ui:                '; docker compose exec -T caddy wget -qS -O /dev/null http://ui/ 2>&1 | grep -m1 HTTP || echo недоступен
      printf 'postgres:          '; docker compose exec -T postgres sh -c 'pg_isready -U \$POSTGRES_USER -d \$POSTGRES_DB' 2>&1 | tail -1
    "
    ;;

  images)
    remote "docker compose images"
    ;;

  version)
    remote "grep -E '^IMAGE_' .env; for s in backend frontend; do printf 'предыдущий рабочий %s: ' \$s; cat .last-ok-\$s 2>/dev/null || echo '(нет)'; done"
    ;;

  env-keys)
    # Только имена. Значения — это пароль базы, токен администратора и ключи
    # S3; вытащить их в переписку с агентом означает их скомпрометировать.
    remote "grep -oE '^[A-Z_][A-Z0-9_]*=' .env 2>/dev/null | tr -d '=' || echo '(нет файла)'"
    ;;

  disk)
    remote "df -h / | tail -n +1; echo; docker system df; echo; du -sh backups 2>/dev/null || true"
    ;;

  resources)
    remote "free -m; echo; uptime; echo; docker stats --no-stream --format 'table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}'"
    ;;

  certs)
    remote "docker compose exec -T caddy sh -c 'find /data -name \"*.crt\" -exec sh -c \"echo -n \\\"{} \\\"; openssl x509 -enddate -noout -in {}\" \;' 2>/dev/null || echo 'сертификатов нет — Caddy их ещё не выписал'"
    ;;

  backups)
    remote "ls -lh backups 2>/dev/null | tail -20 || echo 'каталога backups нет'"
    ;;

  compose)
    remote "docker compose config"
    ;;

  raw)
    [ $# -gt 0 ] || die "нечего выполнять"
    joined="$*"
    [[ "$joined" =~ $ALLOWED_RAW ]] || die "команда «$joined» не в белом списке читающих. Если она действительно нужна — remote-run.sh, он спросит подтверждение."
    # Ни цепочек, ни подстановок: иначе белый список проверяет только первое
    # слово, а выполняется что угодно после ; или $( ).
    case "$joined" in
      *';'*|*'&&'*|*'||'*|*'|'*|*'$('*|*'`'*|*'>'*|*'<'*)
        die "цепочки, пайпы, подстановки и редиректы в raw запрещены — они обходят белый список" ;;
      *' -exec'*|*' -execdir'*|*' -delete'*|*' -ok'*)
        die "find с -exec/-delete — это не чтение" ;;
      # Значения секретов не читаем даже по прямой просьбе. Имена переменных
      # показывает env-keys.
      *.env*|*secrets*)
        die "содержимое .env и secrets/ не выдаём, для имён переменных есть env-keys" ;;
    esac
    remote "$joined"
    ;;

  ""|-h|--help|help)
    usage
    ;;

  *)
    usage
    die "неизвестная команда: $cmd"
    ;;
esac

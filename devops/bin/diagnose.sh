#!/usr/bin/env bash
#
# Один читающий срез машины: с чего начинается разбор любой аварии.
# Ничего не меняет, состоит только из вызовов remote-read.sh.
#
#   ./devops/bin/diagnose.sh            # на экран
#   ./devops/bin/diagnose.sh > out.txt  # приложить к отчёту
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"

require_host
read_sh="$devops_dir/bin/remote-read.sh"

section() {
  echo
  echo "═══ $1"
}

echo "sdlc-pipeline · $ssh_target · $(date '+%F %T')"

# Одна проверка связи вместо восьми таймаутов подряд. Если машина недоступна,
# дальше смотреть нечего — и причина не в приложении.
err_file="$(mktemp)"
if ! ssh "${ssh_opts[@]}" "$ssh_target" true 2>"$err_file"; then
  echo
  echo "✗ машина недоступна по SSH: $(cat "$err_file")"
  rm -f "$err_file"
  echo
  echo "  Смотрите со стороны Timeweb, а не сервера:"
  echo "    ./devops/bin/twc-get.sh servers"
  echo "    ./devops/bin/twc-get.sh servers/<id>"
  echo
  echo "  Если машина новая и НИКОГДА не отвечала — начните с адреса, а не"
  echo "  с настроек: Timeweb выдаёт IPv4 отдельным плавающим адресом, и он"
  echo "  бывает выделен, но не прописан в сети. Признак — таймаут на ЛЮБОМ"
  echo "  порту, включая тот, где ничего не слушает (там должен быть RST):"
  echo "    nc -z -G 8 ${SDLC_SSH_HOST:-<ip>} 22"
  echo "    nc -z -G 8 ${SDLC_SSH_HOST:-<ip>} 80"
  echo "  Лечится перепривязкой адреса, см. devops/README.md."
  exit 1
fi
rm -f "$err_file"

# Стек может быть ещё не развёрнут — на свежей машине это норма, а не авария.
stack_deployed=yes
ssh "${ssh_opts[@]}" "$ssh_target" \
  "test -f $SDLC_REMOTE_DIR/docker-compose.yml" 2>/dev/null || stack_deployed=no

if [ "$stack_deployed" = no ]; then
  echo
  echo "▸ стек ещё не развёрнут: в $SDLC_REMOTE_DIR нет docker-compose.yml."
  echo "  Машина при этом доступна. Дальше — только состояние железа."
  echo "  Что положить на машину — deploy/README.md."
fi

# Порядок не случайный: сверху то, что чаще всего и оказывается причиной,
# снизу — то, на что смотрят, когда первое не объяснило.
if [ "$stack_deployed" = yes ]; then
  section "версия"
  "$read_sh" version || true

  section "контейнеры"
  "$read_sh" ps || true

  section "живость изнутри машины"
  "$read_sh" health || true

  section "ошибки в логах"
  "$read_sh" errors 300 || true
fi

section "диск"
"$read_sh" disk || true

section "память и нагрузка"
"$read_sh" resources || true

section "сертификаты"
"$read_sh" certs || true

section "часы"
"$read_sh" clock || true

section "бэкапы"
"$read_sh" backups || true

if [ -n "${SDLC_DOMAIN:-}" ]; then
  echo
  echo "Снаружи (эти проверки идут с вашей машины, а не с сервера):"
  for path in /healthz /readyz /; do
    code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "https://$SDLC_DOMAIN$path" 2>/dev/null || true)"
    # curl печатает 000 и на разрыв, и на таймаут — подменяем, иначе в отчёте
    # стоит трёхзначное число, похожее на код ответа.
    case "$code" in
      "" | 000) code="нет ответа (DNS, TLS или порт закрыт)" ;;
    esac
    printf '  %-32s %s\n' "https://$SDLC_DOMAIN$path" "$code"
  done
fi

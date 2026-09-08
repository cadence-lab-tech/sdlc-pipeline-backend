#!/usr/bin/env bash
#
# Меняющие команды на сервере. Всё, чего нет в remote-read.sh, идёт сюда —
# и сюда же настроено «спрашивать разрешение» в .claude/settings.json.
#
#   ./devops/bin/remote-run.sh 'docker compose restart worker'
#   ./devops/bin/remote-run.sh './deploy.sh backend a1b2c3d'
#
# Скрипт печатает, что именно собирается сделать, и не пытается угадывать:
# команда уходит на сервер как есть. Единственная защита здесь — список
# того, что не делается через агента вообще (см. ниже и devops/README.md).
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"

require_host
command_line="${*:?укажите команду}"

# Операции, у которых нет отката и цена ошибки — работа коллег. Их выполняет
# человек, руками, глядя на живой сервер. Агент до них не допускается даже
# с подтверждением: подтверждение нажимают быстрее, чем читают.
case "$command_line" in
  *'docker compose down'*|*'compose down -v'*|*'docker volume rm'*|*'docker system prune'*)
    die "останов стека и удаление томов — только руками: в pgdata лежат задачи, артефакты и аудит" ;;
  *'DROP DATABASE'*|*'DROP TABLE'*|*'TRUNCATE'*|*'DROP SCHEMA'*)
    die "разрушающие операции с базой — только руками и после свежего дампа" ;;
  *'rm -rf /'*|*'mkfs'*|*'> /dev/'*)
    die "нет" ;;
esac

echo "▸ $ssh_target: $command_line"
# Без -t намеренно: псевдотерминал ломает вывод, когда скрипт запускают
# не из интерактивной оболочки, а всё, что здесь выполняется, неинтерактивно.
ssh "${ssh_opts[@]}" "$ssh_target" "cd '$SDLC_REMOTE_DIR' && $command_line"

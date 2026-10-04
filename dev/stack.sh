#!/usr/bin/env bash
# Изолированный стек на задачу: своя база, своё приложение, свои порты.
#
#   dev/stack.sh up CLAB-59      — поднять стек задачи и дождаться готовности
#   dev/stack.sh env CLAB-59     — напечатать адреса и переменные (для eval)
#   dev/stack.sh ls              — какие стеки заняты
#   dev/stack.sh logs CLAB-59    — логи api
#   dev/stack.sh psql CLAB-59    — psql в базу стека
#   dev/stack.sh test-db [KEY]   — завести sdlc_test в базе стека и напечатать
#                                  её адрес (для make test-integration); без
#                                  KEY — стек, поднятый из этой рабочей копии
#   dev/stack.sh down CLAB-59    — снести вместе с данными
#
# Зачем. Параллельно идут несколько задач, и у каждой свой агент. Общий стек
# они делят вместе с тестовыми данными: один разворачивает демо, другой сносит
# базу, третий ловит чужие задачи в очереди — и проверки перестают что-либо
# доказывать. Стек на задачу разводит их так же, как git worktree разводит
# рабочие копии.
#
# Порты не выбираются руками: агент их не угадает. Набор выдаётся один раз и
# запоминается в реестре вне репозитория (CLAB_STACKS_DIR, по умолчанию
# ~/.clab/stacks), поэтому повторный up той же задачи поднимает стек на тех же
# адресах, а соседняя задача получает свободные.
#
# Стек — объект тестирования, а не источник задач: за задачей агент по-прежнему
# ходит в рабочий инстанс через MCP и clab, а в своём стеке только проверяет код.
set -euo pipefail

dev=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
compose="$dev/docker-compose.yml"
registry=${CLAB_STACKS_DIR:-$HOME/.clab/stacks}
# Сколько наборов портов выдаём. Каждый стек — это Postgres, api и воркер;
# держать их на машине десятками смысла нет, а промах мимо диапазона лучше
# увидеть сообщением, чем занятым портом соседа.
slots=${CLAB_STACKS_SLOTS:-8}

die() { printf 'stack.sh: %s\n' "$*" >&2; exit 1; }

# Ключ задачи в имя проекта compose: docker принимает только строчные буквы,
# цифры, дефис и подчёркивание.
slug() { printf 'clab-%s' "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-' | sed 's/-*$//')"; }

port_free() {
    ! lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}

# Смещение занято, если его держит стек другой задачи или кто-то посторонний
# уже слушает эти порты.
offset_taken() {
    local off=$1 key=$2 f k o
    for f in "$registry"/*.env; do
        if [ -e "$f" ]; then
            k=$(sed -n 's/^KEY=//p' "$f")
            o=$(sed -n 's/^OFFSET=//p' "$f")
            if [ "$k" != "$key" ] && [ "$o" = "$off" ]; then
                return 0
            fi
        fi
    done
    if port_free "$((18080 + off))" && port_free "$((55440 + off))" && port_free "$((5180 + off))"; then
        return 1
    fi
    return 0
}

alloc() {
    local key=$1 off
    for ((off = 0; off < slots; off++)); do
        if ! offset_taken "$off" "$key"; then
            printf '%s' "$off"
            return 0
        fi
    done
    die "свободных наборов портов нет (CLAB_STACKS_SLOTS=$slots). Снесите ненужные стеки: dev/stack.sh ls"
}

state_file() { printf '%s/%s.env' "$registry" "$(slug "$1")"; }

# Состояние стека: смещение, порты и корень, из которого он собран. Пишется
# один раз при первом up и читается всеми остальными командами.
load_or_create() {
    local key=$1 file off
    file=$(state_file "$key")
    if [ -f "$file" ]; then
        # shellcheck disable=SC1090
        . "$file"
        return
    fi
    [ "${2:-}" = create ] || die "стек $key не поднят: dev/stack.sh up $key"
    mkdir -p "$registry"
    off=$(alloc "$key")
    cat > "$file" <<EOF
KEY=$key
PROJECT=$(slug "$key")
OFFSET=$off
API_PORT=$((18080 + off))
POSTGRES_PORT=$((55440 + off))
UI_PORT=$((5180 + off))
ADMINER_PORT=$((18100 + off))
MINIO_PORT=$((19100 + off))
MINIO_CONSOLE_PORT=$((19200 + off))
SOURCE=$(cd "$dev/.." && pwd)
EOF
    # shellcheck disable=SC1090
    . "$file"
}

# Переменные, с которыми поднимается стек задачи. Фоновые задания выключены
# намеренно: индексация приватных репозиториев и эмбеддинги стоят денег и
# лимитов GitHub, а проверяют в задачном стеке обычно не их. Нужны — up --index.
stack_env() {
    export DEV_STACK_PREFIX="$PROJECT"
    export API_PORT POSTGRES_PORT ADMINER_PORT MINIO_PORT MINIO_CONSOLE_PORT
    if [ "${INDEX:-0}" = 1 ]; then return; fi
    export SDLC_INDEX_ENABLED=false
    export SDLC_EMBEDDINGS_PROVIDER=
    export SDLC_GITHUB_TOKEN=
    export SDLC_BLOB_GC_ENABLED=false
    export SDLC_EMBEDDING_CACHE_GC_ENABLED=false
}

dc() { docker compose -f "$compose" -p "$PROJECT" "$@"; }

wait_ready() {
    local n
    for ((n = 0; n < 90; n++)); do
        curl -fsS "http://localhost:$API_PORT/readyz" >/dev/null 2>&1 && return 0
        sleep 2
    done
    die "стек поднялся, но /readyz не ответил за три минуты: dev/stack.sh logs $KEY"
}

# Выделенная база интеграционных тестов в Postgres стека: тест сносит в ней
# схему целиком, поэтому рядом с базой приложения, а не вместо неё.
test_db_url() { printf 'postgres://sdlc:sdlc@localhost:%s/sdlc_test?sslmode=disable' "$POSTGRES_PORT"; }

# Стек, поднятый из этой рабочей копии: SOURCE в реестре совпадает с её корнем.
# Сравнение по физическому пути — worktree может лежать за символической
# ссылкой, а SOURCE записан тем путём, каким стек поднимали. Печатает ключ;
# стека нет — пусто; стеков несколько — ошибка, выбирать за человека нельзя.
stack_for_copy() {
    local here f src found=
    here=$(cd "$dev/.." && pwd -P)
    for f in "$registry"/*.env; do
        [ -e "$f" ] || continue
        src=$(sed -n 's/^SOURCE=//p' "$f")
        src=$(cd "$src" 2>/dev/null && pwd -P) || continue
        if [ "$src" = "$here" ]; then
            found="$found${found:+ }$(sed -n 's/^KEY=//p' "$f")"
        fi
    done
    case "$found" in
    *" "*) die "из этой копии подняты стеки нескольких задач ($found): укажите ключ — make test-integration KEY=<ключ>" ;;
    esac
    printf '%s' "$found"
}

print_env() {
    # SDLC_API_URL намеренно не экспортируется: его читает и clab, и агент,
    # экспортировав его, увёл бы CLI из рабочего инстанса в локальный стек.
    # Интерфейсу он передаётся префиксом одной команды, а не в оболочку.
    cat <<EOF
# стек задачи $KEY (compose-проект $PROJECT)
export CLAB_STACK_API=http://localhost:$API_PORT
export CLAB_STACK_MCP=http://localhost:$API_PORT/mcp/projects/SDLC
export CLAB_STACK_DB=postgres://sdlc:sdlc@localhost:$POSTGRES_PORT/sdlc?sslmode=disable
export CLAB_STACK_TEST_DB=$(test_db_url)
export SDLC_UI_PORT=$UI_PORT
# интерфейс: SDLC_API_URL=\$CLAB_STACK_API npm run dev
EOF
}

cmd=${1:-}
[ -n "$cmd" ] || die "что делать? up|down|stop|env|ls|logs|psql|e2e|test-db"
shift || true

case "$cmd" in
ls)
    printf '%-12s %-14s %-6s %-6s %-6s %s\n' ЗАДАЧА ПРОЕКТ API БАЗА UI ИСТОЧНИК
    for f in "$registry"/*.env; do
        if [ ! -e "$f" ]; then echo "(стеков нет)"; break; fi
        # shellcheck disable=SC1090
        ( . "$f"; printf '%-12s %-14s %-6s %-6s %-6s %s\n' "$KEY" "$PROJECT" "$API_PORT" "$POSTGRES_PORT" "$UI_PORT" "$SOURCE" )
    done
    ;;
up)
    key=${1:-} ; shift || true
    [ -n "$key" ] || die "какой задаче поднять стек? dev/stack.sh up CLAB-59"
    INDEX=0
    for a in "$@"; do
        if [ "$a" = --index ]; then INDEX=1; fi
    done
    load_or_create "$key" create
    stack_env
    dc up -d --build
    wait_ready
    cat <<EOF

Стек задачи $KEY поднят.

  API        http://localhost:$API_PORT
  MCP        http://localhost:$API_PORT/mcp/projects/SDLC
  Postgres   localhost:$POSTGRES_PORT (sdlc/sdlc/sdlc)
  Интерфейс  SDLC_API_URL=http://localhost:$API_PORT SDLC_UI_PORT=$UI_PORT npm run dev
             http://localhost:$UI_PORT

  Переменные в оболочку:          eval "\$(dev/stack.sh env $KEY)"
  (SDLC_API_URL в них нет: его читает clab, и он смотрит в рабочий инстанс)
  Снести вместе с данными:        dev/stack.sh down $KEY
EOF
    ;;
env)
    load_or_create "${1:?какой задаче}"
    print_env
    ;;
logs)
    load_or_create "${1:?какой задаче}"; shift
    stack_env
    dc logs -f "${1:-api}"
    ;;
psql)
    load_or_create "${1:?какой задаче}"
    stack_env
    dc exec postgres psql -U sdlc -d sdlc
    ;;
e2e)
    # Сквозной проход в своём стеке: база свежая, демо-задача на new — ровно
    # то состояние, с которого сценарий начинается.
    load_or_create "${1:?какой задаче}"
    SDLC_API_URL="http://localhost:$API_PORT/v1" "$dev/e2e.sh"
    ;;
test-db)
    # Выбор базы для make test-integration: вся логика реестра здесь, Makefile
    # его формат не читает. Стек не найден по копии — пустой вывод и код 0:
    # тогда тесты идут в общий стек, как до стеков на задачу. Стек найден, но
    # стоит, — ошибка: тихий уход в общий стек снёс бы схему соседям.
    key=${1:-}
    if [ -z "$key" ]; then
        key=$(stack_for_copy)
        [ -n "$key" ] || exit 0
    fi
    load_or_create "$key"
    stack_env
    if ! dc exec -T postgres pg_isready -U sdlc -d postgres >/dev/null 2>&1; then
        die "Postgres стека $KEY не отвечает (стек остановлен?): dev/stack.sh up $KEY"
    fi
    if [ "$(dc exec -T postgres psql -U sdlc -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname = 'sdlc_test'")" != 1 ]; then
        dc exec -T postgres psql -U sdlc -d postgres -qc 'CREATE DATABASE sdlc_test' >/dev/null \
            || die "не удалось завести sdlc_test в стеке $KEY"
    fi
    printf 'стек задачи %s (compose-проект %s)\n' "$KEY" "$PROJECT" >&2
    test_db_url
    echo
    ;;
stop)
    load_or_create "${1:?какой задаче}"
    stack_env
    dc stop
    ;;
down)
    load_or_create "${1:?какой задаче}"
    stack_env
    dc down -v
    rm -f "$(state_file "$KEY")"
    echo "стек задачи $KEY снесён вместе с данными"
    ;;
*)
    die "неизвестная команда $cmd: up|down|stop|env|ls|logs|psql|e2e|test-db"
    ;;
esac

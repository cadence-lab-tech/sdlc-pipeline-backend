# Окружение сквозных сценариев dev/e2e.sh и dev/e2e-orchestration.sh;
# подключается через source (CLAB-306). Сценарий задаёт окружение clab сам и
# ничего не берёт из окружения запустившего: исполнитель этапа держит в нём
# токен задачи и SDLC_PROJECT рабочего инстанса, а сценарий, ушедший по этим
# данным на рабочий инстанс, нагрузил бы его и писал бы в его проект.
#
#   e2e_env <адрес API> <рабочий каталог сценария>
#
#   - адрес не локальный (не localhost, 127.0.0.1, [::1]) — отказ, код 1,
#     до первого вызова clab;
#   - SDLC_API_URL — этот адрес, SDLC_PROJECT — $E2E_PROJECT или SDLC;
#   - CLAB_CONFIG_DIR — пустой каталог внутри рабочего: «хост не настроен»
#     вместо ~/.config/clab с личным токеном;
#   - SDLC_TOKEN снят. Действующее лицо сценарий задаёт сам на каждый вызов
#     (SDLC_ACTOR), стек на задачу работает в режиме header.

e2e_env() {
    local api=$1 work=$2 hostport host
    hostport=${api#*://}
    hostport=${hostport%%[/?#]*}
    hostport=${hostport##*@}
    case $hostport in
    \[*\]*) host=${hostport%%]*}] ;;
    *) host=${hostport%%:*} ;;
    esac
    case $host in
    localhost | 127.0.0.1 | '[::1]') ;;
    *)
        echo "SDLC_API_URL=$api: адрес не локальный ($host) — сценарий заводит задачи и пишет в базу, ему место только против dev-стека на localhost, 127.0.0.1 или [::1]" >&2
        return 1
        ;;
    esac
    mkdir -m 700 "$work/clab"
    unset SDLC_TOKEN
    export SDLC_API_URL=$api SDLC_PROJECT=${E2E_PROJECT:-SDLC} CLAB_CONFIG_DIR=$work/clab
}

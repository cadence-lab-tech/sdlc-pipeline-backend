#!/usr/bin/env bash
# Сквозной проход эпика: оркестратор забирает эпик, режет его на дочерние
# задачи, задаёт порядок связями, отчитывается о стоимости; эпик стоит под
# гейтом зависимостей, пока дети не закрыты (решения 0024-0026).
#
#   make up && dev/e2e-orchestration.sh                                  # общий dev-стек
#   SDLC_API_URL=http://localhost:<порт>/v1 dev/e2e-orchestration.sh     # стек задачи
#
# Каждый прогон заводит в проекте SDLC свой эпик, поэтому сценарий
# повторяется на той же базе без сброса, а сид-задачи не трогает. EPIC=<ключ>
# берёт существующий: тип epic, статус planning и без дочерних задач — шаги
# 3, 7 и 8 работают с детьми эпика и отменили бы чужие.
set -euo pipefail

API=${SDLC_API_URL:-http://localhost:8088/v1}
CLI=${SDLC_CLI:-./bin/clab}
EPIC=${EPIC:-}
export SDLC_API_URL=$API SDLC_PROJECT=${SDLC_PROJECT:-SDLC}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

as() { SDLC_ACTOR="$1" "$CLI" --json "${@:2}"; }
status() { as human:ci show "$1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["task"]["status"])'; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
file() { printf '%s\n' "$2" > "$work/$1"; echo "$work/$1"; }

# json_or_die печатает JSON ответа clab, а при отказе — одну строку
# «КЛЮЧ: ошибка clab» в stderr и код 1: опечатка в ключе или недоступный
# сервер — не повод для трейсбека python3 на пустом stdout.
json_or_die() {
    local key=$1 out
    shift
    if ! out=$(as human:ci "$@" 2>"$work/clab.err"); then
        echo "$key: $(grep -m1 . "$work/clab.err" || echo "clab $1 завершился с ошибкой")" >&2
        return 1
    fi
    printf '%s\n' "$out"
}

[ -x "$CLI" ] || { echo "нет $CLI — соберите: make build"; exit 1; }

if [ -n "$EPIC" ]; then
    # Явный эпик проверяется до шага 1: сценарий на нём либо пройдёт
    # целиком, либо не начнётся, а не оборвётся на середине.
    origin="задан EPIC"
    if ! shown=$(json_or_die "$EPIC" show "$EPIC") ||
        ! children=$(json_or_die "$EPIC" tasks --parent "$EPIC"); then
        echo "Без EPIC сценарий заводит свой эпик и повторяется на той же базе."
        exit 1
    fi
    printf '%s\n%s\n' "$shown" "$children" | python3 -c '
import json,sys
dec=json.JSONDecoder()
raw=sys.stdin.read()
d,end=dec.raw_decode(raw)
kids=dec.raw_decode(raw[end:].lstrip())[0]["tasks"]
problems=[]
if d["task"]["type"] != "epic": problems.append("тип %s, а сценарий проводит эпик" % d["task"]["type"])
if d["task"]["status"] != "planning": problems.append("статус %s, а сценарий начинается с planning" % d["task"]["status"])
if kids: problems.append("у эпика уже есть дочерние задачи (%s) — шаги 3, 7 и 8 работают с детьми и отменили бы их" % ", ".join(t["key"] for t in kids))
for p in problems: print(sys.argv[1] + ":", p)
sys.exit(1 if problems else 0)' "$EPIC" || {
        echo "Без EPIC сценарий заводит свой эпик и повторяется на той же базе."
        exit 1
    }
else
    origin="заведён сценарием"
    EPIC=$(as human:ci create --type epic --title "e2e: проход эпика" \
        --description "Заведён dev/e2e-orchestration.sh." | python3 -c '
import json,sys; print(json.load(sys.stdin)["key"])')
fi

step "0. Эпик $EPIC ($origin) идёт своим циклом, а не циклом задач"
as human:ci tasks --type epic | python3 -c '
import json,sys
for t in json.load(sys.stdin)["tasks"]:
    if t["key"] == sys.argv[1]: print(" ", t["key"], t["status_key"], "|", t["title"])' "$EPIC"

step "1. Человек отдаёт эпик оркестратору"
as human:ci transition "$EPIC" --to start >/dev/null
echo "статус: $(status "$EPIC")"

step "2. Оркестратор забирает эпик: модель и инструменты приезжают из версии роли"
as agent:orchestrator-bot claim "$EPIC" --session ci/orchestration | python3 -c '
import json,sys; d=json.load(sys.stdin)
print("тип задачи:", d["task"]["type"], "| роль:", d["role"]["key"])
print("отчёт о стоимости:", d["usage_report"])'

step "3. Оркестратор режет эпик на дочерние задачи и задаёт порядок"
# Раннер должен быть известен системе: назначение на несуществующего
# исполнителя — опечатка, и движок её отбивает.
as agent:runner-bot whoami >/dev/null
first=$(as agent:orchestrator-bot create --title "Ручка учёта стоимости" --parent "$EPIC" \
    --repos backend --assignee agent:runner-bot | python3 -c '
import json,sys; d=json.load(sys.stdin)
print(" ", d["key"], "->", d["parent_key"], "| исполнитель:", d["assignee_handle"], file=sys.stderr)
print(d["key"])')
as agent:orchestrator-bot create --title "Стоимость в карточке эпика" --parent "$EPIC" \
    --repos frontend --blocks "$first" | python3 -c '
import json,sys; d=json.load(sys.stdin); print(" ", d["key"], "->", d["parent_key"], "| ждёт", sys.argv[1])' "$first"

# Правило «агент режет только тот эпик, который держит» проверяется уровнем
# участия, а в режиме заголовка права выключены целиком (решение 0021), поэтому
# здесь его не показать: оно проверяется тестами на настоящей схеме
# (TestAgentCreatesOnlyChildrenOfHeldTask).

step "4. Разбивка сдана, человек её утверждает"
as agent:orchestrator-bot submit "$EPIC" --kind planning.breakdown --title "Разбивка эпика" \
    --file "$(file breakdown.md '## Нарезка

1. Ручка учёта стоимости — бэкенд.
2. Стоимость в карточке эпика — фронтенд, ждёт первую.')" >/dev/null
as human:ci decide "$EPIC" --transition breakdown_done --gate human_approval >/dev/null
as agent:orchestrator-bot transition "$EPIC" --to breakdown_done --summary "Эпик нарезан" >/dev/null
echo "статус: $(status "$EPIC")"

step "5. Оркестратор отчитывается о стоимости своего прогона"
as agent:orchestrator-bot usage "$EPIC" --model claude-opus-5 \
    --input 18000 --output 2400 --cost 0.63 --duration-ms 74000 >/dev/null
as human:ci usage "$EPIC" --show | python3 -c '
import json,sys; d=json.load(sys.stdin)
print(" эпик:", d["cost_usd"], "$ | поддерево:", d["subtree_cost_usd"], "$ | прогонов без отчёта:", d["silent_runs"])'

step "6. Эпик не закрыть, пока открыты дочерние задачи"
as human:ci gates "$EPIC" --transition children_done | python3 -c '
import json,sys
for g in json.load(sys.stdin)["gates"]:
    print(" ", g["type"], "->", g["status"], "|", g["reason"])'

step "7. Назначенную задачу забирает только назначенный"
child=$(as human:ci tasks --parent "$EPIC" | python3 -c '
import json,sys
print([t["key"] for t in json.load(sys.stdin)["tasks"] if t.get("assignee_handle")][0])')
as human:ci transition "$child" --to start >/dev/null
as agent:other-bot claim "$child" 2>&1 | head -1 || true
as agent:runner-bot claim "$child" >/dev/null && echo "  $child забрал назначенный агент"
as agent:runner-bot usage "$child" --model claude-haiku-4-5 --input 4000 --output 900 --cost 0.04 >/dev/null

step "8. Дети закрыты — гейт зависимостей пропускает эпик"
for c in $(as human:ci tasks --parent "$EPIC" | python3 -c '
import json,sys
for t in json.load(sys.stdin)["tasks"]: print(t["key"])'); do
    [ "$(status "$c")" = cancelled ] || as human:ci transition "$c" --to cancel --summary "демонстрация" >/dev/null
done
as human:ci transition "$EPIC" --to children_done --summary "Нарезка доведена" >/dev/null

final=$(status "$EPIC")
step "Итог: $EPIC на статусе $final"
[ "$final" = done ] || { echo "ОШИБКА: ожидался статус done"; exit 1; }

echo
echo "Стоимость эпика вместе с детьми:"
as human:ci usage "$EPIC" --show | python3 -c '
import json,sys; d=json.load(sys.stdin)
print("  своя", d["cost_usd"], "$, поддерево", d["subtree_cost_usd"], "$, токенов", d["tokens"])
print("  прогонов с отчётом", d["reported_runs"], ", без отчёта", d["silent_runs"])'

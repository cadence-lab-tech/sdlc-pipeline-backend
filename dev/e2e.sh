#!/usr/bin/env bash
# Сквозной проход задачи по циклу глазами агентов: захват, аренда, сдача
# артефактов, перепроверка ревьюером, апрувы и переходы до терминального
# статуса. Проверяет ровно то, что нельзя проверить юнит-тестом, — что
# движок, аренды, гейты и транспорт складываются в рабочий цикл.
#
#   make up && dev/e2e.sh            # общий dev-стек
#   dev/stack.sh e2e <ключ>          # стек задачи
#
# Каждый прогон заводит в проекте SDLC свою задачу с областью backend, поэтому
# сценарий повторяется на той же базе без сброса. TASK=<ключ> берёт
# существующую задачу: она должна стоять на new, с backend в области и без
# frontend — шаг 10 привязывает PR к backend и показывает отказ на frontend.
set -euo pipefail

API=${SDLC_API_URL:-http://localhost:8088/v1}
CLI=${SDLC_CLI:-./bin/clab}
TASK=${TASK:-}
export SDLC_API_URL=$API SDLC_PROJECT=${SDLC_PROJECT:-SDLC}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

as() { SDLC_ACTOR="$1" "$CLI" --json "${@:2}"; }
status() { as human:ci show "$TASK" | python3 -c 'import json,sys;print(json.load(sys.stdin)["task"]["status"])'; }
step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
file() { printf '%s\n' "$2" > "$work/$1"; echo "$work/$1"; }

[ -x "$CLI" ] || { echo "нет $CLI — соберите: make build"; exit 1; }

if [ -n "$TASK" ]; then
    # Явная задача проверяется до шага 1: сценарий на ней либо пройдёт
    # целиком, либо не начнётся, а не оборвётся на середине.
    origin="задана TASK"
    as human:ci show "$TASK" | python3 -c '
import json,sys
d=json.load(sys.stdin)
hints={r.get("role_hint") for r in d["context"]["repositories"]}
problems=[]
if d["task"]["status"] != "new": problems.append("статус %s, а сценарий начинается с new" % d["task"]["status"])
if "backend" not in hints: problems.append("в области нет backend — шаг 10 привязывает к нему PR")
if "frontend" in hints: problems.append("в области есть frontend — шаг 10 показывает на нём отказ привязки")
for p in problems: print(sys.argv[1] + ":", p)
sys.exit(1 if problems else 0)' "$TASK" || {
        echo "Без TASK сценарий заводит свою задачу и повторяется на той же базе."
        exit 1
    }
else
    origin="заведена сценарием"
    TASK=$(as human:ci create --title "e2e: сквозной проход цикла" \
        --description "Заведена dev/e2e.sh." --repos backend | python3 -c '
import json,sys; print(json.load(sys.stdin)["key"])')
fi

start=$(status)
step "0. Задача $TASK ($origin) в начале цикла: $start"

step "1. Человек запускает задачу в цикл"
as human:ci transition "$TASK" --to start >/dev/null
echo "статус: $(status)"

step "2. Аналитик забирает задачу и получает инструкцию роли"
as agent:analyst-bot claim "$TASK" --session ci/e2e | python3 -c '
import json,sys; d=json.load(sys.stdin)
print("роль:", d["role"]["key"], "версия", d["role"]["version"])
print("контракт:", ", ".join(c["kind"] for c in d["role"]["output_contract"]))
print("аренда до:", d["lease"]["expires_at"], "| heartbeat каждые", d["lease"]["heartbeat_interval_seconds"], "с")'

step "3. Вторая машина на занятой задаче получает отказ"
as agent:other-bot claim "$TASK" 2>&1 | head -1 || true

step "4. Аналитик продлевает аренду и сдаёт артефакты"
as agent:analyst-bot heartbeat "$TASK" >/dev/null
as agent:analyst-bot submit "$TASK" --kind analysis.brief --title "Разбор требований" \
    --file "$(file brief.md '## Требования

REST по проектам и задачам.')" >/dev/null
as agent:analyst-bot submit "$TASK" --kind analysis.acceptance_criteria --title "Критерии приёмки" \
    --file "$(file ac.md '## Критерии

1. GET /v1/projects отдаёт 200 и список проектов.')" >/dev/null
as agent:analyst-bot gates "$TASK" --transition analysis_done | python3 -c '
import json,sys
for g in json.load(sys.stdin)["gates"]: print(" ", g["type"], "->", g["status"], g.get("reason",""))'

step "5. Запрос перехода запускает перепроверку ревьюером"
as agent:analyst-bot transition "$TASK" --to analysis_done --summary "Требования разобраны" >/dev/null 2>&1 || true
as agent:reviewer-bot queue --role reviewer | python3 -c '
import json,sys
for q in json.load(sys.stdin)["queue"]:
    print(" ", q["task"]["key"], q["stage_run"]["kind"], "попытка", q["stage_run"]["attempt"], "| можно забрать:", q["claimable"])'

step "6. Ревьюер забирает перепроверку и возвращает результат на доработку"
as agent:reviewer-bot claim "$TASK" | python3 -c '
import json,sys; d=json.load(sys.stdin)
r=d["context"]["review"]
print("проверяет результат роли:", r["subject_role"], "| гейт:", r["gate"])
print("на входе:", ", ".join(a["kind"] for a in r["subject_artifacts"]))'
as agent:reviewer-bot submit "$TASK" --kind review.verdict --title "Вердикт ревью" \
    --file "$(file fail.json '{"verdict":"fail","findings":[{"severity":"major","text":"Критерий 1 не проверяем"}]}')" >/dev/null
as agent:analyst-bot gates "$TASK" --transition analysis_done | python3 -c '
import json,sys
for g in json.load(sys.stdin)["gates"]:
    if g["type"] == "agent_review": print(" agent_review ->", g["status"], "|", g["reason"])'

step "7. Аналитик дорабатывает — вердикт устаревает, ревью запускается заново"
as agent:analyst-bot submit "$TASK" --kind analysis.acceptance_criteria --title "Критерии приёмки" \
    --file "$(file ac2.md '## Критерии

1. GET /v1/projects отдаёт 200, список проектов и не требует авторизации.')" >/dev/null
as agent:analyst-bot transition "$TASK" --to analysis_done --summary "Критерий уточнён" >/dev/null 2>&1 || true
as agent:reviewer-bot claim "$TASK" | python3 -c '
import json,sys; d=json.load(sys.stdin); print("попытка перепроверки:", d["task"]["attempt"])'
as agent:reviewer-bot submit "$TASK" --kind review.verdict --title "Вердикт ревью" \
    --file "$(file pass.json '{"verdict":"pass","findings":[]}')" >/dev/null

step "8. Человек ставит апрув, задача уходит в архитектуру"
as human:ci decide "$TASK" --transition analysis_done --gate human_approval >/dev/null
as agent:analyst-bot transition "$TASK" --to analysis_done --summary "Требования разобраны и перепроверены" | python3 -c '
import json,sys; d=json.load(sys.stdin)
print("гейты:", ", ".join(f'"'"'{g["type"]}={g["status"]}'"'"' for g in d["gates"]))
print("статус:", d["task"]["status_key"])'

step "9. Архитектура"
as agent:arch-bot claim "$TASK" >/dev/null
as agent:arch-bot submit "$TASK" --kind arch.adr --title "Решение и обоснование" \
    --file "$(file adr.md '## Решение

Слой httpapi поверх движка цикла.')" >/dev/null
as agent:arch-bot submit "$TASK" --kind arch.impact_map --title "Затрагиваемые компоненты" \
    --file "$(file impact.md '## Компоненты

internal/httpapi, internal/workflow')" >/dev/null
as human:ci decide "$TASK" --transition architecture_done --gate traceability \
    --details '{"checked":["ac_covered","impact_complete"]}' >/dev/null
as human:ci decide "$TASK" --transition architecture_done --gate human_approval >/dev/null
as agent:arch-bot transition "$TASK" --to architecture_done --summary "Решение зафиксировано" >/dev/null
echo "статус: $(status)"

step "10. Разработка: PR обязателен, лежит в области задачи, CI-гейт снимает человек"
as agent:dev-bot claim "$TASK" >/dev/null
for kind in dev.plan dev.change_summary dev.test_report; do
    as agent:dev-bot submit "$TASK" --kind "$kind" --title "$kind" \
        --file "$(file "$kind.md" "## $kind

текст")" >/dev/null
done
as agent:dev-bot link "$TASK" --repo frontend --ref 5 2>&1 | head -1 || true
as agent:dev-bot link "$TASK" --repo backend --ref 128 \
    --url https://github.com/cadence-lab-tech/sdlc-pipeline-backend/pull/128 --state open >/dev/null
# Гейт external_check читает состояние проверок, которое приносит воркер из
# GitHub. Локально репозитория с этим PR нет, проверки не приезжают, и гейт
# честно стоит в pending — это не поломка сценария, а его отдельный шаг:
# человек снимает гейт вручную, и снятие остаётся в аудите.
as human:ci decide "$TASK" --transition dev_done --gate ci --status waived \
    --details '{"reason":"CI недоступен в локальном прогоне"}' >/dev/null
as agent:dev-bot transition "$TASK" --to dev_done --summary "Реализовано" >/dev/null
echo "статус: $(status)"

step "11. Ревью изменений: схема контракта роли отбивает вердикт при сдаче, гейт schema_validation сверяет его значение"
as agent:reviewer-bot claim "$TASK" >/dev/null
# Содержимое вердикта проверяется дважды. Схема из контракта роли — при сдаче
# (CLAB-102): {"verdict":"maybe"} без findings не ложится в задачу вовсе.
# Ожидаемое значение — гейтом на переходе: вердикт fail валиден по схеме,
# сдаётся, но review_passed не пускает.
if as agent:reviewer-bot submit "$TASK" --kind review.verdict --title "Вердикт ревью" \
    --file "$(file bad.json '{"verdict":"maybe"}')" >/dev/null 2>"$work/bad.err"; then
    echo "ОШИБКА: невалидный вердикт принят — сдача не сверилась со схемой контракта роли"
    exit 1
fi
[ "$(head -1 "$work/bad.err" | cut -d: -f1)" = validation_failed ] || {
    echo "ОШИБКА: сдача невалидного вердикта отбита не проверкой схемы:"
    cat "$work/bad.err"
    exit 1
}
echo "  сдача отбита: $(head -1 "$work/bad.err")"
tail -n +2 "$work/bad.err" | python3 -c '
import json,sys
for p in json.loads(sys.stdin.read() or "{}").get("problems", []): print("   ", p)'
as agent:reviewer-bot submit "$TASK" --kind review.verdict --title "Вердикт ревью" \
    --file "$work/fail.json" >/dev/null
as agent:reviewer-bot gates "$TASK" --transition review_passed | python3 -c '
import json,sys
gates=json.load(sys.stdin)["gates"]
for g in gates: print(" ", g["type"], "->", g["status"], "|", g.get("reason", ""))
if [g["status"] for g in gates if g["type"] == "schema_validation"] != ["failed"]:
    print("ОШИБКА: гейт schema_validation пропустил вердикт fail"); sys.exit(1)'
as agent:reviewer-bot submit "$TASK" --kind review.verdict --title "Вердикт ревью" \
    --file "$work/pass.json" >/dev/null
as agent:reviewer-bot transition "$TASK" --to review_passed --summary "Изменения проверены" >/dev/null
echo "статус: $(status)"

step "12. QA и закрытие"
as agent:qa-bot claim "$TASK" >/dev/null
as agent:qa-bot submit "$TASK" --kind qa.report --title "Отчёт QA" \
    --file "$(file qa.md '## Проверка

Критерий 1: выполнен.')" >/dev/null
as human:ci decide "$TASK" --transition qa_passed --gate human_approval >/dev/null
as agent:qa-bot transition "$TASK" --to qa_passed --summary "Критерии выполнены" >/dev/null

final=$(status)
step "Итог: $TASK на статусе $final"
[ "$final" = done ] || { echo "ОШИБКА: ожидался статус done"; exit 1; }

echo
echo "Лента задачи:"
curl -sf "$API/tasks/$TASK/timeline?limit=100" -H 'X-SDLC-Actor: human:ci' | python3 -c '
import json,sys
events=list(reversed(json.load(sys.stdin)["events"]))
for e in events: print(f'"'"'  {e["created_at"][11:19]}  {e["actor_handle"] or "-":<14} {e["type"]}'"'"')'

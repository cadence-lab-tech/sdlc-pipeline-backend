#!/usr/bin/env bash
# govulncheck по бинарям образа версии (CLAB-130): то, что увидит у себя
# клиент со сканером, который метит по модулю, а не по вызову.
#
# В CI (ci.yml) govulncheck идёт по исходникам и падает на достижимой
# уязвимости. Здесь — бинарный режим по /api, /worker и /clab собранного
# образа. Бинари собраны с -s -w: таблицы символов нет, и govulncheck считает
# задетым весь пакет модуля, который есть в бинаре. Поэтому находка здесь
# релиз не роняет: заявленная в VEX (openvex.json рядом, not_affected или
# fixed) печатается уведомлением, незаявленная — предупреждением с ID, чтобы
# её разобрали и либо исправили, либо заявили в VEX следующим PR.
#
#   govulncheck-binaries.sh <openvex.json> <бинарь>...
#
# Окружение:
#   GOVULNCHECK          команда govulncheck; по умолчанию закреплённая
#                        версия через go run, как в ci.yml
#   GITHUB_STEP_SUMMARY  файл сводки прогона, если задан
#
# Код выхода 0 при любых находках и при сбое самого govulncheck (это тоже
# предупреждение); 2 — неправильный вызов.
set -euo pipefail

[ $# -ge 2 ] || { echo "использование: $0 <openvex.json> <бинарь>..." >&2; exit 2; }
vex=$1
shift
[ -f "$vex" ] || { echo "нет файла VEX $vex" >&2; exit 2; }
read -r -a govulncheck <<<"${GOVULNCHECK:-go run golang.org/x/vuln/cmd/govulncheck@v1.9.0}"

# Заявленные ID: not_affected и fixed — то, что сканер клиента после VEX
# показывать не должен.
covered=$(jq -r '.statements[] | select(.status == "not_affected" or .status == "fixed") | .vulnerability.name' "$vex" | sort -u)

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
summary=$tmp/summary.md
{
  echo "### govulncheck -mode=binary"
  echo
  echo "| бинарь | находка | VEX |"
  echo "|---|---|---|"
} >"$summary"

uncovered_total=0
for bin in "$@"; do
  name=$(basename "$bin")
  out=$tmp/$name.json
  if ! "${govulncheck[@]}" -mode=binary -format json "$bin" >"$out" 2>"$tmp/err"; then
    echo "::warning title=govulncheck::$name: govulncheck не отработал — бинарь не проверен: $(tail -n 3 "$tmp/err" | tr '\n' ' ')"
    echo "| $name | govulncheck не отработал | — |" >>"$summary"
    continue
  fi
  # Находка уровня символа — то, что текстовый вывод называет «Your code is
  # affected»; уровни модуля и пакета govulncheck сам за находки не считает.
  ids=$(jq -r 'select(.finding) | .finding | select(.trace[0].function != null) | .osv' "$out" | sort -u)
  if [ -z "$ids" ]; then
    echo "$name: находок нет"
    echo "| $name | нет | — |" >>"$summary"
    continue
  fi
  for id in $ids; do
    if grep -qxF -- "$id" <<<"$covered"; then
      echo "::notice title=govulncheck::$name: $id — заявлен в VEX ($vex), клиенту со сканером он не покажется при --vex"
      echo "| $name | $id | заявлен |" >>"$summary"
    else
      echo "::warning title=govulncheck::$name: $id — нет в VEX. Разобрать: исправить зависимость или заявить в $vex (https://pkg.go.dev/vuln/$id)"
      echo "| $name | $id | **нет** |" >>"$summary"
      uncovered_total=$((uncovered_total + 1))
    fi
  done
done

if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  cat "$summary" >>"$GITHUB_STEP_SUMMARY"
fi
cat "$summary"
echo "незаявленных находок: $uncovered_total"

#!/usr/bin/env bash
# Проверка тега выпуска до публикации (CLAB-435).
#
# release.yml идёт по push тега v* и ручным запуском (workflow_dispatch) с
# входом tag — на случай, когда GitHub принял push тега, а прогона не
# создал (так было с v0.25.0, CLAB-431): переставить тег не даёт ruleset
# tags-v-immutable, а перезапускать нечего. Ручной прогон запускается на ref
# самого тега:
#   gh workflow run release.yml --repo <owner/repo> --ref vX.Y.Z -f tag=vX.Y.Z
# Keyless-сертификат cosign берёт identity из <workflow>@<github.ref>: на ref
# тега она та же, что у push (…/release.yml@refs/tags/vX.Y.Z), а прогон на
# main подписал бы образ identity …@refs/heads/main, и подпись не прошла бы
# ни релиз-PR self-hosted, ни clab server, ни get-cadence.sh, ни upgrade.sh.
#
# Скрипт — первый job прогона, его ждут все job'ы с побочным эффектом. Он
# проверяет на любом событии, в том числе на push:
#   - прогон идёт на ref тега, а не ветки;
#   - вход tag (у ручного запуска) совпадает с ref прогона;
#   - имя тега — версия vX.Y.Z;
#   - тег есть на origin и он аннотированный (ref указывает на объект tag);
#   - тег указывает на коммит, и этот коммит — тот, что собирает прогон.
# Не прошло — код 1, ::error:: с причиной, и ничего не публикуется. Прошло —
# в GITHUB_OUTPUT уходят tag=<имя> и commit=<коммит тега>.
#
# Аннотированность читается через API, а не из checkout: actions/checkout на
# ref тега с fetch-depth 1 может не принести объект аннотации.
#
# Скрипт лежит одинаковым в .github/release/ обоих репозиториев, backend и
# frontend; правка — в обоих сразу.
#
# Окружение (в GitHub Actions всё, кроме TAG и GH_TOKEN, задано раннером):
#   TAG                вход tag ручного запуска; на push пуст
#   GITHUB_EVENT_NAME  push | workflow_dispatch
#   GITHUB_REF_TYPE    tag | branch
#   GITHUB_REF_NAME    имя ref прогона
#   GITHUB_SHA         коммит, который собирает прогон
#   GITHUB_REPOSITORY  owner/repo
#   GITHUB_OUTPUT      файл выходов шага
#   GH_TOKEN           токен для gh api (github.token прогона)
set -euo pipefail

: "${GITHUB_REPOSITORY:?нужен GITHUB_REPOSITORY — owner/repo}"
: "${GITHUB_SHA:?нужен GITHUB_SHA — коммит прогона}"
event="${GITHUB_EVENT_NAME:-}"
ref_type="${GITHUB_REF_TYPE:-}"
ref_name="${GITHUB_REF_NAME:-}"
input="${TAG:-}"
repo="$GITHUB_REPOSITORY"

fail() {
  echo "::error::$1 — выпуск не публикуется."
  shift
  for line in "$@"; do echo "$line"; done
  exit 1
}

hint_tag="${input:-vX.Y.Z}"
manual="gh workflow run release.yml --repo $repo --ref $hint_tag -f tag=$hint_tag"

if [ "$event" = workflow_dispatch ] && [ -z "$input" ]; then
  fail "ручной запуск без входа tag" "Запустите на ref тега: $manual"
fi

if [ "$ref_type" != tag ]; then
  fail "прогон выпуска запущен на ${ref_type:-неизвестном ref} «${ref_name}», а не на теге" \
    "Подпись такого прогона получила бы identity на refs/heads/${ref_name}, а не на теге," \
    "и проверки клиента её не приняли бы. Запустите на ref тега: $manual"
fi

if [ -n "$input" ] && [ "$input" != "$ref_name" ]; then
  fail "вход tag «${input}» не совпадает с ref прогона «${ref_name}»" \
    "Запустите на ref того же тега: gh workflow run release.yml --repo $repo --ref $input -f tag=$input"
fi
tag="$ref_name"

# Имя проверяется до запросов: оно идёт в путь API.
if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
  fail "тег «${tag}» не похож на версию vX.Y.Z"
fi

if ! ref=$(gh api "repos/$repo/git/ref/tags/$tag" --jq '.object.type + " " + .object.sha' 2>&1); then
  if printf '%s' "$ref" | grep -qE 'HTTP 404|Not Found'; then
    fail "тега $tag нет на origin ($repo)"
  fi
  fail "не удалось прочитать тег $tag на origin ($repo)" "Ответ API:" "$(printf '%s\n' "$ref" | tail -n 3 | sed 's/^/  /')"
fi
read -r ref_obj_type ref_obj_sha <<<"$ref"

if [ "$ref_obj_type" != tag ]; then
  fail "тег $tag не аннотированный: ref указывает на объект ${ref_obj_type:-?} ${ref_obj_sha:-}, а не на объект tag" \
    "Выпуск ставится аннотированным тегом: git tag -a $tag -m $tag"
fi

if ! obj=$(gh api "repos/$repo/git/tags/$ref_obj_sha" --jq '.object.type + " " + .object.sha' 2>&1); then
  fail "не удалось прочитать объект тега $tag ($ref_obj_sha)" "Ответ API:" "$(printf '%s\n' "$obj" | tail -n 3 | sed 's/^/  /')"
fi
read -r obj_type commit <<<"$obj"

if [ "$obj_type" != commit ]; then
  fail "аннотированный тег $tag указывает на объект ${obj_type:-?} ${commit:-}, а не на коммит"
fi

if [ "$commit" != "$GITHUB_SHA" ]; then
  fail "тег $tag на origin указывает на коммит $commit, а прогон собирает $GITHUB_SHA"
fi

echo "тег $tag аннотированный, коммит $commit (событие ${event:-?})"
if [ -n "${GITHUB_OUTPUT:-}" ]; then
  {
    echo "tag=$tag"
    echo "commit=$commit"
  } >>"$GITHUB_OUTPUT"
fi

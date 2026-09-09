#!/bin/sh
#
# Установка clab на macOS и Linux:
#
#   curl -fsSL https://<адрес сервиса>/install.sh | sh
#
# Скрипт отдаёт сам сервис (GET /install.sh): адрес, с которого его взяли,
# и есть адрес для входа. Бинарники лежат в релизах приватного
# репозитория, поэтому нужен доступ к GitHub от участника организации:
# вошедший gh либо токен в GITHUB_TOKEN.
#
#   CLAB_VERSION=v0.1.0        конкретный релиз вместо последнего
#   CLAB_INSTALL_DIR=~/bin     куда класть; по умолчанию /usr/local/bin,
#                              если он доступен на запись, иначе ~/.local/bin
#
# Тем, у кого есть Homebrew, проще: brew install cadence-lab-tech/tap/clab.
set -eu

repo="cadence-lab-tech/sdlc-pipeline-backend"
host="__CLAB_HOST__"
version="${CLAB_VERSION:-}"

say() { printf '%s\n' "$*" >&2; }
die() { say "✗ $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "нужен $1"; }

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin | linux) ;;
  *) die "поддерживаются macOS и Linux; для Windows возьмите zip из релиза: https://github.com/$repo/releases" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "неизвестная архитектура: $(uname -m)" ;;
esac
need curl
need tar

# Доступ к GitHub: токен из окружения или от вошедшего gh.
token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
if [ -z "$token" ] && command -v gh >/dev/null 2>&1; then
  token=$(gh auth token 2>/dev/null || true)
fi
[ -n "$token" ] || die "нужен доступ к GitHub: gh auth login или GITHUB_TOKEN"

api() {
  curl -fsSL -H "Authorization: Bearer $token" -H "Accept: application/vnd.github+json" \
    "https://api.github.com/$1"
}

# Описание релиза: тег и ассеты. jq на машине не обязателен, поэтому JSON
# режется по фигурным скобкам на строки — у ассета в одной строке
# оказываются его url, id и name, а у вложенного uploader — свои поля.
if [ -n "$version" ]; then
  release=$(api "repos/$repo/releases/tags/$version") || die "нет релиза $version в $repo"
else
  release=$(api "repos/$repo/releases/latest") || die "не нашли релизов в $repo"
fi
flat=$(printf '%s' "$release" | tr -d '\n ' | sed 's/{/\
{/g')
tag=$(printf '%s\n' "$flat" | sed -n 's/.*"tag_name":"\([^"]*\)".*/\1/p' | head -1)
[ -n "$tag" ] || die "не разобрали ответ GitHub о релизе"

asset_id() {
  printf '%s\n' "$flat" | grep "\"name\":\"$1\"" | sed -n 's|.*/releases/assets/\([0-9]*\)".*|\1|p' | head -1
}
fetch() {
  curl -fsSL -H "Authorization: Bearer $token" -H "Accept: application/octet-stream" \
    -o "$2" "https://api.github.com/repos/$repo/releases/assets/$1"
}

name="clab_${tag}_${os}_${arch}"
archive="$name.tar.gz"
archive_id=$(asset_id "$archive")
sums_id=$(asset_id checksums.txt)
[ -n "$archive_id" ] || die "в релизе $tag нет $archive"
[ -n "$sums_id" ] || die "в релизе $tag нет checksums.txt"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "▸ качаем $archive"
fetch "$archive_id" "$tmp/$archive"
fetch "$sums_id" "$tmp/checksums.txt"

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
if command -v sha256sum >/dev/null 2>&1; then
  have=$(sha256sum "$tmp/$archive" | awk '{ print $1 }')
else
  have=$(shasum -a 256 "$tmp/$archive" | awk '{ print $1 }')
fi
[ -n "$want" ] && [ "$want" = "$have" ] || die "контрольная сумма $archive не сошлась"

tar -xzf "$tmp/$archive" -C "$tmp"

dir="${CLAB_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  if [ -w /usr/local/bin ]; then dir=/usr/local/bin; else dir="$HOME/.local/bin"; fi
fi
mkdir -p "$dir"
install -m 0755 "$tmp/$name/clab" "$tmp/$name/clab-runner" "$dir/"

say "✓ clab $tag → $dir"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "  каталога нет в PATH: export PATH=\"$dir:\$PATH\"" ;;
esac
case "$host" in
  __CLAB*) host="https://<адрес сервиса>" ;;
esac
say "  войти: clab auth login --host $host"

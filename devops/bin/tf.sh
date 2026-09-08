#!/usr/bin/env bash
#
# Terraform с подставленным токеном и из правильного каталога.
#
#   ./devops/bin/tf.sh plan
#   ./devops/bin/tf.sh apply
#
# Обёртка нужна не для удобства, а чтобы правила доступа различали чтение
# и запись: `tf.sh plan` разрешён без вопросов, `tf.sh apply` — спрашивает
# (см. .claude/settings.json).
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"

: "${TWC_TOKEN:?не задан TWC_TOKEN (devops/.env — см. .env.example)}"

cd "$devops_dir/terraform"

# `plan -detailed-exitcode` возвращает 2, когда изменения есть. Это не ошибка,
# и заворачивать её в провал скрипта нельзя.
case "${1:-}" in
  plan) shift; exec terraform plan -detailed-exitcode "$@" ;;
  *)    exec terraform "$@" ;;
esac

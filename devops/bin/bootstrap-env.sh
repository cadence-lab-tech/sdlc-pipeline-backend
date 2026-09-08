#!/usr/bin/env bash
#
# Собирает /opt/sdlc/.env на машине из выводов Terraform и свежих секретов —
# один раз, до первой выкладки. Значения секретов не проходят через экран:
# ключи S3 читаются из state напрямую в файл, пароль базы и токен
# администратора генерируются на самой машине.
#
#   ./devops/bin/bootstrap-env.sh
#
# Если .env на машине уже есть, скрипт останавливается: переписать его —
# значит сменить пароль базы под живым томом. Дальше правки только руками.
source "$(dirname "${BASH_SOURCE[0]}")/_common.sh"

require_host
: "${TWC_TOKEN:?не задан TWC_TOKEN (devops/.env — см. .env.example)}"

tf() { (cd "$devops_dir/terraform" && terraform output -raw "$1" 2>/dev/null) || true; }

domain="$(tf domain)"
[ -n "$domain" ] || die "terraform не отдал domain: сначала ./devops/bin/tf.sh apply"

s3_bucket="$(tf s3_bucket)"
s3_endpoint="$(tf s3_endpoint)"
s3_region="$(tf s3_region)"
s3_access="$(tf s3_access_key)"
s3_secret="$(tf s3_secret_key)"

# Апостроф в «Let's» внутри ${:?…} bash 3.2 на macOS считает началом строки —
# формулировка без него.
acme_email="${ACME_EMAIL:?задайте ACME_EMAIL: сюда напишет центр сертификации, если продление начнёт падать}"

if ssh "${ssh_opts[@]}" "$ssh_target" "test -f '$SDLC_REMOTE_DIR/.env'"; then
  die "на машине уже есть $SDLC_REMOTE_DIR/.env — переписывать его этот скрипт не будет"
fi

# Шаблон уходит на машину целиком; секреты дописываются там же, из её
# /dev/urandom, и на ноутбуке не появляются вовсе. Два вызова ssh, а не один:
# у первого stdin занят шаблоном, второй получает скрипт через stdin —
# героическое экранирование в одной команде читать невозможно.
sed \
  -e "s|^SDLC_DOMAIN=.*|SDLC_DOMAIN=$domain|" \
  -e "s|^ACME_EMAIL=.*|ACME_EMAIL=$acme_email|" \
  -e "s|^SDLC_BLOB_S3_BUCKET=.*|SDLC_BLOB_S3_BUCKET=$s3_bucket|" \
  -e "s|^SDLC_BLOB_S3_ENDPOINT=.*|SDLC_BLOB_S3_ENDPOINT=$s3_endpoint|" \
  -e "s|^SDLC_BLOB_S3_REGION=.*|SDLC_BLOB_S3_REGION=$s3_region|" \
  -e "s|^SDLC_BLOB_S3_ACCESS_KEY_ID=.*|SDLC_BLOB_S3_ACCESS_KEY_ID=$s3_access|" \
  -e "s|^SDLC_BLOB_S3_SECRET_ACCESS_KEY=.*|SDLC_BLOB_S3_SECRET_ACCESS_KEY=$s3_secret|" \
  "$devops_dir/../deploy/.env.example" |
  ssh "${ssh_opts[@]}" "$ssh_target" "umask 077 && cat > '$SDLC_REMOTE_DIR/.env'"

# Разделитель в кавычках: ничего из $(...) ниже не раскрывается локально.
ssh "${ssh_opts[@]}" "$ssh_target" "bash -s '$SDLC_REMOTE_DIR'" <<'REMOTE'
set -e
cd "$1"
sed -i "s|^POSTGRES_PASSWORD=.*|POSTGRES_PASSWORD=$(openssl rand -hex 24)|" .env
sed -i "s|^SDLC_ADMIN_TOKEN=.*|SDLC_ADMIN_TOKEN=sdlc_$(openssl rand -hex 24)|" .env
sed -i "s|^SDLC_GITHUB_WEBHOOK_SECRET=.*|SDLC_GITHUB_WEBHOOK_SECRET=$(openssl rand -hex 24)|" .env
chmod 600 .env
REMOTE

echo "✓ $ssh_target:$SDLC_REMOTE_DIR/.env собран"
echo "  токен администратора: ./devops/bin/remote-run.sh 'grep ^SDLC_ADMIN_TOKEN= .env'"
echo "  (он нужен для первого входа; в переписку с агентом его не выносить)"

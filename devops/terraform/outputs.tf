output "server_id" {
  description = "ID машины: twc-get.sh servers/<id>."
  value       = twc_server.main.id
}

output "server_ip" {
  description = "Публичный IPv4. Он же — A-запись домена и SSH_HOST в секретах GitHub."
  # Не twc_server.main.main_ipv4: машине Timeweb выдаёт только IPv6, IPv4
  # приезжает плавающим адресом (см. main.tf).
  value = twc_floating_ip.main.ip
}

output "server_status" {
  value = twc_server.main.status
}

output "domain" {
  description = "Адрес системы: интерфейс, /v1, /mcp, /webhooks."
  value       = var.domain
}

output "ssh_known_hosts_hint" {
  description = "Чем заполнить секрет SSH_KNOWN_HOSTS в репозиториях."
  value       = "ssh-keyscan ${twc_floating_ip.main.ip}"
}

# Учётные данные бакета. `terraform output -raw <имя>` печатает их по одному,
# в обычном выводе плана и apply они скрыты. Их собирает bootstrap-env.sh.
output "s3_endpoint" {
  description = "SDLC_BLOB_S3_ENDPOINT."
  # hostname уже приходит со схемой (https://s3.twcstorage.ru).
  value = var.manage_s3 ? twc_s3_bucket.blobs[0].hostname : null
}

output "s3_bucket" {
  description = "SDLC_BLOB_S3_BUCKET. Полное имя — с префиксом, который выдаёт Timeweb."
  value       = var.manage_s3 ? twc_s3_bucket.blobs[0].full_name : null
}

output "s3_region" {
  description = "SDLC_BLOB_S3_REGION."
  value       = var.manage_s3 ? twc_s3_bucket.blobs[0].location : null
}

output "s3_access_key" {
  value     = var.manage_s3 ? twc_s3_bucket.blobs[0].access_key : null
  sensitive = true
}

output "s3_secret_key" {
  value     = var.manage_s3 ? twc_s3_bucket.blobs[0].secret_key : null
  sensitive = true
}

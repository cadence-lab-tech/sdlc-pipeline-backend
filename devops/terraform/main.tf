/*
 * Инфраструктура sdlc-pipeline в Timeweb Cloud: одна машина, плавающий IPv4,
 * файрвол, A-запись и бакет под содержимое артефактов.
 *
 * Чего здесь нет и быть не может: содержимого /opt/sdlc/.env. Секреты
 * приложения кладёт на машину devops/bin/bootstrap-env.sh один раз;
 * Terraform их не знает — иначе они окажутся в state в открытом виде.
 */

# Ресурсы заводятся в общем проекте аккаунта: отдельный проект в панели —
# ручной перенос, Terraform его не описывает (у файрвола и плавающего IP
# project_id нет вовсе, и половина ресурсов всё равно осталась бы в общем).

data "twc_os" "main" {
  name    = var.os_name
  version = var.os_version
}

resource "twc_ssh_key" "deploy" {
  name = "${var.project_name}-deploy"
  body = var.ssh_public_key
}

resource "twc_server" "main" {
  name     = var.project_name
  hostname = var.project_name
  comment  = "Managed by Terraform (backend/devops/terraform)"

  os_id     = data.twc_os.main.id
  preset_id = var.server_preset_id

  availability_zone = var.availability_zone

  # Пароль root не выпускаем: вход только по ключу. Провайдер разрешает это
  # ровно потому, что ssh_keys_ids заданы.
  ssh_keys_ids              = [twc_ssh_key.deploy.id]
  is_root_password_required = false
  floating_ip_id            = var.attach_floating_ip ? twc_floating_ip.main.id : null

  # Ключ подставляется в шаблон: cloud-init заводит пользователя deploy, под
  # которым ходит выкладка. Root по ключу Timeweb настраивает сам.
  cloud_init = templatefile("${path.module}/../cloud-init.yaml", {
    ssh_public_key = var.ssh_public_key
  })

  lifecycle {
    /*
     * Машина держит базу на локальном томе: задачи, артефакты, аудит и токены
     * коллег. Пересоздание = потеря всего этого, и Terraform делает его молча,
     * если тронуть create-only аргумент (availability_zone, is_ddos_guard,
     * local_network) или сменить ОС. Снимать флаг только осознанно и с
     * восстановлением из дампа в плане работ.
     */
    prevent_destroy = true

    # cloud-init отрабатывает один раз при создании. Правка файла не должна
    # приводить к пересозданию машины.
    ignore_changes = [cloud_init]
  }
}

# --- публичный IPv4 ----------------------------------------------------------
# Машина в Timeweb поднимается только с IPv6: у twc_server нет аргумента,
# который просил бы IPv4. Без него не работает ничего — GitHub Actions ходит
# по SSH с IPv4, A-запись некуда указывать, Let's Encrypt не проходит http-01.
# Плавающий, а не дополнительный IP сервера: он переживает пересоздание
# машины, и A-запись с SSH_KNOWN_HOSTS остаются прежними.
resource "twc_floating_ip" "main" {
  availability_zone = var.availability_zone
  comment           = "${var.project_name} public IPv4 (Managed by Terraform)"
}

# --- файрвол ----------------------------------------------------------------
# Политика по умолчанию — DROP, её задаёт сама Timeweb. Открываем три порта;
# 5432 наружу не открываем никогда.

resource "twc_firewall" "main" {
  name        = "${var.project_name}-web"
  description = "SSH + HTTP + HTTPS. Managed by Terraform"

  link {
    id   = twc_server.main.id
    type = "server"
  }
}

resource "twc_firewall_rule" "ssh" {
  for_each = toset(var.ssh_allowed_cidrs)

  firewall_id = twc_firewall.main.id
  direction   = "ingress"
  protocol    = "tcp"
  port        = "22"
  cidr        = each.value
  description = "SSH"
}

resource "twc_firewall_rule" "http" {
  firewall_id = twc_firewall.main.id
  direction   = "ingress"
  protocol    = "tcp"
  port        = "80"
  cidr        = "0.0.0.0/0"
  # 80-й нужен не только для редиректа: по нему Let's Encrypt проверяет
  # владение доменом (HTTP-01). Закрыть его — остаться без продления через
  # три месяца, а не сразу.
  description = "HTTP + ACME http-01"
}

resource "twc_firewall_rule" "https" {
  firewall_id = twc_firewall.main.id
  direction   = "ingress"
  protocol    = "tcp"
  port        = "443"
  cidr        = "0.0.0.0/0"
  description = "HTTPS"
}

resource "twc_firewall_rule" "egress" {
  firewall_id = twc_firewall.main.id
  direction   = "egress"
  protocol    = "tcp"
  port        = "1-65535"
  cidr        = "0.0.0.0/0"
  description = "Исходящие: GHCR, Let's Encrypt, GitHub API, Voyage, S3"
}

# --- DNS --------------------------------------------------------------------

data "twc_dns_zone" "main" {
  count = var.manage_dns ? 1 : 0
  name  = var.domain_zone
}

resource "twc_dns_rr" "main" {
  count = var.manage_dns ? 1 : 0

  zone_id = data.twc_dns_zone.main[0].id
  # Метка относительно зоны: sdlc.example.ru → sdlc, апекс → @.
  name  = var.domain == var.domain_zone ? "@" : trimsuffix(var.domain, ".${var.domain_zone}")
  type  = "A"
  value = twc_floating_ip.main.ip
}

# --- объектное хранилище под содержимое артефактов ---------------------------

resource "twc_s3_bucket" "blobs" {
  count = var.manage_s3 ? 1 : 0

  name        = "${var.project_name}-blobs"
  description = "Содержимое артефактов и документов (SDLC_BLOB_S3_*). Managed by Terraform"
  # Приватный: содержимое читает и пишет только сервис своими ключами,
  # наружу ссылки не уходят — интерфейс получает содержимое через API.
  type      = "private"
  preset_id = var.s3_preset_id
}

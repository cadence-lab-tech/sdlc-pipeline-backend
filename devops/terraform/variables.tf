variable "project_name" {
  description = "Префикс в именах ресурсов в панели Timeweb."
  type        = string
  default     = "sdlc-pipeline"
}

# --- размещение -------------------------------------------------------------

variable "location" {
  description = "Регион ЦОДа. S3 у Timeweb есть только в ru-1: пресеты хранилища привязаны к нему."
  type        = string
  default     = "ru-1"

  validation {
    condition     = contains(["ru-1", "ru-2", "ru-3", "de-1", "kz-1", "nl-1"], var.location)
    error_message = "Допустимые локации: ru-1, ru-2, ru-3, de-1, kz-1, nl-1."
  }
}

variable "availability_zone" {
  description = <<-EOT
    Зона доступности внутри региона. Обязана соответствовать location:
    ru-1→spb-3, ru-2→nsk-1, ru-3→msk-1, nl-1→ams-1, de-1→fra-1, kz-1→ala-1.
    Менять нельзя — провайдер пересоздаст сервер вместе с базой.
  EOT
  type        = string
  default     = "spb-3"
}

# --- сервер -----------------------------------------------------------------

variable "server_preset_id" {
  description = <<-EOT
    Тариф машины. Пресет, а не конфигуратор: за те же деньги даёт больше диска,
    и цена видна заранее. Список с ценами — `./devops/bin/twc-get.sh
    presets/servers`. По умолчанию Cloud-50 в ru-1: 2 vCPU, 4 ГБ, 50 ГБ NVMe.
    Линейка SSD (SSD-50, дешевле) на момент создания отвечала «No free node».
    Postgres с pgvector, сервис, воркер и два прокси — четыре гигабайта не
    запас, а рабочий минимум: индексация репозиториев держит дельты в памяти.
  EOT
  type        = number
  default     = 2453
}

variable "os_name" {
  type    = string
  default = "ubuntu"
}

variable "os_version" {
  type    = string
  default = "24.04"
}

variable "ssh_public_key" {
  description = <<-EOT
    Публичный ключ, которым в машину ходят и человек, и GitHub Actions.
    Приватная половина — в секрете SSH_KEY обоих репозиториев (бэкенд и
    интерфейс выкладываются каждый своим workflow).
  EOT
  type        = string
}

variable "ssh_allowed_cidrs" {
  description = <<-EOT
    Откуда пускаем на 22-й порт. По умолчанию отовсюду: раннеры GitHub Actions
    ходят с плавающих адресов, пригодного для файрвола списка диапазонов нет.
  EOT
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

# --- домен ------------------------------------------------------------------

variable "domain" {
  description = <<-EOT
    Публичный адрес системы: по нему Caddy выписывает сертификат, сюда ходят
    интерфейс, MCP локальных агентов, CLI и вебхуки GitHub. Один домен на всё —
    интерфейс и API живут на одном origin, второго списка адресов нет.
  EOT
  type        = string
}

variable "manage_dns" {
  description = <<-EOT
    Заводить ли A-запись домена отсюда. Требует, чтобы зона обслуживалась
    в Timeweb: data-источник twc_dns_zone ищет её на аккаунте и падает, если
    домен делегирован на другие NS. Технический домен twc1.net серверам,
    созданным через API, не выдаётся — без своей зоны запись ставится руками
    у регистратора.
  EOT
  type        = bool
  default     = false
}

variable "domain_zone" {
  description = "Корневая зона, в которой лежит domain, например example.ru."
  type        = string
  default     = ""
}

# --- объектное хранилище ----------------------------------------------------

variable "manage_s3" {
  description = <<-EOT
    Заводить ли бакет под вынесенное содержимое артефактов и документов
    (SDLC_BLOB_S3_*). С ним машина держит только базу, а реплики api (решение
    0028) не требуют общего тома. Ключи уходят в outputs как sensitive.
  EOT
  type        = bool
  default     = true
}

variable "s3_preset_id" {
  description = <<-EOT
    Пресет хранилища — единственный способ сайзинга S3 у Timeweb. Список —
    `./devops/bin/twc-get.sh presets/storages`. Пресеты за 1 ₽ — промо, и он
    один на аккаунт: второй бакет на нём не создаётся («Promo storage already
    exists»). По умолчанию Standard 10GB в ru-1.
  EOT
  type        = number
  default     = 4621
}

variable "attach_floating_ip" {
  description = <<-EOT
    Аварийный рычаг, а не настройка. Провайдер не перечитывает floating_ip_id
    с сервера: если привязка отвалилась на стороне облака, Terraform говорит
    «No changes». Лечится в два шага:
      tf.sh apply -var attach_floating_ip=false && tf.sh apply
  EOT
  type        = bool
  default     = true
}

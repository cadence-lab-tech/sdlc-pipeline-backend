terraform {
  required_version = ">= 1.6"

  required_providers {
    twc = {
      # Реестр Timeweb, а не registry.terraform.io: так рекомендует их
      # документация, и это единственный источник, доступный из России без
      # обходных путей. Зеркало в общем реестре — timeweb-cloud/timeweb-cloud.
      source  = "tf.timeweb.cloud/timeweb-cloud/timeweb-cloud"
      version = "~> 1.8"
    }
  }
}

/*
 * Токен провайдер берёт из TWC_TOKEN — в конфиг он не попадает никогда.
 * У токена должно быть ВЫКЛЮЧЕНО подтверждение удаления серверов через
 * Telegram, иначе `terraform destroy` виснет в ожидании ручного нажатия.
 */
provider "twc" {}

// Package migrations отдаёт SQL-файлы схемы и сидов встроенными в бинарник.
//
// Встраивание нужно, чтобы образ был самодостаточным: контейнер поднимает
// схему сам, без монтирования каталога с миграциями и без отдельного шага в
// docker-compose, который пришлось бы держать в согласии с версией сервиса.
package migrations

import "embed"

//go:embed *.sql
var Schema embed.FS

//go:embed seed/*.sql
var Seeds embed.FS

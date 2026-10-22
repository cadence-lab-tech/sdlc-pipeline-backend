package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Переезд встроенной базы на новую мажорную версию PostgreSQL (CLAB-237).
// Данные переносит сам compose новой версии — одноразовые сервисы
// pg-upgrade-dump и pg-upgrade-load перед postgres (postgres/major-upgrade.sh
// пакета): так переезд проходит при любом апгрейде, в том числе старым
// bash-скриптом v0.13.0. clab server upgrade только предупреждает о нём и до
// бэкапа отказывает, если места на диске томов заведомо не хватит.

// pgMajorRe — мажорная версия в теге образа pgvector: pg17, pg18-trixie.
var pgMajorRe = regexp.MustCompile(`:pg(\d+)(?:-[a-z0-9]+)?(?:@sha256:[0-9a-f]+)?$`)

// pgMajor — мажорная версия PostgreSQL образа; 0 — не узнать по тегу.
func pgMajor(image string) int {
	m := pgMajorRe.FindStringSubmatch(image)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// serviceImage — образ сервиса из `docker compose config --format json`.
func serviceImage(raw, service string) string {
	var cfg struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return ""
	}
	return cfg.Services[service].Image
}

// upgradeImageOf — образ SERVICE новой версии так, как его запустит compose
// после апгрейда: те же файлы, что у upgradeImages.
func (i *installation) upgradeImageOf(ctx context.Context, newDir, target, service string) string {
	c := i.upgradeImages(newDir, target)
	// upgradeImages кончается на config --images.
	c.args = append(c.args[:len(c.args)-2], "config", "--format", "json", service)
	out, err := output(ctx, i.e.run, c)
	if err != nil {
		return ""
	}
	return serviceImage(out, service)
}

// checkPostgresMove — встроенная база переедет на новую мажорную версию:
// сказать об этом до бэкапа и отказать, если свободного места на диске
// томов меньше размера базы — переезду нужен новый кластер такого размера
// рядом со старым томом. Не узнать размер или место — предупреждение, не
// отказ: переезд сам остановится без места, и апгрейд напечатает откат.
func (i *installation) checkPostgresMove(ctx context.Context, newDir, target string) error {
	e := i.e
	oldImage, err := i.imageOf(ctx, "postgres")
	if err != nil {
		return nil
	}
	newImage := i.upgradeImageOf(ctx, newDir, target, "postgres")
	from, to := pgMajor(oldImage), pgMajor(newImage)
	if from == 0 || to <= from {
		return nil
	}
	size, sizeErr := i.postgresSize(ctx)
	free, freeErr := i.postgresFree(ctx)
	what := "the database"
	if sizeErr == nil {
		what = "the database (" + humanSize(size) + ")"
	}
	e.warn("the upgrade to %s moves %s from PostgreSQL %d to PostgreSQL %d: while it is dumped and loaded into a new cluster the stack is down, "+
		"and the volume pgdata stays as it is, for a rollback (docs/admin.md, \"PostgreSQL 18\")", target, what, from, to)
	if sizeErr != nil || freeErr != nil {
		e.warn("could not tell the size of the database or the free disk of the volumes: check them before the move (docs/admin.md, \"PostgreSQL 18\")")
		return nil
	}
	if free < size {
		return fmt.Errorf("the move to PostgreSQL %d needs about %s free on the disk of the Docker volumes - the size of the database - and it has %s.\n"+
			"  Free disk space and run the upgrade again (docs/admin.md, \"PostgreSQL 18\"). %s", to, humanSize(size), humanSize(free), nothingChanged)
	}
	e.info("the disk of the volumes has %s free for the move", humanSize(free))
	return nil
}

// postgresSize — размер всех баз кластера в байтах.
func (i *installation) postgresSize(ctx context.Context) (int64, error) {
	out, err := output(ctx, i.e.run, i.compose("exec", "-T", "postgres", "sh", "-c",
		`psql -X -At -U "$POSTGRES_USER" -d postgres -c "SELECT sum(pg_database_size(oid)) FROM pg_database"`))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

// postgresFree — свободное место файловой системы тома базы, в байтах:
// новые тома Docker кладёт рядом.
func (i *installation) postgresFree(ctx context.Context) (int64, error) {
	out, err := output(ctx, i.e.run, i.compose("exec", "-T", "postgres", "df", "-Pk", "/var/lib/postgresql/data"))
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("df: %q", out)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, fmt.Errorf("df: %q", out)
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	return kb * 1024, err
}

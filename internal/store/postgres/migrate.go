package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/migrations"
)

// Реплики сервиса стартуют одновременно, миграции должен катить кто-то один.
// Число произвольное, важно лишь чтобы оно не совпало с другими блокировками.
const migrationAdvisoryLock int64 = 8317441

var migrationFileRe = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.up\.sql$`)

type migration struct {
	version  int64
	name     string
	body     string
	checksum string
}

// Migrate накатывает недостающие миграции по порядку версий.
//
// Уже применённые сверяются по контрольной сумме: правка применённой миграции
// запрещена конвенцией репозитория, и здесь это перестаёт быть договорённостью
// — сервис не поднимется, если файл разошёлся с тем, что легло в базу.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	all, err := loadMigrations()
	if err != nil {
		return err
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("соединение под миграции: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationAdvisoryLock); err != nil {
		return fmt.Errorf("блокировка миграций: %w", err)
	}
	defer func() {
		// Отдельный контекст: отпустить блокировку надо даже при отмене.
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, migrationAdvisoryLock); err != nil {
			log.Error("не удалось снять блокировку миграций", "error", err)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    bigint PRIMARY KEY,
			name       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("создание schema_migrations: %w", err)
	}

	applied := map[int64]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("чтение schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int64
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return err
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	pending := 0
	for _, m := range all {
		if sum, ok := applied[m.version]; ok {
			if sum != m.checksum {
				return fmt.Errorf(
					"миграция %06d_%s уже применена, но файл изменился (в базе %s, на диске %s); "+
						"применённые миграции не редактируются — нужна новая миграция",
					m.version, m.name, sum[:12], m.checksum[:12])
			}
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("начало транзакции миграции %06d: %w", m.version, err)
		}
		if _, err := tx.Exec(ctx, m.body); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("миграция %06d_%s: %w", m.version, m.name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.version, m.name, m.checksum); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("отметка миграции %06d: %w", m.version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("коммит миграции %06d: %w", m.version, err)
		}

		log.Info("миграция применена", "version", m.version, "name", m.name)
		pending++
	}

	if pending == 0 {
		log.Info("схема актуальна", "applied", len(applied))
	}
	return nil
}

// ApplySeeds разворачивает демонстрационные данные — но только на пустой базе.
// Сиды пишут фиксированные ключи проекта и задач и не идемпотентны, поэтому
// повторный прогон обязан быть не «обновить», а «ничего не делать».
func ApplySeeds(ctx context.Context, pool *pgxpool.Pool, names []string, log *slog.Logger) error {
	if len(names) == 0 {
		return nil
	}

	var projects int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM projects`).Scan(&projects); err != nil {
		return fmt.Errorf("проверка пустоты базы: %w", err)
	}
	if projects > 0 {
		log.Info("сиды пропущены: в базе уже есть проекты", "projects", projects)
		return nil
	}

	for _, name := range names {
		body, err := migrations.Seeds.ReadFile(path.Join("seed", name+".sql"))
		if err != nil {
			return fmt.Errorf("сид %q не найден: %w", name, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("применение сида %q: %w", name, err)
		}
		log.Info("сид применён", "seed", name)
	}
	return nil
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.Schema, ".")
	if err != nil {
		return nil, err
	}

	var out []migration
	for _, e := range entries {
		m := migrationFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("номер миграции %q: %w", e.Name(), err)
		}
		body, err := migrations.Schema.ReadFile(e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(normalize(body))
		out = append(out, migration{
			version:  version,
			name:     m[2],
			body:     string(body),
			checksum: hex.EncodeToString(sum[:]),
		})
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("не найдено ни одной миграции — сломан embed в migrations/embed.go")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// normalize приводит переводы строк к \n: иначе checkout на Windows менял бы
// контрольную сумму и сервис отказывался стартовать на неизменённом файле.
func normalize(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "\r\n", "\n"))
}

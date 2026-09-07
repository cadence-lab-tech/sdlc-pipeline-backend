// Package testdb поднимает выделенную базу для интеграционных тестов.
//
// Часть свойств системы не проверяется на моках: инкрементальность прогона
// индексации — это свойство связки «источник плюс база», гибридный поиск
// живёт в SQL, а гейты читают то, что положил воркер. Такие проверки идут по
// настоящей схеме, накатанной теми же миграциями, что и в продакшене.
package testdb

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

// EnvURL — адрес базы для тестов. Выделенной: Store сносит схему целиком.
const EnvURL = "SDLC_TEST_DATABASE_URL"

// Store отдаёт чистую базу со свежей схемой и демонстрационным проектом.
//
// Без переменной окружения тест пропускается: интеграционные проверки не
// должны валить сборку там, где базы нет — в CI без сервисов, например.
func Store(t *testing.T, seeds ...string) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()

	url := os.Getenv(EnvURL)
	if url == "" {
		t.Skip(EnvURL + " не задан — интеграционный тест пропущен")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("подключение к тестовой базе: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		pool.Close()
		t.Fatalf("очистка тестовой базы: %v", err)
	}

	log := Logger()
	if err := postgres.Migrate(ctx, pool, log); err != nil {
		pool.Close()
		t.Fatalf("миграции: %v", err)
	}
	if len(seeds) == 0 {
		seeds = []string{"default_workflow"}
	}
	if err := postgres.ApplySeeds(ctx, pool, seeds, log); err != nil {
		pool.Close()
		t.Fatalf("сид: %v", err)
	}

	t.Cleanup(pool.Close)
	return postgres.New(pool), pool
}

// Logger — тихий логгер: в выводе теста нужны его собственные сообщения, а не
// ход прогона.
func Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

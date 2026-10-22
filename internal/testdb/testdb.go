// Package testdb поднимает выделенную базу для интеграционных тестов.
//
// Часть свойств системы не проверяется на моках: инкрементальность прогона
// индексации — это свойство связки «источник плюс база», гибридный поиск
// живёт в SQL, а гейты читают то, что положил воркер. Такие проверки идут по
// настоящей схеме, накатанной теми же миграциями, что и в продакшене.
//
// База на пакет (CLAB-250). SDLC_TEST_DATABASE_URL — база-вход, а не база
// тестов: каждый тестовый процесс (пакет) заводит рядом с ней на том же
// сервере свою базу и работает только в ней. Store сносит схему целиком, и
// с одной базой на всех параллельные пакеты `go test ./...` сносили бы её
// друг у друга посреди теста. Имя базы пакета — имя базы-входа плюс хеш
// каталога пакета: стабильно от прогона к прогону в одной рабочей копии и
// разное у двух копий на одном сервере. Убирает базы после прогона
// `go run ./internal/testdb/cleanup` (его зовёт make test-integration).
package testdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// EnvURL — адрес базы-входа для тестов. Сама она остаётся пустой: тесты
// заводят рядом свои базы пакетов (PackageDB), роли нужно право CREATEDB.
const EnvURL = "SDLC_TEST_DATABASE_URL"

// hashLen — сколько шестнадцатеричных знаков хеша каталога идёт в имя.
const hashLen = 12

// maxIdent — предел длины идентификатора Postgres (NAMEDATALEN-1), в байтах.
const maxIdent = 63

// Store отдаёт чистую базу со свежей схемой и демонстрационным проектом.
//
// Без переменной окружения тест пропускается: интеграционные проверки не
// должны валить сборку там, где базы нет — в CI без сервисов, например.
func Store(t *testing.T, seeds ...string) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()

	dbURL := URL(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
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

// URL — адрес базы этого пакета: та же база, что готовит Store. Нужен
// тестам, которые подключаются к подготовленной базе сами (предпроверка
// базы). Без переменной окружения тест пропускается, как в Store; база
// пакета заводится при первом вызове в процессе.
func URL(t *testing.T) string {
	t.Helper()
	entry := os.Getenv(EnvURL)
	if entry == "" {
		t.Skip(EnvURL + " не задан — интеграционный тест пропущен")
	}
	pkg.once.Do(func() { pkg.url, pkg.err = preparePackageDB(entry) })
	if pkg.err != nil {
		t.Fatalf("база пакета: %v", pkg.err)
	}
	return pkg.url
}

// pkg — база пакета, одна на тестовый процесс. hold — соединение, которое
// держит её занятой до конца процесса: уборка (cleanup, dev/stack.sh
// test-db) сносит базы без FORCE и занятую не тронет, даже если параллельный
// прогон попал на миг между тестами, когда пулов Store нет.
var pkg struct {
	once sync.Once
	url  string
	err  error
	hold *pgx.Conn
}

func preparePackageDB(entry string) (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("каталог пакета: %w", err)
	}
	name, dbURL, err := PackageDB(entry, dir)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := createDatabase(ctx, entry, name); err != nil {
		return "", err
	}
	hold, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return "", fmt.Errorf("подключение к базе пакета %s: %w", name, err)
	}
	pkg.hold = hold
	return dbURL, nil
}

// createDatabase заводит базу пакета через соединение с базой-входом.
// Параллельные CREATE DATABASE разводит короткий advisory-лок в базе-входе
// (сессионный: CREATE DATABASE не идёт внутри транзакции), а «уже есть» —
// не ошибка: база осталась от прошлого прогона этой копии.
func createDatabase(ctx context.Context, entry, name string) error {
	conn, err := pgx.Connect(ctx, entry)
	if err != nil {
		return fmt.Errorf("подключение к базе-входу %s: %w", EnvURL, err)
	}
	defer conn.Close(context.Background())

	const lockKey = 0x434C4142_0250 // «CLAB» и номер задачи: свой ключ, не чужой
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(lockKey)); err != nil {
		return fmt.Errorf("лок на заведение базы пакета: %w", err)
	}
	var exists bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		return fmt.Errorf("поиск базы пакета %s: %w", name, err)
	}
	if exists {
		return nil
	}
	_, err = conn.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize())
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pgErr) && pgErr.Code == "42P04": // duplicate_database
		return nil
	case errors.As(err, &pgErr) && pgErr.Code == "42501": // insufficient_privilege
		return fmt.Errorf("нет права завести базу пакета %s: роли из %s нужен CREATEDB (%s)", name, EnvURL, pgErr.Message)
	default:
		return fmt.Errorf("заведение базы пакета %s: %w", name, err)
	}
}

// PackageDB — имя и адрес базы пакета из каталога dir для базы-входа entry.
// Одно правило на заведение (Store) и уборку (cleanup): имя базы-входа,
// подчёркивание и хеш каталога, всего не длиннее 63 байт. Каталог берётся
// без символических ссылок, чтобы `go test` и `go list` дали одно имя.
func PackageDB(entry, dir string) (name, dbURL string, err error) {
	u, err := url.Parse(entry)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", "", fmt.Errorf("%s должен быть адресом postgres://…/<база>", EnvURL)
	}
	base := strings.TrimPrefix(u.Path, "/")
	if base == "" || strings.Contains(base, "/") {
		return "", "", fmt.Errorf("в %s нет имени базы-входа", EnvURL)
	}
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	sum := sha256.Sum256([]byte(filepath.Clean(dir)))
	suffix := "_" + hex.EncodeToString(sum[:])[:hashLen]
	if len(base) > maxIdent-len(suffix) {
		base = base[:maxIdent-len(suffix)]
	}
	name = base + suffix
	u.Path = "/" + name
	u.RawPath = ""
	return name, u.String(), nil
}

// Logger — тихий логгер: в выводе теста нужны его собственные сообщения, а не
// ход прогона.
func Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

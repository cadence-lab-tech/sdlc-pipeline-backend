// Команда cleanup сносит базы пакетов интеграционных тестов этой рабочей
// копии (CLAB-250). Её зовёт make test-integration после go test, с любым его
// исходом: в верстаке нет psql, поэтому уборка — на Go и по тому же адресу.
//
//	SDLC_TEST_DATABASE_URL=… go run ./internal/testdb/cleanup $(go list -f '{{.Dir}}' ./...)
//
// Имена вычисляются тем же правилом, что при заведении (testdb.PackageDB), по
// каталогам пакетов из аргументов: базы соседней копии на общем сервере
// другие и не трогаются. DROP DATABASE — без FORCE: базу, к которой подключён
// чужой прогон, Postgres не отдаёт, она остаётся с предупреждением, а код
// выхода от этого не меняется. Ненулевой код — только когда базу-вход не
// открыть или адрес не разобрать.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

func main() {
	if err := run(os.Getenv(testdb.EnvURL), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "уборка баз пакетов:", err)
		os.Exit(1)
	}
}

func run(entry string, dirs []string) error {
	if entry == "" {
		return nil // тесты шли без базы — убирать нечего
	}
	names := make([]string, 0, len(dirs))
	seen := map[string]bool{}
	for _, dir := range dirs {
		name, _, err := testdb.PackageDB(entry, dir)
		if err != nil {
			return err
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, entry)
	if err != nil {
		return fmt.Errorf("подключение к базе-входу: %w", err)
	}
	defer conn.Close(context.Background())

	// Только существующие: DROP DATABASE IF EXISTS по полусотне каталогов без
	// баз засорил бы вывод уведомлениями, а итог нужен короткий.
	rows, err := conn.Query(ctx, `SELECT datname FROM pg_database WHERE datname = ANY($1) ORDER BY datname`, names)
	if err != nil {
		return fmt.Errorf("поиск баз пакетов: %w", err)
	}
	existing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("поиск баз пакетов: %w", err)
	}

	var dropped, left int
	for _, name := range existing {
		_, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize())
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			dropped++
		case errors.As(err, &pgErr) && pgErr.Code == "55006": // object_in_use
			left++
			fmt.Fprintf(os.Stderr, "база пакета %s занята другим прогоном — оставлена\n", name)
		default:
			fmt.Fprintf(os.Stderr, "база пакета %s не снесена: %v\n", name, err)
			left++
		}
	}
	fmt.Printf("уборка баз пакетов: снесено %d, оставлено %d\n", dropped, left)
	return nil
}

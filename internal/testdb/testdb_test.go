package testdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Имя базы пакета — одно правило на заведение и уборку (CLAB-250): стабильно
// для каталога, разное у двух копий, в пределах 63 байт, адрес сохраняет
// параметры базы-входа.
func TestPackageDBName(t *testing.T) {
	const entry = "postgres://sdlc:sdlc@postgres:5432/sdlc_test?sslmode=disable"
	dir := t.TempDir()

	name, dbURL, err := PackageDB(entry, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "sdlc_test_") || len(name) != len("sdlc_test_")+hashLen {
		t.Fatalf("name = %q, want sdlc_test_<%d hex>", name, hashLen)
	}
	u, _ := url.Parse(dbURL)
	if u.Path != "/"+name || u.Query().Get("sslmode") != "disable" || u.Host != "postgres:5432" || u.User.Username() != "sdlc" {
		t.Fatalf("url = %s", dbURL)
	}

	again, _, _ := PackageDB(entry, dir)
	if again != name {
		t.Fatalf("the same directory gave %q, then %q", name, again)
	}
	other, _, _ := PackageDB(entry, t.TempDir())
	if other == name {
		t.Fatalf("two directories share the database %q", name)
	}

	long := strings.Repeat("b", 80)
	longName, _, err := PackageDB("postgres://u@h/"+long, dir)
	if err != nil || len(longName) > maxIdent || !strings.HasSuffix(longName, name[len("sdlc_test"):]) {
		t.Fatalf("long base: %q (%d bytes) %v", longName, len(longName), err)
	}

	for _, bad := range []string{"host=h dbname=x", "postgres://u@h", "postgres://u@h/", "mysql://u@h/x"} {
		if _, _, err := PackageDB(bad, dir); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

// Роль без CREATEDB: заведение базы пакета падает с понятной причиной, а не
// уходит в базу-вход.
func TestCreateDatabaseWithoutCreateDB(t *testing.T) {
	entry := os.Getenv(EnvURL)
	if entry == "" {
		t.Skip(EnvURL + " не задан — интеграционный тест пропущен")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, entry)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)

	role := "nocreatedb_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	password := "Pw-" + uuid.NewString()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+role+" LOGIN NOCREATEDB PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+role) })

	u, _ := url.Parse(entry)
	u.User = url.UserPassword(role, password)
	name := "sdlc_test_nocreatedb_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	err = createDatabase(ctx, u.String(), name)
	if err == nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize())
		t.Fatal("a role without CREATEDB created the package database")
	}
	if !strings.Contains(err.Error(), "CREATEDB") {
		t.Fatalf("the error does not name CREATEDB: %v", err)
	}
}

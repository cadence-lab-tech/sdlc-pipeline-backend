package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// Предпроверка базы (CLAB-127) на настоящем Postgres: схема Cadence, пустая
// база, чужие таблицы, схема новее образа, роль без прав, неверный пароль.

func checkDB(t *testing.T, rawURL string, install bool) postgres.CheckReport {
	t.Helper()
	return postgres.CheckDatabase(context.Background(), rawURL, postgres.CheckOptions{Install: install, Timeout: 10 * time.Second})
}

func item(r postgres.CheckReport, name string) postgres.CheckItem {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return postgres.CheckItem{}
}

// emptySchema — база без объектов приложения и без расширений: testdb.Store
// уже накатил схему, здесь её сносят.
func emptySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `DROP SCHEMA public CASCADE; CREATE SCHEMA public;
		DROP EXTENSION IF EXISTS vector; DROP EXTENSION IF EXISTS pgcrypto;`); err != nil {
		t.Fatal(err)
	}
}

// objectCount — число объектов базы, которые могла бы оставить проверка.
func objectCount(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var ext, rel int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM pg_extension),
		(SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public')`).Scan(&ext, &rel); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("extensions=%d relations=%d", ext, rel)
}

func TestDBCheckCadenceSchema(t *testing.T) {
	testdb.Store(t)
	u := testdb.URL(t)
	latest, _ := postgres.LatestMigration()

	r := checkDB(t, u, false)
	if !r.OK {
		t.Fatalf("a migrated database must pass: %+v", r.Checks)
	}
	if r.Schema != postgres.SchemaCadence || r.SchemaVersion != latest || r.ImageSchemaVersion != latest {
		t.Fatalf("schema = %s %d/%d, want cadence %d", r.Schema, r.SchemaVersion, r.ImageSchemaVersion, latest)
	}
	for _, name := range []string{"connection", "server", "vector", "pgcrypto", "privileges", "schema"} {
		if c := item(r, name); c.Status != postgres.CheckOK {
			t.Errorf("%s: %s %s", name, c.Status, c.Detail)
		}
	}
	// Тестовая база — без TLS: громкое предупреждение, но не отказ.
	if c := item(r, "tls"); c.Status != postgres.CheckWarn || !strings.Contains(c.Detail, "UNENCRYPTED") {
		t.Errorf("tls of sslmode=disable: %+v", c)
	}

	// Установка поверх базы со схемой Cadence — предупреждение (q6).
	r = checkDB(t, u, true)
	if !r.OK || item(r, "schema").Status != postgres.CheckWarn {
		t.Fatalf("install over a Cadence schema must warn, not fail: %+v", r.Checks)
	}
}

func TestDBCheckEmptyDatabaseOnlyReads(t *testing.T) {
	_, pool := testdb.Store(t)
	emptySchema(t, pool)
	before := objectCount(t, pool)

	r := checkDB(t, testdb.URL(t), true)
	if !r.OK || r.Schema != postgres.SchemaEmpty {
		t.Fatalf("an empty database must pass: %+v", r.Checks)
	}
	// Роль тестов — суперпользователь: расширения создаст сервис (критерий 6).
	if c := item(r, "vector"); c.Status != postgres.CheckOK || !strings.Contains(c.Detail, "creates it") {
		t.Errorf("vector: %+v", c)
	}
	if after := objectCount(t, pool); after != before {
		t.Fatalf("the check changed the database: %s -> %s", before, after)
	}
}

func TestDBCheckRefusals(t *testing.T) {
	_, pool := testdb.Store(t)
	ctx := context.Background()
	u := testdb.URL(t)

	// Схема новее образа.
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES (999999, 'future', 'x')`); err != nil {
		t.Fatal(err)
	}
	r := checkDB(t, u, false)
	if r.OK || r.Schema != postgres.SchemaNewer || !strings.Contains(item(r, "schema").Detail, "999999") {
		t.Fatalf("a newer schema must fail: %+v", r.Checks)
	}

	// Чужие таблицы без schema_migrations.
	emptySchema(t, pool)
	if _, err := pool.Exec(ctx, `CREATE TABLE public.orders (id int)`); err != nil {
		t.Fatal(err)
	}
	r = checkDB(t, u, true)
	if r.OK || r.Schema != postgres.SchemaForeign || !strings.Contains(item(r, "schema").Detail, "orders") {
		t.Fatalf("foreign tables must fail: %+v", r.Checks)
	}

	// Параметр, которого не понимает libpq, и адрес без sslmode.
	parsed, _ := url.Parse(u)
	q := parsed.Query()
	q.Set("pool_max_conns", "5")
	parsed.RawQuery = q.Encode()
	if r = checkDB(t, parsed.String(), false); r.OK || !strings.Contains(item(r, "url").Detail, "pool_max_conns") {
		t.Fatalf("pool_max_conns must fail naming it: %+v", r.Checks)
	}
	parsed, _ = url.Parse(u)
	q = parsed.Query()
	q.Del("sslmode")
	parsed.RawQuery = q.Encode()
	if r = checkDB(t, parsed.String(), false); r.OK || item(r, "tls").Status != postgres.CheckFail {
		t.Fatalf("a URL without sslmode must fail: %+v", r.Checks)
	}
}

func TestDBCheckRoleWithoutPrivileges(t *testing.T) {
	_, pool := testdb.Store(t)
	ctx := context.Background()
	emptySchema(t, pool)

	role := "dbcheck_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	password := "Pw-" + uuid.NewString()
	if _, err := pool.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, password)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP ROLE IF EXISTS "+role) })

	parsed, _ := url.Parse(testdb.URL(t))
	parsed.User = url.UserPassword(role, password)
	r := checkDB(t, parsed.String(), true)
	if r.OK {
		t.Fatalf("a role without privileges must fail: %+v", r.Checks)
	}
	vector := item(r, "vector")
	if vector.Status != postgres.CheckFail || !strings.Contains(vector.Hint, "CREATE EXTENSION IF NOT EXISTS vector;") {
		t.Errorf("vector must fail with the SQL for a superuser: %+v", vector)
	}
	if c := item(r, "privileges"); c.Status != postgres.CheckFail || !strings.Contains(c.Detail, "CREATE privilege on the schema public") {
		t.Errorf("privileges must name CREATE on public: %+v", c)
	}

	// Неверный пароль: причина — аутентификация, пароля в выводе нет.
	wrong := "Wrong-" + uuid.NewString()
	parsed.User = url.UserPassword(role, wrong)
	r = checkDB(t, parsed.String(), true)
	auth := item(r, "authentication")
	if r.OK || auth.Status != postgres.CheckFail {
		t.Fatalf("a wrong password must fail as authentication: %+v", r.Checks)
	}
	body, _ := json.Marshal(r)
	if strings.Contains(string(body), wrong) || strings.Contains(string(body), password) {
		t.Fatalf("the report repeats the password: %s", body)
	}
}

func TestDBCheckUnreachable(t *testing.T) {
	r := checkDB(t, "postgres://u:secret-pw@127.0.0.1:1/cadence?sslmode=disable&connect_timeout=2", false)
	if r.OK || item(r, "connection").Status != postgres.CheckFail {
		t.Fatalf("an unreachable host must fail the connection: %+v", r.Checks)
	}
	if body, _ := json.Marshal(r); strings.Contains(string(body), "secret-pw") {
		t.Fatalf("the report repeats the password: %s", body)
	}
}

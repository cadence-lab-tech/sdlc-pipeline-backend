package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Внешняя база (CLAB-127) в clab server: те же шаги, что у скриптов пакета,
// без Docker — подставной исполнитель. Номера — критерии приёмки CLAB-127.

const (
	extPassword = "Cad@nce/x%y"
	extURL      = "postgres://cadence:Cad%40nce%2Fx%25y@db.internal:5432/cadence?sslmode=verify-full&sslrootcert=/secrets/db-ca.pem"
	extSafeURL  = "postgres://cadence@db.internal:5432/cadence?sslmode=verify-full&sslrootcert=/secrets/db-ca.pem"
)

// externalInstalled — установка версии v на внешней базе.
func externalInstalled(t *testing.T, v string, extra map[string]string) string {
	t.Helper()
	if extra == nil {
		extra = map[string]string{}
	}
	if _, ok := extra[externalDBFile]; !ok {
		extra[externalDBFile] = "services: {}\n"
	}
	dir := installed(t, v, extra)
	mustWrite(t, filepath.Join(dir, ".env"), "CADENCE_MODE=proxy\n"+
		"COMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.external-db.yml:compose.override.yml\n"+
		"CADENCE_HTTP_ADDR=127.0.0.1:18190\nCADENCE_DB=external\n"+
		"SDLC_DATABASE_URL='"+extURL+"'\nPOSTGRES_PASSWORD=\nSDLC_ADMIN_TOKEN='sdlc_abc'\n", 0o600)
	return dir
}

func noPasswordIn(t *testing.T, r *fakeRunner, what ...string) {
	t.Helper()
	for _, c := range r.calls {
		if strings.Contains(c, extPassword) || strings.Contains(c, "Cad%40nce%2Fx%25y") {
			t.Errorf("a command line carries the password: %s", c)
		}
	}
	for _, s := range what {
		if strings.Contains(s, extPassword) || strings.Contains(s, "Cad%40nce") {
			t.Errorf("the output carries the password: %s", s)
		}
	}
}

func envHas(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestDBURLParts(t *testing.T) {
	safe, pw := dbURLParts(extURL)
	if safe != extSafeURL || pw != extPassword {
		t.Fatalf("dbURLParts = %q %q", safe, pw)
	}
	if safe, pw := dbURLParts("postgres://u@h/db"); safe != "postgres://u@h/db" || pw != "" {
		t.Errorf("no password: %q %q", safe, pw)
	}
	if got := dbTarget(extURL); got != "db.internal:5432/cadence" {
		t.Errorf("dbTarget = %q", got)
	}
}

func TestFilterTransactionTimeout(t *testing.T) {
	in := "SET statement_timeout = 0;\nSET transaction_timeout = 0;\nCOPY x FROM stdin;\n" + strings.Repeat("a", 100000) + "\n\\.\nSET transaction_timeout = 0;"
	out, err := io.ReadAll(filterTransactionTimeout(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	want := "SET statement_timeout = 0;\nCOPY x FROM stdin;\n" + strings.Repeat("a", 100000) + "\n\\.\n"
	if string(out) != want {
		t.Fatalf("filtered dump differs: %d bytes, want %d", len(out), len(want))
	}
}

func TestCheckDBMode(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"COMPOSE_FILE": "docker-compose.yml:compose.proxy.yml"}, ""},
		{map[string]string{"CADENCE_DB": "external", "SDLC_DATABASE_URL": "postgres://x@h/db", "COMPOSE_FILE": "docker-compose.yml:compose.proxy.yml:compose.external-db.yml"}, ""},
		{map[string]string{"CADENCE_DB": "external", "COMPOSE_FILE": "docker-compose.yml:compose.proxy.yml:compose.external-db.yml"}, "needs SDLC_DATABASE_URL"},
		{map[string]string{"CADENCE_DB": "external", "SDLC_DATABASE_URL": "postgres://x@h/db", "COMPOSE_FILE": "docker-compose.yml:compose.proxy.yml"}, "does not include compose.external-db.yml"},
		{map[string]string{"COMPOSE_FILE": "docker-compose.yml:compose.proxy.yml:compose.external-db.yml"}, "but CADENCE_DB is bundled"},
		{map[string]string{"CADENCE_DB": "cloud"}, "expected bundled or external"},
	}
	for _, c := range cases {
		e, _, _ := testEnv(t, t.TempDir(), newFakeRunner(), nil)
		err := (&installation{e: e, env: c.env, hasEnv: true}).checkDBMode()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%v: %v, want %q", c.env, err, c.want)
		}
	}
}

// 10: бэкап внешней базы клиентом бэкапа, без контейнера postgres, пароль —
// только в окружении; 11: база недоступна — ошибка и нет файла дампа.
func TestServerBackupExternal(t *testing.T) {
	dir := externalInstalled(t, "v0.7.0", nil)
	r := newFakeRunner()
	r.postgresRunning = false
	e, out, errw := testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	dump := filepath.Join(dir, "backups", "cadence-v0.7.0-20261018-030000.sql.gz")
	if fi, err := os.Stat(dump); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("dump: %v %v", fi, err)
	}
	call := "docker compose run --rm --no-deps -T -e PGPASSWORD db-client pg_dump -d " + extSafeURL + " " + strings.Join(dumpOptions, " ")
	if !r.called(call) {
		t.Fatalf("no pg_dump of the backup client: %v", r.calls)
	}
	if !envHas(r.envs[call], "PGPASSWORD="+extPassword) {
		t.Error("the backup client gets no PGPASSWORD")
	}
	if r.called("docker compose exec -T postgres") || r.called("docker compose ps -q --status running postgres") {
		t.Error("the external mode must not need the postgres container")
	}
	if !strings.Contains(out.String(), "dumping the external database db.internal:5432/cadence") {
		t.Errorf("output: %s", out)
	}
	noPasswordIn(t, r, out.String(), errw.String())

	r = newFakeRunner()
	r.dbClientFails = true
	e, _, errw = testEnv(t, dir, r, nil)
	e.now = func() time.Time { return time.Date(2026, 10, 18, 4, 0, 0, 0, time.UTC) }
	if code := runServer(context.Background(), e, []string{"backup"}); code != 1 || !strings.Contains(errw.String(), "pg_dump of the external database failed") {
		t.Fatalf("an unreachable database: code %d %s", code, errw)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "backups", "cadence-v0.7.0-20261018-04*"))
	if len(left) > 0 {
		t.Errorf("a failed backup left %v", left)
	}
}

// 12: восстановление во внешнюю базу — проверка связи до остановки, одна
// транзакция с очисткой схемы, без DROP DATABASE и контейнера postgres.
func TestServerRestoreExternal(t *testing.T) {
	dir := externalInstalled(t, "v0.7.0", nil)
	dump := filepath.Join(dir, "backups", "cadence-v0.7.0-20261017-030000.sql.gz")
	mustWrite(t, dump, gzipped(t, "SET transaction_timeout = 0;\nCREATE TABLE public.x (id int);\n"), 0o600)
	r := newFakeRunner()
	r.postgresRunning = false
	e, out, errw := testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"restore", "--yes", dump}); code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	check := "docker compose run --rm --no-deps -T -e PGPASSWORD db-client psql -X -q -At -d " + extSafeURL + " -c SELECT 1"
	load := "docker compose run --rm --no-deps -T -e PGPASSWORD db-client psql -X -q -v ON_ERROR_STOP=1 --single-transaction -d " + extSafeURL
	order := []string{check, "docker compose stop api worker", load, "docker compose up -d --remove-orphans"}
	at := 0
	for _, c := range r.calls {
		if at < len(order) && c == order[at] {
			at++
		}
	}
	if at != len(order) {
		t.Fatalf("calls out of order, stopped at %q: %v", order[min(at, len(order)-1)], r.calls)
	}
	body := r.stdin[load]
	if !strings.HasPrefix(body, dbResetSQL) || !strings.HasSuffix(body, "CREATE TABLE public.x (id int);\n") || strings.Contains(body, "transaction_timeout") {
		t.Errorf("load stdin: %q", body)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "DROP DATABASE") || strings.Contains(c, "up -d postgres") || strings.Contains(c, "exec -T postgres") {
			t.Errorf("the external restore touched the bundled database: %s", c)
		}
	}
	noPasswordIn(t, r, out.String(), errw.String())

	// Недоступная база: ничего не остановлено.
	r = newFakeRunner()
	r.dbClientFails = true
	e, _, errw = testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"restore", "--yes", dump}); code != 1 ||
		!strings.Contains(errw.String(), "cannot connect to the external database") || r.called("docker compose stop") {
		t.Fatalf("an unreachable database must stop nothing: %d %s %v", code, errw, r.calls)
	}
}

// 16: апгрейд внешней базы — без контейнера postgres, предпроверка образами
// новой версии до бэкапа; её отказ ничего не меняет.
func TestServerUpgradeExternal(t *testing.T) {
	newExtra := map[string]string{externalDBFile: "services: {}\n# v0.10.1\n"}
	dir := externalInstalled(t, "v0.10.0", nil)
	releases := t.TempDir()
	publish(t, releases, "v0.10.0", pkgFiles("v0.10.0", map[string]string{externalDBFile: "services: {}\n"}), false)
	publish(t, releases, "v0.10.1", pkgFiles("v0.10.1", newExtra), true)
	vars := map[string]string{"CADENCE_RELEASES_URL": "file://" + releases}

	r := newFakeRunner()
	r.postgresRunning = false
	r.dbcheckFails = true
	e, _, errw := testEnv(t, dir, r, vars)
	e.cli = "v0.10.1"
	if code := runServer(context.Background(), e, []string{"upgrade"}); code != 1 ||
		!strings.Contains(errw.String(), "the external database is not ready for v0.10.1") {
		t.Fatalf("a failed check must stop the upgrade: %d %s", code, errw)
	}
	if v := strings.TrimSpace(readFile(t, filepath.Join(dir, "VERSION"))); v != "v0.10.0" {
		t.Errorf("VERSION changed to %s", v)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "backups", "cadence-*.sql.gz")); len(left) > 0 {
		t.Errorf("no backup must be taken before the check: %v", left)
	}
	var precheck string
	for _, c := range r.calls {
		if strings.Contains(c, "api dbcheck") {
			precheck = c
		}
	}
	if !strings.Contains(precheck, "--project-directory "+dir+" -f ") || !strings.Contains(precheck, "/new/docker-compose.yml -f ") ||
		!strings.HasSuffix(precheck, "/new/"+externalDBFile+" run --rm --no-deps -T api dbcheck") {
		t.Errorf("the check must use the compose files of the new package: %q", precheck)
	}
	if !envHas(r.envs[precheck], "CADENCE_VERSION=v0.10.1") {
		t.Error("the check must run the images of v0.10.1")
	}

	r = newFakeRunner()
	r.postgresRunning = false
	e, out, errw := testEnv(t, dir, r, vars)
	e.cli = "v0.10.1"
	if code := runServer(context.Background(), e, []string{"upgrade"}); code != 0 {
		t.Fatalf("upgrade: %s", errw)
	}
	if !isFile(filepath.Join(dir, "backups", "cadence-v0.10.0-20261018-030000.sql.gz")) ||
		!r.called("docker compose run --rm --no-deps -T -e PGPASSWORD db-client pg_dump") {
		t.Error("the backup of the external database is missing")
	}
	if !strings.Contains(out.String(), "clab server restore --yes backups/cadence-v0.10.0-") && !strings.Contains(out.String(), "upgraded v0.10.0 -> v0.10.1") {
		t.Errorf("output: %s", out)
	}
}

// 17: status называет базу без пароля.
func TestServerStatusDatabase(t *testing.T) {
	dir := externalInstalled(t, "v0.7.0", nil)
	r := newFakeRunner()
	e, out, _ := testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"status"}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "Database:      external db.internal:5432/cadence (sslmode=verify-full)") {
		t.Errorf("status: %s", out)
	}
	noPasswordIn(t, r, out.String())

	jsonOut = true
	t.Cleanup(func() { jsonOut = false })
	out.Reset()
	if code := runServer(context.Background(), e, []string{"status"}); code != 0 {
		t.Fatal(code)
	}
	var view map[string]any
	if err := json.Unmarshal(out.Bytes(), &view); err != nil || view["database"] != "external db.internal:5432/cadence (sslmode=verify-full)" {
		t.Errorf("status --json: %v %s", err, out)
	}
	noPasswordIn(t, r, out.String())

	bundled := installed(t, "v0.7.0", nil)
	jsonOut = false
	e, out, _ = testEnv(t, bundled, newFakeRunner(), nil)
	runServer(context.Background(), e, []string{"status"})
	if !strings.Contains(out.String(), "Database:      bundled") {
		t.Errorf("bundled status: %s", out)
	}
}

// 1, 2, 4-7: install на внешнюю базу — .env, CA, предпроверка до записи и
// отказы.
func TestServerInstallExternal(t *testing.T) {
	f := installSetup(t, nil)
	ca := filepath.Join(f.root, "rds-ca.pem")
	mustWrite(t, ca, "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n", 0o600)
	d := f.dir("cadence")
	url := "postgres://cadence:Cad%40nce%2Fx%25y@db.internal:5432/cadence"
	// Относительный путь к CA — от текущего каталога (критерий 3).
	if code := f.install("--mode", "proxy", "--http-addr", "127.0.0.1:18190", "--yes", "--dir", d,
		"--external-db", url, "--external-db-ca", "rds-ca.pem"); code != 0 {
		t.Fatalf("code %d\n%s\n%s", code, f.out, f.errw)
	}
	env := f.env(d)
	want := map[string]string{
		"CADENCE_DB":        "external",
		"SDLC_DATABASE_URL": url + "?sslrootcert=/secrets/db-ca.pem&sslmode=verify-full",
		"POSTGRES_PASSWORD": "",
		"COMPOSE_FILE":      "docker-compose.yml:compose.proxy.yml:compose.external-db.yml:compose.override.yml",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s=%q, want %q", k, env[k], v)
		}
	}
	if readFile(t, filepath.Join(d, "secrets", "db-ca.pem")) != readFile(t, ca) || modeOf(t, filepath.Join(d, "secrets", "db-ca.pem")) != 0o644 {
		t.Error("the CA must be copied to secrets/db-ca.pem with mode 644")
	}
	var precheck string
	for _, c := range f.r.calls {
		if strings.Contains(c, "api dbcheck") {
			precheck = c
		}
	}
	if precheck != "docker compose run --rm --no-deps -T api dbcheck --install" {
		t.Fatalf("precheck: %q", precheck)
	}
	penv := f.r.envs[precheck]
	if !envHas(penv, "COMPOSE_FILE=docker-compose.yml:"+externalDBFile) || !envHas(penv, "SDLC_DATABASE_URL="+want["SDLC_DATABASE_URL"]) ||
		!envHas(penv, "COMPOSE_PROJECT_NAME=cadence") {
		t.Errorf("precheck environment: %v", penv)
	}
	if !f.r.called("docker compose run --rm --no-deps -T -e PGPASSWORD db-client psql") || !f.r.called("docker compose down -v --remove-orphans") {
		t.Errorf("the backup client check and the cleanup are missing: %v", f.r.calls)
	}
	if !strings.Contains(f.out.String(), "Database:") || !strings.Contains(f.out.String(), "external db.internal:5432/cadence") {
		t.Errorf("output: %s", f.out)
	}
	noPasswordIn(t, f.r, f.out.String(), f.errw.String())
}

func TestServerInstallExternalRefusals(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		setup func(*fakeRunner)
		want  string
	}{
		{"CA without URL", []string{"--external-db-ca", "ca.pem"}, nil, "--external-db-ca needs --external-db URL"},
		{"key=value", []string{"--external-db", "host=db user=x"}, nil, "--external-db must be a URL"},
		{"no CA file", []string{"--external-db", "postgres://u:p@h/db", "--external-db-ca", "missing.pem"}, nil, "no readable file"},
		{"CA twice", []string{"--external-db", "postgres://u:p@h/db?sslrootcert=/x", "--external-db-ca", "ca.pem"}, nil, "the CA is given twice"},
		{"old compose", []string{"--external-db", "postgres://u:p@h/db?sslmode=require"}, func(r *fakeRunner) { r.composeVersion = "2.19.1" },
			"needs Docker Compose 2.20.0 or newer"},
		{"database not ready", []string{"--external-db", "postgres://u:p@h/db?sslmode=require"}, func(r *fakeRunner) { r.dbcheckFails = true },
			"the external database is not ready"},
		{"backup client", []string{"--external-db", "postgres://u:p@h/db?sslmode=require"}, func(r *fakeRunner) { r.dbClientFails = true },
			"the backup client (libpq) cannot connect"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := installSetup(t, nil)
			mustWrite(t, filepath.Join(f.root, "ca.pem"), "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----\n", 0o644)
			if c.setup != nil {
				c.setup(f.r)
			}
			d := f.dir("cadence")
			code := f.install(append([]string{"--mode", "proxy", "--http-addr", "127.0.0.1:18190", "--yes", "--dir", d}, c.args...)...)
			if code == 0 || !strings.Contains(f.errw.String(), c.want) {
				t.Fatalf("code %d, want %q in:\n%s", code, c.want, f.errw)
			}
			f.noEnv(d)
			if f.r.called("docker compose up") || f.r.called("docker compose pull") {
				t.Errorf("a refusal must start nothing: %v", f.r.calls)
			}
		})
	}
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Секреты файлами (CLAB-200) в clab server: адрес и пароль внешней базы из
// secrets/ установки, порядок оверлея, отказ на пути вне secrets/.

func TestCheckDBModeSecretFiles(t *testing.T) {
	ext := "docker-compose.yml:compose.proxy.yml:compose.external-db.yml"
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"CADENCE_DB": "external", "SDLC_DATABASE_URL_FILE": "/secrets/db-url", "COMPOSE_FILE": ext}, ""},
		{map[string]string{"CADENCE_DB": "external", "SDLC_DATABASE_URL": "postgres://x@h/db", "SDLC_DATABASE_URL_FILE": "/secrets/db-url", "COMPOSE_FILE": ext},
			"both SDLC_DATABASE_URL and SDLC_DATABASE_URL_FILE"},
		{map[string]string{"CADENCE_DB": "external", "COMPOSE_FILE": ext}, "needs SDLC_DATABASE_URL or SDLC_DATABASE_URL_FILE"},
		{map[string]string{"CADENCE_DB": "external", "SDLC_DATABASE_URL_FILE": "/secrets/db-url",
			"COMPOSE_FILE": "docker-compose.yml:compose.proxy.yml:compose.secrets.yml:compose.external-db.yml:compose.override.yml"}, ""},
		{map[string]string{"CADENCE_DB": "external", "SDLC_DATABASE_URL_FILE": "/secrets/db-url",
			"COMPOSE_FILE": ext + ":compose.secrets.yml"}, "lists compose.secrets.yml after compose.external-db.yml"},
		{map[string]string{"COMPOSE_FILE": "docker-compose.yml:compose.proxy.yml:compose.secrets.yml:compose.override.yml"}, ""},
	}
	for _, c := range cases {
		e, _, _ := testEnv(t, t.TempDir(), newFakeRunner(), nil)
		err := (&installation{e: e, env: c.env, hasEnv: true}).checkDBMode()
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%v: %v, want %q", c.env, err, c.want)
		}
	}
}

func TestHostSecretPath(t *testing.T) {
	i := &installation{dir: "/opt/cadence"}
	for in, want := range map[string]string{
		"/secrets/db-url":     "/opt/cadence/secrets/db-url",
		"/secrets/sub/db-url": "/opt/cadence/secrets/sub/db-url",
		"/run/secrets/db_pw":  "/opt/cadence/secrets/db_pw",
	} {
		if got, err := i.hostSecretPath("SDLC_DATABASE_URL_FILE", in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"/etc/x", "/secrets/../etc/x", "/secrets/sub/../db-url", "/secrets/./x", "/secrets//x",
		"/secrets/", "relative/x", "/run/secrets"} {
		_, err := i.hostSecretPath("SDLC_DATABASE_URL_FILE", in)
		if err == nil || !strings.Contains(err.Error(), "SDLC_DATABASE_URL_FILE") || !strings.Contains(err.Error(), "secrets/") {
			t.Errorf("%s: %v", in, err)
		}
	}
}

// fileInstalled — установка на внешней базе с секретами из env (строки
// .env) и файлами secrets/.
func fileInstalled(t *testing.T, v, env string, files map[string]string) string {
	t.Helper()
	dir := externalInstalled(t, v, nil)
	mustWrite(t, filepath.Join(dir, ".env"), "CADENCE_MODE=proxy\n"+
		"COMPOSE_FILE=docker-compose.yml:compose.proxy.yml:compose.secrets.yml:compose.external-db.yml:compose.override.yml\n"+
		"CADENCE_HTTP_ADDR=127.0.0.1:18190\nCADENCE_DB=external\nPOSTGRES_PASSWORD=\nSDLC_ADMIN_TOKEN=\n"+env, 0o600)
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o711); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		mustWrite(t, filepath.Join(dir, "secrets", name), body, 0o400)
	}
	return dir
}

// AC-11: адрес в SDLC_DATABASE_URL_FILE или пароль в
// SDLC_DATABASE_PASSWORD_FILE — бэкап клиентом бэкапа, пароль только в
// PGPASSWORD.
func TestServerBackupExternalFromFiles(t *testing.T) {
	for name, c := range map[string]struct {
		env   string
		files map[string]string
		label string
	}{
		"url file": {"SDLC_DATABASE_URL=\nSDLC_DATABASE_URL_FILE=/secrets/db-url\n",
			map[string]string{"db-url": extURL + "\n"}, "(URL in /secrets/db-url)"},
		"password file": {"SDLC_DATABASE_URL='" + extSafeURL + "'\nSDLC_DATABASE_PASSWORD_FILE=/run/secrets/db_password\n",
			map[string]string{"db_password": extPassword + "\r\n"}, "db.internal:5432/cadence"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := fileInstalled(t, "v0.7.0", c.env, c.files)
			r := newFakeRunner()
			r.postgresRunning = false
			e, out, errw := testEnv(t, dir, r, nil)
			if code := runServer(context.Background(), e, []string{"backup"}); code != 0 {
				t.Fatalf("code %d: %s", code, errw)
			}
			call := "docker compose run --rm --no-deps -T -e PGPASSWORD db-client pg_dump -d " + extSafeURL + " " + strings.Join(dumpOptions, " ")
			if !r.called(call) {
				t.Fatalf("no pg_dump of the backup client: %v", r.calls)
			}
			if !envHas(r.envs[call], "PGPASSWORD="+extPassword) {
				t.Error("the backup client gets no PGPASSWORD from the file")
			}
			if !strings.Contains(out.String(), "dumping the external database "+c.label) {
				t.Errorf("output: %s", out)
			}
			noPasswordIn(t, r, out.String(), errw.String())

			// Тот же поток у restore: проверка связи и загрузка.
			dump := filepath.Join(dir, "backups", "cadence-v0.7.0-20261017-030000.sql.gz")
			mustWrite(t, dump, gzipped(t, "CREATE TABLE public.x (id int);\n"), 0o600)
			r = newFakeRunner()
			r.postgresRunning = false
			e, out, errw = testEnv(t, dir, r, nil)
			if code := runServer(context.Background(), e, []string{"restore", "--yes", dump}); code != 0 {
				t.Fatalf("restore %d: %s", code, errw)
			}
			load := "docker compose run --rm --no-deps -T -e PGPASSWORD db-client psql -X -q -v ON_ERROR_STOP=1 --single-transaction -d " + extSafeURL
			if !r.called(load) || !envHas(r.envs[load], "PGPASSWORD="+extPassword) {
				t.Errorf("restore without the password of the file: %v", r.calls)
			}
			noPasswordIn(t, r, out.String(), errw.String())
		})
	}
}

// AC-13: файл вне secrets/ — отказ до дампа с именем переменной.
func TestServerBackupRefusesFileOutsideSecrets(t *testing.T) {
	dir := fileInstalled(t, "v0.7.0", "SDLC_DATABASE_URL=\nSDLC_DATABASE_URL_FILE=/etc/x\n", nil)
	r := newFakeRunner()
	r.postgresRunning = false
	e, _, errw := testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 1 ||
		!strings.Contains(errw.String(), "SDLC_DATABASE_URL_FILE is '/etc/x'") || !strings.Contains(errw.String(), "secrets/") {
		t.Fatalf("code %d: %s", code, errw)
	}
	if r.called("docker compose run --rm --no-deps -T -e PGPASSWORD db-client") {
		t.Errorf("pg_dump ran: %v", r.calls)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "backups", "cadence-*.sql.gz")); len(left) > 0 {
		t.Errorf("a dump was left: %v", left)
	}

	// Файл в secrets/, но не читается (пуст) — та же остановка.
	dir = fileInstalled(t, "v0.7.0", "SDLC_DATABASE_URL=\nSDLC_DATABASE_URL_FILE=/secrets/db-url\n", map[string]string{"db-url": "\n"})
	r = newFakeRunner()
	r.postgresRunning = false
	e, _, errw = testEnv(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 1 || !strings.Contains(errw.String(), "is empty") {
		t.Fatalf("empty file: %d %s", code, errw)
	}
}

// Проверка внешней базы при апгрейде берёт оверлей секретов из
// COMPOSE_FILE — до compose.external-db.yml.
func TestServerUpgradeExternalWithSecretsOverlay(t *testing.T) {
	dir := fileInstalled(t, "v0.10.0", "SDLC_DATABASE_URL=\nSDLC_DATABASE_URL_FILE=/secrets/db-url\n",
		map[string]string{"db-url": extURL})
	releases := t.TempDir()
	extra := map[string]string{externalDBFile: "services: {}\n", secretsFile: "services: {}\n"}
	publish(t, releases, "v0.10.0", pkgFiles("v0.10.0", extra), false)
	publish(t, releases, "v0.10.1", pkgFiles("v0.10.1", extra), true)
	r := newFakeRunner()
	r.postgresRunning = false
	r.dbcheckFails = true
	e, _, errw := testEnv(t, dir, r, map[string]string{"CADENCE_RELEASES_URL": "file://" + releases})
	e.cli = "v0.10.1"
	if code := runServer(context.Background(), e, []string{"upgrade"}); code != 1 {
		t.Fatalf("code %d: %s", code, errw)
	}
	var precheck string
	for _, c := range r.calls {
		if strings.Contains(c, "api dbcheck") {
			precheck = c
		}
	}
	if !strings.Contains(precheck, "/new/"+secretsFile+" -f ") || !strings.HasSuffix(precheck, "/new/"+externalDBFile+" run --rm --no-deps -T api dbcheck") {
		t.Errorf("the check must take the secrets overlay before %s: %q", externalDBFile, precheck)
	}
}

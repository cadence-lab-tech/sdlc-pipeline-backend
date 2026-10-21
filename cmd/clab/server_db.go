package main

// Режим базы установки (CLAB-127): bundled — контейнер postgres стека,
// external — своя база клиента по SDLC_DATABASE_URL с
// compose.external-db.yml в COMPOSE_FILE. Правила и тексты — те же, что в
// lib.sh пакета (раздел «database»): их читают cron клиентов и проверки
// ci/ в обоих исполнителях.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/secrets"
)

const (
	externalDBFile = "compose.external-db.yml"
	// Оверлей секретов файлами (CLAB-200): Docker secrets пароля встроенной
	// базы и токена администратора. В COMPOSE_FILE — до externalDBFile:
	// тот возвращает SDLC_DATABASE_PASSWORD_FILE сервисов к значению .env.
	secretsFile = "compose.secrets.yml"
	// depends_on.required у compose.external-db.yml появился в Compose 2.20.
	externalDBMinCompose = "2.20.0"
	// Файл CA внешней базы в secrets/ установки (в контейнерах — /secrets).
	externalDBCAFile = "db-ca.pem"
)

// dumpOptions — pg_dump каждого бэкапа в обоих режимах (DUMP_OPTIONS lib.sh):
// без владельцев, прав и расширений дамп грузится в пустую базу её
// владельцем без суперпользователя — встроенной базы во внешнюю и обратно.
var dumpOptions = []string{"--clean", "--if-exists", "--no-owner", "--no-privileges",
	"--exclude-extension=vector", "--exclude-extension=pgcrypto"}

func (i *installation) externalDB() bool {
	return i.getDefault("CADENCE_DB", "bundled") == "external"
}

// checkDBMode — lib.sh check_db_mode: CADENCE_DB, SDLC_DATABASE_URL (или
// SDLC_DATABASE_URL_FILE) и COMPOSE_FILE согласованы. Файлы секретов здесь
// не читаются: их читает тот, кому нужен адрес (dbConn).
func (i *installation) checkDBMode() error {
	cf := i.get("COMPOSE_FILE")
	files := strings.Split(cf, ":")
	at := func(name string) int {
		for n, f := range files {
			if f == name {
				return n
			}
		}
		return -1
	}
	listed := at(externalDBFile) >= 0
	if i.get("SDLC_DATABASE_URL") != "" && i.get("SDLC_DATABASE_URL_FILE") != "" {
		return errors.New("both SDLC_DATABASE_URL and SDLC_DATABASE_URL_FILE are set in .env: keep one of them")
	}
	switch mode := i.getDefault("CADENCE_DB", "bundled"); mode {
	case "bundled":
		if listed {
			return fmt.Errorf("COMPOSE_FILE in .env ('%s') lists %s, but CADENCE_DB is bundled: set CADENCE_DB=external with SDLC_DATABASE_URL, or drop the file from COMPOSE_FILE", cf, externalDBFile)
		}
	case "external":
		if i.get("SDLC_DATABASE_URL") == "" && i.get("SDLC_DATABASE_URL_FILE") == "" {
			return errors.New("CADENCE_DB=external needs SDLC_DATABASE_URL or SDLC_DATABASE_URL_FILE in .env")
		}
		if !listed {
			return fmt.Errorf("COMPOSE_FILE in .env ('%s') does not include %s for CADENCE_DB=external: add it after compose.%s.yml",
				cf, externalDBFile, i.getDefault("CADENCE_MODE", "tls"))
		}
		if s := at(secretsFile); s > at(externalDBFile) {
			return fmt.Errorf("COMPOSE_FILE in .env ('%s') lists %s after %s: put it before, otherwise the password file of the bundled database reaches the service",
				cf, secretsFile, externalDBFile)
		}
	default:
		return fmt.Errorf("CADENCE_DB in .env is '%s', expected bundled or external", mode)
	}
	return nil
}

// hostSecretPath — lib.sh host_secret_path: путь из переменной name — путь
// в контейнере — как путь на этой машине. Известны только файлы secrets/
// установки: /secrets/<файл> (каталог смонтирован в api, worker и
// db-client) и /run/secrets/<имя> Docker secrets, у которых в
// compose.secrets.yml и по образцу admin.md файл secrets/<имя>.
// Путь без «.», «..» и пустых частей: ни выйти из secrets/, ни разойтись с
// разбором lib.sh.
func (i *installation) hostSecretPath(name, p string) (string, error) {
	for _, prefix := range []string{"/secrets/", "/run/secrets/"} {
		if rel, ok := strings.CutPrefix(p, prefix); ok && plainRelPath(rel) {
			return filepath.Join(i.dir, "secrets", filepath.FromSlash(rel)), nil
		}
	}
	return "", fmt.Errorf("%s is '%s': clab server reads this file on this machine and knows only the files in secrets/ of the installation "+
		"(/secrets/<file> or /run/secrets/<name> in the containers). Move the file into secrets/ and set %s=/secrets/<file> (docs/admin.md, \"Secrets in files\")",
		name, p, name)
}

func plainRelPath(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// secretSource — секреты так, как их видит скрипт пакета: .env поверх
// окружения, файлы — из secrets/ установки.
func (i *installation) secretSource() secrets.Source {
	return secrets.Source{Getenv: i.get, Path: i.hostSecretPath}
}

// dbConn — lib.sh db_conn: адрес внешней базы без пароля и пароль — из
// SDLC_DATABASE_URL, SDLC_DATABASE_URL_FILE или с SDLC_DATABASE_PASSWORD_FILE
// (решение 0052).
func (i *installation) dbConn() (safe, password string, err error) {
	raw, err := i.secretSource().DatabaseURL()
	if err != nil {
		return "", "", err
	}
	if raw == "" {
		return "", "", errors.New("CADENCE_DB=external needs SDLC_DATABASE_URL or SDLC_DATABASE_URL_FILE in .env")
	}
	safe, password = dbURLParts(raw)
	return safe, password, nil
}

// externalTarget — lib.sh db_label: host:port/база внешней базы для
// сообщений, а у адреса в файле — имя файла: файл ради надписи не читается.
func (i *installation) externalTarget() string {
	if raw := i.get("SDLC_DATABASE_URL"); raw != "" || i.get("SDLC_DATABASE_URL_FILE") == "" {
		return dbTarget(raw)
	}
	return "(URL in " + i.get("SDLC_DATABASE_URL_FILE") + ")"
}

var composeVersionRe = regexp.MustCompile(`^v?([0-9]+\.[0-9]+\.[0-9]+)`)

// requireComposeForExternalDB — lib.sh require_compose_for_external_db.
func requireComposeForExternalDB(ctx context.Context, e *serverEnv) error {
	out, _ := output(ctx, e.run, cmdSpec{name: "docker", args: []string{"compose", "version", "--short"}})
	have := ""
	if m := composeVersionRe.FindStringSubmatch(strings.TrimSpace(out)); m != nil {
		have = m[1]
	}
	if have == "" || versionLess("v"+have, "v"+externalDBMinCompose) {
		if have == "" {
			have = "an unknown version"
		}
		return fmt.Errorf("the external database needs Docker Compose %s or newer (%s uses depends_on.required); this machine has %s.\n"+
			"  Update the docker-compose-plugin package: https://docs.docker.com/compose/install/linux/", externalDBMinCompose, externalDBFile, have)
	}
	return nil
}

var dbURLRe = regexp.MustCompile(`^(postgres(?:ql)?://)([^:@/?#]*)(?::([^@/?#]*))?@(.*)$`)

// dbURLParts — lib.sh db_url_parts: адрес без пароля и пароль. Пароль
// уходит клиенту бэкапа переменной PGPASSWORD: командную строку видит любой
// пользователь машины (ps).
func dbURLParts(raw string) (safe, password string) {
	m := dbURLRe.FindStringSubmatch(raw)
	if m == nil {
		return raw, ""
	}
	password, err := url.PathUnescape(m[3])
	if err != nil {
		password = m[3]
	}
	return m[1] + m[2] + "@" + m[4], password
}

// dbTarget — lib.sh db_target: host:port/база, никогда не пароль.
func dbTarget(raw string) string {
	s := raw
	if j := strings.Index(s, "://"); j >= 0 {
		s = s[j+3:]
	}
	if j := strings.LastIndex(s, "@"); j >= 0 {
		s = s[j+1:]
	}
	if j := strings.Index(s, "?"); j >= 0 {
		s = s[:j]
	}
	return s
}

// dbLabel — строка «Database:» у status и install.
func (i *installation) dbLabel() string {
	if !i.externalDB() {
		return "bundled"
	}
	label := "external " + i.externalTarget()
	if u, err := url.Parse(i.get("SDLC_DATABASE_URL")); err == nil && i.get("SDLC_DATABASE_URL") != "" {
		if m := u.Query().Get("sslmode"); m != "" {
			label += " (sslmode=" + m + ")"
		}
	}
	return label
}

// dbCheck — lib.sh db_check: /api dbcheck образа бэкенда с выводом к
// человеку. compose — окружение запуска: у install .env ещё нет, у upgrade
// файлы новой версии.
func (i *installation) dbCheck(ctx context.Context, c cmdSpec) error {
	c.stdout, c.stderr = i.e.out, i.e.errw
	return i.e.run.run(ctx, c)
}

// dbClient — lib.sh db_client: команда клиента бэкапа (db-client
// compose.external-db.yml) с паролем базы в PGPASSWORD (dbConn).
func (i *installation) dbClient(password string, args ...string) cmdSpec {
	c := i.compose(append([]string{"run", "--rm", "--no-deps", "-T", "-e", "PGPASSWORD", "db-client"}, args...)...)
	c.env = append(c.env, "PGPASSWORD="+password)
	return c
}

// dbClientCheck — lib.sh db_client_check: клиент бэкапа (libpq) подключается
// тем же адресом, что и сервис.
func (i *installation) dbClientCheck(ctx context.Context) error {
	safe, password, err := i.dbConn()
	if err != nil {
		fmt.Fprintf(i.e.errw, "✗ %v\n", err)
		return err
	}
	c := i.dbClient(password, "psql", "-X", "-q", "-At", "-d", safe, "-c", "SELECT 1")
	c.stdout, c.stderr = io.Discard, i.e.errw
	return i.e.run.run(ctx, c)
}

// dbResetSQL — lib.sh db_reset_sql дословно: перед дампом во внешней базе
// расширения создаются (если роль может) или остаются, а объектов
// приложения в схеме public не остаётся — без DROP DATABASE и базы
// postgres, которых роли управляемого сервера не дают.
const dbResetSQL = `SET client_min_messages = warning;
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
DO $cadence$
DECLARE
  stmts text[];
  stmt text;
BEGIN
  SELECT coalesce(array_agg(s ORDER BY o), '{}') INTO stmts FROM (
    SELECT 1 AS o, format('DROP %s IF EXISTS public.%I CASCADE',
             CASE c.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW'
               WHEN 'S' THEN 'SEQUENCE' WHEN 'f' THEN 'FOREIGN TABLE' ELSE 'TABLE' END, c.relname) AS s
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
      AND NOT EXISTS (SELECT 1 FROM pg_depend d
                      WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
    UNION ALL
    SELECT 2, format('DROP %s IF EXISTS %s CASCADE',
             CASE p.prokind WHEN 'p' THEN 'PROCEDURE' WHEN 'a' THEN 'AGGREGATE' ELSE 'FUNCTION' END,
             p.oid::regprocedure)
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
    WHERE n.nspname = 'public' AND p.prokind IN ('f', 'p', 'a')
      AND NOT EXISTS (SELECT 1 FROM pg_depend d
                      WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
    UNION ALL
    SELECT 3, format('DROP %s IF EXISTS public.%I CASCADE', CASE t.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END, t.typname)
    FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
    WHERE n.nspname = 'public' AND t.typtype IN ('c', 'd', 'e', 'r')
      AND (t.typrelid = 0 OR (SELECT c.relkind FROM pg_class c WHERE c.oid = t.typrelid) = 'c')
      AND NOT EXISTS (SELECT 1 FROM pg_depend d
                      WHERE d.classid = 'pg_type'::regclass AND d.objid = t.oid AND d.deptype = 'e')
  ) x;
  FOREACH stmt IN ARRAY stmts LOOP
    EXECUTE stmt;
  END LOOP;
END
$cadence$;
`

// filterTransactionTimeout — строки дампа, кроме SET transaction_timeout:
// параметр есть с PostgreSQL 17, и дамп сервера 17 не загрузился бы в 15 или
// 16 (sed restore.sh).
func filterTransactionTimeout(src io.Reader) io.Reader {
	return &lineFilter{r: bufio.NewReaderSize(src, 64*1024)}
}

type lineFilter struct {
	r   *bufio.Reader
	buf []byte
	err error
}

func (f *lineFilter) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		if f.err != nil {
			return 0, f.err
		}
		line, err := f.r.ReadBytes('\n')
		if string(bytes.TrimSuffix(line, []byte("\n"))) != "SET transaction_timeout = 0;" {
			f.buf = line
		}
		f.err = err
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

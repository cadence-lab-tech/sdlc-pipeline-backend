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
	"regexp"
	"strings"
)

const (
	externalDBFile = "compose.external-db.yml"
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

// checkDBMode — lib.sh check_db_mode: CADENCE_DB, SDLC_DATABASE_URL и
// COMPOSE_FILE согласованы.
func (i *installation) checkDBMode() error {
	cf := i.get("COMPOSE_FILE")
	listed := strings.Contains(":"+cf+":", ":"+externalDBFile+":")
	switch mode := i.getDefault("CADENCE_DB", "bundled"); mode {
	case "bundled":
		if listed {
			return fmt.Errorf("COMPOSE_FILE in .env ('%s') lists %s, but CADENCE_DB is bundled: set CADENCE_DB=external with SDLC_DATABASE_URL, or drop the file from COMPOSE_FILE", cf, externalDBFile)
		}
	case "external":
		if i.get("SDLC_DATABASE_URL") == "" {
			return errors.New("CADENCE_DB=external needs SDLC_DATABASE_URL in .env")
		}
		if !listed {
			return fmt.Errorf("COMPOSE_FILE in .env ('%s') does not include %s for CADENCE_DB=external: add it after compose.%s.yml",
				cf, externalDBFile, i.getDefault("CADENCE_MODE", "tls"))
		}
	default:
		return fmt.Errorf("CADENCE_DB in .env is '%s', expected bundled or external", mode)
	}
	return nil
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
	label := "external " + dbTarget(i.get("SDLC_DATABASE_URL"))
	if u, err := url.Parse(i.get("SDLC_DATABASE_URL")); err == nil {
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
// compose.external-db.yml) с PGPASSWORD из SDLC_DATABASE_URL.
func (i *installation) dbClient(args ...string) cmdSpec {
	_, password := dbURLParts(i.get("SDLC_DATABASE_URL"))
	c := i.compose(append([]string{"run", "--rm", "--no-deps", "-T", "-e", "PGPASSWORD", "db-client"}, args...)...)
	c.env = append(c.env, "PGPASSWORD="+password)
	return c
}

// dbClientCheck — lib.sh db_client_check: клиент бэкапа (libpq) подключается
// тем же адресом, что и сервис.
func (i *installation) dbClientCheck(ctx context.Context) error {
	safe, _ := dbURLParts(i.get("SDLC_DATABASE_URL"))
	c := i.dbClient("psql", "-X", "-q", "-At", "-d", safe, "-c", "SELECT 1")
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

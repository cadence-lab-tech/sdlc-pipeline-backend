package postgres

// Предпроверка базы перед установкой и апгрейдом (CLAB-127): годится ли база
// по адресу SDLC_DATABASE_URL для этой версии сервиса. Её зовут `/api
// dbcheck` образа (install.sh, upgrade.sh, clab server, в будущем initContainer
// Helm), поэтому она подключается тем же драйвером и с тем же TLS, что и
// сервис, и только читает: проба CREATE EXTENSION идёт в транзакции с
// откатом.
//
// Тексты — по-английски: их печатают скрипты пакета клиенту рядом с его
// документацией (как вывод clab server, решение 0045).

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Поддерживаемые версии сервера: PostgreSQL 15-18 (решение человека по
// CLAB-127, q2). Ниже 15 у схемы public другие права по умолчанию, выше 18
// никто не проверял.
const (
	MinServerVersionNum = 150000
	MaxServerVersionNum = 189999
)

// minVectorVersion — индексы HNSW появились в pgvector 0.5.0.
const minVectorVersion = "0.5.0"

// URLParams — параметры адреса, которые одинаково понимают pgx сервиса и
// libpq клиента бэкапа (pg_dump, psql). Остальное pgx отправил бы серверу как
// параметр сессии, а libpq отказал бы: адрес, годный сервису, сломал бы
// бэкап, или наоборот.
var URLParams = []string{
	"host", "port", "dbname", "user", "password",
	"sslmode", "sslrootcert", "sslcert", "sslkey", "sslpassword", "sslsni", "sslnegotiation",
	"channel_binding", "connect_timeout", "target_session_attrs", "application_name", "options",
}

// Статусы пункта проверки.
const (
	CheckOK   = "ok"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// Состояние схемы в базе.
const (
	SchemaEmpty   = "empty"
	SchemaCadence = "cadence"
	SchemaNewer   = "newer"
	SchemaForeign = "foreign"
)

// CheckItem — один пункт проверки.
type CheckItem struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
}

// CheckReport — итог предпроверки. Пароля в нём нет ни в одном поле.
type CheckReport struct {
	OK                 bool        `json:"ok"`
	Target             string      `json:"target,omitempty"`
	SSLMode            string      `json:"sslmode,omitempty"`
	ServerVersion      string      `json:"server_version,omitempty"`
	Schema             string      `json:"schema,omitempty"`
	SchemaVersion      int64       `json:"schema_version,omitempty"`
	ImageSchemaVersion int64       `json:"image_schema_version"`
	Checks             []CheckItem `json:"checks"`
}

// CheckOptions — для кого проверка.
type CheckOptions struct {
	// Install — база для новой установки: схема Cadence в ней допустима, но
	// это повод предупредить (установка возьмёт чужие для неё данные).
	Install bool
	// Timeout — на подключение и запросы; по умолчанию 15 секунд.
	Timeout time.Duration
}

func (r *CheckReport) add(name, status, detail, hint string) {
	r.Checks = append(r.Checks, CheckItem{Name: name, Status: status, Detail: detail, Hint: hint})
	if status == CheckFail {
		r.OK = false
	}
}

// CheckDatabase проверяет базу по адресу: параметры адреса и TLS, связь,
// версию сервера, расширения, права роли и состояние схемы.
func CheckDatabase(ctx context.Context, rawURL string, o CheckOptions) CheckReport {
	r := CheckReport{OK: true}
	if latest, err := LatestMigration(); err == nil {
		r.ImageSchemaVersion = latest
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	params, err := checkURL(rawURL)
	if err != nil {
		r.add("url", CheckFail, err.Error(),
			"SDLC_DATABASE_URL is postgres://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=verify-full&sslrootcert=/secrets/db-ca.pem; "+
				"allowed parameters: "+strings.Join(URLParams, ", "))
		return r
	}
	r.Target = params.target
	r.SSLMode = params.sslmode
	r.add("url", CheckOK, params.target, "")
	if status, detail, hint := tlsPolicy(params); status != "" {
		r.add("tls", status, detail, hint)
		if status == CheckFail {
			return r
		}
	}

	cfg, err := pgx.ParseConfig(rawURL)
	if err != nil {
		msg := scrub(err.Error(), params.password)
		if strings.Contains(msg, "sslrootcert") || strings.Contains(msg, "CA file") || strings.Contains(msg, "sslcert") || strings.Contains(msg, "sslkey") {
			r.add("tls", CheckFail, msg, "the file must exist inside the container and be readable by uid 65532: put it into secrets/ (mounted as /secrets) with mode 644")
		} else {
			r.add("url", CheckFail, msg, "")
		}
		return r
	}
	cfg.ConnectTimeout = o.Timeout
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		name, detail, hint := classifyConnectError(err, params)
		r.add(name, CheckFail, detail, hint)
		return r
	}
	defer conn.Close(context.WithoutCancel(ctx))
	r.add("connection", CheckOK, fmt.Sprintf("connected to %s as %s (%s)", params.target, cfg.User, tlsState(conn)), "")

	if !r.checkServer(ctx, conn) {
		return r
	}
	r.checkExtension(ctx, conn, "vector", minVectorVersion, cfg.Database)
	r.checkExtension(ctx, conn, "pgcrypto", "", cfg.Database)
	if r.checkPrivileges(ctx, conn, cfg.User) {
		r.checkSchema(ctx, conn, o.Install)
	}
	return r
}

// urlParams — разобранный адрес без пароля в печатных полях.
type urlParams struct {
	target      string // host:port/db
	sslmode     string // как задан; пусто — не задан
	sslrootcert string
	password    string
}

// checkURL разбирает адрес так, как его потом прочтут pgx и libpq:
// только postgres:// (у формы key=value нет однозначного разбора в shell
// скриптов пакета), параметры — из общего списка.
func checkURL(raw string) (urlParams, error) {
	var p urlParams
	if strings.TrimSpace(raw) == "" {
		return p, errors.New("SDLC_DATABASE_URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Текст ошибки net/url повторяет адрес целиком, с паролем.
		return p, errors.New("SDLC_DATABASE_URL is not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return p, fmt.Errorf("SDLC_DATABASE_URL must start with postgres:// or postgresql://, not %s://", u.Scheme)
	}
	if u.User != nil {
		p.password, _ = u.User.Password()
	}
	allowed := map[string]bool{}
	for _, k := range URLParams {
		allowed[k] = true
	}
	var unknown []string
	for k := range u.Query() {
		if !allowed[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return p, fmt.Errorf("SDLC_DATABASE_URL has the parameter %s, which the service and the backup client (libpq) do not read the same way", strings.Join(unknown, ", "))
	}
	q := u.Query()
	host := u.Host
	if h := q.Get("host"); h != "" {
		host = h
	}
	if host == "" {
		return p, errors.New("SDLC_DATABASE_URL has no host")
	}
	if !strings.Contains(host, ":") || strings.HasSuffix(host, "]") {
		port := q.Get("port")
		if port == "" {
			port = "5432"
		}
		host = host + ":" + port
	}
	db := strings.TrimPrefix(u.Path, "/")
	if d := q.Get("dbname"); d != "" {
		db = d
	}
	if db == "" {
		return p, errors.New("SDLC_DATABASE_URL names no database (postgres://USER:PASSWORD@HOST:PORT/DATABASE)")
	}
	p.target = host + "/" + db
	p.sslmode = q.Get("sslmode")
	p.sslrootcert = q.Get("sslrootcert")
	return p, nil
}

// tlsPolicy — мягкая политика sslmode (решение человека по CLAB-127, q3):
// без sslmode — отказ (pgx и libpq тихо взяли бы prefer); verify-* без
// sslrootcert — отказ (pgx взял бы системные корни, а libpq — файл в
// домашнем каталоге, которого в контейнере нет); require — предупреждение;
// disable, allow и prefer — громкое предупреждение, установка идёт.
func tlsPolicy(p urlParams) (status, detail, hint string) {
	switch p.sslmode {
	case "":
		return CheckFail, "sslmode is not set in SDLC_DATABASE_URL",
			"name it explicitly: sslmode=verify-full with the CA of the server (--external-db-ca, or sslrootcert=system for a public CA); " +
				"sslmode=disable only on a network you trust"
	case "verify-full", "verify-ca":
		if p.sslrootcert == "" {
			return CheckFail, "sslmode=" + p.sslmode + " without sslrootcert",
				"give the CA of the server: --external-db-ca FILE (it lands in /secrets), or sslrootcert=system for a CA the system trusts"
		}
		return CheckOK, "sslmode=" + p.sslmode + ", CA " + p.sslrootcert, ""
	case "require":
		return CheckWarn, "sslmode=require: the connection is encrypted, but the certificate of the server is not verified",
			"prefer sslmode=verify-full with the CA of the server"
	case "disable", "allow", "prefer":
		return CheckWarn, "sslmode=" + p.sslmode + ": the connection may be UNENCRYPTED and the server is not verified; passwords and data cross the network in the clear",
			"use sslmode=verify-full with the CA of the server unless the database is on a network you fully trust"
	default:
		return CheckFail, "sslmode=" + p.sslmode + " is not a valid mode",
			"one of verify-full, verify-ca, require, prefer, allow, disable"
	}
}

// classifyConnectError — понятная причина отказа подключения: TLS,
// аутентификация, нет базы, сеть. Пароль из текста вырезается.
func classifyConnectError(err error, p urlParams) (name, detail, hint string) {
	msg := scrub(err.Error(), p.password)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000":
			return "authentication", fmt.Sprintf("the server refused the role: %s (SQLSTATE %s)", scrub(pgErr.Message, p.password), pgErr.Code),
				"check the user and the password in SDLC_DATABASE_URL (percent-encode special characters of the password) and pg_hba of the server"
		case "3D000":
			return "connection", fmt.Sprintf("the database does not exist: %s", scrub(pgErr.Message, p.password)),
				"create it first: CREATE DATABASE cadence OWNER cadence;"
		}
	}
	var unknownCA x509.UnknownAuthorityError
	var hostErr x509.HostnameError
	var verifyErr *tls.CertificateVerificationError
	if errors.As(err, &unknownCA) || errors.As(err, &hostErr) || errors.As(err, &verifyErr) ||
		strings.Contains(msg, "x509:") || strings.Contains(msg, "tls:") || strings.Contains(msg, "TLS") || strings.Contains(msg, "SSL") {
		return "tls", "TLS to " + p.target + " failed: " + msg,
			"the certificate of the server must be signed by the CA in sslrootcert and name the host of SDLC_DATABASE_URL; check --external-db-ca and the host"
	}
	var netErr net.Error
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) || errors.As(err, &netErr) || strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") {
		return "connection", "cannot reach " + p.target + ": " + msg,
			"the host must be reachable from the containers of the installation (not localhost: that is the container itself)"
	}
	return "connection", "cannot connect to " + p.target + ": " + msg, ""
}

func tlsState(conn *pgx.Conn) string {
	if _, ok := conn.PgConn().Conn().(*tls.Conn); ok {
		return "TLS"
	}
	return "no TLS"
}

// scrub вырезает пароль из текста ошибки: драйвер его не печатает, но
// сервер и сеть могут повторить кусок адреса.
func scrub(s, password string) string {
	if password == "" {
		return s
	}
	s = strings.ReplaceAll(s, password, "***")
	return strings.ReplaceAll(s, url.QueryEscape(password), "***")
}

func (r *CheckReport) checkServer(ctx context.Context, conn *pgx.Conn) bool {
	var num, version string
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version_num'), current_setting('server_version')`).Scan(&num, &version); err != nil {
		r.add("server", CheckFail, "cannot read the server version: "+err.Error(), "")
		return false
	}
	r.ServerVersion = version
	n, _ := strconv.Atoi(num)
	if n < MinServerVersionNum || n > MaxServerVersionNum {
		r.add("server", CheckFail, fmt.Sprintf("PostgreSQL %s is not supported", version),
			"Cadence Lab supports PostgreSQL 15, 16, 17 and 18")
		return false
	}
	r.add("server", CheckOK, "PostgreSQL "+version, "")
	return true
}

// checkExtension — расширение установлено нужной версии, либо роль может
// создать его сама (проба в транзакции с откатом).
func (r *CheckReport) checkExtension(ctx context.Context, conn *pgx.Conn, name, minVersion, database string) {
	var installed, available *string
	err := conn.QueryRow(ctx, `
		SELECT e.extversion, a.default_version
		FROM (SELECT $1::text AS name) n
		LEFT JOIN pg_extension e ON e.extname = n.name
		LEFT JOIN pg_available_extensions a ON a.name = n.name`, name).Scan(&installed, &available)
	if err != nil {
		r.add(name, CheckFail, "cannot read the extensions: "+err.Error(), "")
		return
	}
	superuserHint := fmt.Sprintf("as a superuser (rds_superuser on Amazon RDS, cloudsqlsuperuser on Cloud SQL, azure_pg_admin on Azure) connect to the database %s and run: CREATE EXTENSION IF NOT EXISTS %s;", database, name)
	if installed != nil {
		if minVersion != "" && versionBelow(*installed, minVersion) {
			r.add(name, CheckFail, fmt.Sprintf("%s %s is installed, %s or newer is needed", name, *installed, minVersion),
				fmt.Sprintf("as a superuser run: ALTER EXTENSION %s UPDATE;", name))
			return
		}
		r.add(name, CheckOK, fmt.Sprintf("%s %s is installed", name, *installed), "")
		return
	}
	if available == nil {
		r.add(name, CheckFail, fmt.Sprintf("the server does not offer the extension %s", name),
			fmt.Sprintf("install %s on the server or enable it at your provider, then %s", name, strings.TrimPrefix(superuserHint, "as a superuser ")))
		return
	}
	if minVersion != "" && versionBelow(*available, minVersion) {
		r.add(name, CheckFail, fmt.Sprintf("the server offers %s %s, %s or newer is needed", name, *available, minVersion),
			"upgrade pgvector on the server")
		return
	}
	if err := probeCreateExtension(ctx, conn, name); err != nil {
		r.add(name, CheckFail, fmt.Sprintf("%s is not installed, and this role cannot create it: %s", name, pgMessage(err)), superuserHint)
		return
	}
	r.add(name, CheckOK, fmt.Sprintf("%s is not installed; the service creates it on the first start (this role may)", name), "")
}

func probeCreateExtension(ctx context.Context, conn *pgx.Conn, name string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	_, err = tx.Exec(ctx, "CREATE EXTENSION "+pgx.Identifier{name}.Sanitize())
	return err
}

// checkPrivileges — роль может создавать объекты в схеме public; без USAGE
// на неё схему не прочитать, и её состояние не проверяется.
func (r *CheckReport) checkPrivileges(ctx context.Context, conn *pgx.Conn, user string) bool {
	var exists, usage, create bool
	err := conn.QueryRow(ctx, `
		SELECT to_regnamespace('public') IS NOT NULL,
		       CASE WHEN to_regnamespace('public') IS NULL THEN false ELSE has_schema_privilege('public', 'USAGE') END,
		       CASE WHEN to_regnamespace('public') IS NULL THEN false ELSE has_schema_privilege('public', 'CREATE') END`).Scan(&exists, &usage, &create)
	if err != nil {
		r.add("privileges", CheckFail, "cannot read the privileges: "+err.Error(), "")
		return false
	}
	switch {
	case !exists:
		r.add("privileges", CheckFail, "the database has no schema public", "as its owner run: CREATE SCHEMA public;")
	case !usage || !create:
		r.add("privileges", CheckFail, fmt.Sprintf("the role %s has no CREATE privilege on the schema public", user),
			fmt.Sprintf("make the role the owner of the database (ALTER DATABASE ... OWNER TO %s), or as the owner run: GRANT USAGE, CREATE ON SCHEMA public TO %s;", user, user))
	default:
		r.add("privileges", CheckOK, fmt.Sprintf("the role %s may create objects in the schema public", user), "")
	}
	return exists && usage
}

func (r *CheckReport) checkSchema(ctx context.Context, conn *pgx.Conn, install bool) {
	var hasMigrations bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&hasMigrations); err != nil {
		r.add("schema", CheckFail, "cannot read the schema: "+err.Error(), "")
		return
	}
	if hasMigrations {
		var v *int64
		if err := conn.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&v); err != nil {
			r.add("schema", CheckFail, "cannot read schema_migrations: "+err.Error(), "")
			return
		}
		if v != nil {
			r.SchemaVersion = *v
		}
		if r.SchemaVersion > r.ImageSchemaVersion {
			r.Schema = SchemaNewer
			r.add("schema", CheckFail, fmt.Sprintf("the Cadence schema in the database is at version %d, newer than these images know (%d)", r.SchemaVersion, r.ImageSchemaVersion),
				"a newer version of Cadence Lab wrote it: run that version, or restore a backup taken on this one")
			return
		}
		r.Schema = SchemaCadence
		if install {
			r.add("schema", CheckWarn, fmt.Sprintf("the database already holds a Cadence schema at version %d: the installation will use its data", r.SchemaVersion),
				"install into an empty database unless you mean to take that data over")
			return
		}
		r.add("schema", CheckOK, fmt.Sprintf("Cadence schema at version %d; these images apply up to %d", r.SchemaVersion, r.ImageSchemaVersion), "")
		return
	}
	rows, err := conn.Query(ctx, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'v', 'm', 'f')
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d
		                  WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
		ORDER BY c.relname LIMIT 6`)
	if err != nil {
		r.add("schema", CheckFail, "cannot list the tables: "+err.Error(), "")
		return
	}
	foreign, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		r.add("schema", CheckFail, "cannot list the tables: "+err.Error(), "")
		return
	}
	if len(foreign) > 0 {
		r.Schema = SchemaForeign
		list := strings.Join(foreign, ", ")
		if len(foreign) > 5 {
			list = strings.Join(foreign[:5], ", ") + ", ..."
		}
		r.add("schema", CheckFail, "the schema public holds tables that are not Cadence's ("+list+")",
			"give Cadence Lab a database of its own: CREATE DATABASE cadence OWNER cadence;")
		return
	}
	r.Schema = SchemaEmpty
	r.add("schema", CheckOK, "empty: the service applies the schema on the first start", "")
}

func pgMessage(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Message
	}
	return err.Error()
}

// versionBelow — версия расширения вида 0.8.0 ниже min.
func versionBelow(have, min string) bool {
	a, b := strings.Split(have, "."), strings.Split(min, ".")
	for i := 0; i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		y, _ = strconv.Atoi(b[i])
		if x != y {
			return x < y
		}
	}
	return false
}

// LatestMigration — номер последней миграции, встроенной в бинарник.
func LatestMigration() (int64, error) {
	all, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	return all[len(all)-1].version, nil
}

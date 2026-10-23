// Package secrets — секретные переменные окружения сервиса и их файловая
// форма (CLAB-200, решение 0052).
//
// Каждая секретная переменная X принимает и X_FILE — путь к файлу, значение
// которого — содержимое без завершающих переводов строки. Так секреты
// приходят из Docker secrets и секретов Kubernetes, которые монтируются в
// контейнер файлами и не видны ни в .env, ни в docker inspect.
//
// List — единственный перечень секретов: из него читают config.Load,
// offsite.FromEnv, github.SecretFromEnv, tracing.FromEnv и clab server, по
// нему сверяется
// таблица «Secrets in files» в docs/admin.md пакета и маскирует support-bundle
// (CLAB-138). Новая секретная переменная — строка здесь; Value отказывает в
// чтении имени, которого в перечне нет.
//
// Тексты ошибок — по-английски: их читает администратор установки в логе
// контейнера и в выводе скриптов пакета, рядом с документацией клиента.
package secrets

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Form — в каком виде переменная приходит в окружение.
type Form int

const (
	// ValueOrFile — X со значением или X_FILE с путём к файлу; обе сразу —
	// ошибка.
	ValueOrFile Form = iota
	// FileOnly — только X_FILE: значение — содержимое файла. Имя в перечне
	// без суффикса _FILE.
	FileOnly
	// PathOnly — переменная сама путь, файл открывает её читатель (ключ
	// GitHub App). Имя в перечне — как в окружении.
	PathOnly
	// Image — переменную читает чужой образ (postgres), сервис её не читает;
	// в перечне она ради документации и маски.
	Image
)

// Var — секретная переменная.
type Var struct {
	// Name — имя в окружении; у Template — префикс имени.
	Name string
	Form Form
	// Template — имя шаблоном: Name<NAME>, токены репозиториев по
	// secret_ref.
	Template bool
	// Readers — кто читает: api, worker, dbcheck, backup-s3, postgres.
	Readers []string
	// Via — какой разбор читателя принимает переменную, если не общий
	// (config.Load у api, воркера и dbcheck): ViaTracing — трассировки api.
	Via string
}

// ViaTracing — переменную разбирает tracing.FromEnv api, а не config.Load
// (CLAB-235): заголовки OTLP.
const ViaTracing = "tracing"

// ValueEnv — имя формы значением, пусто, если её нет.
func (v Var) ValueEnv() string {
	switch v.Form {
	case ValueOrFile, Image:
		return v.Name
	}
	return ""
}

// FileEnv — имя переменной с путём к файлу.
func (v Var) FileEnv() string {
	if v.Form == PathOnly {
		return v.Name
	}
	return v.Name + "_FILE"
}

// ReadBy — читает ли переменную reader.
func (v Var) ReadBy(reader string) bool {
	for _, r := range v.Readers {
		if r == reader {
			return true
		}
	}
	return false
}

// Имена, на которые ссылается код сервиса.
const (
	DatabaseURL      = "SDLC_DATABASE_URL"
	DatabasePassword = "SDLC_DATABASE_PASSWORD"
	GitHubTokenOf    = "SDLC_GITHUB_TOKEN_"
)

// List — секреты на main; порядок — порядок таблицы в docs/admin.md.
var List = []Var{
	{Name: DatabaseURL, Readers: []string{"api", "worker", "dbcheck"}},
	{Name: DatabasePassword, Form: FileOnly, Readers: []string{"api", "worker", "dbcheck"}},
	{Name: "SDLC_ADMIN_TOKEN", Readers: []string{"api"}},
	{Name: "SDLC_GITHUB_TOKEN", Readers: []string{"api", "worker"}},
	{Name: GitHubTokenOf, Template: true, Readers: []string{"api", "worker"}},
	{Name: "SDLC_GITHUB_APP_KEY_FILE", Form: PathOnly, Readers: []string{"api", "worker"}},
	{Name: "SDLC_GITHUB_APP_CLIENT_SECRET", Readers: []string{"api"}},
	{Name: "SDLC_GITHUB_WEBHOOK_SECRET", Readers: []string{"api"}},
	{Name: "SDLC_EMBEDDINGS_API_KEY", Readers: []string{"api", "worker"}},
	{Name: "SDLC_VOYAGE_API_KEY", Readers: []string{"api", "worker"}},
	{Name: "SDLC_BLOB_S3_ACCESS_KEY_ID", Readers: []string{"api", "worker"}},
	{Name: "SDLC_BLOB_S3_SECRET_ACCESS_KEY", Readers: []string{"api", "worker"}},
	{Name: "SDLC_BLOB_S3_SESSION_TOKEN", Readers: []string{"api", "worker"}},
	// Заголовки экспортёра трассировок (CLAB-235): в них ключи APM.
	{Name: "OTEL_EXPORTER_OTLP_HEADERS", Readers: []string{"api"}, Via: ViaTracing},
	{Name: "OTEL_EXPORTER_OTLP_TRACES_HEADERS", Readers: []string{"api"}, Via: ViaTracing},
	{Name: "BACKUP_S3_ACCESS_KEY_ID", Readers: []string{"backup-s3"}},
	{Name: "BACKUP_S3_SECRET_ACCESS_KEY", Readers: []string{"backup-s3"}},
	{Name: "POSTGRES_PASSWORD", Form: Image, Readers: []string{"postgres"}},
}

// Lookup — переменная перечня по имени в окружении (X, X_FILE, имя по
// шаблону).
func Lookup(name string) (Var, bool) {
	for _, v := range List {
		if v.Template {
			if strings.HasPrefix(name, v.Name) && len(name) > len(v.Name) {
				return v, true
			}
			continue
		}
		if name == v.ValueEnv() || name == v.FileEnv() || name == v.Name {
			return v, true
		}
	}
	return Var{}, false
}

// Source — откуда читаются переменные и файлы.
type Source struct {
	Getenv func(string) string
	// Path переводит путь из переменной name в путь, по которому файл
	// открывается здесь; nil — как есть. clab server переводит пути
	// контейнера (/secrets, /run/secrets) в secrets/ установки.
	Path func(name, path string) (string, error)
	// Read читает файл по пути после Path; nil — os.ReadFile. clab server
	// дочитывает файл, отданный сервису (65532, 400), одноразовым
	// контейнером (CLAB-252). Ошибка Read уходит в ошибку File как есть,
	// поэтому в ней не должно быть содержимого.
	Read func(path string) ([]byte, error)
}

// OS — окружение процесса, пути как есть.
func OS() Source { return Source{Getenv: os.Getenv} }

// Value — значение секрета name формы ValueOrFile: X или содержимое X_FILE.
// Пусто в обеих — пусто без ошибки; непустые обе — ошибка с обоими именами
// (решение 0052: тихого выбора нет, забытое в .env значение всплывает).
func (s Source) Value(name string) (string, error) {
	v, ok := Lookup(name)
	if !ok || v.Form != ValueOrFile {
		return "", fmt.Errorf("%s is not a secret of secrets.List that takes a value or a file", name)
	}
	value, path := s.Getenv(name), s.Getenv(name+"_FILE")
	switch {
	case value != "" && path != "":
		return "", fmt.Errorf("both %s and %s_FILE are set: keep one of them", name, name)
	case path != "":
		return s.File(name+"_FILE", path)
	}
	return value, nil
}

// File — содержимое файла переменной name без завершающих \n и \r\n. В
// ошибке — имя, путь и причина ОС (*os.PathError несёт операцию и путь),
// содержимого нет никогда.
func (s Source) File(name, path string) (string, error) {
	if s.Path != nil {
		p, err := s.Path(name, path)
		if err != nil {
			return "", err
		}
		path = p
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s: %s is a directory, not a file", name, path)
	}
	// ВРЕМЕННО (CLAB-253, AC-4): откат исправления CLAB-252 — хук Read
	// игнорируется, файл читается прямым os.ReadFile. Ветка не сливается.
	read := os.ReadFile
	raw, err := read(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	value := strings.TrimRight(string(raw), "\r\n")
	if value == "" {
		return "", fmt.Errorf("%s: %s is empty", name, path)
	}
	return value, nil
}

// DatabaseURL — адрес базы: SDLC_DATABASE_URL или содержимое
// SDLC_DATABASE_URL_FILE, а с SDLC_DATABASE_PASSWORD_FILE — пароль из файла
// в этом адресе. Пароль подставляется разбором адреса, а не склейкой строк:
// спецсимволы пароля экранируются. Адрес с непустым паролем вместе с файлом
// пароля — ошибка: какой из двух верный, не угадать. Пусто везде — пусто
// без ошибки, отказ — дело вызывающего.
func (s Source) DatabaseURL() (string, error) {
	raw, err := s.Value(DatabaseURL)
	if err != nil {
		return "", err
	}
	pwFile := s.Getenv(DatabasePassword + "_FILE")
	if pwFile == "" {
		return raw, nil
	}
	password, err := s.File(DatabasePassword+"_FILE", pwFile)
	if err != nil {
		return "", err
	}
	if raw == "" {
		return "", fmt.Errorf("%s_FILE is set, but %s and %s_FILE are empty: the password needs a URL without one",
			DatabasePassword, DatabaseURL, DatabaseURL)
	}
	return WithPassword(raw, password)
}

// WithPassword — адрес postgres:// с паролем password. В ошибке разбора
// адреса нет: url.Parse печатает его целиком, с паролем.
func WithPassword(raw, password string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", fmt.Errorf("%s_FILE needs %s as a URL postgres://USER@HOST:PORT/DATABASE; the URL does not parse as one",
			DatabasePassword, DatabaseURL)
	}
	if u.User != nil {
		if p, set := u.User.Password(); set && p != "" {
			return "", fmt.Errorf("both %s_FILE and a password in the URL of %s (or %s_FILE) are set: keep one of them",
				DatabasePassword, DatabaseURL, DatabaseURL)
		}
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}

package secrets

import (
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// marker — содержимое файлов в тестах: его не должно быть ни в одном тексте
// ошибки.
const marker = "s3cr3t-marker-7f2a"

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// AC-2: завершающий перевод строки отрезается, внутренние остаются.
func TestFileTrimsTrailingNewline(t *testing.T) {
	pem := "-----BEGIN KEY-----\nAAAA\nBBBB\n-----END KEY-----"
	for body, want := range map[string]string{
		"v\n":        "v",
		"v\r\n":      "v",
		"v":          "v",
		pem + "\n":   pem,
		"a\r\nb\n\n": "a\r\nb",
	} {
		src := Source{Getenv: envOf(map[string]string{"SDLC_ADMIN_TOKEN_FILE": writeFile(t, body)})}
		got, err := src.Value("SDLC_ADMIN_TOKEN")
		if err != nil || got != want {
			t.Errorf("%q: %q, %v; want %q", body, got, err, want)
		}
	}
}

// R1: пустая X_FILE — незаданная, как пустая X.
func TestEmptyFileVariableIsUnset(t *testing.T) {
	got, err := Source{Getenv: envOf(map[string]string{"SDLC_ADMIN_TOKEN": "v", "SDLC_ADMIN_TOKEN_FILE": ""})}.Value("SDLC_ADMIN_TOKEN")
	if err != nil || got != "v" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = Source{Getenv: envOf(nil)}.Value("SDLC_ADMIN_TOKEN")
	if err != nil || got != "" {
		t.Fatalf("both empty: %q %v", got, err)
	}
}

// AC-3: обе формы — ошибка с обоими именами и без значения.
func TestBothFormsAreAnError(t *testing.T) {
	src := Source{Getenv: envOf(map[string]string{
		"SDLC_GITHUB_WEBHOOK_SECRET": marker, "SDLC_GITHUB_WEBHOOK_SECRET_FILE": writeFile(t, marker),
	})}
	_, err := src.Value("SDLC_GITHUB_WEBHOOK_SECRET")
	if err == nil {
		t.Fatal("both forms accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "SDLC_GITHUB_WEBHOOK_SECRET ") || !strings.Contains(msg, "SDLC_GITHUB_WEBHOOK_SECRET_FILE") ||
		strings.Contains(msg, marker) {
		t.Fatalf("message: %s", msg)
	}
}

// AC-4: файла нет, нет прав, каталог, пустой файл — ошибка с именем X_FILE,
// путём и причиной, без содержимого.
func TestBadFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	empty := writeFile(t, "\n\r\n")
	cases := map[string]struct {
		path, reason string
	}{
		"missing":   {missing, "no such file or directory"},
		"directory": {dir, "is a directory"},
		"empty":     {empty, "is empty"},
	}
	if os.Geteuid() != 0 {
		// root читает любой файл: этот случай прогоняется от обычного
		// пользователя (в верстаке — su nobody, test.report).
		noPerm := writeFile(t, marker)
		if err := os.Chmod(noPerm, 0o000); err != nil {
			t.Fatal(err)
		}
		cases["permission"] = struct{ path, reason string }{noPerm, "permission denied"}
	} else {
		t.Log("root: the permission case needs another user")
	}
	for name, c := range cases {
		_, err := Source{Getenv: envOf(map[string]string{"SDLC_VOYAGE_API_KEY_FILE": c.path})}.Value("SDLC_VOYAGE_API_KEY")
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, "SDLC_VOYAGE_API_KEY_FILE") || !strings.Contains(msg, c.path) ||
			!strings.Contains(msg, c.reason) || strings.Contains(msg, marker) {
			t.Errorf("%s: %s", name, msg)
		}
	}
}

// Value читает только секреты перечня: секрет мимо перечня — ошибка кода.
func TestValueRefusesNamesOutsideTheList(t *testing.T) {
	if _, err := OS().Value("SDLC_LOG_LEVEL"); err == nil {
		t.Fatal("a variable outside the list was read as a secret")
	}
	if _, err := OS().Value("SDLC_DATABASE_PASSWORD"); err == nil {
		t.Fatal("a file-only secret was read as a value")
	}
	if v, ok := Lookup("SDLC_GITHUB_TOKEN_ACME_FILE"); !ok || !v.Template {
		t.Fatal("the template of repository tokens")
	}
	if _, ok := Lookup(GitHubTokenOf); ok {
		t.Fatal("the bare prefix is no variable")
	}
}

// AC-5: пароль из файла в адресе, со спецсимволами; адрес файлом.
func TestDatabaseURL(t *testing.T) {
	const password = "p@:/%?#&= x"
	pwFile := writeFile(t, password+"\n")
	url, err := Source{Getenv: envOf(map[string]string{
		DatabaseURL:                "postgres://sdlc:@postgres:5432/sdlc?sslmode=disable",
		DatabasePassword + "_FILE": pwFile,
	})}.DatabaseURL()
	if err != nil {
		t.Fatal(err)
	}
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); pw != password || u.User.Username() != "sdlc" || u.Host != "postgres:5432" ||
		u.Path != "/sdlc" || u.RawQuery != "sslmode=disable" {
		t.Fatalf("url %s", url)
	}

	full := "postgres://u:" + marker + "@db:5432/x?sslmode=require"
	got, err := Source{Getenv: envOf(map[string]string{DatabaseURL + "_FILE": writeFile(t, full+"\n")})}.DatabaseURL()
	if err != nil || got != full {
		t.Fatalf("URL file: %q %v", got, err)
	}
	// Без пароля в адресе пароль просто добавляется.
	got, err = Source{Getenv: envOf(map[string]string{
		DatabaseURL: "postgres://u@db/x", DatabasePassword + "_FILE": writeFile(t, "pw"),
	})}.DatabaseURL()
	if err != nil || got != "postgres://u:pw@db/x" {
		t.Fatalf("no password in the URL: %q %v", got, err)
	}

	refusals := map[string]map[string]string{
		"password twice": {DatabaseURL: "postgres://u:" + marker + "@db/x", DatabasePassword + "_FILE": pwFile},
		"no URL":         {DatabasePassword + "_FILE": pwFile},
		"not a URL":      {DatabaseURL: "host=db password=" + marker, DatabasePassword + "_FILE": pwFile},
		"bad URL":        {DatabaseURL: "postgres://u:" + marker + "@db:port/x", DatabasePassword + "_FILE": pwFile},
		"URL twice":      {DatabaseURL: "postgres://u@db/x", DatabaseURL + "_FILE": writeFile(t, full)},
		"missing file":   {DatabaseURL: "postgres://u@db/x", DatabasePassword + "_FILE": "/nonexistent/pw"},
	}
	for name, env := range refusals {
		_, err := Source{Getenv: envOf(env)}.DatabaseURL()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "p@:/") {
			t.Errorf("%s: the secret in %s", name, err)
		}
	}
}

// Path переводит путь: так clab server находит файлы контейнера на хосте.
func TestSourcePath(t *testing.T) {
	host := writeFile(t, "v")
	src := Source{
		Getenv: envOf(map[string]string{"BACKUP_S3_SECRET_ACCESS_KEY_FILE": "/secrets/key"}),
		Path: func(name, p string) (string, error) {
			if name != "BACKUP_S3_SECRET_ACCESS_KEY_FILE" || p != "/secrets/key" {
				t.Errorf("Path(%q, %q)", name, p)
			}
			return host, nil
		},
	}
	if v, err := src.Value("BACKUP_S3_SECRET_ACCESS_KEY"); err != nil || v != "v" {
		t.Fatalf("%q %v", v, err)
	}
}

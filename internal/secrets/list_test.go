package secrets_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/offsite"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/secrets"
)

const marker = "Mk7fileValue42"

func file(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// sample — значение секрета, которое примет его читатель: адрес базы
// разбирается, остальное — строка.
func sample(v secrets.Var) string {
	if v.Name == secrets.DatabaseURL {
		return "postgres://u:" + marker + "@db:5432/x"
	}
	return marker
}

// needs — что ещё нужно читателю, чтобы принять секрет: не копия имён
// перечня, а правила config.Load, которые держат пары переменных.
var needs = map[string]map[string]string{
	"SDLC_GITHUB_APP_CLIENT_SECRET": {"SDLC_GITHUB_APP_CLIENT_ID": "Iv1.id"},
}

// loadConfig — config.Load на окружении env поверх минимального.
func loadConfig(t *testing.T, env map[string]string) (config.Config, error) {
	t.Helper()
	if _, ok := env[secrets.DatabaseURL]; !ok {
		if _, ok := env[secrets.DatabaseURL+"_FILE"]; !ok {
			env[secrets.DatabaseURL] = "postgres://u:@db:5432/x"
		}
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	return config.Load()
}

// AC-1: каждая переменная перечня читается файлом так же, как значением, у
// своего читателя. Тест обходит сам перечень: строка без поддержки чтения
// (или с читателем, которого тест не знает) его роняет.
func TestEverySecretReadsFromFile(t *testing.T) {
	for _, v := range secrets.List {
		t.Run(v.Name, func(t *testing.T) {
			if v.Form == secrets.Image {
				// POSTGRES_PASSWORD_FILE читает образ postgres, не наш код.
				if !reflect.DeepEqual(v.Readers, []string{"postgres"}) {
					t.Fatalf("Image with readers %v", v.Readers)
				}
				return
			}
			if len(v.Readers) == 0 {
				t.Fatal("a secret without readers")
			}
			// api, worker и dbcheck читают одним разбором (config.Load и
			// Source.DatabaseURL): одна проверка на всех.
			viaConfig, viaOffsite := false, false
			for _, r := range v.Readers {
				switch r {
				case "api", "worker", "dbcheck":
					viaConfig = true
				case "backup-s3":
					viaOffsite = true
				default:
					t.Fatalf("unknown reader %q: teach this test to read it", r)
				}
			}
			if viaConfig {
				checkConfig(t, v)
			}
			if viaOffsite {
				checkOffsite(t, v)
			}
		})
	}
}

func checkConfig(t *testing.T, v secrets.Var) {
	t.Helper()
	base := func() map[string]string {
		env := map[string]string{}
		for k, val := range needs[v.Name] {
			env[k] = val
		}
		return env
	}
	if v.Template {
		// Токен репозитория читает провайдер по имени из базы.
		t.Setenv(v.Name+"PROBE", "")
		t.Setenv(v.Name+"PROBE_FILE", file(t, marker+"\n"))
		got, err := github.SecretFromEnv("probe")
		if err != nil || got != marker {
			t.Fatalf("SecretFromEnv: %q %v", got, err)
		}
		return
	}
	switch v.Form {
	case secrets.PathOnly:
		// Путь отдаётся читателю как есть: файл открывает github.LoadApp.
		p := file(t, "key") + marker
		env := base()
		env[v.FileEnv()] = p
		cfg, err := loadConfig(t, env)
		if err != nil || !strings.Contains(fmt.Sprintf("%+v", cfg), p) {
			t.Fatalf("path %s not in the config: %v", p, err)
		}
	case secrets.FileOnly:
		env := base()
		env[v.FileEnv()] = file(t, marker+"\n")
		cfg, err := loadConfig(t, env)
		if err != nil || !strings.Contains(fmt.Sprintf("%+v", cfg), marker) {
			t.Fatalf("the file of %s did not reach the config: %v", v.FileEnv(), err)
		}
	case secrets.ValueOrFile:
		env := base()
		env[v.ValueEnv()] = sample(v)
		byValue, err := loadConfig(t, env)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(fmt.Sprintf("%+v", byValue), marker) {
			t.Fatalf("%s is not read by config.Load", v.Name)
		}
		env = base()
		env[v.ValueEnv()] = ""
		env[v.FileEnv()] = file(t, sample(v)+"\n")
		byFile, err := loadConfig(t, env)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(byValue, byFile) {
			t.Fatalf("%s: the file gives another config", v.FileEnv())
		}
		// AC-3: обе формы — отказ старта с обоими именами.
		env = base()
		env[v.ValueEnv()] = sample(v)
		env[v.FileEnv()] = file(t, sample(v))
		if _, err := loadConfig(t, env); err == nil || !strings.Contains(err.Error(), v.FileEnv()) ||
			strings.Contains(err.Error(), marker) {
			t.Fatalf("both forms: %v", err)
		}
	default:
		t.Fatalf("form %d", v.Form)
	}
}

func checkOffsite(t *testing.T, v secrets.Var) {
	t.Helper()
	env := map[string]string{"BACKUP_S3_BUCKET": "b"}
	for _, o := range secrets.List {
		if o.ReadBy("backup-s3") {
			env[o.ValueEnv()] = "other"
		}
	}
	env[v.ValueEnv()] = marker
	byValue, ok, err := offsite.FromEnv(func(k string) string { return env[k] })
	if !ok || err != nil || !strings.Contains(fmt.Sprintf("%+v", byValue), marker) {
		t.Fatalf("%s is not read by offsite.FromEnv: %v", v.Name, err)
	}
	env[v.ValueEnv()] = ""
	env[v.FileEnv()] = file(t, marker+"\r\n")
	byFile, ok, err := offsite.FromEnv(func(k string) string { return env[k] })
	if !ok || err != nil || !reflect.DeepEqual(byValue, byFile) {
		t.Fatalf("%s: %+v %v", v.FileEnv(), byFile, err)
	}
	env[v.ValueEnv()] = marker
	if _, _, err := offsite.FromEnv(func(k string) string { return env[k] }); err == nil ||
		!strings.Contains(err.Error(), v.FileEnv()) || strings.Contains(err.Error(), marker) {
		t.Fatalf("both forms: %v", err)
	}
}

// AC-7: пустые обе формы ключа — сообщение называет обе.
func TestOffsiteEmptyKeyNamesBothForms(t *testing.T) {
	_, ok, err := offsite.FromEnv(func(k string) string {
		return map[string]string{"BACKUP_S3_BUCKET": "b", "BACKUP_S3_ACCESS_KEY_ID": "k"}[k]
	})
	if !ok || err == nil {
		t.Fatal("no refusal")
	}
	for _, name := range []string{"BACKUP_S3_SECRET_ACCESS_KEY ", "BACKUP_S3_SECRET_ACCESS_KEY_FILE"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%q not in %s", name, err)
		}
	}
}

// AC-6: запасные AWS_* — только при пустых обеих формах SDLC_BLOB_S3_*.
func TestBlobS3FallsBackToAWS(t *testing.T) {
	cfg, err := loadConfig(t, map[string]string{"AWS_SECRET_ACCESS_KEY": "aws-key"})
	if err != nil || cfg.BlobS3.SecretKey != "aws-key" {
		t.Fatalf("fallback: %q %v", cfg.BlobS3.SecretKey, err)
	}
	cfg, err = loadConfig(t, map[string]string{
		"AWS_SECRET_ACCESS_KEY": "aws-key", "SDLC_BLOB_S3_SECRET_ACCESS_KEY_FILE": file(t, "file-key\n"),
	})
	if err != nil || cfg.BlobS3.SecretKey != "file-key" {
		t.Fatalf("file over AWS: %q %v", cfg.BlobS3.SecretKey, err)
	}
}

// AC-3: токен репозитория в обеих формах — ошибка клиента с обоими именами,
// общий токен не подставляется.
func TestRepositoryTokenBothForms(t *testing.T) {
	t.Setenv("SDLC_GITHUB_TOKEN_ACME", marker)
	t.Setenv("SDLC_GITHUB_TOKEN_ACME_FILE", file(t, marker))
	p := github.NewProvider(github.New("http://127.0.0.1:1", "instance-token"), nil, github.SecretFromEnv)
	_, err := p.Client(t.Context(), github.Credentials{SecretRef: "acme"})
	if err == nil || !strings.Contains(err.Error(), "SDLC_GITHUB_TOKEN_ACME ") ||
		!strings.Contains(err.Error(), "SDLC_GITHUB_TOKEN_ACME_FILE") || strings.Contains(err.Error(), marker) {
		t.Fatalf("both forms: %v", err)
	}
}

// AC-4: плохой файл секрета останавливает config.Load с именем и путём.
func TestLoadStopsOnBadFile(t *testing.T) {
	_, err := loadConfig(t, map[string]string{"SDLC_ADMIN_TOKEN_FILE": "/secrets/missing"})
	if err == nil || !strings.Contains(err.Error(), "SDLC_ADMIN_TOKEN_FILE") || !strings.Contains(err.Error(), "/secrets/missing") {
		t.Fatalf("%v", err)
	}
}

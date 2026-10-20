package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore/s3test"
)

func s3Env(t *testing.T, url string) {
	t.Helper()
	for k, v := range map[string]string{
		"BACKUP_S3_BUCKET": "bk", "BACKUP_S3_ENDPOINT": url, "BACKUP_S3_REGION": "us-east-1",
		"BACKUP_S3_PREFIX": "nightly", "BACKUP_S3_ACCESS_KEY_ID": "AKIAEXAMPLE",
		"BACKUP_S3_SECRET_ACCESS_KEY": "very-secret", "BACKUP_S3_KEEP": "1", "BACKUP_S3_PATH_STYLE": "",
	} {
		t.Setenv(k, v)
	}
}

func TestBackupS3Usage(t *testing.T) {
	var out, errw bytes.Buffer
	if code := backupS3([]string{"--help"}, &out, &errw); code != 0 || !strings.Contains(out.String(), "BACKUP_S3_KEEP") {
		t.Fatalf("help: %d %s", code, out.String())
	}
	if code := backupS3([]string{"nope"}, &out, &errw); code != 2 {
		t.Fatalf("unknown: %d", code)
	}
	t.Setenv("BACKUP_S3_BUCKET", "")
	errw.Reset()
	if code := backupS3([]string{"list"}, &out, &errw); code != 1 || !strings.Contains(errw.String(), "BACKUP_S3_BUCKET is not set") {
		t.Fatalf("no bucket: %d %s", code, errw.String())
	}
}

// put → list → get по подставному бакету: вывод с префиксами скриптов,
// имя дампа в stdout у get, ни строки с секретом.
func TestBackupS3PutListGet(t *testing.T) {
	fake := s3test.New("bk", "us-east-1", "very-secret")
	server := httptest.NewServer(fake)
	defer server.Close()
	s3Env(t, server.URL)
	dir := t.TempDir()
	for _, stamp := range []string{"20261019-030000", "20261020-030000"} {
		dump := filepath.Join(dir, "cadence-v0.10.0-"+stamp+".sql.gz")
		os.WriteFile(dump, []byte("dump "+stamp), 0o600)
		var out, errw bytes.Buffer
		if code := backupS3([]string{"put", dump}, &out, &errw); code != 0 {
			t.Fatalf("put: %d %s", code, errw.String())
		}
		if !strings.Contains(out.String(), "✓ uploaded cadence-v0.10.0-"+stamp+".sql.gz, cadence-v0.10.0-"+stamp+".sha256 to s3://bk/nightly/") {
			t.Fatalf("put output: %s", out.String())
		}
		if stamp == "20261020-030000" && !strings.Contains(out.String(), "keeps 1 of BACKUP_S3_KEEP=1 backups; removed cadence-v0.10.0-20261019-030000") {
			t.Fatalf("keep: %s", out.String())
		}
	}
	var out, errw bytes.Buffer
	if code := backupS3([]string{"list"}, &out, &errw); code != 0 || strings.Count(out.String(), "\n") != 1 ||
		!strings.HasPrefix(out.String(), "cadence-v0.10.0-20261020-030000") {
		t.Fatalf("list: %d %q %s", code, out.String(), errw.String())
	}
	into := t.TempDir()
	out.Reset()
	errw.Reset()
	if code := backupS3([]string{"get", "--dir", into, "--version", "v0.10.0"}, &out, &errw); code != 0 ||
		out.String() != "cadence-v0.10.0-20261020-030000.sql.gz\n" || !strings.Contains(errw.String(), "the checksums match") {
		t.Fatalf("get: %d %q %s", code, out.String(), errw.String())
	}
	if code := backupS3([]string{"get", "--dir", into, "--version", "v0.9.0"}, &out, &errw); code != 3 {
		t.Fatalf("get of a missing version: %d", code)
	}

	// Неверный ключ: код 1, причина от S3, секрета в выводе нет.
	t.Setenv("BACKUP_S3_SECRET_ACCESS_KEY", "wrong-secret")
	out.Reset()
	errw.Reset()
	dump := filepath.Join(dir, "cadence-v0.10.0-20261021-030000.sql.gz")
	os.WriteFile(dump, []byte("dump"), 0o600)
	if code := backupS3([]string{"put", dump}, &out, &errw); code != 1 || !strings.Contains(errw.String(), "403") {
		t.Fatalf("wrong key: %d %s", code, errw.String())
	}
	if strings.Contains(out.String()+errw.String(), "wrong-secret") {
		t.Fatal("the secret is printed")
	}
}

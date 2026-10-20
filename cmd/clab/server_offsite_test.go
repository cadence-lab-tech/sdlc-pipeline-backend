package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Копия вне машины и verify (CLAB-132): сценарии ci/check-stack.sh без
// Docker — подставной исполнитель отвечает за /api backup-s3, одноразовый
// PostgreSQL и /api dbcheck.

// testEnv10 — testEnv с clab v0.10.0: старший clab не работает со стеком
// новее себя.
func testEnv10(t *testing.T, cwd string, r *fakeRunner, vars map[string]string) (*serverEnv, *bytes.Buffer, *bytes.Buffer) {
	e, out, errw := testEnv(t, cwd, r, vars)
	e.cli = "v0.10.0"
	return e, out, errw
}

func gzipFile(p, content string) {
	os.MkdirAll(filepath.Dir(p), 0o700)
	f, _ := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	gz := gzip.NewWriter(f)
	gz.Write([]byte(content))
	gz.Close()
	f.Close()
}

func blobsArchive(t *testing.T, p string, files int) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "./ab/", Typeflag: tar.TypeDir, Mode: 0o755})
	for n := 0; n < files; n++ {
		body := strings.Repeat("x", n+1)
		tw.WriteHeader(&tar.Header{Name: "./ab/" + sha(body), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	f.Close()
}

const s3EnvLines = "BACKUP_S3_BUCKET=bk\nBACKUP_S3_ENDPOINT=http://minio:9000\nBACKUP_S3_PREFIX=nightly\n" +
	"BACKUP_S3_ACCESS_KEY_ID=AKIAEXAMPLE\nBACKUP_S3_SECRET_ACCESS_KEY=very-secret-key\n"

func withS3(t *testing.T, dir string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, ".env"), readFile(t, filepath.Join(dir, ".env"))+s3EnvLines, 0o600)
}

// Критерии 1, 2, 5, 6, 8: выгрузка после бэкапа именами переменных, без
// значений в командной строке; сбой — код 1 и локальный файл на месте; без
// бакета — ни слова про S3.
func TestServerBackupUpload(t *testing.T) {
	dir := installed(t, "v0.10.0", nil)
	dump := filepath.Join(dir, "backups", "cadence-v0.10.0-20261018-030000.sql.gz")

	// Без бакета — как на v0.9.0.
	r := newFakeRunner()
	e, out, errw := testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 0 {
		t.Fatalf("code %d: %s", code, errw)
	}
	if r.called("docker compose run --rm --no-deps -T --user") || strings.Contains(out.String()+errw.String(), "S3") ||
		strings.Contains(out.String()+errw.String(), "upload") {
		t.Fatalf("without a bucket: %v\n%s%s", r.calls, out, errw)
	}

	withS3(t, dir)
	r = newFakeRunner()
	e, out, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 0 {
		t.Fatalf("upload: %d %s", code, errw)
	}
	put := "docker compose run --rm --no-deps -T --user 1000:1000 -e BACKUP_S3_BUCKET -e BACKUP_S3_ENDPOINT -e BACKUP_S3_REGION " +
		"-e BACKUP_S3_PREFIX -e BACKUP_S3_ACCESS_KEY_ID -e BACKUP_S3_SECRET_ACCESS_KEY -e BACKUP_S3_PATH_STYLE -e BACKUP_S3_KEEP " +
		"-v " + filepath.Join(dir, "backups") + ":/backups api backup-s3 put /backups/cadence-v0.10.0-20261018-030000.sql.gz"
	if !r.called(put) {
		t.Fatalf("no put: %v", r.calls)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "very-secret-key") {
			t.Fatalf("the secret on a command line: %s", c)
		}
	}
	found := false
	for _, kv := range r.envs[put] {
		found = found || kv == "BACKUP_S3_SECRET_ACCESS_KEY=very-secret-key"
	}
	if !found {
		t.Fatal("the secret does not reach docker through its environment")
	}
	if !strings.Contains(out.String(), "▸ uploading cadence-v0.10.0-20261018-030000.sql.gz to s3://bk/nightly/") ||
		strings.Index(out.String(), "✓ 2026-10-18 03:00:00") > strings.Index(out.String(), "▸ uploading") {
		t.Fatalf("output: %s", out)
	}

	// Сбой выгрузки: код 1, локальный бэкап на месте.
	os.Remove(dump)
	r = newFakeRunner()
	r.s3PutFails = true
	e, _, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 1 {
		t.Fatalf("failed upload: code %d", code)
	}
	if !isFile(dump) || !strings.Contains(errw.String(), "✗ the off-machine copy of cadence-v0.10.0-20261018-030000.sql.gz to s3://bk/nightly/ failed (see above); the local backup is kept: "+dump) {
		t.Fatalf("failed upload: %s", errw)
	}

	// --no-upload — только локально.
	r = newFakeRunner()
	e, _, _ = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "--no-upload"}); code != 0 || r.called("docker compose run --rm --no-deps -T --user") {
		t.Fatalf("--no-upload: %d %v", code, r.calls)
	}
	if code := runServer(context.Background(), e, []string{"backup", "verify", "--no-upload"}); code != 2 {
		t.Fatalf("verify --no-upload: %d", code)
	}

	// Образ без /api backup-s3: понятный отказ, локальный бэкап сделан.
	old := installed(t, "v0.9.0", nil)
	withS3(t, old)
	r = newFakeRunner()
	e, _, errw = testEnv10(t, old, r, nil)
	if code := runServer(context.Background(), e, []string{"backup"}); code != 1 ||
		!strings.Contains(errw.String(), "need Cadence Lab v0.10.0 or newer; this installation is on v0.9.0") ||
		!isFile(filepath.Join(old, "backups", "cadence-v0.9.0-20261018-030000.sql.gz")) {
		t.Fatalf("old image: %s", errw)
	}
}

// Критерий 7 (q2): сбой выгрузки бэкапа перед апгрейдом — предупреждение,
// апгрейд идёт дальше.
func TestServerUpgradeUploadWarns(t *testing.T) {
	f := upgradeSetup(t, map[string]string{"BACKUP_S3_BUCKET": "bk", "BACKUP_S3_ACCESS_KEY_ID": "k", "BACKUP_S3_SECRET_ACCESS_KEY": "s"})
	if code := f.upgrade(); code != 0 {
		t.Fatalf("upgrade: %s", f.errw)
	}
	if !strings.Contains(f.errw.String(), "! off-machine backups (BACKUP_S3_BUCKET) need Cadence Lab v0.10.0") ||
		!strings.Contains(f.errw.String(), "! the upgrade goes on: its rollback uses the local backup") {
		t.Fatalf("warning: %s", f.errw)
	}
	if f.version(t) != "v0.6.1" {
		t.Fatalf("VERSION %s", f.version(t))
	}
}

func verifySetup(t *testing.T) (string, string) {
	t.Helper()
	dir := installed(t, "v0.10.0", nil)
	dump := filepath.Join(dir, "backups", "cadence-v0.10.0-20261018-030000.sql.gz")
	gzipFile(filepath.Join(dir, "backups", "cadence-v0.10.0-20261017-030000.sql.gz"), "-- older\n")
	gzipFile(dump, "-- PostgreSQL database dump\n")
	blobsArchive(t, strings.TrimSuffix(dump, ".sql.gz")+".blobs.tar.gz", 3)
	return dir, dump
}

// cleanedUp — одноразовые контейнер с томом и сеть удалены.
func cleanedUp(r *fakeRunner) bool {
	return r.called("docker rm -f -v cadence-verify-") && r.called("docker network rm cadence-verify-")
}

// Критерии 9-11, 13, 15: самый новый локальный бэкап этой версии грузится в
// одноразовый PostgreSQL, схема сверяется dbcheck образа, всё временное
// удаляется; рабочий стек не останавливается.
func TestServerBackupVerifyLocal(t *testing.T) {
	dir, _ := verifySetup(t)
	r := newFakeRunner()
	e, out, errw := testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify"}); code != 0 {
		t.Fatalf("verify: %d %s", code, errw)
	}
	for _, want := range []string{
		"▸ verifying cadence-v0.10.0-20261018-030000.sql.gz (",
		") from backups/cadence-v0.10.0-20261018-030000.sql.gz",
		"▸ loading it into a throwaway PostgreSQL (pgvector/pgvector:pg17); the stack keeps running",
		"✓ loaded in 0s",
		"✓ schema 51: the schema of the images of v0.10.0",
		"✓ blobs archive cadence-v0.10.0-20261018-030000.blobs.tar.gz: 3 files",
		"✓ backup cadence-v0.10.0-20261018-030000.sql.gz restores: schema 51, 2 projects, 5 tasks, 9 artifacts",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if !cleanedUp(r) {
		t.Errorf("not cleaned up: %v", r.calls)
	}
	for _, c := range r.calls {
		if strings.Contains(c, "compose stop") || strings.Contains(c, "compose up") || strings.Contains(c, "exec -T postgres") {
			t.Errorf("verify touched the stack: %s", c)
		}
	}
	load := ""
	for c, in := range r.stdin {
		if strings.HasPrefix(c, "docker exec -i cadence-verify-") {
			load = in
		}
	}
	if load != "-- PostgreSQL database dump\n" {
		t.Errorf("loaded %q", load)
	}
	if !r.called("docker run -d --name cadence-verify-") || !r.called("docker network create --label com.cadence.verify=cadence cadence-verify-") {
		t.Errorf("throwaway database: %v", r.calls)
	}
	dbcheck := ""
	for _, c := range r.calls {
		if strings.HasSuffix(c, "dbcheck --json") {
			dbcheck = c
		}
	}
	if !strings.Contains(dbcheck, "ghcr.io/cadence-lab-tech/sdlc-pipeline-backend:v0.10.0 dbcheck --json") || strings.Contains(dbcheck, "postgres://") {
		t.Errorf("dbcheck: %s", dbcheck)
	}

	// Явный файл.
	r = newFakeRunner()
	e, out, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify", "backups/cadence-v0.10.0-20261017-030000.sql.gz"}); code != 0 ||
		!strings.Contains(out.String(), "restores: schema 51") ||
		!strings.Contains(errw.String(), "! no cadence-v0.10.0-20261017-030000.blobs.tar.gz next to the dump: this backup has no blobs volume") {
		t.Errorf("explicit file: %d %s", code, out)
	}
}

// Критерии 11, 12, 14: отказы — код 1, одноразовое удалено.
func TestServerBackupVerifyFailures(t *testing.T) {
	dir, _ := verifySetup(t)

	r := newFakeRunner()
	r.verifyLoadFails = true
	e, _, errw := testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify"}); code != 1 ||
		!strings.Contains(errw.String(), "✗ backup cadence-v0.10.0-20261018-030000.sql.gz does not restore: loading it failed") || !cleanedUp(r) {
		t.Errorf("load fails: %d %s %v", code, errw, r.calls)
	}

	r = newFakeRunner()
	r.dbcheckJSON = `{"ok": true, "schema": "cadence", "schema_version": 49, "image_schema_version": 51}`
	e, _, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify"}); code != 1 ||
		!strings.Contains(errw.String(), "does not match its version: schema 49, the images of v0.10.0 apply up to 51") || !cleanedUp(r) {
		t.Errorf("schema mismatch: %d %s", code, errw)
	}
	r = newFakeRunner()
	r.dbcheckJSON = `{"ok": true, "schema": "cadence", "schema_version": 49, "image_schema_version": 51}`
	e, _, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify", "--ignore-version"}); code != 0 ||
		!strings.Contains(errw.String(), "! schema 49: the images of v0.10.0 apply up to 51 (--ignore-version)") {
		t.Errorf("--ignore-version: %d %s", code, errw)
	}

	r = newFakeRunner()
	r.dbcheckJSON = `{"ok": false, "schema": "empty", "schema_version": 0, "image_schema_version": 51}`
	e, out, errw := testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify"}); code != 1 ||
		!strings.Contains(errw.String(), "the images of v0.10.0 do not accept its schema") || !strings.Contains(out.String(), "✗ schema") || !cleanedUp(r) {
		t.Errorf("dbcheck fails: %d %s %s", code, out, errw)
	}

	// Бэкап другой версии — отказ с подсказкой, как у restore; ничего не
	// запускалось.
	gzipFile(filepath.Join(dir, "backups", "cadence-v0.9.0-20261001-030000.sql.gz"), "-- old\n")
	r = newFakeRunner()
	e, _, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify", "backups/cadence-v0.9.0-20261001-030000.sql.gz"}); code != 1 ||
		!strings.Contains(errw.String(), "the backup was taken on v0.9.0, the package is on v0.10.0") ||
		!strings.Contains(errw.String(), "--ignore-version") || !strings.Contains(errw.String(), "cadence-self-hosted-v0.9.0.tar.gz") ||
		r.called("docker run -d") {
		t.Errorf("other version: %d %s", code, errw)
	}

	// Нечего проверять.
	empty := installed(t, "v0.10.0", nil)
	r = newFakeRunner()
	e, _, errw = testEnv10(t, empty, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify"}); code != 1 ||
		!strings.Contains(errw.String(), "✗ nothing to verify: no backup of v0.10.0 in backups/") {
		t.Errorf("nothing: %d %s", code, errw)
	}
}

// Критерии 9, 13: с бакетом verify берёт бэкап оттуда во временный каталог
// и убирает его; бакет без бэкапа этой версии — предупреждение и локальный.
func TestServerBackupVerifyFromBucket(t *testing.T) {
	dir, _ := verifySetup(t)
	withS3(t, dir)
	r := newFakeRunner()
	r.s3GetName = "cadence-v0.10.0-20261019-030000.sql.gz"
	e, out, errw := testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify"}); code != 0 {
		t.Fatalf("verify: %d %s", code, errw)
	}
	if !strings.Contains(out.String(), "▸ verifying cadence-v0.10.0-20261019-030000.sql.gz (") ||
		!strings.Contains(out.String(), ") from s3://bk/nightly/") ||
		!strings.Contains(out.String(), "✓ backup cadence-v0.10.0-20261019-030000.sql.gz restores") {
		t.Errorf("output: %s", out)
	}
	if !r.called("docker compose run --rm --no-deps -T --user 1000:1000") {
		t.Errorf("no get: %v", r.calls)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "backups", ".verify-*")); len(left) != 0 {
		t.Errorf("the download is left: %v", left)
	}

	r = newFakeRunner()
	r.s3GetCode = 3
	e, out, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify"}); code != 0 ||
		!strings.Contains(errw.String(), "! no backup of v0.10.0 in s3://bk/nightly/: verifying the newest local one") ||
		!strings.Contains(out.String(), "from backups/cadence-v0.10.0-20261018-030000.sql.gz") {
		t.Errorf("fallback: %d %s %s", code, out, errw)
	}

	r = newFakeRunner()
	r.s3GetCode = 1
	e, _, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "verify"}); code != 1 ||
		!strings.Contains(errw.String(), "fetching the backup from s3://bk/nightly/ failed") || r.called("docker run -d") {
		t.Errorf("checksum or network: %d %s", code, errw)
	}
}

// fetch (q4): бэкап из бакета в backups/ и команда восстановления.
func TestServerBackupFetch(t *testing.T) {
	dir := installed(t, "v0.10.0", nil)
	r := newFakeRunner()
	e, _, errw := testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "fetch"}); code != 1 || !strings.Contains(errw.String(), "BACKUP_S3_BUCKET is not set") {
		t.Fatalf("no bucket: %d %s", code, errw)
	}
	withS3(t, dir)
	r = newFakeRunner()
	r.s3GetName = "cadence-v0.10.0-20261019-030000.sql.gz"
	e, out, errw := testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "fetch", "cadence-v0.10.0-20261019-030000"}); code != 0 {
		t.Fatalf("fetch: %d %s", code, errw)
	}
	if !r.called("docker compose run --rm --no-deps -T --user 1000:1000") || !strings.HasSuffix(r.calls[len(r.calls)-1], "backup-s3 get --dir /backups cadence-v0.10.0-20261019-030000") {
		t.Errorf("calls: %v", r.calls)
	}
	if !isFile(filepath.Join(dir, "backups", "cadence-v0.10.0-20261019-030000.sql.gz")) ||
		!strings.Contains(out.String(), "✓ backups/cadence-v0.10.0-20261019-030000.sql.gz; restore it with: clab server restore --yes backups/cadence-v0.10.0-20261019-030000.sql.gz") {
		t.Errorf("fetched: %s", out)
	}
	r = newFakeRunner()
	r.s3GetCode = 3
	e, _, errw = testEnv10(t, dir, r, nil)
	if code := runServer(context.Background(), e, []string{"backup", "fetch"}); code != 1 || !strings.Contains(errw.String(), "nothing to fetch from s3://bk/nightly/") {
		t.Errorf("nothing: %d %s", code, errw)
	}
}

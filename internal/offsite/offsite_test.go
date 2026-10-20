package offsite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore/s3test"
)

const testSecret = "s3cr3t/with+chars"

func testStore(t *testing.T, keep int) (*Store, *s3test.Fake) {
	t.Helper()
	fake := s3test.New("bk", "us-east-1", testSecret)
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg := Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bk", Prefix: "cadence/",
		AccessKey: "AKIAEXAMPLE", SecretKey: testSecret, PathStyle: true, Keep: keep}
	st, err := Open(cfg, blobstore.S3Config{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return st, fake
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, ok, err := FromEnv(env(nil)); ok || err != nil {
		t.Fatalf("без бакета: ok=%v err=%v", ok, err)
	}
	cfg, ok, err := FromEnv(env(map[string]string{"BACKUP_S3_BUCKET": "b", "BACKUP_S3_ACCESS_KEY_ID": "k",
		"BACKUP_S3_SECRET_ACCESS_KEY": "s", "BACKUP_S3_PREFIX": "/a/b/"}))
	if !ok || err != nil || cfg.Region != "us-east-1" || cfg.Endpoint != "https://s3.us-east-1.amazonaws.com" ||
		cfg.Prefix != "a/b/" || !cfg.PathStyle || cfg.Keep != DefaultKeep {
		t.Fatalf("умолчания: %+v %v", cfg, err)
	}
	for name, m := range map[string]map[string]string{
		"без ключа":    {"BACKUP_S3_BUCKET": "b", "BACKUP_S3_ACCESS_KEY_ID": "k"},
		"KEEP=0":       {"BACKUP_S3_BUCKET": "b", "BACKUP_S3_ACCESS_KEY_ID": "k", "BACKUP_S3_SECRET_ACCESS_KEY": "s", "BACKUP_S3_KEEP": "0"},
		"PATH_STYLE=x": {"BACKUP_S3_BUCKET": "b", "BACKUP_S3_ACCESS_KEY_ID": "k", "BACKUP_S3_SECRET_ACCESS_KEY": "s", "BACKUP_S3_PATH_STYLE": "x"},
	} {
		_, ok, err := FromEnv(env(m))
		if !ok || err == nil {
			t.Errorf("%s: ожидали отказ", name)
		} else if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s: секрет в тексте", name)
		}
	}
}

// Выгрузка: дамп, архив blobs и .sha256 с суммами обоих (критерий 2).
func TestUploadAndFetch(t *testing.T) {
	st, fake := testStore(t, 14)
	ctx := context.Background()
	dir := t.TempDir()
	dump := filepath.Join(dir, "cadence-v0.10.0-20261020-030000.sql.gz")
	write(t, dump, "dump bytes")
	write(t, filepath.Join(dir, "cadence-v0.10.0-20261020-030000.blobs.tar.gz"), "blobs bytes")
	res, err := st.Upload(ctx, dump)
	if err != nil {
		t.Fatal(err)
	}
	want := "cadence/cadence-v0.10.0-20261020-030000.blobs.tar.gz,cadence/cadence-v0.10.0-20261020-030000.sha256,cadence/cadence-v0.10.0-20261020-030000.sql.gz"
	if got := strings.Join(fake.Keys(), ","); got != want {
		t.Fatalf("объекты: %s", got)
	}
	sums, _ := fake.Get("cadence/cadence-v0.10.0-20261020-030000.sha256")
	if string(sums) != sum("dump bytes")+"  cadence-v0.10.0-20261020-030000.sql.gz\n"+sum("blobs bytes")+"  cadence-v0.10.0-20261020-030000.blobs.tar.gz\n" {
		t.Fatalf(".sha256: %q", sums)
	}
	if res.Files[len(res.Files)-1] != "cadence-v0.10.0-20261020-030000.sha256" {
		t.Fatalf("сумма не последней: %v", res.Files)
	}
	// Сумма последней и по сети: бэкап без неё — недоделанный.
	last := fake.Requests[len(fake.Requests)-2] // перед листингом хранения
	if !strings.HasPrefix(last, "PUT cadence/cadence-v0.10.0-20261020-030000.sha256") {
		t.Fatalf("последний PUT: %s (%v)", last, fake.Requests)
	}

	// Fetch: файлы встают на место, суммы сходятся.
	into := t.TempDir()
	b, err := st.Pick(ctx, "", "v0.10.0")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.Fetch(ctx, b, into)
	if err != nil {
		t.Fatal(err)
	}
	if got.Dump != filepath.Join(into, "cadence-v0.10.0-20261020-030000.sql.gz") || got.Blobs == "" {
		t.Fatalf("fetched: %+v", got)
	}
	if fi, _ := os.Stat(got.Dump); fi.Mode().Perm() != 0o600 {
		t.Fatalf("права дампа: %v", fi.Mode())
	}

	// Испорченный объект: отказ по сумме, файла нет — ни целого, ни .part
	// (критерий 12).
	fake.Put("cadence/cadence-v0.10.0-20261020-030000.sql.gz", []byte("tampered"))
	into = t.TempDir()
	_, err = st.Fetch(ctx, b, into)
	var ce *ChecksumError
	if !errors.As(err, &ce) {
		t.Fatalf("испорченный объект: %v", err)
	}
	if entries, _ := os.ReadDir(into); len(entries) != 0 {
		t.Fatalf("после отказа остались файлы: %v", entries)
	}
}

// Хранение: после KEEP+2 бэкапов — ровно KEEP самых новых, со всеми файлами;
// чужие объекты и объекты вне префикса целы (критерий 4).
func TestPrune(t *testing.T) {
	st, fake := testStore(t, 3)
	ctx := context.Background()
	fake.Put("cadence/other.txt", []byte("not ours"))
	fake.Put("cadence/sub/cadence-v0.9.0-20200101-000000.sql.gz", []byte("nested"))
	fake.Put("elsewhere/cadence-v0.9.0-20200101-000000.sql.gz", []byte("outside"))
	// Недоделанный старый бэкап (нет суммы) уходит вместе со старыми.
	fake.Put("cadence/cadence-v0.9.0-20261001-030000.sql.gz", []byte("half"))
	dir := t.TempDir()
	stamps := []string{"20261015-030000", "20261016-030000", "20261017-030000", "20261018-030000", "20261019-030000"}
	var res Uploaded
	for i, s := range stamps {
		// Версия меняется посреди: хранение считает по времени, не по версии
		// (бэкап перед апгрейдом — наравне с ночными, q6).
		v := "v0.9.0"
		if i >= 2 {
			v = "v0.10.0"
		}
		dump := filepath.Join(dir, "cadence-"+v+"-"+s+".sql.gz")
		write(t, dump, "dump "+s)
		var err error
		if res, err = st.Upload(ctx, dump); err != nil {
			t.Fatal(err)
		}
	}
	if res.Kept != 3 || len(res.Removed) != 1 || res.Removed[0] != "cadence-v0.9.0-20261016-030000" {
		t.Fatalf("итог: %+v", res)
	}
	backups, err := st.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var stems []string
	for _, b := range backups {
		stems = append(stems, b.Stem)
		if !b.Complete() {
			t.Errorf("%s неполный", b.Stem)
		}
	}
	if strings.Join(stems, ",") != "cadence-v0.10.0-20261019-030000,cadence-v0.10.0-20261018-030000,cadence-v0.10.0-20261017-030000" {
		t.Fatalf("осталось: %v", stems)
	}
	for _, k := range []string{"cadence/other.txt", "cadence/sub/cadence-v0.9.0-20200101-000000.sql.gz", "elsewhere/cadence-v0.9.0-20200101-000000.sql.gz"} {
		if _, ok := fake.Get(k); !ok {
			t.Errorf("чужой объект %s удалён", k)
		}
	}
	if _, ok := fake.Get("cadence/cadence-v0.9.0-20261001-030000.sql.gz"); ok {
		t.Error("недоделанный старый бэкап остался")
	}
}

// Сбой выгрузки: ошибка с кодом S3, старые копии не тронуты (критерий 5).
func TestUploadFailureKeepsOldCopies(t *testing.T) {
	st, fake := testStore(t, 1)
	ctx := context.Background()
	dir := t.TempDir()
	old := filepath.Join(dir, "cadence-v0.10.0-20261019-030000.sql.gz")
	write(t, old, "old")
	if _, err := st.Upload(ctx, old); err != nil {
		t.Fatal(err)
	}
	fake.Fail = func(r *http.Request) int {
		if r.Method == http.MethodPut {
			return http.StatusForbidden
		}
		return 0
	}
	dump := filepath.Join(dir, "cadence-v0.10.0-20261020-030000.sql.gz")
	write(t, dump, "new")
	_, err := st.Upload(ctx, dump)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("ошибка: %v", err)
	}
	if strings.Contains(err.Error(), testSecret) {
		t.Fatal("секрет в ошибке")
	}
	if _, ok := fake.Get("cadence/cadence-v0.10.0-20261019-030000.sql.gz"); !ok {
		t.Fatal("старая копия удалена после сбоя")
	}
}

// Неверный ключ: подпись не сходится — понятный отказ, без секрета.
func TestWrongSecret(t *testing.T) {
	fake := s3test.New("bk", "us-east-1", "right")
	server := httptest.NewServer(fake)
	defer server.Close()
	st, err := Open(Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "bk", AccessKey: "k", SecretKey: "wrong-secret", PathStyle: true, Keep: 1},
		blobstore.S3Config{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.List(context.Background())
	if err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") || strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("ошибка: %v", err)
	}
}

func TestPick(t *testing.T) {
	st, fake := testStore(t, 14)
	ctx := context.Background()
	for _, k := range []string{
		"cadence-v0.9.0-20261018-030000.sql.gz", "cadence-v0.9.0-20261018-030000.sha256",
		"cadence-v0.10.0-20261019-030000.sql.gz", "cadence-v0.10.0-20261019-030000.sha256",
		"cadence-v0.10.0-20261020-030000.sql.gz", // выгрузка не закончена
	} {
		fake.Put("cadence/"+k, []byte("x"))
	}
	for _, tc := range []struct{ name, version, want string }{
		{"", "", "cadence-v0.10.0-20261019-030000"},
		{"", "v0.9.0", "cadence-v0.9.0-20261018-030000"},
		{"cadence-v0.9.0-20261018-030000.sql.gz", "", "cadence-v0.9.0-20261018-030000"},
		{"backups/cadence-v0.9.0-20261018-030000", "", "cadence-v0.9.0-20261018-030000"},
	} {
		b, err := st.Pick(ctx, tc.name, tc.version)
		if err != nil || b.Stem != tc.want {
			t.Errorf("Pick(%q, %q) = %s, %v; ожидали %s", tc.name, tc.version, b.Stem, err, tc.want)
		}
	}
	if _, err := st.Pick(ctx, "", "v0.8.0"); !errors.Is(err, ErrNone) {
		t.Errorf("нет версии: %v", err)
	}
	if _, err := st.Pick(ctx, "cadence-v0.10.0-20261020-030000", ""); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("недоделанный: %v", err)
	}
}

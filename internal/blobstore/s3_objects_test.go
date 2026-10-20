package blobstore_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore/s3test"
)

const objSecret = "секрет/со+спецсимволами"

func newObjectS3(t *testing.T) (*blobstore.S3, *s3test.Fake) {
	t.Helper()
	fake := s3test.New("backups", "eu-central-1", objSecret)
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	s3, err := blobstore.NewS3(blobstore.S3Config{
		Endpoint: server.URL, Region: "eu-central-1", Bucket: "backups",
		AccessKey: "AKIAEXAMPLE", SecretKey: objSecret, PathStyle: true, HTTP: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s3, fake
}

// Дамп больше части уходит multipart'ом: в одном запросе не больше части,
// собранный объект совпадает с исходным байт в байт (CLAB-132, критерий 3).
func TestPutObjectMultipart(t *testing.T) {
	s3, fake := newObjectS3(t)
	ctx := context.Background()
	content := make([]byte, 2*blobstore.MinPartSize+12345)
	rand.Read(content)
	if err := s3.PutObject(ctx, "nightly/dump.sql.gz", bytes.NewReader(content), int64(len(content)), blobstore.MinPartSize); err != nil {
		t.Fatal(err)
	}
	got, ok := fake.Get("nightly/dump.sql.gz")
	if !ok || !bytes.Equal(got, content) {
		t.Fatalf("собранный объект не совпал: %d байт против %d", len(got), len(content))
	}
	if fake.MaxBody > blobstore.MinPartSize {
		t.Fatalf("тело запроса %d байт больше части %d: выгрузка держала дамп в памяти", fake.MaxBody, blobstore.MinPartSize)
	}
	parts := 0
	for _, r := range fake.Requests {
		if strings.HasPrefix(r, "PUT nightly/dump.sql.gz?partNumber=") {
			parts++
		}
	}
	if parts != 3 {
		t.Fatalf("частей %d, ожидали 3: %v", parts, fake.Requests)
	}

	// Маленький файл — одним PUT.
	if err := s3.PutObject(ctx, "small", strings.NewReader("abc"), 3, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := fake.Get("small"); string(got) != "abc" {
		t.Fatalf("small: %q", got)
	}
	// Часть меньше минимума S3 — отказ до сети.
	if err := s3.PutObject(ctx, "x", strings.NewReader("abc"), 3, 1024); err == nil {
		t.Fatal("часть меньше 5 МиБ принята")
	}
}

// Упавшая часть отменяет выгрузку: брошенных частей не остаётся, объекта нет.
func TestPutObjectMultipartAborts(t *testing.T) {
	s3, fake := newObjectS3(t)
	fake.Fail = func(r *http.Request) int {
		if r.URL.Query().Get("partNumber") == "2" {
			return http.StatusInternalServerError
		}
		return 0
	}
	content := make([]byte, 2*blobstore.MinPartSize)
	err := s3.PutObject(context.Background(), "dump", bytes.NewReader(content), int64(len(content)), blobstore.MinPartSize)
	var oe *blobstore.ObjectError
	if !errors.As(err, &oe) || !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("ошибка: %v", err)
	}
	// Текст — статус и код S3, без тела ответа целиком (там ключ доступа).
	if strings.Contains(err.Error(), "AKIAEXAMPLE") {
		t.Fatalf("в ошибке ключ доступа: %v", err)
	}
	if _, ok := fake.Get("dump"); ok {
		t.Fatal("объект появился")
	}
	aborted := false
	for _, r := range fake.Requests {
		aborted = aborted || strings.HasPrefix(r, "DELETE dump?uploadId=")
	}
	if !aborted {
		t.Fatalf("выгрузка не отменена: %v", fake.Requests)
	}
}

// Листинг — постранично, только прямо под префиксом; подпись запроса с
// параметрами сходится (fake проверяет её из разобранной строки запроса).
func TestListGetDeleteObjects(t *testing.T) {
	s3, fake := newObjectS3(t)
	fake.PageSize = 2
	ctx := context.Background()
	for _, k := range []string{"nightly backups/a", "nightly backups/b~1", "nightly backups/c", "nightly backups/sub/d", "other/e"} {
		fake.Put(k, []byte(k))
	}
	objs, err := s3.ListObjects(ctx, "nightly backups/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "nightly backups/a,nightly backups/b~1,nightly backups/c" {
		t.Fatalf("листинг: %v", keys)
	}
	var buf bytes.Buffer
	if err := s3.GetObject(ctx, "nightly backups/b~1", &buf); err != nil || buf.String() != "nightly backups/b~1" {
		t.Fatalf("get: %v %q", err, buf.String())
	}
	if err := s3.GetObject(ctx, "missing", &buf); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s3.DeleteObject(ctx, "nightly backups/a"); err != nil {
		t.Fatal(err)
	}
	if err := s3.DeleteObject(ctx, "nightly backups/a"); err != nil {
		t.Fatalf("повторное удаление: %v", err)
	}
	if _, ok := fake.Get("nightly backups/a"); ok {
		t.Fatal("не удалён")
	}
}

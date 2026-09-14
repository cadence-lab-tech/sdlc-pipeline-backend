package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeS3 — минимальный бакет в памяти. Он же проверяет подпись: канонический
// запрос собирается заново из того, что реально пришло по сети, поэтому
// расхождение между подписанным и отправленным ловится здесь.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    int
	deletes int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := verifySignature(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[r.URL.Path] = body
		f.puts++
	case http.MethodGet, http.MethodHead:
		content, ok := f.objects[r.URL.Path]
		if !ok {
			http.Error(w, "NoSuchKey", http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			w.Write(content)
		}
	case http.MethodDelete:
		// Настоящий S3 отвечает 204 и на удаление того, чего не было.
		delete(f.objects, r.URL.Path)
		f.deletes++
		w.WriteHeader(http.StatusNoContent)
	}
}

func verifySignature(r *http.Request) error {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, s3Algorithm+" ") {
		return errors.New("нет подписи")
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	sum := sha256.Sum256(body)
	if got := r.Header.Get("X-Amz-Content-Sha256"); got != hex.EncodeToString(sum[:]) {
		return errors.New("x-amz-content-sha256 не сходится с телом")
	}

	var credential, signedHeaders, signature string
	for _, part := range strings.Split(strings.TrimPrefix(auth, s3Algorithm+" "), ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch name {
		case "Credential":
			credential = value
		case "SignedHeaders":
			signedHeaders = value
		case "Signature":
			signature = value
		}
	}
	_, scope, ok := strings.Cut(credential, "/")
	if !ok {
		return errors.New("сломанный Credential")
	}

	var headers strings.Builder
	for _, name := range strings.Split(signedHeaders, ";") {
		value := r.Header.Get(name)
		if name == "host" {
			value = r.Host
		}
		headers.WriteString(name + ":" + strings.TrimSpace(value) + "\n")
	}
	canonical := strings.Join([]string{
		r.Method, canonicalPath(r.URL.Path), r.URL.RawQuery,
		headers.String(), signedHeaders, hex.EncodeToString(sum[:]),
	}, "\n")
	hash := sha256.Sum256([]byte(canonical))
	stamp := r.Header.Get("X-Amz-Date")
	toSign := strings.Join([]string{s3Algorithm, stamp, scope, hex.EncodeToString(hash[:])}, "\n")

	key := hmacSHA256([]byte("AWS4"+testSecret), stamp[:8])
	key = hmacSHA256(key, "eu-central-1")
	key = hmacSHA256(key, s3Service)
	key = hmacSHA256(key, "aws4_request")
	if hex.EncodeToString(hmacSHA256(key, toSign)) != signature {
		return errors.New("подпись не сходится")
	}
	return nil
}

const testSecret = "секрет/со+спецсимволами"

func newTestS3(t *testing.T) (*S3, *fakeS3) {
	t.Helper()
	bucket := &fakeS3{objects: map[string][]byte{}}
	server := httptest.NewServer(bucket)
	t.Cleanup(server.Close)

	s3, err := NewS3(S3Config{
		Endpoint:  server.URL,
		Region:    "eu-central-1",
		Bucket:    "sdlc-blobs",
		Prefix:    "artifacts/",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: testSecret,
		PathStyle: true,
		HTTP:      server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s3, bucket
}

func TestS3RoundTrip(t *testing.T) {
	s3, bucket := newTestS3(t)
	ctx := context.Background()
	content := []byte("большой артефакт")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	uri, err := s3.Put(ctx, sha, content, "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	if want := "s3://sdlc-blobs/artifacts/" + sha[:2] + "/" + sha; uri != want {
		t.Fatalf("адрес %q, ожидали %q", uri, want)
	}
	// Путь в бакете path-style: endpoint/bucket/key.
	if _, ok := bucket.objects["/sdlc-blobs/artifacts/"+sha[:2]+"/"+sha]; !ok {
		t.Fatalf("объект лёг не туда: %v", keysOf(bucket))
	}

	// Адресация по содержимому: второй Put те же байты не заливает.
	again, err := s3.Put(ctx, sha, content, "text/markdown")
	if err != nil || again != uri {
		t.Fatalf("повторная запись: %v, %q против %q", err, again, uri)
	}
	if bucket.puts != 1 {
		t.Fatalf("заливок %d, ожидали одну", bucket.puts)
	}

	got, err := s3.Get(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("прочитано не то: %q", got)
	}
}

func TestS3RejectsForeignAddresses(t *testing.T) {
	s3, _ := newTestS3(t)
	ctx := context.Background()
	missing := hex.EncodeToString(make([]byte, 32))

	if _, err := s3.Get(ctx, "s3://sdlc-blobs/artifacts/00/"+missing); !errors.Is(err, ErrNotFound) {
		t.Fatalf("отсутствующее содержимое: ожидали ErrNotFound, получили %v", err)
	}
	// Чужой бакет и файловый адрес — не наши данные, отказываем, а не молчим.
	if _, err := s3.Get(ctx, "s3://other/artifacts/00/"+missing); err == nil {
		t.Fatal("чужой бакет должен отбиваться")
	}
	if _, err := s3.Get(ctx, "file://"+missing); err == nil {
		t.Fatal("файловый адрес должен отбиваться")
	}
}

func TestS3VirtualHostAddressing(t *testing.T) {
	s3, err := NewS3(S3Config{
		Endpoint:  "https://s3.eu-central-1.amazonaws.com",
		Region:    "eu-central-1",
		Bucket:    "sdlc-blobs",
		AccessKey: "AKIAEXAMPLE",
		SecretKey: testSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := s3.request(context.Background(), http.MethodGet, "aa/bb", nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "sdlc-blobs.s3.eu-central-1.amazonaws.com" || req.URL.Path != "/aa/bb" {
		t.Fatalf("адрес без path-style: %s", req.URL)
	}
	// Подпись покрывает host: иначе запрос уходит в один бакет, а подписан на другой.
	if !strings.Contains(req.Header.Get("Authorization"), "SignedHeaders=host;") {
		t.Fatalf("host не подписан: %s", req.Header.Get("Authorization"))
	}
}

func TestS3NeedsFullConfiguration(t *testing.T) {
	full := S3Config{Endpoint: "http://minio:9000", Region: "us-east-1",
		Bucket: "b", AccessKey: "k", SecretKey: "s"}
	for name, broken := range map[string]func(*S3Config){
		"без бакета":  func(c *S3Config) { c.Bucket = "" },
		"без адреса":  func(c *S3Config) { c.Endpoint = "" },
		"без региона": func(c *S3Config) { c.Region = "" },
		"без ключа":   func(c *S3Config) { c.SecretKey = "" },
	} {
		cfg := full
		broken(&cfg)
		if _, err := NewS3(cfg); err == nil {
			t.Fatalf("%s: ожидали отказ", name)
		}
	}
}

func keysOf(b *fakeS3) []string {
	var keys []string
	for k := range b.objects {
		keys = append(keys, k)
	}
	return keys
}

// Сборка мусора приходит в хранилище после удаления строки в blobs: адрес
// должен разбираться в тот же ключ, что писал Put, а повтор — быть безвредным.
func TestS3Delete(t *testing.T) {
	s3, bucket := newTestS3(t)
	ctx := context.Background()
	content := []byte("осиротевший артефакт")
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	uri, err := s3.Put(ctx, sha, content, "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	if err := s3.Delete(ctx, uri); err != nil {
		t.Fatal(err)
	}
	if _, ok := bucket.objects["/sdlc-blobs/artifacts/"+sha[:2]+"/"+sha]; ok {
		t.Fatalf("объект остался: %v", keysOf(bucket))
	}
	if _, err := s3.Get(ctx, uri); !errors.Is(err, ErrNotFound) {
		t.Fatalf("после удаления: ожидали ErrNotFound, получили %v", err)
	}
	if err := s3.Delete(ctx, uri); err != nil {
		t.Fatalf("повторное удаление: %v", err)
	}
	if bucket.deletes != 2 {
		t.Fatalf("удалений %d, ожидали два", bucket.deletes)
	}
	// Чужой бакет и файловый адрес отбиваются так же, как в Get.
	if err := s3.Delete(ctx, "s3://other/artifacts/00/"+sha); err == nil {
		t.Fatal("чужой бакет должен отбиваться")
	}
	if err := s3.Delete(ctx, "file://"+sha); err == nil {
		t.Fatal("файловый адрес должен отбиваться")
	}
}

// 404 — успех: объекта нет, а сборщику нужно именно это. Всё остальное —
// ошибка, иначе утечка места прошла бы незамеченной.
func TestS3DeleteStatuses(t *testing.T) {
	sha := hex.EncodeToString(make([]byte, 32))
	for status, wantErr := range map[int]bool{
		http.StatusNoContent:           false,
		http.StatusOK:                  false,
		http.StatusNotFound:            false,
		http.StatusForbidden:           true,
		http.StatusInternalServerError: true,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		s3, err := NewS3(S3Config{
			Endpoint: server.URL, Region: "eu-central-1", Bucket: "sdlc-blobs",
			AccessKey: "AKIAEXAMPLE", SecretKey: testSecret, PathStyle: true,
			HTTP: server.Client(),
		})
		if err != nil {
			t.Fatal(err)
		}
		err = s3.Delete(context.Background(), "s3://sdlc-blobs/00/"+sha)
		if (err != nil) != wantErr {
			t.Fatalf("код %d: получили %v, ошибка ожидалась: %v", status, err, wantErr)
		}
		server.Close()
	}
}

package blobstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// sinkTransport — хранилище, которое тело записи только хэширует: так
// видно, сколько памяти тратит на запись сам клиент.
type sinkTransport struct {
	got     string
	payload string
	length  int64
}

func (s *sinkTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp := &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}, Request: r}
	switch r.Method {
	case http.MethodHead:
		resp.StatusCode = http.StatusNotFound
	case http.MethodPut:
		h := sha256.New()
		if _, err := io.Copy(h, r.Body); err != nil {
			return nil, err
		}
		r.Body.Close()
		s.got = hex.EncodeToString(h.Sum(nil))
		s.payload = r.Header.Get("X-Amz-Content-Sha256")
		s.length = r.ContentLength
	}
	return resp, nil
}

// AC-2.1: PutFile подписывает запрос sha256, посчитанным при спуле, и
// гонит файл в сеть потоком — память клиента не растёт на размер файла.
func TestS3PutFileStreamsSignedBody(t *testing.T) {
	const size = 16 << 20
	sink := &sinkTransport{}
	s3, err := NewS3(S3Config{Endpoint: "http://s3.test", Region: "r", Bucket: "b",
		AccessKey: "a", SecretKey: "s", PathStyle: true, HTTP: &http.Client{Transport: sink}})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), ".spool-*")
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(rand.Reader, size)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	sha := hex.EncodeToString(h.Sum(nil))

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := s3.PutFile(context.Background(), sha, f.Name(), size, ""); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)

	if sink.got != sha || sink.payload != sha || sink.length != size {
		t.Fatalf("ушло: тело %s, подпись %s, длина %d; ждали %s и %d", sink.got, sink.payload, sink.length, sha, size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > size/10 {
		t.Fatalf("на запись %d байт выделено %d: файл читается в память", size, allocated)
	}
}

// Зависшее хранилище обрывает чтение по простою, а не держит его вечно:
// общего срока у потока нет, есть срок без единого байта.
func TestS3OpenStalls(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(strings.Repeat("x", 10)))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer server.Close()
	defer close(release)

	s3, err := NewS3(S3Config{Endpoint: server.URL, Region: "r", Bucket: "b",
		AccessKey: "a", SecretKey: "s", PathStyle: true, HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	s3.idle = 200 * time.Millisecond
	r, err := s3.Open(context.Background(), "s3://b/ab/abc")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(r)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("оборванный поток прочитан без ошибки")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("зависшее хранилище держит чтение дольше простоя")
	}
}

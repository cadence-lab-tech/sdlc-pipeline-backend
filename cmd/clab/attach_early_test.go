package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Сервер отказывает, не дочитав тело (403 по кругу записи токена, 400 по
// порядку полей): клиент, который льёт файл потоком, обязан показать ответ
// сервера, а не «broken pipe» или «connection reset».
func TestAttachEarlyServerRefusalIsShown(t *testing.T) {
	withJSON(t, false)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"version":"v0.18.0","limits":{"max_file_bytes":1073741824}}`))
			return
		}
		// Тело не читаем вовсе.
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"задача вне круга записи токена"}}`))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL + "/v1", origin: srv.URL, actor: "user:alice", http: srv.Client()}

	path := filepath.Join(t.TempDir(), "big.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(24 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	for i := 0; i < 5; i++ {
		_, _, err = capture(t, func() error { return cmdAttach(context.Background(), c, []string{"CLAB-9", path}) })
		if err == nil || !strings.Contains(err.Error(), "forbidden: задача вне круга записи токена") {
			t.Fatalf("попытка %d: %v", i, err)
		}
	}
}

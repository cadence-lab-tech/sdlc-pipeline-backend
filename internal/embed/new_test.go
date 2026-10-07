package embed

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
)

func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func openaiConfig(url string) config.Config {
	return config.Config{
		EmbeddingProvider:       "openai",
		EmbeddingURL:            url,
		EmbeddingModel:          "nomic-embed-text",
		EmbeddingBatch:          8,
		EmbeddingSendDimensions: true,
	}
}

// К2.1: модель отдаёт 768 — сборка падает с понятной ошибкой.
func TestNewRejectsWrongDimension(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	f.dim = 768
	log, _ := captureLog()

	e, err := New(context.Background(), openaiConfig(srv.URL+"/v1"), log)
	if err == nil {
		t.Fatalf("модель на 768 прошла пробу: %v", e)
	}
	for _, want := range []string{"nomic-embed-text", "openai", "768", "1024"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("в ошибке нет %q: %v", want, err)
		}
	}
	if len(f.calls()) != 1 {
		t.Fatalf("проба — один запрос, было %d", len(f.calls()))
	}
}

// К2.2: провайдер отвечает 503 — рабочий эмбеддер и предупреждение.
func TestNewToleratesUnavailableProvider(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	f.respond = func(w http.ResponseWriter, _ map[string]any, _ int) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	log, buf := captureLog()

	start := time.Now()
	e, err := New(context.Background(), openaiConfig(srv.URL+"/v1"), log)
	if err != nil || e == nil {
		t.Fatalf("503 при пробе остановил старт: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("проба ждала ретраев %s: старт не должен на них висеть", time.Since(start))
	}
	if len(f.calls()) != 1 {
		t.Fatalf("проба повторялась: %d запросов", len(f.calls()))
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "503") {
		t.Fatalf("в логе нет предупреждения о 503: %s", buf.String())
	}
}

// К2.2: сервер закрыт — тоже рабочий эмбеддер и предупреждение.
func TestNewToleratesClosedServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	log, buf := captureLog()

	e, err := New(context.Background(), openaiConfig(url+"/v1"), log)
	if err != nil || e == nil {
		t.Fatalf("недоступный сервер остановил старт: %v", err)
	}
	if !strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("в логе нет предупреждения: %s", buf.String())
	}
}

// Неверный ключ — ошибка настройки, старт останавливается (решение по
// вопросу аналитика 1).
func TestNewRejectsUnauthorized(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		f, srv := newFakeOpenAI(t)
		f.respond = func(w http.ResponseWriter, _ map[string]any, _ int) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
		}
		log, _ := captureLog()
		if _, err := New(context.Background(), openaiConfig(srv.URL+"/v1"), log); err == nil ||
			!strings.Contains(err.Error(), "invalid api key") {
			t.Fatalf("%d при пробе не остановил старт: %v", code, err)
		}
	}
}

// К2.3: для voyage проба тоже идёт — запрос уходит на адрес Voyage.
func TestNewProbesVoyage(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	log, _ := captureLog()
	cfg := config.Config{
		EmbeddingProvider: "voyage",
		VoyageAPIURL:      srv.URL + "/v1/embeddings",
		VoyageAPIKey:      "pa-test",
		EmbeddingModel:    "voyage-code-3",
		EmbeddingBatch:    8,
	}
	e, err := New(context.Background(), cfg, log)
	if err != nil || e == nil || e.Model() != "voyage-code-3" {
		t.Fatalf("voyage: %v, %v", e, err)
	}
	calls := f.calls()
	if len(calls) != 1 || calls[0].auth != "Bearer pa-test" || calls[0].body["input_type"] != "query" {
		t.Fatalf("проба voyage: %+v", calls)
	}

	f.dim = 512
	if _, err := New(context.Background(), cfg, log); err == nil || !strings.Contains(err.Error(), "512") {
		t.Fatalf("voyage с размерностью 512 прошёл пробу: %v", err)
	}
}

// К2.3, К5.1: none — nil и ни одного запроса; hash — заглушка без сети.
func TestNewWithoutProvider(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	log, _ := captureLog()
	for _, provider := range []string{"none", ""} {
		cfg := openaiConfig(srv.URL + "/v1")
		cfg.EmbeddingProvider = provider
		e, err := New(context.Background(), cfg, log)
		if err != nil || e != nil {
			t.Fatalf("провайдер %q: %v, %v", provider, e, err)
		}
	}
	cfg := openaiConfig(srv.URL + "/v1")
	cfg.EmbeddingProvider = "hash"
	if e, err := New(context.Background(), cfg, log); err != nil || e == nil || e.Model() != "hash-dev" {
		t.Fatalf("hash: %v, %v", e, err)
	}
	if n := len(f.calls()); n != 0 {
		t.Fatalf("без HTTP-провайдера ушло %d запросов", n)
	}

	cfg.EmbeddingProvider = "cohere"
	if _, err := New(context.Background(), cfg, log); err == nil || !strings.Contains(err.Error(), "openai") {
		t.Fatalf("неизвестный провайдер: %v", err)
	}
}

// Успешная проба пишет провайдера и модель в лог.
func TestNewLogsProviderAndModel(t *testing.T) {
	_, srv := newFakeOpenAI(t)
	log, buf := captureLog()
	cfg := openaiConfig(srv.URL + "/v1")
	cfg.EmbeddingModel = "bge-m3"
	if _, err := New(context.Background(), cfg, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "provider=openai") || !strings.Contains(buf.String(), "model=bge-m3") {
		t.Fatalf("в логе нет провайдера и модели: %s", buf.String())
	}
}

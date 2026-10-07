package embed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOpenAI — OpenAI-совместимый сервер в памяти: записывает запросы и
// отвечает тем, что задал тест.
type fakeOpenAI struct {
	mu       sync.Mutex
	requests []recorded
	// respond по умолчанию отдаёт векторы размерности dim в прямом порядке.
	respond func(w http.ResponseWriter, req map[string]any, n int)
	dim     int
}

type recorded struct {
	method, path, auth string
	body               map[string]any
}

func newFakeOpenAI(t *testing.T) (*fakeOpenAI, *httptest.Server) {
	t.Helper()
	f := &fakeOpenAI{dim: Dim}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeOpenAI) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), body})
	respond := f.respond
	f.mu.Unlock()

	n := 0
	if in, ok := body["input"].([]any); ok {
		n = len(in)
	}
	if respond != nil {
		respond(w, body, n)
		return
	}
	writeVectors(w, n, f.dim, false)
}

func (f *fakeOpenAI) calls() []recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recorded(nil), f.requests...)
}

// writeVectors отвечает n векторами размерности dim; i-й вектор помечен
// значением i в нулевой координате, чтобы порядок был проверяем.
func writeVectors(w http.ResponseWriter, n, dim int, reversed bool) {
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	data := make([]item, 0, n)
	for i := range n {
		idx := i
		if reversed {
			idx = n - 1 - i
		}
		v := make([]float32, dim)
		v[0] = float32(idx)
		data = append(data, item{Index: idx, Embedding: v})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// К1.2: путь, тело и заголовок запроса.
func TestOpenAIRequestShape(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	o := NewOpenAI(srv.URL+"/v1/", "sk-test", "text-embedding-3-small", 8, true)

	vecs, err := o.Embed(context.Background(), []string{"один", "два"}, KindDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || len(vecs[0]) != Dim {
		t.Fatalf("векторы: %d штук", len(vecs))
	}
	calls := f.calls()
	if len(calls) != 1 {
		t.Fatalf("ожидали один запрос, было %d", len(calls))
	}
	c := calls[0]
	if c.method != http.MethodPost || c.path != "/v1/embeddings" {
		t.Fatalf("запрос %s %s, ожидали POST /v1/embeddings", c.method, c.path)
	}
	if c.auth != "Bearer sk-test" {
		t.Fatalf("Authorization = %q", c.auth)
	}
	if c.body["model"] != "text-embedding-3-small" {
		t.Fatalf("model = %v", c.body["model"])
	}
	if in, ok := c.body["input"].([]any); !ok || len(in) != 2 {
		t.Fatalf("input не массив из двух: %v", c.body["input"])
	}
	if d, ok := c.body["dimensions"].(float64); !ok || int(d) != Dim {
		t.Fatalf("dimensions = %v", c.body["dimensions"])
	}
	if _, ok := c.body["input_type"]; ok {
		t.Fatal("у OpenAI-совместимого API нет input_type, а он ушёл")
	}
}

// К1.3: без ключа нет Authorization, с выключенной размерностью нет dimensions.
func TestOpenAIWithoutKeyAndDimensions(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	o := NewOpenAI(srv.URL+"/v1", "", "bge-m3", 8, false)

	if _, err := o.Embed(context.Background(), []string{"текст"}, KindQuery); err != nil {
		t.Fatal(err)
	}
	c := f.calls()[0]
	if c.auth != "" {
		t.Fatalf("без ключа ушёл Authorization: %q", c.auth)
	}
	if _, ok := c.body["dimensions"]; ok {
		t.Fatalf("с выключенной размерностью в теле dimensions: %v", c.body)
	}
}

// Пачки режутся по Batch, ответ складывается в исходном порядке.
func TestOpenAIBatches(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	o := NewOpenAI(srv.URL+"/v1", "", "m", 2, true)
	vecs, err := o.Embed(context.Background(), []string{"a", "b", "c", "d", "e"}, KindDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 5 || len(f.calls()) != 3 {
		t.Fatalf("векторов %d, запросов %d", len(vecs), len(f.calls()))
	}
}

// К1.4: перепутанный порядок раскладывается по index.
func TestOpenAIReordersByIndex(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	f.respond = func(w http.ResponseWriter, _ map[string]any, n int) { writeVectors(w, n, Dim, true) }
	o := NewOpenAI(srv.URL+"/v1", "", "m", 8, true)

	vecs, err := o.Embed(context.Background(), []string{"a", "b", "c"}, KindDocument)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		if int(v[0]) != i {
			t.Fatalf("вектор %d на месте %d: порядок ответа не разложен по index", int(v[0]), i)
		}
	}
}

// К1.4: 429 с Retry-After повторяется.
func TestOpenAIRetries429(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	var mu sync.Mutex
	attempt := 0
	f.respond = func(w http.ResponseWriter, _ map[string]any, n int) {
		mu.Lock()
		attempt++
		first := attempt == 1
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"slow down"}`))
			return
		}
		writeVectors(w, n, Dim, false)
	}
	o := NewOpenAI(srv.URL+"/v1", "", "m", 8, true)
	start := time.Now()
	if _, err := o.Embed(context.Background(), []string{"a"}, KindDocument); err != nil {
		t.Fatalf("429 не повторился: %v", err)
	}
	if len(f.calls()) != 2 {
		t.Fatalf("ожидали два запроса, было %d", len(f.calls()))
	}
	if waited := time.Since(start); waited < time.Second || waited >= 2*time.Second {
		t.Fatalf("пауза %s не по Retry-After: 1", waited)
	}
}

// К1.4: 400 не повторяется, ошибка содержит код и начало тела.
func TestOpenAIDoesNotRetry400(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	f.respond = func(w http.ResponseWriter, _ map[string]any, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"unknown field dimensions"}`))
	}
	o := NewOpenAI(srv.URL+"/v1", "", "m", 8, true)
	_, err := o.Embed(context.Background(), []string{"a"}, KindDocument)
	if err == nil {
		t.Fatal("400 прошёл как успех")
	}
	if len(f.calls()) != 1 {
		t.Fatalf("400 повторился: %d запросов", len(f.calls()))
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "unknown field dimensions") {
		t.Fatalf("в ошибке нет кода и тела: %v", err)
	}
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusBadRequest {
		t.Fatalf("ошибка не StatusError: %v", err)
	}
}

// Вектор не той размерности — DimensionError без повтора.
func TestOpenAIWrongDimension(t *testing.T) {
	f, srv := newFakeOpenAI(t)
	f.dim = 768
	o := NewOpenAI(srv.URL+"/v1", "", "nomic-embed-text", 8, false)
	_, err := o.Embed(context.Background(), []string{"a"}, KindDocument)
	var dim *DimensionError
	if !errors.As(err, &dim) || dim.Got != 768 {
		t.Fatalf("ожидали DimensionError 768, получили %v", err)
	}
	if len(f.calls()) != 1 {
		t.Fatalf("неверная размерность повторилась: %d запросов", len(f.calls()))
	}
}

// Пустой адрес — адрес OpenAI, хвостовой слэш не удваивается.
func TestOpenAIDefaultURL(t *testing.T) {
	if o := NewOpenAI("", "", "m", 0, true); o.BaseURL != "https://api.openai.com/v1" || o.Batch != defaultBatch {
		t.Fatalf("умолчания: %q, пачка %d", o.BaseURL, o.Batch)
	}
	if o := NewOpenAI("http://ollama:11434/v1/", "", "m", 0, true); o.BaseURL != "http://ollama:11434/v1" {
		t.Fatalf("хвостовой слэш не снят: %q", o.BaseURL)
	}
}

package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Серверные спаны (CLAB-204): шаблон маршрута, без данных проекта и
// секретов, ошибка на 5xx, входящий traceparent, пробы не трассируются.

func tracedServer(t *testing.T) (http.Handler, *tracetest.InMemoryExporter) {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	log := slog.New(slog.DiscardHandler)
	cfg := config.Config{Env: "test", AuthMode: workflow.AuthToken}
	auth := workflow.NewAuth(nil, log, cfg.AuthMode)
	mcpServer := mcp.NewServer(log, nil, nil, nil, auth, "v0.10.0")
	h := NewServer(cfg, log, nil, nil, nil, auth, mcpServer).
		WithVersion("v0.10.0").WithTracing(tp).Handler()
	return h, exp
}

func do(h http.Handler, method, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// spanText — всё, что спан отдаёт сборщику: имя, атрибуты, события, статус.
func spanText(s tracetest.SpanStub) string {
	var b strings.Builder
	b.WriteString(s.Name)
	for _, kv := range s.Attributes {
		b.WriteString(" " + string(kv.Key) + "=" + kv.Value.Emit())
	}
	for _, e := range s.Events {
		b.WriteString(" " + e.Name)
		for _, kv := range e.Attributes {
			b.WriteString(" " + string(kv.Key) + "=" + kv.Value.Emit())
		}
	}
	b.WriteString(" " + s.Status.Description)
	return b.String()
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// AC-3.
func TestTracingSpanUsesRouteTemplate(t *testing.T) {
	h, exp := tracedServer(t)
	const token = "clab_tok_0123456789abcdef"

	do(h, http.MethodGet, "/v1/tasks/CLAB-1?x=secret", map[string]string{"Authorization": "Bearer " + token})
	do(h, http.MethodGet, "/v1/auth/cli/ABCD-EFGH", nil)
	do(h, http.MethodPost, "/mcp/projects/CLAB", nil)
	do(h, http.MethodGet, "/no-such-path/CLAB-2?x=secret", nil)
	do(h, "BREW", "/v1/tasks/CLAB-3", nil)

	spans := exp.GetSpans()
	want := []string{
		"GET /v1/tasks/:taskKey",
		"GET /v1/auth/cli/:code",
		"POST /mcp/projects/:projectKey",
		"GET",
		"_OTHER",
	}
	if len(spans) != len(want) {
		t.Fatalf("спанов %d, ждали %d: %v", len(spans), len(want), spans)
	}
	for i, s := range spans {
		if s.Name != want[i] {
			t.Errorf("спан %d: %q, ждали %q", i, s.Name, want[i])
		}
		text := spanText(s)
		for _, leak := range []string{"CLAB-1", "CLAB-2", "CLAB-3", "secret", "ABCD-EFGH", token, "Bearer", "BREW"} {
			if strings.Contains(text, leak) {
				t.Errorf("спан %q несёт %q: %s", s.Name, leak, text)
			}
		}
		for _, kv := range s.Attributes {
			switch string(kv.Key) {
			case "http.request.method", "http.route", "http.response.status_code", "network.protocol.version":
			default:
				t.Errorf("спан %q: атрибут вне списка R6: %s", s.Name, kv.Key)
			}
		}
	}
	if r := attr(spans[0], "http.route"); r != "/v1/tasks/:taskKey" {
		t.Errorf("http.route=%q", r)
	}
	if r := attr(spans[3], "http.route"); r != "" {
		t.Errorf("у запроса мимо маршрутов http.route=%q", r)
	}
	if c := attr(spans[3], "http.response.status_code"); c != "404" {
		t.Errorf("код 404 не записан: %q", c)
	}
}

// AC-4: паника, превращённая recovery в 500, — статус Error; 404 — нет.
func TestTracingSpanErrorOn5xx(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	defer func() { _ = tp.Shutdown(t.Context()) }()

	r := gin.New()
	r.Use(requestTracing(tp), recovery(slog.New(slog.DiscardHandler)))
	r.GET("/v1/boom/:id", func(*gin.Context) { panic("kaboom-detail") })

	if rec := do(r, http.MethodGet, "/v1/boom/CLAB-7", nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("код %d", rec.Code)
	}
	do(r, http.MethodGet, "/nope", nil)

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("спанов %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error || attr(spans[0], "http.response.status_code") != "500" {
		t.Fatalf("500: статус %v, код %s", spans[0].Status, attr(spans[0], "http.response.status_code"))
	}
	if strings.Contains(spanText(spans[0]), "kaboom") {
		t.Fatalf("текст паники в спане: %s", spanText(spans[0]))
	}
	if spans[1].Status.Code == codes.Error {
		t.Fatal("404 помечен ошибкой")
	}
}

// AC-5: входящий traceparent — тот же trace id и родитель; baggage не берётся.
func TestTracingAcceptsTraceparent(t *testing.T) {
	h, exp := tracedServer(t)
	const (
		traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
		spanID  = "00f067aa0ba902b7"
	)
	do(h, http.MethodGet, "/v1/instance", map[string]string{
		"traceparent": "00-" + traceID + "-" + spanID + "-01",
		"baggage":     "tenant=acme-secret",
	})
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("спанов %d", len(spans))
	}
	s := spans[0]
	if got := s.SpanContext.TraceID().String(); got != traceID {
		t.Fatalf("trace id %s, ждали %s", got, traceID)
	}
	if got := s.Parent.SpanID().String(); got != spanID || !s.Parent.IsRemote() {
		t.Fatalf("родитель %s (remote=%v), ждали %s", got, s.Parent.IsRemote(), spanID)
	}
	if strings.Contains(spanText(s), "acme-secret") || strings.Contains(spanText(s), "tenant") {
		t.Fatalf("baggage в спане: %s", spanText(s))
	}
}

// AC-6: пробы не трассируются.
func TestTracingSkipsProbes(t *testing.T) {
	h, exp := tracedServer(t)
	do(h, http.MethodGet, "/healthz", nil)
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("спанов на /healthz: %d", n)
	}
	// /readyz без базы паникует внутри ручки: recovery отвечает 500, спана
	// всё равно нет — проба пропускается до вызова ручки.
	do(h, http.MethodGet, "/readyz", nil)
	if n := len(exp.GetSpans()); n != 0 {
		t.Fatalf("спанов на /readyz: %d", n)
	}
}

// AC-8: без провайдера ответы прежние и спанов нет — слоя нет вовсе.
func TestTracingDisabledIsTransparent(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	cfg := config.Config{Env: "test", AuthMode: workflow.AuthToken}
	auth := workflow.NewAuth(nil, log, cfg.AuthMode)
	h := NewServer(cfg, log, nil, nil, nil, auth, nil).WithVersion("v0.10.0").WithTracing(nil).Handler()
	rec := do(h, http.MethodGet, "/healthz", map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"})
	if rec.Code != http.StatusOK {
		t.Fatalf("код %d", rec.Code)
	}
	for k := range rec.Header() {
		if strings.EqualFold(k, "traceparent") || strings.EqualFold(k, "tracestate") {
			t.Fatalf("заголовок трассировки в ответе: %s", k)
		}
	}
}

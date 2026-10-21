package tracing

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// AC-8: включено только явно, пустое значение равно отсутствию.
func TestFromEnvDecision(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		on   bool
	}{
		{"нет переменных", nil, false},
		{"все OTEL_* пустые, как из compose пакета", map[string]string{
			envEndpoint: "", envTracesEndpoint: "", envExporter: "", envProtocol: "",
			envTracesProtocol: "", envSDKDisabled: "", "OTEL_EXPORTER_OTLP_HEADERS": "",
			"OTEL_SERVICE_NAME": "", "OTEL_RESOURCE_ATTRIBUTES": "",
			"OTEL_TRACES_SAMPLER": "", "OTEL_TRACES_SAMPLER_ARG": "",
		}, false},
		{"пробелы — тоже пусто", map[string]string{envEndpoint: "  ", envExporter: " "}, false},
		{"только OTEL_SERVICE_NAME", map[string]string{"OTEL_SERVICE_NAME": "acme"}, false},
		{"none при заданном адресе", map[string]string{
			envEndpoint: "http://collector:4318", envExporter: "none"}, false},
		{"OTEL_SDK_DISABLED=true при заданном адресе", map[string]string{
			envEndpoint: "http://collector:4318", envSDKDisabled: "TRUE"}, false},
		{"задан адрес", map[string]string{envEndpoint: "http://collector:4318"}, true},
		{"задан адрес сигнала", map[string]string{
			envTracesEndpoint: "https://apm.example.com/otlp/v1/traces"}, true},
		{"otlp без адреса", map[string]string{envExporter: "otlp"}, true},
		{"OTEL_SDK_DISABLED=false не выключает", map[string]string{
			envEndpoint: "http://collector:4318", envSDKDisabled: "false"}, true},
		{"http/protobuf явно", map[string]string{
			envEndpoint: "http://collector:4318", envProtocol: "http/protobuf"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := FromEnv(envOf(tc.env))
			if err != nil {
				t.Fatalf("ошибка: %v", err)
			}
			if s.Enabled != tc.on {
				t.Fatalf("Enabled=%v, ждали %v", s.Enabled, tc.on)
			}
		})
	}
}

// AC-9: неподдерживаемое — отказ старта с именем переменной.
func TestFromEnvRejectsUnsupported(t *testing.T) {
	cases := []struct {
		env map[string]string
		key string
	}{
		{map[string]string{envExporter: "zipkin"}, envExporter},
		{map[string]string{envExporter: "otlp,console"}, envExporter},
		{map[string]string{envEndpoint: "http://c:4317", envProtocol: "grpc"}, envProtocol},
		{map[string]string{envEndpoint: "http://c:4318", envTracesProtocol: "http/json"}, envTracesProtocol},
		// Неподдерживаемое отбивается и при выключенных трассировках.
		{map[string]string{envSDKDisabled: "true", envProtocol: "grpc"}, envProtocol},
		{map[string]string{envEndpoint: "otel-collector:4318"}, envEndpoint},
		{map[string]string{envTracesEndpoint: "collector"}, envTracesEndpoint},
		{map[string]string{envEndpoint: "ftp://collector:4318"}, envEndpoint},
	}
	for _, tc := range cases {
		_, err := FromEnv(envOf(tc.env))
		if err == nil {
			t.Fatalf("%v: ошибки нет", tc.env)
		}
		if !strings.Contains(err.Error(), tc.key) {
			t.Fatalf("%v: в ошибке нет имени переменной %s: %v", tc.env, tc.key, err)
		}
	}
}

// R9: адрес в журнале — без userinfo и строки запроса, как его выберет SDK.
func TestLogEndpoint(t *testing.T) {
	cases := map[[2]string]string{
		{"http://collector:4318", ""}:                 "http://collector:4318/v1/traces",
		{"https://u:secret@apm.example.com/otlp", ""}: "https://apm.example.com/otlp/v1/traces",
		{"", "https://apm.example.com/x?api_key=k#f"}: "https://apm.example.com/x",
		{"http://ignored:4318", "http://traces:4318"}: "http://traces:4318/",
		{"", ""}: "https://localhost:4318/v1/traces",
	}
	for in, want := range cases {
		if got := logEndpoint(in[0], in[1]); got != want {
			t.Errorf("logEndpoint(%q, %q) = %q, ждали %q", in[0], in[1], got, want)
		}
	}
}

// AC-8: выключено — ни провайдера, ни экспортёра, глобальный провайдер не
// тронут; nil-безопасные методы.
func TestStartDisabled(t *testing.T) {
	var buf bytes.Buffer
	tr, err := Start(context.Background(), Settings{}, "v1.2.3", slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil || tr != nil {
		t.Fatalf("Start выключенного: %v, %v", tr, err)
	}
	if tr.Provider() != nil {
		t.Fatal("провайдер у выключенных трассировок")
	}
	if err := tr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Fatal("глобальный провайдер подменён SDK")
	}
	if buf.Len() != 0 {
		t.Fatalf("журнал выключенных трассировок не пуст: %s", buf.String())
	}
}

// collector — приёмник OTLP/HTTP: запоминает запросы POST /v1/traces.
type collector struct {
	mu      sync.Mutex
	reqs    []*collectortrace.ExportTraceServiceRequest
	headers []http.Header
	paths   []string
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req collectortrace.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.reqs = append(c.reqs, &req)
	c.headers = append(c.headers, r.Header.Clone())
	c.paths = append(c.paths, r.URL.Path)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	out, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})
	_, _ = w.Write(out)
}

// AC-1, AC-2, AC-7 без стека: спан доходит до сборщика с ресурсом, версия
// сборки не переопределяется окружением, заголовки уходят сборщику, но не
// в журнал, строка о включении одна и без userinfo.
func TestExportReachesCollector(t *testing.T) {
	col := &collector{}
	srv := httptest.NewServer(col)
	defer srv.Close()

	endpoint := strings.Replace(srv.URL, "http://", "http://user:pw-secret@", 1)
	env := map[string]string{
		envEndpoint:                  endpoint,
		"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer s3cr3t",
		"OTEL_SERVICE_NAME":          "acme-cadence",
		"OTEL_RESOURCE_ATTRIBUTES":   "deployment.environment=prod,service.version=forged",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	settings, err := FromEnv(envOf(env))
	if err != nil || !settings.Enabled {
		t.Fatalf("FromEnv: %+v, %v", settings, err)
	}

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	tr, err := Start(context.Background(), settings, "v1.2.3", log)
	if err != nil {
		t.Fatal(err)
	}
	_, span := tr.Provider().Tracer("test").Start(context.Background(), "GET /v1/instance")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	col.mu.Lock()
	defer col.mu.Unlock()
	if len(col.reqs) != 1 {
		t.Fatalf("сборщик принял %d запросов, ждали 1", len(col.reqs))
	}
	if col.paths[0] != "/v1/traces" {
		t.Fatalf("путь %q", col.paths[0])
	}
	if got := col.headers[0].Get("Authorization"); got != "Bearer s3cr3t" {
		t.Fatalf("заголовок из OTEL_EXPORTER_OTLP_HEADERS не дошёл: %q", got)
	}
	rs := col.reqs[0].GetResourceSpans()
	if len(rs) != 1 {
		t.Fatalf("resource spans: %d", len(rs))
	}
	attrs := map[string]string{}
	for _, kv := range rs[0].GetResource().GetAttributes() {
		attrs[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	want := map[string]string{
		"service.name": "acme-cadence", "service.version": "v1.2.3",
		"deployment.environment": "prod",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("ресурс %s=%q, ждали %q (все: %v)", k, attrs[k], v, attrs)
		}
	}
	if n := len(rs[0].GetScopeSpans()); n != 1 || rs[0].GetScopeSpans()[0].GetSpans()[0].GetName() != "GET /v1/instance" {
		t.Fatalf("спаны: %v", rs[0].GetScopeSpans())
	}

	logs := buf.String()
	if n := strings.Count(logs, "трассировки включены"); n != 1 {
		t.Fatalf("строк о включении %d, ждали 1:\n%s", n, logs)
	}
	for _, secret := range []string{"s3cr3t", "pw-secret", "user:"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("в журнале %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs, strings.TrimPrefix(srv.URL, "http://")+"/v1/traces") {
		t.Fatalf("в строке о включении нет адреса сборщика:\n%s", logs)
	}
}

// R5: без OTEL_SERVICE_NAME — cadence-api.
func TestResourceDefaultServiceName(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	res, err := newResource(context.Background(), "v9.9.9", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range res.Attributes() {
		got[string(kv.Key)] = kv.Value.Emit()
	}
	if got["service.name"] != DefaultServiceName || got["service.version"] != "v9.9.9" {
		t.Fatalf("ресурс: %v", got)
	}
}

// AC-11 без стека: сборщик за закрытым портом или зависший — Shutdown
// укладывается в срок контекста, а не ждёт таймаута и повторов экспортёра.
func TestShutdownBoundedWhenCollectorDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().String()
	_ = ln.Close()

	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer hung.Close()
	defer close(release)

	for name, endpoint := range map[string]string{
		"закрытый порт": "http://" + closed,
		"зависший":      hung.URL,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(envEndpoint, endpoint)
			tr, err := Start(context.Background(), Settings{Enabled: true, Endpoint: endpoint},
				"v1.2.3", slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			_, span := tr.Provider().Tracer("test").Start(context.Background(), "GET /v1/instance")
			span.End()

			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			start := time.Now()
			err = tr.Shutdown(ctx)
			if took := time.Since(start); took > 2*time.Second {
				t.Fatalf("Shutdown занял %s при сроке 500ms", took)
			}
			t.Logf("Shutdown за %s: %v", time.Since(start).Round(time.Millisecond), err)
		})
	}
}

// R7: ошибки экспорта — в журнал не чаще раза в интервал, с числом
// пропущенных.
func TestErrorHandlerRateLimited(t *testing.T) {
	var buf bytes.Buffer
	h := newErrorHandler(slog.New(slog.NewTextHandler(&buf, nil)), time.Minute)
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	for range 100 {
		h.Handle(errors.New("connection refused"))
	}
	if n := strings.Count(buf.String(), "ошибка экспорта"); n != 1 {
		t.Fatalf("строк за минуту %d, ждали 1:\n%s", n, buf.String())
	}
	now = now.Add(61 * time.Second)
	h.Handle(errors.New("connection refused"))
	if n := strings.Count(buf.String(), "ошибка экспорта"); n != 2 {
		t.Fatalf("строк после интервала %d, ждали 2", n)
	}
	if !strings.Contains(buf.String(), "suppressed=99") {
		t.Fatalf("нет числа пропущенных:\n%s", buf.String())
	}
}

// AC-13: SDK и экспортёр — только в api; воркер, clab и раннер их не тянут.
func TestOnlyAPILinksSDK(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("нет go в PATH")
	}
	cmd := exec.Command(goBin, "list", "-deps", "./cmd/worker", "./cmd/clab", "./cmd/clab-runner")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if strings.HasPrefix(pkg, "go.opentelemetry.io/") ||
			strings.HasSuffix(pkg, "/internal/tracing") {
			t.Errorf("воркер, clab или раннер тянет %s", pkg)
		}
	}

	cmd = exec.Command(goBin, "list", "-deps", "./cmd/api")
	cmd.Dir = "../.."
	out, err = cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	if !strings.Contains(string(out), "go.opentelemetry.io/otel/sdk/trace\n") {
		t.Fatal("api без SDK трассировок — проверка выше ничего не доказывает")
	}
}

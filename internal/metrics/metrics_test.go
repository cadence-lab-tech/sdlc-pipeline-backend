package metrics

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Метрики эксплуатации (CLAB-126). Базы не нужно: реестр, слушатель и учёт
// заданий проверяются выводом /metrics — тем, что увидит Prometheus.

func scrape(t *testing.T, r *Registry, accept string) (string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String(), rec.Header().Get("Content-Type")
}

// value — значение серии с точным набором меток; -1 — серии нет.
func value(t *testing.T, body, series string) float64 {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		return -1
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("%s: %v", series, err)
	}
	return v
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// AC-1, AC-7, AC-15: формат Prometheus, версия сборки с компонентом,
// метрики рантайма и процесса.
func TestRegistryExposesBuildInfoAndRuntime(t *testing.T) {
	for _, component := range []string{"api", "worker"} {
		r := New(component, "v0.10.0")
		body, ctype := scrape(t, r, "")
		if !strings.HasPrefix(ctype, "text/plain; version=0.0.4") {
			t.Fatalf("Content-Type %q", ctype)
		}
		series := `cadence_build_info{component="` + component + `",go_version="` +
			runtime.Version() + `",version="v0.10.0"}`
		if value(t, body, series) != 1 {
			t.Fatalf("нет %s в выводе:\n%s", series, body)
		}
		if !strings.Contains(body, "\ngo_goroutines ") {
			t.Fatal("нет метрик рантайма go_*")
		}
		if runtime.GOOS == "linux" && !strings.Contains(body, "\nprocess_cpu_seconds_total ") {
			t.Fatal("нет метрик процесса process_*")
		}
	}

	_, ctype := scrape(t, New("api", "v0.10.0"), "application/openmetrics-text; version=1.0.0")
	if !strings.HasPrefix(ctype, "application/openmetrics-text") {
		t.Fatalf("OpenMetrics по Accept: Content-Type %q", ctype)
	}
}

// AC-10: статистика пула, предел — SDLC_DB_MAX_CONNS. Пул без MinConns в
// базу при создании не ходит, адрес может быть недоступен.
func TestPoolCollectorReportsMaxConns(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u:p@127.0.0.1:1/db?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 7
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	r := New("api", "dev")
	r.Pool(pool)
	body, _ := scrape(t, r, "")
	if v := value(t, body, "cadence_db_pool_max_connections"); v != 7 {
		t.Fatalf("max_connections = %v, ждали 7", v)
	}
	for _, s := range []string{
		`cadence_db_pool_connections{state="acquired"}`,
		`cadence_db_pool_connections{state="idle"}`,
		`cadence_db_pool_connections{state="constructing"}`,
		"cadence_db_pool_acquires_total",
		"cadence_db_pool_acquire_seconds_total",
		"cadence_db_pool_empty_acquires_total",
		"cadence_db_pool_empty_acquire_wait_seconds_total",
		"cadence_db_pool_canceled_acquires_total",
	} {
		if value(t, body, s) < 0 {
			t.Fatalf("нет серии %s", s)
		}
	}
}

// AC-8: метки запроса — метод из закрытого набора, шаблон маршрута, класс
// кода; несовпавший маршрут — unmatched.
func TestAPIRequestLabels(t *testing.T) {
	r := New("api", "dev")
	m := NewAPI(r)
	m.Request("GET", "/v1/tasks/:taskKey", 200, 10*time.Millisecond)
	m.Request("GET", "", 404, time.Millisecond)
	m.Request("BREW", "", 404, time.Millisecond)
	m.Request("POST", "/v1/projects/:projectKey/tasks", 503, time.Second)

	body, _ := scrape(t, r, "")
	for _, s := range []string{
		`cadence_http_request_duration_seconds_count{code="2xx",method="GET",route="/v1/tasks/:taskKey"}`,
		`cadence_http_request_duration_seconds_count{code="4xx",method="GET",route="unmatched"}`,
		`cadence_http_request_duration_seconds_count{code="4xx",method="OTHER",route="unmatched"}`,
		`cadence_http_request_duration_seconds_count{code="5xx",method="POST",route="/v1/projects/:projectKey/tasks"}`,
	} {
		if value(t, body, s) != 1 {
			t.Fatalf("нет %s:\n%s", s, body)
		}
	}
	if strings.Contains(body, "BREW") {
		t.Fatal("произвольный метод попал в метку")
	}
}

// AC-9: серии исходов вебхука заведены нулями до первой доставки.
func TestWebhookOutcomesPreinitialized(t *testing.T) {
	r := New("api", "dev")
	m := NewAPI(r)
	body, _ := scrape(t, r, "")
	for _, o := range webhookOutcomes {
		if v := value(t, body, `cadence_webhook_deliveries_total{outcome="`+o+`",provider="github"}`); v != 0 {
			t.Fatalf("%s до доставок: %v", o, v)
		}
	}
	m.WebhookDelivery("github", WebhookBadSignature)
	body, _ = scrape(t, r, "")
	if v := value(t, body, `cadence_webhook_deliveries_total{outcome="bad_signature",provider="github"}`); v != 1 {
		t.Fatalf("bad_signature = %v", v)
	}
}

// AC-14: ошибка задания — счётчик, успешный проход — метка времени,
// длительность — всегда; остановка процесса ошибкой не считается.
func TestWorkerJobAccounting(t *testing.T) {
	r := New("worker", "dev")
	m := NewWorker(r, "lease_sweep", "index_run")
	ctx := context.Background()

	body, _ := scrape(t, r, "")
	if v := value(t, body, `cadence_worker_job_errors_total{job="index_run"}`); v != 0 {
		t.Fatalf("счётчик ошибок не заведён заранее: %v", v)
	}
	if v := value(t, body, `cadence_worker_job_last_success_timestamp_seconds{job="index_run"}`); v != -1 {
		t.Fatalf("метка успеха заведена до первого прохода: %v", v)
	}

	boom := errors.New("база недоступна")
	if err := m.Job(ctx, "index_run", func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("ошибка задания не вернулась: %v", err)
	}
	before := float64(time.Now().Unix())
	if err := m.Job(ctx, "lease_sweep", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_ = m.Job(cancelled, "lease_sweep", func() error { return context.Canceled })

	body, _ = scrape(t, r, "")
	if v := value(t, body, `cadence_worker_job_errors_total{job="index_run"}`); v != 1 {
		t.Fatalf("ошибок index_run = %v", v)
	}
	if v := value(t, body, `cadence_worker_job_errors_total{job="lease_sweep"}`); v != 0 {
		t.Fatalf("остановка посчитана ошибкой: %v", v)
	}
	if v := value(t, body, `cadence_worker_job_last_success_timestamp_seconds{job="lease_sweep"}`); v < before {
		t.Fatalf("метка успеха lease_sweep = %v, раньше %v", v, before)
	}
	if v := value(t, body, `cadence_worker_job_last_success_timestamp_seconds{job="index_run"}`); v != -1 {
		t.Fatalf("неудачный проход сдвинул метку успеха: %v", v)
	}
	if v := value(t, body, `cadence_worker_job_duration_seconds_count{job="lease_sweep"}`); v != 2 {
		t.Fatalf("длительность lease_sweep учтена %v раз", v)
	}
}

// AC-11…13: прогоны индексации, возвращённые аренды и снимок состояния базы.
func TestWorkerRunsLeasesAndState(t *testing.T) {
	r := New("worker", "dev")
	m := NewWorker(r)
	m.IndexRunFinished(false, 2*time.Second)
	m.IndexRunFinished(true, time.Second)
	m.IndexRunFinished(false, time.Second)
	m.LeasesExpired(3)
	m.LeasesExpired(0)
	m.SetState(State{QueuedRuns: 4, RunningRuns: 1, OldestQueued: 90 * time.Second,
		LiveLeases: 2, ExpiredLeases: 1, HeadFailures: 5}, time.Unix(1_700_000_000, 0))

	body, _ := scrape(t, r, "")
	for series, want := range map[string]float64{
		`cadence_index_runs_total{result="completed"}`:     2,
		`cadence_index_runs_total{result="failed"}`:        1,
		`cadence_index_run_duration_seconds_count`:         3,
		`cadence_leases_expired_total`:                     3,
		`cadence_index_queue_runs{state="queued"}`:         4,
		`cadence_index_queue_runs{state="running"}`:        1,
		`cadence_index_queue_oldest_age_seconds`:           90,
		`cadence_leases{state="live"}`:                     2,
		`cadence_leases{state="expired"}`:                  1,
		`cadence_repository_head_failures`:                 5,
		`cadence_worker_state_refreshed_timestamp_seconds`: 1.7e9,
	} {
		if v := value(t, body, series); v != want {
			t.Fatalf("%s = %v, ждали %v", series, v, want)
		}
	}
}

// Процесс без SDLC_METRICS_ADDR зовёт те же методы на nil: ничего не
// падает, задание исполняется.
func TestNilMetricsAreNoop(t *testing.T) {
	var w *Worker
	ran := false
	if err := w.Job(context.Background(), "x", func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("задание на nil-метриках: ran=%v err=%v", ran, err)
	}
	w.IndexRunFinished(true, time.Second)
	w.LeasesExpired(1)
	w.SetState(State{}, time.Now())
	var a *API
	a.Request("GET", "/", 200, time.Second)
	a.WebhookDelivery("github", WebhookQueued)
	if NewAPI(nil) != nil || NewWorker(nil) != nil {
		t.Fatal("nil-реестр дал не nil-метрики")
	}
}

// AC-1, AC-2, AC-5: отдельный слушатель отдаёт только /metrics; пустой
// адрес — ни одного порта; занятый порт — ошибка старта.
func TestStartListener(t *testing.T) {
	srv, err := Start("", New("worker", "dev"), discard())
	if err != nil || srv != nil {
		t.Fatalf("пустой адрес: srv=%v err=%v", srv, err)
	}

	r := New("worker", "v0.10.0")
	srv, err = Start("127.0.0.1:0", r, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown(context.Background())

	resp, err := http.Get("http://" + srv.Addr() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `cadence_build_info{component="worker"`) {
		t.Fatalf("/metrics: %d\n%s", resp.StatusCode, body)
	}
	resp, err = http.Get("http://" + srv.Addr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("слушатель метрик отдал не только /metrics: %d", resp.StatusCode)
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if _, err := Start(busy.Addr().String(), r, discard()); err == nil ||
		!strings.Contains(err.Error(), "SDLC_METRICS_ADDR") {
		t.Fatalf("занятый порт не остановил старт: %v", err)
	}
}

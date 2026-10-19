package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/config"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/mcp"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/metrics"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Метрики эксплуатации на транспорте (CLAB-126): маршрут шаблоном, без
// ключей проекта, исходы вебхуков; на основном порту /metrics нет.

func opsScrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	reg.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func opsValue(t *testing.T, body, series string) float64 {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(series) + ` (\S+)$`).FindStringSubmatch(body)
	if m == nil {
		return -1
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// opsServer — роутер в режиме токенов без базы: запросы без токена
// отбиваются аутентификацией, но маршрут уже выбран и попадает в метрику.
func opsServer(t *testing.T, cfg config.Config) (http.Handler, *metrics.Registry) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	cfg.Env = "test"
	cfg.AuthMode = workflow.AuthToken
	auth := workflow.NewAuth(nil, log, cfg.AuthMode)
	reg := metrics.New("api", "v0.10.0")
	mcpServer := mcp.NewServer(log, nil, nil, nil, auth, "v0.10.0")
	h := NewServer(cfg, log, nil, nil, nil, auth, mcpServer).
		WithMetrics(metrics.NewAPI(reg)).Handler()
	return h, reg
}

func send(h http.Handler, method, path string) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Code
}

// AC-3, AC-6, AC-8.
func TestRequestMetricsUseRouteTemplate(t *testing.T) {
	h, reg := opsServer(t, config.Config{})

	send(h, http.MethodGet, "/v1/projects/ACME/tasks")
	send(h, http.MethodGet, "/v1/tasks/ACME-1")
	send(h, http.MethodPost, "/mcp/projects/ACME")
	send(h, http.MethodGet, "/nope/ACME-2")
	send(h, http.MethodGet, "/healthz")
	// На основном порту /metrics нет — ни при включённых метриках, ни без
	// них: его отдаёт только отдельный слушатель.
	if code := send(h, http.MethodGet, "/metrics"); code != http.StatusNotFound {
		t.Fatalf("GET /metrics на основном порту: %d", code)
	}

	body := opsScrape(t, reg)
	for _, s := range []string{
		`cadence_http_request_duration_seconds_count{code="4xx",method="GET",route="/v1/projects/:projectKey/tasks"}`,
		`cadence_http_request_duration_seconds_count{code="4xx",method="GET",route="/v1/tasks/:taskKey"}`,
		`cadence_http_request_duration_seconds_count{code="4xx",method="POST",route="/mcp/projects/:projectKey"}`,
		`cadence_http_request_duration_seconds_count{code="2xx",method="GET",route="/healthz"}`,
	} {
		if opsValue(t, body, s) != 1 {
			t.Fatalf("нет %s:\n%s", s, body)
		}
	}
	// /nope/ACME-2 и /metrics — оба мимо маршрутов.
	if v := opsValue(t, body, `cadence_http_request_duration_seconds_count{code="4xx",method="GET",route="unmatched"}`); v != 2 {
		t.Fatalf("unmatched = %v", v)
	}
	if strings.Contains(body, "ACME") {
		t.Fatalf("ключ проекта попал в метрики:\n%s", body)
	}
}

func signed(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func webhook(h http.Handler, event, delivery, body, signature string) int {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", signature)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func webhookCount(t *testing.T, reg *metrics.Registry, outcome string) float64 {
	t.Helper()
	return opsValue(t, opsScrape(t, reg),
		`cadence_webhook_deliveries_total{outcome="`+outcome+`",provider="github"}`)
}

// AC-9 без базы: выключенные вебхуки и неверная подпись.
func TestWebhookMetricsWithoutDatabase(t *testing.T) {
	h, reg := opsServer(t, config.Config{})
	if code := webhook(h, "ping", "d-1", `{}`, "sha256=00"); code != http.StatusConflict {
		t.Fatalf("без секрета: %d", code)
	}
	if v := webhookCount(t, reg, "disabled"); v != 1 {
		t.Fatalf("disabled = %v", v)
	}

	h, reg = opsServer(t, config.Config{GitHubWebhookSecret: "s3cret"})
	if code := webhook(h, "ping", "d-2", `{}`, signed("other", `{}`)); code != http.StatusUnauthorized {
		t.Fatalf("чужая подпись: %d", code)
	}
	if v := webhookCount(t, reg, "bad_signature"); v != 1 {
		t.Fatalf("bad_signature = %v", v)
	}
}

// AC-9 на базе: событие, которое не обрабатывается, — ignored; повтор той
// же доставки — duplicate; принятое (ping) — queued; ошибка движка — error.
func TestWebhookMetricsOutcomes(t *testing.T) {
	store, _ := testdb.Store(t)
	log := slog.New(slog.DiscardHandler)
	engine := workflow.NewEngine(store, log, workflow.Options{})
	auth := workflow.NewAuth(store, log, workflow.AuthHeader)
	reg := metrics.New("api", "dev")
	h := NewServer(config.Config{Env: "test", GitHubWebhookSecret: "s3cret"}, log, store, engine,
		workflow.NewAuthoring(store, log, engine), auth, nil).
		WithMetrics(metrics.NewAPI(reg)).Handler()

	body := `{"zen":"keep it logically awesome"}`
	for i := 0; i < 2; i++ {
		if code := webhook(h, "issues", "delivery-issues-1", body, signed("s3cret", body)); code != http.StatusOK {
			t.Fatalf("доставка %d: %d", i+1, code)
		}
	}
	if code := webhook(h, "ping", "delivery-ping-1", body, signed("s3cret", body)); code != http.StatusOK {
		t.Fatalf("ping: %d", code)
	}
	if webhook(h, "ping", "", body, signed("s3cret", body)) != http.StatusBadRequest {
		t.Fatal("доставка без X-GitHub-Delivery принята")
	}
	for outcome, want := range map[string]float64{"ignored": 1, "duplicate": 1, "error": 1, "queued": 1} {
		if v := webhookCount(t, reg, outcome); v != want {
			t.Fatalf("%s = %v, ждали %v", outcome, v, want)
		}
	}
}

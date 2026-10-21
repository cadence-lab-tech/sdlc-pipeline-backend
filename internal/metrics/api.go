package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// RouteUnmatched — значение метки route для запроса, не попавшего ни в один
// маршрут. Сырой путь в метку не идёт: в нём ключи проектов, а у метки
// стала бы неограниченная кардинальность от любого сканера.
const RouteUnmatched = "unmatched"

// Исходы доставки вебхука. Набор закрытый: метка не принимает ничего, кроме
// них.
const (
	WebhookQueued       = "queued"
	WebhookIgnored      = "ignored"
	WebhookDuplicate    = "duplicate"
	WebhookBadSignature = "bad_signature"
	WebhookDisabled     = "disabled"
	WebhookError        = "error"
)

var webhookOutcomes = []string{WebhookQueued, WebhookIgnored, WebhookDuplicate,
	WebhookBadSignature, WebhookDisabled, WebhookError}

// API — метрики процесса api: запросы и вебхуки. Состояния базы здесь нет
// намеренно — его отдаёт воркер (см. пакет).
type API struct {
	requests *prometheus.HistogramVec
	webhooks *prometheus.CounterVec
	audit    prometheus.Counter
}

// NewAPI заводит метрики api на реестре; nil-реестр — nil-метрики.
func NewAPI(r *Registry) *API {
	if r == nil {
		return nil
	}
	m := &API{
		requests: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "http_request_duration_seconds",
			Help:      "Длительность HTTP-запросов api по методу, шаблону маршрута и классу кода ответа.",
			Buckets:   httpBuckets,
		}, []string{"method", "route", "code"}),
		webhooks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "webhook_deliveries_total",
			Help:      "Доставки вебхуков по провайдеру и исходу.",
		}, []string{"provider", "outcome"}),
	}
	m.audit = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      "audit_write_failures_total",
		Help:      "Записи журнала действий, которые не удалось сохранить.",
	})
	// Серии исходов заводятся нулями заранее: правило оповещения на рост
	// bad_signature должно видеть ряд и до первой плохой доставки.
	for _, o := range webhookOutcomes {
		m.webhooks.WithLabelValues("github", o)
	}
	r.register(m.requests, m.webhooks, m.audit)
	return m
}

// Request учитывает запрос. route — шаблон маршрута, пусто — RouteUnmatched.
// Код ответа сводится к классу (2xx, 4xx, 5xx): точный код умножил бы ряды
// гистограммы на число кодов, а оповещению нужна только доля 5xx.
func (m *API) Request(method, route string, status int, took time.Duration) {
	if m == nil {
		return
	}
	if route == "" {
		route = RouteUnmatched
	}
	m.requests.WithLabelValues(normalizeMethod(method), route, codeClass(status)).
		Observe(took.Seconds())
}

// WebhookDelivery учитывает доставку вебхука провайдера с исходом outcome
// (одна из констант Webhook*).
func (m *API) WebhookDelivery(provider, outcome string) {
	if m == nil {
		return
	}
	m.webhooks.WithLabelValues(provider, outcome).Inc()
}

// AuditWriteFailed учитывает несохранённую запись журнала действий: вызов
// при этом прошёл, а след пропал — это видно только здесь и в логе.
func (m *API) AuditWriteFailed() {
	if m == nil {
		return
	}
	m.audit.Inc()
}

// normalizeMethod держит метку method в закрытом наборе: метод присылает
// клиент, и произвольная строка в нём — та же неограниченная кардинальность.
func normalizeMethod(m string) string {
	switch m {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
		return m
	}
	return "OTHER"
}

func codeClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return strconv.Itoa(status/100) + "xx"
}

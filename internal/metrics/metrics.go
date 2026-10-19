// Package metrics — метрики эксплуатации api и воркера в формате Prometheus
// (CLAB-126).
//
// Это не продуктовые метрики цикла (`GET /v1/projects/:key/metrics`), а то,
// что нужно администратору инстанса: жив ли процесс, копится ли очередь,
// теряются ли аренды, сколько отвечает API, не исчерпан ли пул базы.
//
// Правила, которые держит пакет:
//   - Реестр свой, а не глобальный prometheus.DefaultRegisterer: в тестах и в
//     двух бинарниках набор метрик разный, а глобальный реестр делил бы его
//     между всеми, кто импортировал пакет.
//   - Ни одна метка не несёт данных проекта: ключей проектов и задач,
//     репозиториев, хэндлов, токенов, request_id. Метрики уходят в чужой
//     Prometheus, и ключ проекта в метке — это утечка, а у метки с ключом
//     задачи ещё и неограниченная кардинальность. Маршрут — шаблоном gin.
//   - Состояние базы (очередь, аренды, головы веток) отдаёт только воркер:
//     реплик api может быть несколько, и сумма по ним задвоила бы очередь.
//   - Все методы безопасны на nil: процесс без SDLC_METRICS_ADDR зовёт их так
//     же, и проверок «включены ли метрики» в коде заданий нет.
package metrics

import (
	"net/http"
	"runtime"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// namespace — префикс всех своих метрик.
const namespace = "cadence"

// Registry — реестр одного процесса.
type Registry struct {
	reg *prometheus.Registry
}

// New заводит реестр процесса component («api» или «worker») с метриками
// рантайма, процесса и версией сборки.
func New(component, version string) *Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "build_info",
			Help:      "Версия сборки процесса; значение всегда 1.",
			ConstLabels: prometheus.Labels{
				"component":  component,
				"version":    version,
				"go_version": runtime.Version(),
			},
		}, func() float64 { return 1 }),
	)
	return &Registry{reg: reg}
}

// Handler отдаёт /metrics: text exposition по умолчанию, OpenMetrics — по
// Accept.
func (r *Registry) Handler() http.Handler {
	if r == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(r.reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		Registry:          r.reg,
	})
}

// Pool вешает на реестр статистику пула соединений с базой.
func (r *Registry) Pool(pool *pgxpool.Pool) {
	if r == nil || pool == nil {
		return
	}
	r.reg.MustRegister(newPoolCollector(pool))
}

func (r *Registry) register(cs ...prometheus.Collector) {
	if r == nil {
		return
	}
	r.reg.MustRegister(cs...)
}

// Корзины длительностей. HTTP — от миллисекунд до таймаута запроса (30 с по
// умолчанию); задания воркера и прогоны индексации — до получаса: полный
// прогон большого репозитория идёт минутами, а IndexStuckAfter — 30 минут.
var (
	httpBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30}
	jobBuckets  = []float64{.01, .05, .1, .5, 1, 5, 10, 30, 60, 120, 300, 600, 1800}
)

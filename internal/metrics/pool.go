package metrics

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// poolCollector читает pgxpool.Stat() в момент scrape: это счётчики в
// памяти процесса, в базу он не ходит.
type poolCollector struct {
	pool *pgxpool.Pool

	conns, max                 *prometheus.Desc
	acquires, acquireSeconds   *prometheus.Desc
	waits, waitSeconds         *prometheus.Desc
	canceledAcquires, newConns *prometheus.Desc
}

func newPoolCollector(pool *pgxpool.Pool) *poolCollector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "db_pool", name), help, labels, nil)
	}
	return &poolCollector{
		pool:             pool,
		conns:            d("connections", "Соединения пула по состоянию: acquired — заняты запросом, idle — свободны, constructing — открываются.", "state"),
		max:              d("max_connections", "Предел пула (SDLC_DB_MAX_CONNS)."),
		acquires:         d("acquires_total", "Успешные взятия соединения из пула."),
		acquireSeconds:   d("acquire_seconds_total", "Суммарное время взятия соединения, включая ожидание."),
		waits:            d("empty_acquires_total", "Взятия, которым пришлось ждать: свободных соединений не было."),
		waitSeconds:      d("empty_acquire_wait_seconds_total", "Суммарное время ожидания соединения при пустом пуле."),
		canceledAcquires: d("canceled_acquires_total", "Взятия, отменённые контекстом до получения соединения."),
		newConns:         d("new_connections_total", "Открытые пулом соединения."),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.conns, c.max, c.acquires, c.acquireSeconds,
		c.waits, c.waitSeconds, c.canceledAcquires, c.newConns} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}
	counter := func(d *prometheus.Desc, v float64) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v)
	}
	gauge(c.conns, float64(s.AcquiredConns()), "acquired")
	gauge(c.conns, float64(s.IdleConns()), "idle")
	gauge(c.conns, float64(s.ConstructingConns()), "constructing")
	gauge(c.max, float64(s.MaxConns()))
	counter(c.acquires, float64(s.AcquireCount()))
	counter(c.acquireSeconds, s.AcquireDuration().Seconds())
	counter(c.waits, float64(s.EmptyAcquireCount()))
	counter(c.waitSeconds, s.EmptyAcquireWaitTime().Seconds())
	counter(c.canceledAcquires, float64(s.CanceledAcquireCount()))
	counter(c.newConns, float64(s.NewConnsCount()))
}

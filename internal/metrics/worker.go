package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Worker — метрики воркера: задания цикла, прогоны индексации, аренды и
// снимок состояния базы.
type Worker struct {
	jobErrors      *prometheus.CounterVec
	jobDuration    *prometheus.HistogramVec
	jobLastSuccess *prometheus.GaugeVec

	indexRuns        *prometheus.CounterVec
	indexRunDuration prometheus.Histogram
	leasesExpired    prometheus.Counter

	queueRuns      *prometheus.GaugeVec
	queueOldest    prometheus.Gauge
	leases         *prometheus.GaugeVec
	headFailures   prometheus.Gauge
	stateRefreshed prometheus.Gauge
}

// NewWorker заводит метрики воркера на реестре. jobs — имена заданий цикла:
// их счётчики ошибок заводятся нулями заранее, чтобы rate() по ним работал
// с первой ошибки, а не со второй. Метку последнего успеха заранее не
// заводим: ноль в ней выглядел бы как «не проходило с 1970-го» и поднял бы
// оповещение на задании, которое выключено настройкой.
func NewWorker(r *Registry, jobs ...string) *Worker {
	if r == nil {
		return nil
	}
	m := &Worker{
		jobErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "worker", Name: "job_errors_total",
			Help: "Ошибки заданий цикла воркера.",
		}, []string{"job"}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "worker", Name: "job_duration_seconds",
			Help:    "Длительность одного прохода задания воркера.",
			Buckets: jobBuckets,
		}, []string{"job"}),
		jobLastSuccess: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "worker", Name: "job_last_success_timestamp_seconds",
			Help: "Unix-время последнего прохода задания без ошибки.",
		}, []string{"job"}),
		indexRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "index_runs_total",
			Help: "Исполненные прогоны индексации по результату.",
		}, []string{"result"}),
		indexRunDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace, Name: "index_run_duration_seconds",
			Help:    "Длительность прогона индексации от захвата до записи результата.",
			Buckets: jobBuckets,
		}),
		leasesExpired: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "leases_expired_total",
			Help: "Этапы, возвращённые воркером в общий доступ из-за истёкшей аренды.",
		}),
		queueRuns: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "index_queue_runs",
			Help: "Прогоны индексации в очереди (queued) и в работе (running).",
		}, []string{"state"}),
		queueOldest: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "index_queue_oldest_age_seconds",
			Help: "Возраст самого старого прогона в очереди; 0 — очередь пуста.",
		}),
		leases: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Name: "leases",
			Help: "Этапы под арендой агента: live — аренда действует, expired — истекла и ещё не возвращена.",
		}, []string{"state"}),
		headFailures: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "repository_head_failures",
			Help: "Отслеживаемые ветки, голова которых не читается два прохода подряд и дольше.",
		}),
		stateRefreshed: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "worker", Name: "state_refreshed_timestamp_seconds",
			Help: "Unix-время снимка состояния базы, по которому посчитаны очередь, аренды и головы веток.",
		}),
	}
	for _, j := range jobs {
		m.jobErrors.WithLabelValues(j)
	}
	for _, r := range []string{"completed", "failed"} {
		m.indexRuns.WithLabelValues(r)
	}
	r.register(m.jobErrors, m.jobDuration, m.jobLastSuccess, m.indexRuns,
		m.indexRunDuration, m.leasesExpired, m.queueRuns, m.queueOldest,
		m.leases, m.headFailures, m.stateRefreshed)
	return m
}

// Job исполняет проход задания job и учитывает его: длительность всегда,
// ошибку — счётчиком, успех — меткой времени. Ошибка возвращается как есть:
// что с ней делать (обычно — в лог), решает воркер.
//
// Отмена контекста ошибкой задания не считается: это остановка процесса, а
// не сбой, и при каждом выкатывании она поднимала бы оповещение.
func (m *Worker) Job(ctx context.Context, job string, fn func() error) error {
	if m == nil {
		return fn()
	}
	start := time.Now()
	err := fn()
	m.jobDuration.WithLabelValues(job).Observe(time.Since(start).Seconds())
	switch {
	case err == nil:
		m.jobLastSuccess.WithLabelValues(job).Set(float64(time.Now().UnixNano()) / 1e9)
	case ctx.Err() == nil:
		m.jobErrors.WithLabelValues(job).Inc()
	}
	return err
}

// IndexRunFinished учитывает исполненный прогон индексации.
func (m *Worker) IndexRunFinished(failed bool, took time.Duration) {
	if m == nil {
		return
	}
	result := "completed"
	if failed {
		result = "failed"
	}
	m.indexRuns.WithLabelValues(result).Inc()
	m.indexRunDuration.Observe(took.Seconds())
}

// LeasesExpired учитывает этапы, которые воркер вернул из-за истёкшей
// аренды.
func (m *Worker) LeasesExpired(n int) {
	if m == nil || n <= 0 {
		return
	}
	m.leasesExpired.Add(float64(n))
}

// State — снимок состояния базы для метрик воркера.
type State struct {
	QueuedRuns, RunningRuns int
	// OldestQueued — возраст самого старого прогона в очереди; 0 — пусто.
	OldestQueued  time.Duration
	LiveLeases    int
	ExpiredLeases int
	// HeadFailures — ветки с серией провалов чтения головы от двух.
	HeadFailures int
}

// SetState выставляет снимок состояния базы. Снимок берёт воркер на своём
// тике, а не scrape: сколько бы Prometheus ни опрашивал, база получает один
// запрос за период.
func (m *Worker) SetState(s State, at time.Time) {
	if m == nil {
		return
	}
	m.queueRuns.WithLabelValues("queued").Set(float64(s.QueuedRuns))
	m.queueRuns.WithLabelValues("running").Set(float64(s.RunningRuns))
	m.queueOldest.Set(s.OldestQueued.Seconds())
	m.leases.WithLabelValues("live").Set(float64(s.LiveLeases))
	m.leases.WithLabelValues("expired").Set(float64(s.ExpiredLeases))
	m.headFailures.Set(float64(s.HeadFailures))
	m.stateRefreshed.Set(float64(at.UnixNano()) / 1e9)
}

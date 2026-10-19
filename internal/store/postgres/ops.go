package postgres

import (
	"context"
	"time"
)

// OpsState — состояние базы, которое администратор видит метриками воркера
// (CLAB-126): очередь индексации, аренды агентов, нечитаемые головы веток.
// Только счётчики по всему инстансу: ни проектов, ни репозиториев в нём нет
// намеренно — метрики уходят в чужой Prometheus.
type OpsState struct {
	QueuedRuns  int
	RunningRuns int
	// OldestQueued — возраст самого старого прогона в очереди, включая
	// отложенные на повтор; 0 — очередь пуста.
	OldestQueued  time.Duration
	LiveLeases    int
	ExpiredLeases int
	HeadFailures  int
}

// OpsState снимает состояние одним проходом по трём источникам.
//
// Очередь читается по частичному индексу repo_index_runs_queue, аренды — по
// stage_runs_expiring_leases: оба с тем же предикатом, что здесь. Прогонов в
// running индекса нет, но таблица прогонов держится вычисткой (retain на
// ветку), а запрос идёт раз в тик воркера, не на каждый scrape.
//
// Аренда «истекла» по тому же правилу, что у ExpireLeases: claimed или
// in_progress с lease_expires_at не позже now(). Головы веток — по правилу
// «репозиторий не читается» (indexHealthFrom): серия от двух проходов по
// ветке, которая всё ещё отслеживается, у репозитория в целях индексации.
func (s *Store) OpsState(ctx context.Context, q Querier) (OpsState, error) {
	var st OpsState
	var oldestSeconds float64
	err := q.QueryRow(ctx, `
		SELECT
		    (SELECT count(*) FROM repo_index_runs WHERE state = 'queued'),
		    (SELECT count(*) FROM repo_index_runs WHERE state = 'running'),
		    coalesce((SELECT extract(epoch FROM now() - min(created_at))::float8
		              FROM repo_index_runs WHERE state = 'queued'), 0),
		    (SELECT count(*) FROM stage_runs
		     WHERE state IN ('claimed', 'in_progress') AND lease_expires_at > now()),
		    (SELECT count(*) FROM stage_runs
		     WHERE state IN ('claimed', 'in_progress') AND lease_expires_at <= now()),
		    (SELECT count(*) FROM repository_head_failures hf
		     JOIN repositories r ON r.id = hf.repository_id
		     LEFT JOIN github_installations gi ON gi.installation_id = r.installation_id
		     WHERE hf.failures >= 2 AND hf.branch = ANY (r.watch_branches)
		       AND r.index_enabled AND `+indexTargetLive+`)`).
		Scan(&st.QueuedRuns, &st.RunningRuns, &oldestSeconds,
			&st.LiveLeases, &st.ExpiredLeases, &st.HeadFailures)
	if err != nil {
		return OpsState{}, translate(err)
	}
	st.OldestQueued = time.Duration(oldestSeconds * float64(time.Second))
	return st, nil
}

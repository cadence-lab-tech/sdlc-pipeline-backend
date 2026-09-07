package postgres

import (
	"context"
	"strconv"
)

// Запросы под вебхуки GitHub. Обработчик вебхука только пишет в базу: ставит
// коммит в очередь индексации и помечает PR как требующие пересинхронизации.
// В сеть за состоянием проверок по-прежнему ходит воркер (решение 0020).

// TouchPullRequest обновляет состояние привязанного PR тем, что принёс
// вебхук, и сбрасывает отметку синхронизации: проверки на новом head
// приедут воркером на следующем проходе.
func (s *Store) TouchPullRequest(ctx context.Context, q Querier, provider, owner, name string, number int, state, headSHA, url string) (int, error) {
	tag, err := q.Exec(ctx, `
		UPDATE task_repo_refs rr
		SET state = coalesce(nullif($5, ''), rr.state),
		    head_sha = coalesce(nullif($6, ''), rr.head_sha),
		    url = coalesce(nullif($7, ''), rr.url),
		    synced_at = NULL
		FROM repositories r
		WHERE rr.repository_id = r.id
		  AND r.provider = $1 AND lower(r.owner) = lower($2) AND lower(r.name) = lower($3)
		  AND rr.ref_type = 'pull_request' AND rr.ref = $4`,
		provider, owner, name, strconv.Itoa(number), state, headSHA, url)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

// InvalidateChecks помечает PR с этим head как требующие пересинхронизации:
// пришло событие о проверке, и снимок в базе устарел.
func (s *Store) InvalidateChecks(ctx context.Context, q Querier, provider, owner, name, headSHA string) (int, error) {
	if headSHA == "" {
		return 0, nil
	}
	tag, err := q.Exec(ctx, `
		UPDATE task_repo_refs rr
		SET synced_at = NULL
		FROM repositories r
		WHERE rr.repository_id = r.id
		  AND r.provider = $1 AND lower(r.owner) = lower($2) AND lower(r.name) = lower($3)
		  AND rr.ref_type = 'pull_request' AND rr.head_sha = $4`,
		provider, owner, name, headSHA)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

// PruneIndexRuns удаляет старые завершённые прогоны индексации, оставляя
// keep последних на каждую ветку и все, на которые ссылаются базовые линии
// задач: от них считается дрейф.
//
// Индекс на коммит неизменяем и растёт линейно по коммитам; без вычистки
// code_chunks копятся на каждый пуш. Содержимое и векторы при этом остаются
// в blobs и embedding_cache — удаляются только строки прогона.
func (s *Store) PruneIndexRuns(ctx context.Context, q Querier, keep int) (int, error) {
	if keep < 1 {
		keep = 1
	}
	tag, err := q.Exec(ctx, `
		WITH ranked AS (
		    SELECT id, row_number() OVER (
		        PARTITION BY repository_id, branch ORDER BY finished_at DESC) AS rn
		    FROM repo_index_runs
		    WHERE state = 'completed'
		)
		DELETE FROM repo_index_runs ir
		USING ranked
		WHERE ir.id = ranked.id AND ranked.rn > $1
		  AND NOT EXISTS (SELECT 1 FROM task_baselines b WHERE b.index_run_id = ir.id)
		  AND NOT EXISTS (SELECT 1 FROM repo_index_runs c WHERE c.parent_index_run_id = ir.id
		                  AND c.state <> 'completed')`, keep)
	if err != nil {
		return 0, translate(err)
	}
	return int(tag.RowsAffected()), nil
}

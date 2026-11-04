package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Ранг бэклога (решение 0075): хранилище даёт блокировку группы, её задачи
// в порядке ранга и запись рангов. Правила перемещения — в движке
// (workflow.RankTask).

// rankGroupSQL — задачи группы ранга: верхний уровень проекта ($2 пуст)
// или дети одного родителя. Параметры $1 (проект) и $2 (родитель).
const rankGroupSQL = `t.project_id = $1 AND (($2::uuid IS NULL AND t.parent_id IS NULL) OR t.parent_id = $2)`

// LockRankGroup берёт до конца транзакции блокировку группы ранга. Под ней
// идут перемещение и заведение задачи: оба ставят ключ относительно соседей
// и края группы, и без очереди два одновременных вызова получили бы один
// ключ на двоих. Строки задач не блокируются — переход задачи по циклу
// перемещение не ждёт.
func (s *Store) LockRankGroup(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, parentID *uuid.UUID) error {
	_, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended(
		    'task_rank:' || $1::uuid::text || ':' || coalesce($2::uuid::text, ''), 0))`, projectID, parentID)
	return translate(err)
}

// RankEntry — задача группы ранга: ключ, ранг и закрыта ли она (статус
// конечный в версии цикла задачи).
type RankEntry struct {
	ID         uuid.UUID
	Key        string
	Rank       string
	Terminal   bool
	StatusName string
}

// RankGroup — все задачи группы, закрытые тоже, в порядке бэклога (ранг,
// затем номер).
func (s *Store) RankGroup(ctx context.Context, q Querier, projectID uuid.UUID, parentID *uuid.UUID) ([]RankEntry, error) {
	rows, err := q.Query(ctx, `
		SELECT t.id, t.key, t.rank, ws.is_terminal, ws.name
		FROM tasks t JOIN workflow_statuses ws ON ws.id = t.status_id
		WHERE `+rankGroupSQL+`
		ORDER BY `+backlogOrderSQL, projectID, parentID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := []RankEntry{}
	for rows.Next() {
		var e RankEntry
		if err := rows.Scan(&e.ID, &e.Key, &e.Rank, &e.Terminal, &e.StatusName); err != nil {
			return nil, translate(err)
		}
		out = append(out, e)
	}
	return out, translate(rows.Err())
}

// SetTaskRanks пишет ранги задачам: ids[i] получает ranks[i]. updated_at не
// трогается — перемещение и перераздача постановку не меняют. Строки с тем
// же рангом не переписываются.
func (s *Store) SetTaskRanks(ctx context.Context, tx pgx.Tx, ids []uuid.UUID, ranks []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		UPDATE tasks t SET rank = v.rank
		FROM unnest($1::uuid[], $2::text[]) AS v(id, rank)
		WHERE t.id = v.id AND t.rank IS DISTINCT FROM v.rank`, ids, ranks)
	return translate(err)
}

// appendRank — ранг новой задачи: после последнего в группе. Ключ вышел
// длиннее порога — группа перераздаётся равномерным набором в прежнем
// порядке, а новая задача получает следующий ключ набора.
func (s *Store) appendRank(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, parentID *uuid.UUID) (string, error) {
	if err := s.LockRankGroup(ctx, tx, projectID, parentID); err != nil {
		return "", err
	}
	var last *string
	if err := tx.QueryRow(ctx, `SELECT max(t.rank) FROM tasks t WHERE `+rankGroupSQL,
		projectID, parentID).Scan(&last); err != nil {
		return "", translate(err)
	}
	prev := ""
	if last != nil {
		prev = *last
	}
	if k, err := domain.RankBetween(prev, ""); err == nil && len(k) <= domain.RankMaxLen {
		return k, nil
	}
	group, err := s.RankGroup(ctx, tx, projectID, parentID)
	if err != nil {
		return "", err
	}
	keys := domain.RankSpread(len(group) + 1)
	ids := make([]uuid.UUID, len(group))
	for i, e := range group {
		ids[i] = e.ID
	}
	if err := s.SetTaskRanks(ctx, tx, ids, keys[:len(group)]); err != nil {
		return "", err
	}
	return keys[len(group)], nil
}

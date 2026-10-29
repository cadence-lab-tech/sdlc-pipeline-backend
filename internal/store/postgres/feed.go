package postgres

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// FeedCursor — позиция в ленте событий: метка и id последнего отданного
// события. Порядок ленты — (created_at, id), тот же, что у индекса
// task_events_cursor (миграция 000036). Наружу курсор уходит непрозрачной
// строкой (String), клиент его не разбирает.
type FeedCursor struct {
	At time.Time
	ID uuid.UUID
}

// String — base64url от «микросекунды.uuid». Микросекунды — точность
// timestamptz: курсор, пройдя через клиента, сравнивается с базой без
// округления.
func (c FeedCursor) String() string {
	raw := strconv.FormatInt(c.At.UnixMicro(), 10) + "." + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseFeedCursor — обратное к String. Испорченный курсор — invalid_input,
// а не пустая лента: клиент с битым курсором иначе ждал бы вечно.
func ParseFeedCursor(s string) (FeedCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return FeedCursor{}, apperr.InvalidInput("курсор событий испорчен")
	}
	micros, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return FeedCursor{}, apperr.InvalidInput("курсор событий испорчен")
	}
	n, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return FeedCursor{}, apperr.InvalidInput("курсор событий испорчен")
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return FeedCursor{}, apperr.InvalidInput("курсор событий испорчен")
	}
	return FeedCursor{At: time.UnixMicro(n).UTC(), ID: u}, nil
}

// FeedScope — чья лента: задачи или всего проекта. Задано ровно одно поле.
type FeedScope struct {
	TaskID    uuid.UUID
	ProjectID uuid.UUID
}

// FeedHead — курсор «с этого момента»: время базы и нулевой id. Голова не
// ищется по ленте: у проекта без свежих событий поиск последнего шёл бы
// индексом назад через чужие проекты, а «с этого момента» и есть смысл
// рукопожатия без after. Время берётся у базы, а не у процесса: метки
// событий ставит clock_timestamp() той же базы, и расхождение часов реплики
// с базой не теряет события.
func (s *Store) FeedHead(ctx context.Context, q Querier) (FeedCursor, error) {
	var at time.Time
	if err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return FeedCursor{}, translate(err)
	}
	return FeedCursor{At: at}, nil
}

// FeedAfter — события области после курсора по возрастанию (created_at, id),
// не больше limit. Лента задачи идёт индексом (task_id, created_at), лента
// проекта — диапазоном task_events_cursor с соединением на tasks.
func (s *Store) FeedAfter(ctx context.Context, q Querier, scope FeedScope, after FeedCursor, limit int) ([]domain.FeedEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	filter := `e.task_id = $1`
	arg := scope.TaskID
	if scope.TaskID == uuid.Nil {
		filter, arg = `t.project_id = $1`, scope.ProjectID
	}
	rows, err := q.Query(ctx, `
		SELECT e.id, e.type, t.key, p.handle, e.created_at
		FROM task_events e
		JOIN tasks t ON t.id = e.task_id
		LEFT JOIN principals p ON p.id = e.actor_id
		WHERE `+filter+` AND (e.created_at, e.id) > ($2, $3)
		ORDER BY e.created_at, e.id
		LIMIT $4`, arg, after.At, after.ID, limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.FeedEvent{}
	for rows.Next() {
		var e domain.FeedEvent
		if err := rows.Scan(&e.ID, &e.Type, &e.TaskKey, &e.Actor, &e.CreatedAt); err != nil {
			return nil, translate(err)
		}
		out = append(out, e)
	}
	return out, translate(rows.Err())
}

// FeedRecent — следы событий последних window по всей ленте, для будильника
// ожидающих: один короткий запрос на реплику API раз в такт, сколько бы
// ожидающих в ней ни было. Окно назад, а не курсор: событие с ранней меткой,
// зафиксированное позже соседнего, курсор бы перешагнул, а окно его ещё
// застанет. Новые следы будильник отличает от уже виденных по id.
func (s *Store) FeedRecent(ctx context.Context, q Querier, window time.Duration, limit int) ([]domain.FeedMark, error) {
	rows, err := q.Query(ctx, `
		SELECT e.id, e.task_id, t.project_id
		FROM task_events e
		JOIN tasks t ON t.id = e.task_id
		WHERE e.created_at > clock_timestamp() - make_interval(secs => $1)
		ORDER BY e.created_at, e.id
		LIMIT $2`, window.Seconds(), limit)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.FeedMark{}
	for rows.Next() {
		var m domain.FeedMark
		if err := rows.Scan(&m.ID, &m.TaskID, &m.ProjectID); err != nil {
			return nil, translate(err)
		}
		out = append(out, m)
	}
	return out, translate(rows.Err())
}

package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// AuditTarget — то, по чему запись журнала находит проект и задачу. Поля
// приходят из пути запроса или из аргументов тула как есть, без отдельного
// похода в базу на каждое: строка вставляется одним запросом, который сам
// сводит ключи и идентификаторы к проекту и задаче. Что не нашлось — NULL.
type AuditTarget struct {
	ProjectID  *uuid.UUID
	ProjectKey string
	TaskKey    string
	ArtifactID *uuid.UUID
	SignalID   *uuid.UUID
	StageRunID *uuid.UUID
	WorkflowID *uuid.UUID
	// Токен и приглашение — цели действия (отзыв), а не предъявленный токен:
	// по ним отзыв токена проекта попадает в журнал проекта.
	TokenID  *uuid.UUID
	InviteID *uuid.UUID
}

// NewAuditRecord — строка журнала к вставке. Параметры уже вычищены
// писателем (workflow.Audit): хранилище секретов не разбирает.
type NewAuditRecord struct {
	Target      AuditTarget
	PrincipalID *uuid.UUID
	TokenID     *uuid.UUID
	TokenName   *string
	TokenPrefix *string
	TokenKind   *string
	Channel     string
	Action      string
	Params      json.RawMessage
	Outcome     string
	HTTPStatus  *int
	RequestID   string
}

// InsertAudit вставляет запись журнала.
//
// Проект — явный, иначе по ключу, иначе проект задачи, версии цикла, токена
// или приглашения. Задача — по ключу, артефакту, сигналу или прогону; задача
// чужого проекта не пишется: запрос к проекту X с ключом задачи проекта Y
// остаётся записью проекта X без задачи.
func (s *Store) InsertAudit(ctx context.Context, q Querier, r NewAuditRecord) error {
	var taskProjectKey string
	if r.Target.TaskKey != "" {
		// Ключ без номера — не ключ задачи; запись всё равно пишется.
		taskProjectKey, _ = splitTaskKey(r.Target.TaskKey)
	}
	params := r.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	_, err := q.Exec(ctx, `
		INSERT INTO audit_log (project_id, task_id, principal_id, token_id,
		                       token_name, token_prefix, token_kind,
		                       channel, action, params, outcome, http_status, request_id)
		SELECT coalesce(pr.pid, t.project_id),
		       CASE WHEN pr.pid IS NULL OR t.project_id = pr.pid THEN t.id END,
		       $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, nullif($21, '')
		FROM (SELECT coalesce($1::uuid,
		                      (SELECT id FROM projects WHERE key = nullif($2, '')),
		                      (SELECT project_id FROM workflows WHERE id = $7),
		                      (SELECT project_id FROM agent_tokens WHERE id = $8),
		                      (SELECT project_id FROM invites WHERE id = $9)) AS pid) pr
		LEFT JOIN tasks t ON t.id = coalesce(
		    (SELECT tk.id FROM tasks tk JOIN projects p ON p.id = tk.project_id
		      WHERE p.key = nullif($10, '') AND tk.key = nullif($3, '')),
		    (SELECT task_id FROM artifacts WHERE id = $4),
		    (SELECT task_id FROM task_drift_signals WHERE id = $5),
		    (SELECT task_id FROM stage_runs WHERE id = $6))`,
		r.Target.ProjectID, r.Target.ProjectKey, r.Target.TaskKey,
		r.Target.ArtifactID, r.Target.SignalID, r.Target.StageRunID,
		r.Target.WorkflowID, r.Target.TokenID, r.Target.InviteID, taskProjectKey,
		r.PrincipalID, r.TokenID, r.TokenName, r.TokenPrefix, r.TokenKind,
		r.Channel, r.Action, params, r.Outcome, r.HTTPStatus, r.RequestID)
	return translate(err)
}

// AuditFilter — выборка журнала. Ровно одна область: задача, проект или
// инстанс (записи без проекта).
type AuditFilter struct {
	TaskID    *uuid.UUID
	ProjectID *uuid.UUID
	Instance  bool

	// Фильтры. Пустые не применяются.
	TaskKey         string // внутри проекта
	PrincipalKind   string
	PrincipalHandle string
	TokenID         *uuid.UUID
	TokenPrefix     string
	Channel         string
	Action          string
	Outcome         string
	Since           *time.Time
	Until           *time.Time

	Cursor string
	Limit  int
}

// AuditEntry — запись журнала в том виде, в каком её отдаёт API
// (docs/MCP_CONTRACT.md, «Журнал действий»).
type AuditEntry struct {
	ID         uuid.UUID       `json:"id"`
	At         time.Time       `json:"at"`
	ProjectKey *string         `json:"project_key"`
	TaskKey    *string         `json:"task_key"`
	Principal  *AuditPrincipal `json:"principal"`
	Token      *AuditToken     `json:"token"`
	Channel    string          `json:"channel"`
	Action     string          `json:"action"`
	Params     json.RawMessage `json:"params"`
	Outcome    AuditOutcome    `json:"outcome"`
	RequestID  *string         `json:"request_id"`
}

type AuditPrincipal struct {
	Handle string               `json:"handle"`
	Kind   domain.PrincipalKind `json:"kind"`
}

// AuditToken — снимок токена на момент вызова. id пропадает, если строку
// токена удалили, имя, префикс и вид остаются.
type AuditToken struct {
	ID     *uuid.UUID `json:"id"`
	Name   *string    `json:"name"`
	Prefix *string    `json:"prefix"`
	Kind   *string    `json:"kind"`
}

// AuditOutcome — исход: ok, код ошибки домена и HTTP-статус (только REST).
type AuditOutcome struct {
	OK     bool    `json:"ok"`
	Code   *string `json:"code"`
	Status *int    `json:"status"`
}

// AuditPage — страница журнала, новые сверху. Next — курсор следующей
// страницы, nil — дальше ничего.
type AuditPage struct {
	Entries []AuditEntry `json:"entries"`
	Next    *string      `json:"next"`
}

const (
	auditDefaultLimit = 50
	auditMaxLimit     = 200
)

// ListAudit отдаёт страницу журнала. Курсор — (created_at, id) последней
// записи страницы: смещение поехало бы, пока журнал пишется.
func (s *Store) ListAudit(ctx context.Context, q Querier, f AuditFilter) (AuditPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = auditDefaultLimit
	}
	if limit > auditMaxLimit {
		limit = auditMaxLimit
	}

	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	switch {
	case f.TaskID != nil:
		where = append(where, "a.task_id = "+arg(*f.TaskID))
	case f.ProjectID != nil:
		where = append(where, "a.project_id = "+arg(*f.ProjectID))
	case f.Instance:
		where = append(where, "a.project_id IS NULL")
	default:
		return AuditPage{}, apperr.Internal(nil, "выборка журнала без области")
	}
	if f.TaskKey != "" {
		where = append(where, "t.key = "+arg(f.TaskKey))
	}
	if f.PrincipalHandle != "" {
		where = append(where, "p.handle = "+arg(f.PrincipalHandle))
	}
	if f.PrincipalKind != "" {
		where = append(where, "p.kind = "+arg(f.PrincipalKind))
	}
	if f.TokenID != nil {
		where = append(where, "a.token_id = "+arg(*f.TokenID))
	}
	if f.TokenPrefix != "" {
		where = append(where, "a.token_prefix = "+arg(f.TokenPrefix))
	}
	if f.Channel != "" {
		where = append(where, "a.channel = "+arg(f.Channel))
	}
	if f.Action != "" {
		where = append(where, "a.action = "+arg(f.Action))
	}
	if f.Outcome != "" {
		where = append(where, "a.outcome = "+arg(f.Outcome))
	}
	if f.Since != nil {
		where = append(where, "a.created_at >= "+arg(*f.Since))
	}
	if f.Until != nil {
		where = append(where, "a.created_at < "+arg(*f.Until))
	}
	if f.Cursor != "" {
		at, id, err := decodeAuditCursor(f.Cursor)
		if err != nil {
			return AuditPage{}, err
		}
		where = append(where, "(a.created_at, a.id) < ("+arg(at)+", "+arg(id)+")")
	}

	rows, err := q.Query(ctx, `
		SELECT a.id, a.created_at, pr.key, t.key, p.handle, p.kind,
		       a.token_id, a.token_name, a.token_prefix, a.token_kind,
		       a.channel, a.action, a.params, a.outcome, a.http_status, a.request_id
		FROM audit_log a
		LEFT JOIN projects pr ON pr.id = a.project_id
		LEFT JOIN tasks t ON t.id = a.task_id
		LEFT JOIN principals p ON p.id = a.principal_id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT `+arg(limit+1), args...)
	if err != nil {
		return AuditPage{}, translate(err)
	}
	defer rows.Close()

	page := AuditPage{Entries: []AuditEntry{}}
	for rows.Next() {
		var e AuditEntry
		var handle, kind *string
		var tok AuditToken
		var outcome string
		if err := rows.Scan(&e.ID, &e.At, &e.ProjectKey, &e.TaskKey, &handle, &kind,
			&tok.ID, &tok.Name, &tok.Prefix, &tok.Kind,
			&e.Channel, &e.Action, &e.Params, &outcome, &e.Outcome.Status, &e.RequestID); err != nil {
			return AuditPage{}, translate(err)
		}
		if handle != nil {
			e.Principal = &AuditPrincipal{Handle: *handle, Kind: domain.PrincipalKind(*kind)}
		}
		if tok.ID != nil || tok.Name != nil || tok.Prefix != nil {
			e.Token = &tok
		}
		e.Outcome.OK = outcome == "ok"
		if !e.Outcome.OK {
			e.Outcome.Code = &outcome
		}
		page.Entries = append(page.Entries, e)
	}
	if err := rows.Err(); err != nil {
		return AuditPage{}, translate(err)
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		last := page.Entries[limit-1]
		next := encodeAuditCursor(last.At, last.ID)
		page.Next = &next
	}
	return page, nil
}

// PurgeAudit удаляет до limit записей старше before — самые старые первыми.
// Пачка с пределом, а не один DELETE: журнал за полгода — миллионы строк, и
// одна транзакция на всё держала бы блокировки и раздувала WAL.
func (s *Store) PurgeAudit(ctx context.Context, q Querier, before time.Time, limit int) (int64, error) {
	tag, err := q.Exec(ctx, `
		DELETE FROM audit_log WHERE id IN (
		    SELECT id FROM audit_log WHERE created_at < $1
		    ORDER BY created_at LIMIT $2)`, before, limit)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// TokenByHashAny находит токен по хэшу в любом состоянии — отозванный,
// истёкший, отключённого принципала. Нужен только журналу: отказ 401 по
// такому токену пишется со ссылкой на него, а TokenByHash их отсекает.
func (s *Store) TokenByHashAny(ctx context.Context, q Querier, hash string) (domain.AccessToken, domain.Principal, error) {
	var t domain.AccessToken
	var p domain.Principal
	err := q.QueryRow(ctx, `
		SELECT t.id, t.principal_id, t.kind, t.name, t.token_prefix,
		       p.id, p.kind, p.handle
		FROM agent_tokens t
		JOIN principals p ON p.id = t.principal_id
		WHERE t.token_hash = $1`, hash,
	).Scan(&t.ID, &t.PrincipalID, &t.TokenKind, &t.Name, &t.Prefix, &p.ID, &p.Kind, &p.Handle)
	if err != nil {
		return t, p, notFound(err, "токен не найден")
	}
	t.Handle, t.Kind = p.Handle, p.Kind
	return t, p, nil
}

func encodeAuditCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeAuditCursor(c string) (time.Time, uuid.UUID, error) {
	bad := apperr.InvalidInput("курсор журнала не разбирается")
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	ts, idText, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, bad
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	return at, id, nil
}

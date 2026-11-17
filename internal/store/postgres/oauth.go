package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Сервер авторизации OAuth 2.1 (CLAB-481, решение 0079). В базе только
// sha256 кодов и refresh; времена приходят параметром от workflow.OAuth, а
// не из now() базы: сроки кода, запроса и refresh проверяются по одним
// часам, и тест подменяет их, не трогая базу.

// ── клиенты DCR ─────────────────────────────────────────────────────────────

const oauthClientCols = `id, client_name, redirect_uris, software_id, software_version, created_at`

func scanOAuthClient(row interface{ Scan(...any) error }) (domain.OAuthClient, error) {
	var c domain.OAuthClient
	err := row.Scan(&c.ID, &c.Name, &c.RedirectURIs, &c.SoftwareID, &c.SoftwareVersion, &c.CreatedAt)
	return c, err
}

func (s *Store) CreateOAuthClient(ctx context.Context, q Querier, c domain.OAuthClient) (domain.OAuthClient, error) {
	out, err := scanOAuthClient(q.QueryRow(ctx, `
		INSERT INTO oauth_clients (id, client_name, redirect_uris, software_id, software_version, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+oauthClientCols,
		c.ID, c.Name, c.RedirectURIs, c.SoftwareID, c.SoftwareVersion, c.CreatedAt))
	return out, translate(err)
}

func (s *Store) GetOAuthClient(ctx context.Context, q Querier, id string) (domain.OAuthClient, error) {
	c, err := scanOAuthClient(q.QueryRow(ctx, `SELECT `+oauthClientCols+` FROM oauth_clients WHERE id = $1`, id))
	if err != nil {
		return c, notFound(err, "клиент не зарегистрирован")
	}
	return c, nil
}

// LockOAuthRegistrations — advisory-блокировка регистраций до конца
// транзакции: подсчёт пределов и вставка идут под ней, и две реплики api не
// пропустят по одной регистрации сверх предела каждая.
func (s *Store) LockOAuthRegistrations(ctx context.Context, q Querier) error {
	_, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('oauth_dcr_registrations'))`)
	return translate(err)
}

// OAuthRegistrationsSince — сколько клиентов зарегистрировано с момента
// since и когда самый ранний из них: по нему считается Retry-After.
func (s *Store) OAuthRegistrationsSince(ctx context.Context, q Querier, since time.Time) (int, *time.Time, error) {
	var n int
	var oldest *time.Time
	err := q.QueryRow(ctx, `
		SELECT count(*), min(created_at) FROM oauth_clients WHERE created_at > $1`, since).Scan(&n, &oldest)
	return n, oldest, translate(err)
}

// liveGrantOf — у клиента есть выдача с неотозванным токеном.
const liveGrantOf = `EXISTS (SELECT 1 FROM oauth_grants g JOIN agent_tokens t ON t.id = g.token_id
	WHERE g.client_id = c.id AND t.revoked_at IS NULL)`

// CountIdleOAuthClients — клиенты DCR без живой выдачи.
func (s *Store) CountIdleOAuthClients(ctx context.Context, q Querier) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM oauth_clients c WHERE NOT `+liveGrantOf).Scan(&n)
	return n, translate(err)
}

// PurgeIdleOAuthClients удаляет клиентов DCR без живой выдачи, у которых и
// регистрация, и погасание последней выдачи старше before.
func (s *Store) PurgeIdleOAuthClients(ctx context.Context, q Querier, before time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `
		DELETE FROM oauth_clients c
		WHERE NOT `+liveGrantOf+`
		  AND c.created_at < $1
		  AND COALESCE((SELECT max(t.revoked_at) FROM oauth_grants g JOIN agent_tokens t ON t.id = g.token_id
		                WHERE g.client_id = c.id), c.created_at) < $1`, before)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// ── запросы авторизации ─────────────────────────────────────────────────────

const oauthRequestCols = `id, client_id, client_kind, client_name, redirect_uri, state, code_challenge,
	scope, resource, project_key, created_at, expires_at, decision, decided_at, decided_by,
	code_hash, code_expires_at, code_used_at, grant_id`

func scanOAuthRequest(row interface{ Scan(...any) error }) (domain.OAuthAuthRequest, error) {
	var r domain.OAuthAuthRequest
	err := row.Scan(&r.ID, &r.ClientID, &r.ClientKind, &r.ClientName, &r.RedirectURI, &r.State, &r.CodeChallenge,
		&r.Scope, &r.Resource, &r.ProjectKey, &r.CreatedAt, &r.ExpiresAt, &r.Decision, &r.DecidedAt, &r.DecidedBy,
		&r.CodeHash, &r.CodeExpiresAt, &r.CodeUsedAt, &r.GrantID)
	return r, err
}

func (s *Store) CreateOAuthRequest(ctx context.Context, q Querier, r domain.OAuthAuthRequest) (domain.OAuthAuthRequest, error) {
	out, err := scanOAuthRequest(q.QueryRow(ctx, `
		INSERT INTO oauth_authorization_requests (client_id, client_kind, client_name, redirect_uri, state,
		    code_challenge, scope, resource, project_key, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING `+oauthRequestCols,
		r.ClientID, r.ClientKind, r.ClientName, r.RedirectURI, r.State,
		r.CodeChallenge, r.Scope, r.Resource, r.ProjectKey, r.CreatedAt, r.ExpiresAt))
	return out, translate(err)
}

// GetOAuthRequest — запрос по id; forUpdate берёт строку под блокировку:
// решение и обмен кода идут по одному.
func (s *Store) GetOAuthRequest(ctx context.Context, q Querier, id uuid.UUID, forUpdate bool) (domain.OAuthAuthRequest, error) {
	sql := `SELECT ` + oauthRequestCols + ` FROM oauth_authorization_requests WHERE id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	r, err := scanOAuthRequest(q.QueryRow(ctx, sql, id))
	if err != nil {
		return r, notFound(err, "запрос авторизации не найден")
	}
	return r, nil
}

// OAuthRequestByCode — запрос по sha256 кода, под блокировкой.
func (s *Store) OAuthRequestByCode(ctx context.Context, q Querier, codeHash string) (domain.OAuthAuthRequest, error) {
	r, err := scanOAuthRequest(q.QueryRow(ctx, `SELECT `+oauthRequestCols+`
		FROM oauth_authorization_requests WHERE code_hash = $1 FOR UPDATE`, codeHash))
	if err != nil {
		return r, notFound(err, "код не найден")
	}
	return r, nil
}

// DecideOAuthRequest записывает решение; у allow — хеш кода и его срок.
func (s *Store) DecideOAuthRequest(ctx context.Context, q Querier, id uuid.UUID, decision string, by uuid.UUID,
	at time.Time, codeHash *string, codeExpires *time.Time) error {
	tag, err := q.Exec(ctx, `
		UPDATE oauth_authorization_requests
		SET decision = $2, decided_by = $3, decided_at = $4, code_hash = $5, code_expires_at = $6
		WHERE id = $1 AND decision IS NULL`, id, decision, by, at, codeHash, codeExpires)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("запрос авторизации уже решён")
	}
	return nil
}

// UseOAuthCode гасит код и запоминает выданную по нему выдачу: повтор кода
// найдёт её и отзовёт.
func (s *Store) UseOAuthCode(ctx context.Context, q Querier, id uuid.UUID, at time.Time, grantID uuid.UUID) error {
	_, err := q.Exec(ctx, `
		UPDATE oauth_authorization_requests SET code_used_at = $2, grant_id = $3 WHERE id = $1`, id, at, grantID)
	return translate(err)
}

// PurgeOAuthRequests удаляет запросы, срок которых истёк раньше before.
func (s *Store) PurgeOAuthRequests(ctx context.Context, q Querier, before time.Time) (int64, error) {
	tag, err := q.Exec(ctx, `DELETE FROM oauth_authorization_requests WHERE expires_at < $1`, before)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

// ── выдачи ──────────────────────────────────────────────────────────────────

const oauthGrantSelect = `
	SELECT g.id, g.token_id, g.principal_id, g.client_id, g.client_kind, g.client_name, g.redirect_host,
	       g.resource, g.project_id, pr.key, g.scope, g.created_at, g.refreshed_at,
	       t.last_used_at, t.revoked_at, g.revoke_reason
	FROM oauth_grants g
	JOIN agent_tokens t ON t.id = g.token_id
	LEFT JOIN projects pr ON pr.id = g.project_id`

func scanOAuthGrant(row interface{ Scan(...any) error }) (domain.OAuthGrant, error) {
	var g domain.OAuthGrant
	err := row.Scan(&g.ID, &g.TokenID, &g.PrincipalID, &g.ClientID, &g.ClientKind, &g.ClientName, &g.RedirectHost,
		&g.Resource, &g.ProjectID, &g.ProjectKey, &g.Scope, &g.CreatedAt, &g.RefreshedAt,
		&g.LastUsedAt, &g.RevokedAt, &g.RevokeReason)
	return g, err
}

type CreateOAuthGrantInput struct {
	TokenID      uuid.UUID
	PrincipalID  uuid.UUID
	ClientID     string
	ClientKind   string
	ClientName   string
	RedirectHost string
	Resource     string
	ProjectID    *uuid.UUID
	Scope        string
	CreatedAt    time.Time
}

func (s *Store) CreateOAuthGrant(ctx context.Context, q Querier, in CreateOAuthGrantInput) (domain.OAuthGrant, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO oauth_grants (token_id, principal_id, client_id, client_kind, client_name, redirect_host,
		                          resource, project_id, scope, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		in.TokenID, in.PrincipalID, in.ClientID, in.ClientKind, in.ClientName, in.RedirectHost,
		in.Resource, in.ProjectID, in.Scope, in.CreatedAt).Scan(&id)
	if err != nil {
		return domain.OAuthGrant{}, translate(err)
	}
	return s.GetOAuthGrant(ctx, q, id, false)
}

// GetOAuthGrant — выдача по id; forUpdate блокирует строку выдачи: refresh,
// отзыв и повтор кода одной выдачи идут по одному.
func (s *Store) GetOAuthGrant(ctx context.Context, q Querier, id uuid.UUID, forUpdate bool) (domain.OAuthGrant, error) {
	sql := oauthGrantSelect + ` WHERE g.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF g`
	}
	g, err := scanOAuthGrant(q.QueryRow(ctx, sql, id))
	if err != nil {
		return g, notFound(err, "выдача не найдена")
	}
	return g, nil
}

// OAuthGrantByAccessHash — выдача по sha256 access-токена, в любом
// состоянии: отзыв по RFC 7009 принимает и истёкший access.
func (s *Store) OAuthGrantByAccessHash(ctx context.Context, q Querier, hash string) (domain.OAuthGrant, error) {
	g, err := scanOAuthGrant(q.QueryRow(ctx, oauthGrantSelect+` WHERE t.token_hash = $1`, hash))
	if err != nil {
		return g, notFound(err, "выдача не найдена")
	}
	return g, nil
}

// ListOAuthGrants — выдачи человека, живые сначала.
func (s *Store) ListOAuthGrants(ctx context.Context, q Querier, principalID uuid.UUID) ([]domain.OAuthGrant, error) {
	rows, err := q.Query(ctx, oauthGrantSelect+`
		WHERE g.principal_id = $1 ORDER BY t.revoked_at NULLS FIRST, g.created_at DESC`, principalID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := []domain.OAuthGrant{}
	for rows.Next() {
		g, err := scanOAuthGrant(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, g)
	}
	return out, translate(rows.Err())
}

// RevokeOAuthGrant гасит токен выдачи и пишет причину. false — выдача уже
// была отозвана: причина остаётся первая.
func (s *Store) RevokeOAuthGrant(ctx context.Context, q Querier, grantID uuid.UUID, reason string, at time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `
		UPDATE agent_tokens SET revoked_at = $2
		WHERE id = (SELECT token_id FROM oauth_grants WHERE id = $1) AND revoked_at IS NULL`, grantID, at)
	if err != nil {
		return false, translate(err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	_, err = q.Exec(ctx, `
		UPDATE oauth_grants SET revoke_reason = $2 WHERE id = $1 AND revoke_reason IS NULL`, grantID, reason)
	return true, translate(err)
}

// MarkGrantRevokedByToken — причина отзыва выдачи, чей токен отозвали
// общим путём (DELETE /v1/auth/tokens/:id). Не выдача — ничего.
func (s *Store) MarkGrantRevokedByToken(ctx context.Context, q Querier, tokenID uuid.UUID, reason string) error {
	_, err := q.Exec(ctx, `
		UPDATE oauth_grants SET revoke_reason = $2 WHERE token_id = $1 AND revoke_reason IS NULL`, tokenID, reason)
	return translate(err)
}

// RotateGrantToken заменяет у строки токена выдачи хеш, префикс и срок:
// прежний access гаснет в ту же транзакцию, строка и token_id остаются.
func (s *Store) RotateGrantToken(ctx context.Context, q Querier, grantID uuid.UUID, hash, prefix string, expires, at time.Time) error {
	_, err := q.Exec(ctx, `
		UPDATE agent_tokens SET token_hash = $2, token_prefix = $3, expires_at = $4
		WHERE id = (SELECT token_id FROM oauth_grants WHERE id = $1)`, grantID, hash, prefix, expires)
	if err != nil {
		return translate(err)
	}
	_, err = q.Exec(ctx, `UPDATE oauth_grants SET refreshed_at = $2 WHERE id = $1`, grantID, at)
	return translate(err)
}

// ExpiredOAuthGrants — живые выдачи, у которых нет действующего refresh:
// единственный неиспользованный истёк (или его нет вовсе).
func (s *Store) ExpiredOAuthGrants(ctx context.Context, q Querier, now time.Time) ([]domain.OAuthGrant, error) {
	rows, err := q.Query(ctx, oauthGrantSelect+`
		WHERE t.revoked_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM oauth_refresh_tokens r
		                  WHERE r.grant_id = g.id AND r.used_at IS NULL AND r.expires_at > $1)`, now)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := []domain.OAuthGrant{}
	for rows.Next() {
		g, err := scanOAuthGrant(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, g)
	}
	return out, translate(rows.Err())
}

// ── refresh ─────────────────────────────────────────────────────────────────

const oauthRefreshCols = `id, grant_id, parent_id, created_at, expires_at, used_at`

func scanOAuthRefresh(row interface{ Scan(...any) error }) (domain.OAuthRefresh, error) {
	var r domain.OAuthRefresh
	err := row.Scan(&r.ID, &r.GrantID, &r.ParentID, &r.CreatedAt, &r.ExpiresAt, &r.UsedAt)
	return r, err
}

func (s *Store) CreateOAuthRefresh(ctx context.Context, q Querier, grantID uuid.UUID, hash string, parent *uuid.UUID,
	at, expires time.Time) error {
	_, err := q.Exec(ctx, `
		INSERT INTO oauth_refresh_tokens (grant_id, token_hash, parent_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)`, grantID, hash, parent, at, expires)
	return translate(err)
}

// OAuthRefreshByHash — refresh по sha256, погашенный тоже: по нему узнаётся
// повтор.
func (s *Store) OAuthRefreshByHash(ctx context.Context, q Querier, hash string) (domain.OAuthRefresh, error) {
	r, err := scanOAuthRefresh(q.QueryRow(ctx, `SELECT `+oauthRefreshCols+`
		FROM oauth_refresh_tokens WHERE token_hash = $1`, hash))
	if err != nil {
		return r, notFound(err, "refresh не найден")
	}
	return r, nil
}

// LiveOAuthRefresh — действующий refresh выдачи; false — его нет.
func (s *Store) LiveOAuthRefresh(ctx context.Context, q Querier, grantID uuid.UUID) (domain.OAuthRefresh, bool, error) {
	r, err := scanOAuthRefresh(q.QueryRow(ctx, `SELECT `+oauthRefreshCols+`
		FROM oauth_refresh_tokens WHERE grant_id = $1 AND used_at IS NULL`, grantID))
	if err != nil {
		if isNoRows(err) {
			return r, false, nil
		}
		return r, false, translate(err)
	}
	return r, true, nil
}

// UseOAuthRefresh гасит refresh; false — он уже погашен.
func (s *Store) UseOAuthRefresh(ctx context.Context, q Querier, id uuid.UUID, at time.Time) (bool, error) {
	tag, err := q.Exec(ctx, `UPDATE oauth_refresh_tokens SET used_at = $2 WHERE id = $1 AND used_at IS NULL`, id, at)
	if err != nil {
		return false, translate(err)
	}
	return tag.RowsAffected() == 1, nil
}

// PurgeDeadGrantRefresh удаляет refresh выдач с отозванным токеном: по ним
// уже ничего не выпустить, а повтор отозванной выдачи и так invalid_grant.
func (s *Store) PurgeDeadGrantRefresh(ctx context.Context, q Querier) (int64, error) {
	tag, err := q.Exec(ctx, `
		DELETE FROM oauth_refresh_tokens r USING oauth_grants g, agent_tokens t
		WHERE r.grant_id = g.id AND t.id = g.token_id AND t.revoked_at IS NOT NULL`)
	if err != nil {
		return 0, translate(err)
	}
	return tag.RowsAffected(), nil
}

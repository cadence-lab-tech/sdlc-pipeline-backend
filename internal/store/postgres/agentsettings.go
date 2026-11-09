package postgres

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Версии настройки агентов проекта (CLAB-425, решение 0077 п. 4.3). Здесь —
// запросы; проверка документа, права и порядок операций — в движке
// (workflow/agentsettings.go). Инвариант «не больше одного черновика и одной
// активной» держат частичные уникальные индексы миграции 000047, поэтому
// гонку двух заведений или активаций решает база, а не чтение перед записью.

const agentSettingsSelect = `
	SELECT v.id, v.project_id, v.version, v.state, v.document, v.changelog,
	       cb.handle, v.created_at, ab.handle, v.activated_at
	FROM agent_settings_versions v
	LEFT JOIN principals cb ON cb.id = v.created_by
	LEFT JOIN principals ab ON ab.id = v.activated_by`

func scanAgentSettings(row interface{ Scan(...any) error }) (domain.AgentSettingsVersion, error) {
	var v domain.AgentSettingsVersion
	var doc []byte
	err := row.Scan(&v.ID, &v.ProjectID, &v.Version, &v.State, &doc, &v.Changelog,
		&v.CreatedBy, &v.CreatedAt, &v.ActivatedBy, &v.ActivatedAt)
	v.Document = json.RawMessage(doc)
	return v, err
}

// ActiveAgentSettings — активная версия проекта; нет её — not_found.
func (s *Store) ActiveAgentSettings(ctx context.Context, q Querier, projectID uuid.UUID) (domain.AgentSettingsVersion, error) {
	v, err := scanAgentSettings(q.QueryRow(ctx, agentSettingsSelect+`
		WHERE v.project_id = $1 AND v.state = 'active'`, projectID))
	if err != nil {
		return v, notFound(err, "у проекта нет активной настройки агентов")
	}
	return v, nil
}

// ActiveAgentSettingsID — id активной версии для снимка на захвате; nil —
// настройки нет. Отдельный узкий запрос: захват не читает документ.
func (s *Store) ActiveAgentSettingsID(ctx context.Context, q Querier, projectID uuid.UUID) (*uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		SELECT id FROM agent_settings_versions WHERE project_id = $1 AND state = 'active'`,
		projectID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, translate(err)
	}
	return &id, nil
}

// AgentSettingsByVersion — версия проекта по номеру, в том числе черновик.
func (s *Store) AgentSettingsByVersion(ctx context.Context, q Querier, projectID uuid.UUID, version int) (domain.AgentSettingsVersion, error) {
	v, err := scanAgentSettings(q.QueryRow(ctx, agentSettingsSelect+`
		WHERE v.project_id = $1 AND v.version = $2`, projectID, version))
	if err != nil {
		return v, notFound(err, "версии %d настройки агентов у проекта нет", version)
	}
	return v, nil
}

// AgentSettingsByID — версия по id: снимок прогона ссылается на неё.
func (s *Store) AgentSettingsByID(ctx context.Context, q Querier, id uuid.UUID) (domain.AgentSettingsVersion, error) {
	v, err := scanAgentSettings(q.QueryRow(ctx, agentSettingsSelect+` WHERE v.id = $1`, id))
	if err != nil {
		return v, notFound(err, "версия настройки агентов не найдена")
	}
	return v, nil
}

// ListAgentSettings — все версии проекта по номеру, с документами: список в
// ответе их не несёт, но sha256 считается по документу.
func (s *Store) ListAgentSettings(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.AgentSettingsVersion, error) {
	rows, err := q.Query(ctx, agentSettingsSelect+`
		WHERE v.project_id = $1 ORDER BY v.version`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := []domain.AgentSettingsVersion{}
	for rows.Next() {
		v, err := scanAgentSettings(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, v)
	}
	return out, translate(rows.Err())
}

// LockAgentSettingsDraft — черновик проекта под FOR UPDATE; нет — not_found.
// Вторая из двух одновременных активаций ждёт первую и после неё черновика
// не находит: строка уже active, условие state = 'draft' перепроверяется.
func (s *Store) LockAgentSettingsDraft(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) (domain.AgentSettingsVersion, error) {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `
		SELECT id FROM agent_settings_versions
		WHERE project_id = $1 AND state = 'draft' FOR UPDATE`, projectID).Scan(&id); err != nil {
		return domain.AgentSettingsVersion{}, notFound(err, "черновика настройки агентов у проекта нет")
	}
	return s.AgentSettingsByID(ctx, tx, id)
}

// InsertAgentSettingsDraft заводит черновик с номером «наибольший у проекта
// + 1». Черновик уже есть (в том числе заведённый соседним запросом между
// чтением и записью) — conflict: это ловит индекс, а не проверка до записи.
func (s *Store) InsertAgentSettingsDraft(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, doc json.RawMessage, changelog *string, by uuid.UUID) (domain.AgentSettingsVersion, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO agent_settings_versions (project_id, version, state, document, changelog, created_by)
		VALUES ($1,
		        (SELECT coalesce(max(version), 0) + 1 FROM agent_settings_versions WHERE project_id = $1),
		        'draft', $2, $3, $4)
		RETURNING id`, projectID, []byte(doc), changelog, nullUUID(by)).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.AgentSettingsVersion{}, apperr.Conflict(
				"у проекта уже есть черновик настройки агентов: замените его (PUT …/agent-settings/draft) или удалите")
		}
		return domain.AgentSettingsVersion{}, translate(err)
	}
	return s.AgentSettingsByID(ctx, tx, id)
}

// UpdateAgentSettingsDraft заменяет документ и описание версии id — только
// черновика. Версия active или retired — conflict: правится только черновик
// (решение 0016), и это правило стоит на записи, а не на ручке.
func (s *Store) UpdateAgentSettingsDraft(ctx context.Context, tx pgx.Tx, id uuid.UUID, doc json.RawMessage, changelog *string) (domain.AgentSettingsVersion, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE agent_settings_versions SET document = $2, changelog = $3
		WHERE id = $1 AND state = 'draft'`, id, []byte(doc), changelog)
	if err != nil {
		return domain.AgentSettingsVersion{}, translate(err)
	}
	if tag.RowsAffected() == 0 {
		v, err := s.AgentSettingsByID(ctx, tx, id)
		if err != nil {
			return v, err
		}
		return v, apperr.Conflict("правится только черновик: версия %d настройки агентов в состоянии %s", v.Version, v.State).
			WithDetails(map[string]any{"version": v.Version, "state": v.State})
	}
	return s.AgentSettingsByID(ctx, tx, id)
}

// DeleteAgentSettingsDraft удаляет черновик проекта; нет — not_found.
func (s *Store) DeleteAgentSettingsDraft(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) (domain.AgentSettingsVersion, error) {
	v, err := s.LockAgentSettingsDraft(ctx, tx, projectID)
	if err != nil {
		return v, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_settings_versions WHERE id = $1 AND state = 'draft'`, v.ID); err != nil {
		return v, translate(err)
	}
	return v, nil
}

// RetireAgentSettings снимает активную версию проекта, если она есть, и
// отдаёт её номер. Идёт раньше ActivateAgentSettings в той же транзакции:
// индекс одной активной проверяется по оператору, как при активации цикла.
func (s *Store) RetireAgentSettings(ctx context.Context, tx pgx.Tx, projectID uuid.UUID) (*int, error) {
	var version int
	err := tx.QueryRow(ctx, `
		UPDATE agent_settings_versions SET state = 'retired'
		WHERE project_id = $1 AND state = 'active'
		RETURNING version`, projectID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, translate(err)
	}
	return &version, nil
}

// ActivateAgentSettings делает черновик id активным от имени by.
func (s *Store) ActivateAgentSettings(ctx context.Context, tx pgx.Tx, id, by uuid.UUID) (domain.AgentSettingsVersion, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE agent_settings_versions SET state = 'active', activated_by = $2, activated_at = now()
		WHERE id = $1 AND state = 'draft'`, id, nullUUID(by))
	if err != nil {
		return domain.AgentSettingsVersion{}, translate(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.AgentSettingsVersion{}, apperr.NotFound("черновика настройки агентов у проекта нет")
	}
	return s.AgentSettingsByID(ctx, tx, id)
}

// nullUUID — пустой id как NULL: внутренние пути без принципала.
func nullUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

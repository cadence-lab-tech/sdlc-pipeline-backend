package postgres

// Библиотека инструкций проекта (000040, решение 0069). Здесь только
// запросы: форма ключа и содержимого, «не изменилось», перенос закрепления
// и «обновить в ролях» — в workflow.

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// latestRoleDocs — документы последних версий неархивных ролей проекта $1:
// роль, номер версии, имя и sha документа, закреплённая версия инструкции.
// Последняя версия — той же точкой, что у захвата (latestRoleVersionOf).
// Отставание, «где используется» и сводка списка считаются от неё одной.
var latestRoleDocs = `
	SELECT r.id AS role_id, r.key AS role_key, r.name AS role_name, rv.version AS role_version,
	       d.name AS doc_name, d.blob_sha AS doc_sha, d.position AS doc_position,
	       iv.instruction_id AS pin_instruction, iv.version AS pin_version
	FROM roles r
	JOIN role_versions rv ON rv.id = ` + latestRoleVersionOf("r.id") + `
	JOIN role_version_documents d ON d.role_version_id = rv.id
	LEFT JOIN instruction_versions iv ON iv.id = d.instruction_version_id
	WHERE r.project_id = $1 AND r.archived_at IS NULL`

const instructionCols = `i.id, i.project_id, i.key, i.name, i.summary, i.created_by, i.created_at,
	coalesce((SELECT max(version) FROM instruction_versions v WHERE v.instruction_id = i.id), 0)`

func scanInstruction(row interface{ Scan(...any) error }) (domain.Instruction, error) {
	var in domain.Instruction
	err := row.Scan(&in.ID, &in.ProjectID, &in.Key, &in.Name, &in.Summary, &in.CreatedBy,
		&in.CreatedAt, &in.LatestVersion)
	return in, err
}

// ListInstructions — инструкции проекта по ключу со сводкой закреплений по
// последним версиям ролей.
func (s *Store) ListInstructions(ctx context.Context, q Querier, projectID uuid.UUID) ([]domain.Instruction, error) {
	rows, err := q.Query(ctx, `SELECT `+instructionCols+`
		FROM instructions i WHERE i.project_id = $1 ORDER BY i.key`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	out := []domain.Instruction{}
	for rows.Next() {
		in, err := scanInstruction(rows)
		if err != nil {
			rows.Close()
			return nil, translate(err)
		}
		out = append(out, in)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	if err := s.attachPinCounts(ctx, q, projectID, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachPinCounts — сколько последних версий ролей закрепили инструкцию и
// сколько из них ниже её последней версии.
func (s *Store) attachPinCounts(ctx context.Context, q Querier, projectID uuid.UUID, list []domain.Instruction) error {
	if len(list) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `
		SELECT l.pin_instruction, count(*),
		       count(*) FILTER (WHERE l.pin_version < (SELECT max(version) FROM instruction_versions v
		                                                WHERE v.instruction_id = l.pin_instruction))
		FROM (`+latestRoleDocs+`) l
		WHERE l.pin_instruction IS NOT NULL
		GROUP BY l.pin_instruction`, projectID)
	if err != nil {
		return translate(err)
	}
	defer rows.Close()
	type counts struct{ pinned, lagging int }
	by := map[uuid.UUID]counts{}
	for rows.Next() {
		var id uuid.UUID
		var c counts
		if err := rows.Scan(&id, &c.pinned, &c.lagging); err != nil {
			return translate(err)
		}
		by[id] = c
	}
	if err := rows.Err(); err != nil {
		return translate(err)
	}
	for i := range list {
		c := by[list[i].ID]
		list[i].PinnedRoles, list[i].LaggingRoles = c.pinned, c.lagging
	}
	return nil
}

// GetInstructionByKey — инструкция проекта по ключу со сводкой.
func (s *Store) GetInstructionByKey(ctx context.Context, q Querier, projectID uuid.UUID, key string) (domain.Instruction, error) {
	in, err := scanInstruction(q.QueryRow(ctx, `SELECT `+instructionCols+`
		FROM instructions i WHERE i.project_id = $1 AND i.key = $2`, projectID, key))
	if err != nil {
		return in, notFound(err, "инструкции %q нет в проекте", key)
	}
	list := []domain.Instruction{in}
	if err := s.attachPinCounts(ctx, q, projectID, list); err != nil {
		return in, err
	}
	return list[0], nil
}

// LockInstruction берёт строку инструкции под FOR UPDATE: номер новой
// версии выдаётся из максимума, и две одновременные версии иначе взяли бы
// один номер (отказ уникальности вместо очереди).
func (s *Store) LockInstruction(ctx context.Context, q Querier, id uuid.UUID) error {
	var got uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM instructions WHERE id = $1 FOR UPDATE`, id).Scan(&got)
	return notFound(err, "инструкция не найдена")
}

type CreateInstructionInput struct {
	ProjectID uuid.UUID
	Key       string
	Name      string
	Summary   *string
	CreatedBy uuid.UUID
}

// CreateInstruction заводит инструкцию без версий; версию 1 заводит
// вызывающий в той же транзакции.
func (s *Store) CreateInstruction(ctx context.Context, q Querier, in CreateInstructionInput) (domain.Instruction, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO instructions (project_id, key, name, summary, created_by)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (project_id, key) DO NOTHING
		RETURNING id`, in.ProjectID, in.Key, in.Name, in.Summary, nilUUID(in.CreatedBy)).Scan(&id)
	if err != nil {
		if apperr.As(translate(err)).Code == apperr.CodeNotFound {
			return domain.Instruction{}, apperr.Conflict("инструкция %s уже есть в проекте", in.Key).
				WithDetails(map[string]any{"key": in.Key})
		}
		return domain.Instruction{}, translate(err)
	}
	return s.GetInstructionByKey(ctx, q, in.ProjectID, in.Key)
}

// UpdateInstruction меняет только вывеску: ключ и версии не правятся.
func (s *Store) UpdateInstruction(ctx context.Context, q Querier, id uuid.UUID, name, summary *string) error {
	_, err := q.Exec(ctx, `
		UPDATE instructions SET name = coalesce($2, name), summary = coalesce($3, summary)
		WHERE id = $1`, id, name, summary)
	return translate(err)
}

type CreateInstructionVersionInput struct {
	InstructionID uuid.UUID
	Kind          string
	Content       []byte
	Changelog     *string
	CreatedBy     uuid.UUID
}

const instructionVersionCols = `v.id, v.instruction_id, v.version, v.kind, v.blob_sha, b.size_bytes,
	v.changelog, v.created_by, p.handle, v.created_at`

const instructionVersionFrom = `
	FROM instruction_versions v
	JOIN blobs b ON b.sha256 = v.blob_sha
	LEFT JOIN principals p ON p.id = v.created_by`

func scanInstructionVersion(row interface{ Scan(...any) error }) (domain.InstructionVersion, error) {
	var v domain.InstructionVersion
	err := row.Scan(&v.ID, &v.InstructionID, &v.Version, &v.Kind, &v.SHA256, &v.SizeBytes,
		&v.Changelog, &v.CreatedBy, &v.CreatedByName, &v.CreatedAt)
	return v, err
}

// CreateInstructionVersion кладёт содержимое в blobs и заводит следующую
// версию. Вызывающий держит транзакцию и строку инструкции под замком
// (LockInstruction).
func (s *Store) CreateInstructionVersion(ctx context.Context, q Querier, in CreateInstructionVersionInput) (domain.InstructionVersion, error) {
	sha, err := s.PutBlob(ctx, q, in.Content, "text/markdown")
	if err != nil {
		return domain.InstructionVersion{}, err
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO instruction_versions (instruction_id, version, kind, blob_sha, changelog, created_by)
		SELECT $1, coalesce(max(version), 0) + 1, $2, $3, $4, $5
		FROM instruction_versions WHERE instruction_id = $1
		RETURNING id`, in.InstructionID, in.Kind, sha, in.Changelog, nilUUID(in.CreatedBy)).Scan(&id); err != nil {
		return domain.InstructionVersion{}, translate(err)
	}
	v, err := scanInstructionVersion(q.QueryRow(ctx, `SELECT `+instructionVersionCols+instructionVersionFrom+`
		WHERE v.id = $1`, id))
	if err != nil {
		return v, translate(err)
	}
	text := string(in.Content)
	v.Content = &text
	return v, nil
}

// ListInstructionVersions — версии инструкции от новой к старой, без
// содержимого.
func (s *Store) ListInstructionVersions(ctx context.Context, q Querier, instructionID uuid.UUID) ([]domain.InstructionVersion, error) {
	rows, err := q.Query(ctx, `SELECT `+instructionVersionCols+instructionVersionFrom+`
		WHERE v.instruction_id = $1 ORDER BY v.version DESC`, instructionID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := []domain.InstructionVersion{}
	for rows.Next() {
		v, err := scanInstructionVersion(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, v)
	}
	return out, translate(rows.Err())
}

// GetInstructionVersion — версия по номеру; withContent — с текстом.
func (s *Store) GetInstructionVersion(ctx context.Context, q Querier, instructionID uuid.UUID, version int, withContent bool) (domain.InstructionVersion, error) {
	v, err := scanInstructionVersion(q.QueryRow(ctx, `SELECT `+instructionVersionCols+instructionVersionFrom+`
		WHERE v.instruction_id = $1 AND v.version = $2`, instructionID, version))
	if err != nil {
		return v, notFound(err, "версии %d инструкции нет", version)
	}
	if withContent {
		return v, s.attachInstructionContent(ctx, q, &v)
	}
	return v, nil
}

// FindInstructionVersionBySHA — версия инструкции с этими байтами; у
// нескольких версий одного текста (возврат к прежнему) — последняя.
// Нет такой — not_found.
func (s *Store) FindInstructionVersionBySHA(ctx context.Context, q Querier, instructionID uuid.UUID, sha string) (domain.InstructionVersion, error) {
	v, err := scanInstructionVersion(q.QueryRow(ctx, `SELECT `+instructionVersionCols+instructionVersionFrom+`
		WHERE v.instruction_id = $1 AND v.blob_sha = $2 ORDER BY v.version DESC LIMIT 1`, instructionID, sha))
	if err != nil {
		return v, notFound(err, "версии с этим текстом у инструкции нет")
	}
	return v, nil
}

func (s *Store) attachInstructionContent(ctx context.Context, q Querier, v *domain.InstructionVersion) error {
	content, _, err := s.BlobContent(ctx, q, v.SHA256)
	if err != nil {
		return err
	}
	text := string(content)
	v.Content = &text
	return nil
}

// InstructionUsage — последние версии неархивных ролей проекта с
// документом <ключ>.md: закреплённым за любой версией этой инструкции или
// обычным (Pinned nil). Документ, закреплённый за другой инструкцией под
// тем же именем, невозможен: имя закреплённого — всегда <ключ>.md.
func (s *Store) InstructionUsage(ctx context.Context, q Querier, projectID, instructionID uuid.UUID, key string) ([]domain.InstructionUsage, error) {
	rows, err := q.Query(ctx, `
		SELECT l.role_key, l.role_name, l.role_version, l.pin_version, l.doc_sha
		FROM (`+latestRoleDocs+`) l
		WHERE l.doc_name = $2 AND (l.pin_instruction IS NULL OR l.pin_instruction = $3)
		ORDER BY l.role_key`, projectID, key+".md", instructionID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := []domain.InstructionUsage{}
	for rows.Next() {
		var u domain.InstructionUsage
		if err := rows.Scan(&u.RoleKey, &u.RoleName, &u.RoleVersion, &u.Pinned, &u.SHA256); err != nil {
			return nil, translate(err)
		}
		out = append(out, u)
	}
	return out, translate(rows.Err())
}

// RoleInstructionLags — отставание ролей проекта по библиотеке: роль →
// закрепления её последней версии ниже последней версии инструкции.
// Считается по всем ролям, включая архивные: список с include_archived
// показывает их так же.
func (s *Store) RoleInstructionLags(ctx context.Context, q Querier, projectID uuid.UUID) (map[uuid.UUID][]domain.InstructionLag, error) {
	rows, err := q.Query(ctx, `
		SELECT r.id, i.key, iv.version, latest.version
		FROM roles r
		JOIN role_versions rv ON rv.id = `+latestRoleVersionOf("r.id")+`
		JOIN role_version_documents d ON d.role_version_id = rv.id
		JOIN instruction_versions iv ON iv.id = d.instruction_version_id
		JOIN instructions i ON i.id = iv.instruction_id
		CROSS JOIN LATERAL (SELECT max(version) AS version FROM instruction_versions v
		                    WHERE v.instruction_id = i.id) latest
		WHERE r.project_id = $1 AND iv.version < latest.version
		ORDER BY r.id, d.position`, projectID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]domain.InstructionLag{}
	for rows.Next() {
		var id uuid.UUID
		var l domain.InstructionLag
		if err := rows.Scan(&id, &l.Key, &l.Pinned, &l.Latest); err != nil {
			return nil, translate(err)
		}
		out[id] = append(out[id], l)
	}
	return out, translate(rows.Err())
}

// nilUUID — пустой идентификатор как NULL: в режиме заголовка без
// принципала created_by не ставится.
func nilUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

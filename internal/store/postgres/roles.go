package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

const roleVersionCols = `id, role_id, version, instructions, input_kinds, output_contract,
	       tool_policy, model_hint, model_level, repo_scope, context_budget, changelog, created_at`

func scanRoleVersion(row interface{ Scan(...any) error }) (domain.RoleVersion, error) {
	var rv domain.RoleVersion
	err := row.Scan(&rv.ID, &rv.RoleID, &rv.Version, &rv.Instructions, &rv.InputKinds,
		&rv.OutputContract, &rv.ToolPolicy, &rv.ModelHint, &rv.ModelLevel, &rv.RepoScope,
		&rv.ContextBudget, &rv.Changelog, &rv.CreatedAt)
	return rv, err
}

const roleCols = `r.id, r.project_id, r.key, r.name, r.summary, r.archived_at,
	       (SELECT max(version) FROM role_versions v WHERE v.role_id = r.id)`

func scanRole(row interface{ Scan(...any) error }) (domain.Role, error) {
	var r domain.Role
	err := row.Scan(&r.ID, &r.ProjectID, &r.Key, &r.Name, &r.Summary,
		&r.ArchivedAt, &r.LatestVersion)
	return r, err
}

func (s *Store) ListRoles(ctx context.Context, q Querier, projectID uuid.UUID, includeArchived bool) ([]domain.Role, error) {
	rows, err := q.Query(ctx, `SELECT `+roleCols+`
		FROM roles r
		WHERE r.project_id = $1 AND ($2 OR r.archived_at IS NULL)
		ORDER BY r.key`, projectID, includeArchived)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.Role{}
	for rows.Next() {
		role, err := scanRole(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, role)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetRoleByKey(ctx context.Context, q Querier, projectID uuid.UUID, key string) (domain.Role, error) {
	role, err := scanRole(q.QueryRow(ctx, `SELECT `+roleCols+`
		FROM roles r WHERE r.project_id = $1 AND r.key = $2`, projectID, key))
	if err != nil {
		return role, notFound(err, "роль %q не найдена в проекте", key)
	}
	return role, nil
}

type CreateRoleInput struct {
	ProjectID uuid.UUID
	Key       string
	Name      string
	Summary   *string
	CreatedBy uuid.UUID
}

func (s *Store) CreateRole(ctx context.Context, q Querier, in CreateRoleInput) (domain.Role, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `
		INSERT INTO roles (project_id, key, name, summary, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		in.ProjectID, in.Key, in.Name, in.Summary, in.CreatedBy).Scan(&id)
	if err != nil {
		return domain.Role{}, translate(err)
	}
	return s.GetRoleByKey(ctx, q, in.ProjectID, in.Key)
}

// UpdateRole меняет только вывеску роли. Поведение роли живёт в версиях и
// правится добавлением версии, а не редактированием.
func (s *Store) UpdateRole(ctx context.Context, q Querier, roleID uuid.UUID, name *string, summary *string) error {
	_, err := q.Exec(ctx, `
		UPDATE roles SET name = coalesce($2, name), summary = coalesce($3, summary)
		WHERE id = $1`, roleID, name, summary)
	return translate(err)
}

// ArchiveRole убирает роль из выбора, не трогая историю. archived = false
// возвращает её обратно.
func (s *Store) ArchiveRole(ctx context.Context, q Querier, roleID uuid.UUID, archived bool) error {
	_, err := q.Exec(ctx, `
		UPDATE roles SET archived_at = CASE WHEN $2 THEN now() ELSE NULL END
		WHERE id = $1`, roleID, archived)
	return translate(err)
}

// RoleUsage — статусы, на которых роль стоит исполнителем или проверяющим.
// Нужен, чтобы не архивировать роль молча из-под работающего цикла.
type RoleUsage struct {
	WorkflowID    uuid.UUID `json:"workflow_id"`
	WorkflowState string    `json:"workflow_state"`
	StatusKey     string    `json:"status_key"`
	IsPrimary     bool      `json:"is_primary"`
}

func (s *Store) RoleUsage(ctx context.Context, q Querier, roleID uuid.UUID) ([]RoleUsage, error) {
	rows, err := q.Query(ctx, `
		SELECT w.id, w.state, ws.key, sr.is_primary
		FROM status_roles sr
		JOIN workflow_statuses ws ON ws.id = sr.status_id
		JOIN workflows w ON w.id = ws.workflow_id
		WHERE sr.role_id = $1 AND w.state <> 'retired'
		ORDER BY w.state, ws.position`, roleID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []RoleUsage{}
	for rows.Next() {
		var u RoleUsage
		if err := rows.Scan(&u.WorkflowID, &u.WorkflowState, &u.StatusKey, &u.IsPrimary); err != nil {
			return nil, translate(err)
		}
		out = append(out, u)
	}
	return out, translate(rows.Err())
}

func (s *Store) ListRoleVersions(ctx context.Context, q Querier, roleID uuid.UUID) ([]domain.RoleVersion, error) {
	rows, err := q.Query(ctx, `SELECT `+roleVersionCols+`
		FROM role_versions WHERE role_id = $1 ORDER BY version DESC`, roleID)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	out := []domain.RoleVersion{}
	for rows.Next() {
		rv, err := scanRoleVersion(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, rv)
	}
	return out, translate(rows.Err())
}

func (s *Store) GetRoleVersionByNumber(ctx context.Context, q Querier, roleID uuid.UUID, version int) (domain.RoleVersion, error) {
	rv, err := scanRoleVersion(q.QueryRow(ctx, `SELECT `+roleVersionCols+`
		FROM role_versions WHERE role_id = $1 AND version = $2`, roleID, version))
	if err != nil {
		return rv, notFound(err, "версия %d роли не найдена", version)
	}
	return rv, nil
}

// RoleDocumentInput — документ заводимой версии; позиция — порядок в срезе.
// Форму имени, типа и содержимого проверяет workflow до вызова.
type RoleDocumentInput struct {
	Name    string
	Kind    string
	Content []byte
	// Закрепление за версией инструкции (000040); nil — обычный документ.
	// Байты обязаны совпадать с версией: это держит внешний ключ схемы.
	InstructionVersionID *uuid.UUID
}

type CreateRoleVersionInput struct {
	RoleID uuid.UUID
	// Instructions — то, что ляжет в колонку role_versions.instructions:
	// склейка документов (workflow.ConcatDocuments). Её читают реплики и
	// clab прежних версий.
	Instructions   string
	Documents      []RoleDocumentInput
	InputKinds     []string
	OutputContract json.RawMessage
	ToolPolicy     json.RawMessage
	ModelHint      *string
	ModelLevel     *string
	RepoScope      []string
	ContextBudget  json.RawMessage
	Changelog      *string
	CreatedBy      uuid.UUID
}

// CreateRoleVersion добавляет новую версию инструкции роли вместе с её
// документами. Всё на одном q: вызывающий держит транзакцию, и отказ на
// любом шаге не оставляет ни версии, ни строк документов.
//
// Версия иммутабельна и номер выдаётся из максимума в этой же транзакции:
// stage_runs пинят конкретную версию при заходе на статус, поэтому правка
// роли не должна менять то, по чему уже работали.
func (s *Store) CreateRoleVersion(ctx context.Context, q Querier, in CreateRoleVersionInput) (domain.RoleVersion, error) {
	if in.InputKinds == nil {
		in.InputKinds = []string{}
	}
	if in.RepoScope == nil {
		in.RepoScope = []string{}
	}
	if len(in.OutputContract) == 0 {
		in.OutputContract = json.RawMessage(`[]`)
	}
	if len(in.ToolPolicy) == 0 {
		in.ToolPolicy = json.RawMessage(`{}`)
	}
	if len(in.ContextBudget) == 0 {
		in.ContextBudget = json.RawMessage(`{}`)
	}

	shas, err := s.putRoleDocumentBlobs(ctx, q, in.Documents)
	if err != nil {
		return domain.RoleVersion{}, err
	}

	rv, err := scanRoleVersion(q.QueryRow(ctx, `
		INSERT INTO role_versions (role_id, version, instructions, input_kinds,
		            output_contract, tool_policy, model_hint, repo_scope,
		            context_budget, changelog, created_by, model_level)
		SELECT $1, coalesce(max(version), 0) + 1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
		FROM role_versions WHERE role_id = $1
		RETURNING `+roleVersionCols,
		in.RoleID, in.Instructions, in.InputKinds, in.OutputContract, in.ToolPolicy,
		in.ModelHint, in.RepoScope, in.ContextBudget, in.Changelog, in.CreatedBy, in.ModelLevel))
	if err != nil {
		return rv, translate(err)
	}

	rv.Documents = make([]domain.RoleDocument, 0, len(in.Documents))
	for i, d := range in.Documents {
		if _, err := q.Exec(ctx, `
			INSERT INTO role_version_documents (role_version_id, position, name, kind, blob_sha,
			            instruction_version_id)
			VALUES ($1, $2, $3, $4, $5, $6)`, rv.ID, i, d.Name, d.Kind, shas[i], d.InstructionVersionID); err != nil {
			return rv, translate(err)
		}
		content := string(d.Content)
		rv.Documents = append(rv.Documents, domain.RoleDocument{
			Name: d.Name, Kind: d.Kind, Position: i, SHA256: shas[i],
			SizeBytes: int64(len(d.Content)), Content: &content,
		})
	}
	// Ключ и номер закреплений — чтением, а не со входа: ответ заведения
	// тот же, что у GET версии.
	return rv, s.attachPins(ctx, q, rv.ID, rv.Documents)
}

// putRoleDocumentBlobs кладёт содержимое документов в blobs и отдаёт sha по
// порядку документов. Пишется по возрастанию sha, каждый sha — один раз:
// PutBlob держит advisory-замок по sha до коммита, и две конкурирующие
// версии с общими документами в разном порядке взяли бы замки крест-накрест
// — дедлок, который Postgres разорвёт ошибкой 500 одной из записей.
func (s *Store) putRoleDocumentBlobs(ctx context.Context, q Querier, docs []RoleDocumentInput) ([]string, error) {
	shas := make([]string, len(docs))
	content := map[string][]byte{}
	for i, d := range docs {
		sum := sha256.Sum256(d.Content)
		shas[i] = hex.EncodeToString(sum[:])
		content[shas[i]] = d.Content
	}
	order := make([]string, 0, len(content))
	for sha := range content {
		order = append(order, sha)
	}
	sort.Strings(order)
	for _, sha := range order {
		got, err := s.PutBlob(ctx, q, content[sha], "text/markdown")
		if err != nil {
			return nil, err
		}
		if got != sha {
			return nil, apperr.Internal(nil, "sha документа роли %s, а blobs вернул %s", sha, got)
		}
	}
	return shas, nil
}

// latestRoleVersionOf — единственное место выбора текущей версии роли:
// последней по номеру (решение 0068, п. 1.8). roleID — SQL-выражение с
// идентификатором роли (параметр или колонка внешнего запроса); результат —
// скалярный подзапрос с id версии, NULL у роли без версий. Его подставляют
// PrimaryRole, переезд задач при активации цикла (MoveWaitingTasks) и
// LatestRoleVersion. Предложения версий (000044, решение 0072) живут в
// своей таблице и сюда не попадают: версию из предложения заводит только
// утверждение. 000011_orchestration несёт свою копию, но она применена и не
// правится.
func latestRoleVersionOf(roleID string) string {
	return `(SELECT id FROM role_versions WHERE role_id = ` + roleID + ` ORDER BY version DESC LIMIT 1)`
}

// LatestRoleVersion — текущая версия роли, без документов. У роли без
// версий — not_found.
func (s *Store) LatestRoleVersion(ctx context.Context, q Querier, roleID uuid.UUID) (domain.RoleVersion, error) {
	rv, err := scanRoleVersion(q.QueryRow(ctx, `SELECT `+roleVersionCols+`
		FROM role_versions WHERE id = `+latestRoleVersionOf("$1"), roleID))
	if err != nil {
		return rv, notFound(err, "у роли нет ни одной версии")
	}
	return rv, nil
}

// AttachRoleDocuments заполняет Documents у версий одним запросом на все
// (без N+1 в списке версий). withContent — читать ли содержимое: списки его
// не отдают.
//
// Версия без строк документов читается как один main.md из колонки
// instructions (решение 0068, п. 6.3): такую пишут реплика прежней версии во
// время апгрейда, прежний образ после отката и сиды, вставляющие
// role_versions напрямую. Правило живёт только здесь — его берут GET,
// связка, MCP и (CLAB-344) захват.
func (s *Store) AttachRoleDocuments(ctx context.Context, q Querier, versions []domain.RoleVersion, withContent bool) error {
	if len(versions) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(versions))
	for i, v := range versions {
		ids[i] = v.ID
	}
	byVersion, err := s.documentsOf(ctx, q, "role_version_documents", "role_version_id", ids, withContent)
	if err != nil {
		return err
	}

	for i := range versions {
		docs, ok := byVersion[versions[i].ID]
		if !ok {
			docs = []domain.RoleDocument{mainFromColumn(versions[i].Instructions, withContent)}
		}
		versions[i].Documents = docs
	}
	return nil
}

// mainFromColumn — main.md версии без строк документов; sha и размер — от
// байтов колонки, как у документа, перенесённого миграцией.
func mainFromColumn(instructions string, withContent bool) domain.RoleDocument {
	sum := sha256.Sum256([]byte(instructions))
	d := domain.RoleDocument{
		Name: domain.MainRoleDocument, Kind: domain.DefaultRoleDocumentKind, Position: 0,
		SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(instructions)),
	}
	if withContent {
		text := instructions
		d.Content = &text
	}
	return d
}

// attachPins проставляет закрепления документам только что заведённой
// версии по позиции.
func (s *Store) attachPins(ctx context.Context, q Querier, versionID uuid.UUID, docs []domain.RoleDocument) error {
	rows, err := q.Query(ctx, `
		SELECT d.position, iv.id, i.key, iv.version
		FROM role_version_documents d
		JOIN instruction_versions iv ON iv.id = d.instruction_version_id
		JOIN instructions i ON i.id = iv.instruction_id
		WHERE d.role_version_id = $1`, versionID)
	if err != nil {
		return translate(err)
	}
	defer rows.Close()
	for rows.Next() {
		var pos int
		var pin domain.RoleDocumentPin
		if err := rows.Scan(&pos, &pin.ID, &pin.Key, &pin.Version); err != nil {
			return translate(err)
		}
		if pos >= 0 && pos < len(docs) {
			p := pin
			docs[pos].Instruction = &p
		}
	}
	return translate(rows.Err())
}

// LockRole берёт строку роли под FOR UPDATE до конца транзакции: «обновить
// в ролях» сверяет «от какой версии считал» с последней и заводит
// следующую, и параллельная правка роли между этими шагами иначе прошла бы
// незамеченной.
func (s *Store) LockRole(ctx context.Context, q Querier, roleID uuid.UUID) error {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM roles WHERE id = $1 FOR UPDATE`, roleID).Scan(&id)
	return notFound(err, "роль не найдена")
}

// documentsOf читает документы владельцев ids одним запросом: версий роли
// (role_version_documents) или предложений (role_version_proposal_documents,
// 000044) — форма строк у них одна. table и owner — имена из кода, не из
// запроса. withContent — читать ли содержимое: списки его не отдают.
func (s *Store) documentsOf(ctx context.Context, q Querier, table, owner string, ids []uuid.UUID, withContent bool) (map[uuid.UUID][]domain.RoleDocument, error) {
	rows, err := q.Query(ctx, `
		SELECT d.`+owner+`, d.position, d.name, d.kind, d.blob_sha, b.size_bytes,
		       CASE WHEN $2 AND b.storage = 'inline' THEN b.content END, b.storage,
		       iv.id, i.key, iv.version
		FROM `+table+` d
		JOIN blobs b ON b.sha256 = d.blob_sha
		LEFT JOIN instruction_versions iv ON iv.id = d.instruction_version_id
		LEFT JOIN instructions i ON i.id = iv.instruction_id
		WHERE d.`+owner+` = ANY($1)
		ORDER BY d.`+owner+`, d.position`, ids, withContent)
	if err != nil {
		return nil, translate(err)
	}
	type external struct {
		id  uuid.UUID
		idx int
	}
	byOwner := map[uuid.UUID][]domain.RoleDocument{}
	var pending []external
	for rows.Next() {
		var id uuid.UUID
		var d domain.RoleDocument
		var content []byte
		var storage string
		var pinID *uuid.UUID
		var pinKey *string
		var pinVersion *int
		if err := rows.Scan(&id, &d.Position, &d.Name, &d.Kind, &d.SHA256, &d.SizeBytes, &content, &storage,
			&pinID, &pinKey, &pinVersion); err != nil {
			rows.Close()
			return nil, translate(err)
		}
		if pinID != nil {
			d.Instruction = &domain.RoleDocumentPin{ID: *pinID, Key: *pinKey, Version: *pinVersion}
		}
		if withContent {
			if storage == "inline" {
				text := string(content)
				d.Content = &text
			} else {
				pending = append(pending, external{id, len(byOwner[id])})
			}
		}
		byOwner[id] = append(byOwner[id], d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	// Вынесенное во внешнее хранилище читается после закрытия выборки: на
	// том же соединении транзакции второй запрос посреди чтения строк нельзя.
	for _, p := range pending {
		d := &byOwner[p.id][p.idx]
		content, _, err := s.BlobContent(ctx, q, d.SHA256)
		if err != nil {
			return nil, err
		}
		text := string(content)
		d.Content = &text
	}
	return byOwner, nil
}

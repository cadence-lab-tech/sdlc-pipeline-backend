package postgres

// Предложения версий ролей (000044, CLAB-381, решение 0072 п. 5). Право,
// проверки содержимого и утверждение — в workflow; здесь только запись и
// чтение строк.

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

const proposalCols = `p.id, p.role_id, r.key, p.base_version, p.state, p.changelog,
	       p.input_kinds, p.output_contract, p.tool_policy, p.model_hint, p.model_level,
	       p.repo_scope, p.context_budget, p.created_by, pc.handle, p.created_at,
	       p.decided_by, pd.handle, p.decided_at, p.decision_note, p.result_version`

const proposalFrom = `
		FROM role_version_proposals p
		JOIN roles r ON r.id = p.role_id
		JOIN principals pc ON pc.id = p.created_by
		LEFT JOIN principals pd ON pd.id = p.decided_by`

func scanProposal(row interface{ Scan(...any) error }) (domain.RoleProposal, error) {
	var p domain.RoleProposal
	err := row.Scan(&p.ID, &p.RoleID, &p.Role, &p.BaseVersion, &p.State, &p.Changelog,
		&p.InputKinds, &p.OutputContract, &p.ToolPolicy, &p.ModelHint, &p.ModelLevel,
		&p.RepoScope, &p.ContextBudget, &p.CreatedBy, &p.CreatedByName, &p.CreatedAt,
		&p.DecidedBy, &p.DecidedByName, &p.DecidedAt, &p.DecisionNote, &p.ResultVersion)
	return p, err
}

// CreateRoleProposalInput — строка предложения; форму документов, контракт
// и уровень проверил workflow тем же правилом, что у версии роли.
type CreateRoleProposalInput struct {
	RoleID         uuid.UUID
	BaseVersion    int
	Documents      []RoleDocumentInput
	InputKinds     []string
	OutputContract json.RawMessage
	ToolPolicy     json.RawMessage
	ModelHint      *string
	ModelLevel     *string
	RepoScope      []string
	ContextBudget  json.RawMessage
	Changelog      string
	CreatedBy      uuid.UUID
}

// CreateRoleProposal записывает предложение в состоянии pending вместе с
// документами: байты — в blobs тем же путём, что у версии роли, на одном q.
func (s *Store) CreateRoleProposal(ctx context.Context, q Querier, in CreateRoleProposalInput) (domain.RoleProposal, error) {
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
		return domain.RoleProposal{}, err
	}
	var id uuid.UUID
	if err := q.QueryRow(ctx, `
		INSERT INTO role_version_proposals (role_id, base_version, input_kinds, output_contract,
		            tool_policy, model_hint, model_level, repo_scope, context_budget, changelog, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id`,
		in.RoleID, in.BaseVersion, in.InputKinds, in.OutputContract, in.ToolPolicy, in.ModelHint,
		in.ModelLevel, in.RepoScope, in.ContextBudget, in.Changelog, in.CreatedBy).Scan(&id); err != nil {
		return domain.RoleProposal{}, translate(err)
	}
	for i, d := range in.Documents {
		if _, err := q.Exec(ctx, `
			INSERT INTO role_version_proposal_documents (proposal_id, position, name, kind, blob_sha,
			            instruction_version_id)
			VALUES ($1, $2, $3, $4, $5, $6)`, id, i, d.Name, d.Kind, shas[i], d.InstructionVersionID); err != nil {
			return domain.RoleProposal{}, translate(err)
		}
	}
	return s.GetRoleProposal(ctx, q, id, false)
}

// GetRoleProposal — предложение с документами и их содержимым. forUpdate
// берёт строку под замок до конца транзакции: решение по предложению
// сверяет и меняет его состояние одним куском.
func (s *Store) GetRoleProposal(ctx context.Context, q Querier, id uuid.UUID, forUpdate bool) (domain.RoleProposal, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE OF p"
	}
	p, err := scanProposal(q.QueryRow(ctx, `SELECT `+proposalCols+proposalFrom+`
		WHERE p.id = $1`+lock, id))
	if err != nil {
		return p, notFound(err, "предложение %s не найдено", id)
	}
	list := []domain.RoleProposal{p}
	if err := s.attachProposalDocuments(ctx, q, list, true); err != nil {
		return p, err
	}
	return list[0], nil
}

// ListRoleProposals — предложения роли, новые первыми; state пусто — все.
// Документы — без содержимого, как в списке версий.
func (s *Store) ListRoleProposals(ctx context.Context, q Querier, roleID uuid.UUID, state string) ([]domain.RoleProposal, error) {
	rows, err := q.Query(ctx, `SELECT `+proposalCols+proposalFrom+`
		WHERE p.role_id = $1 AND ($2 = '' OR p.state = $2)
		ORDER BY p.created_at DESC, p.id`, roleID, state)
	if err != nil {
		return nil, translate(err)
	}
	out := []domain.RoleProposal{}
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			rows.Close()
			return nil, translate(err)
		}
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, translate(err)
	}
	return out, s.attachProposalDocuments(ctx, q, out, false)
}

func (s *Store) attachProposalDocuments(ctx context.Context, q Querier, list []domain.RoleProposal, withContent bool) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	byOwner, err := s.documentsOf(ctx, q, "role_version_proposal_documents", "proposal_id", ids, withContent)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Documents = byOwner[list[i].ID]
		if list[i].Documents == nil {
			list[i].Documents = []domain.RoleDocument{}
		}
	}
	return nil
}

// DecideRoleProposalInput — закрытие предложения: состояние, кто и почему;
// ResultVersion — только у утверждённого.
type DecideRoleProposalInput struct {
	ID            uuid.UUID
	State         string
	DecidedBy     uuid.UUID
	Note          *string
	ResultVersion *int
}

// DecideRoleProposal переводит ожидающее предложение в решённое. Условие
// state = 'pending' стоит в самом UPDATE: решение по уже решённому — 409,
// даже если вызывающий забыл взять строку под замок.
func (s *Store) DecideRoleProposal(ctx context.Context, q Querier, in DecideRoleProposalInput) error {
	tag, err := q.Exec(ctx, `
		UPDATE role_version_proposals
		SET state = $2, decided_by = $3, decided_at = now(), decision_note = $4, result_version = $5
		WHERE id = $1 AND state = 'pending'`,
		in.ID, in.State, in.DecidedBy, in.Note, in.ResultVersion)
	if err != nil {
		return translate(err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.Conflict("предложение уже решено")
	}
	return nil
}

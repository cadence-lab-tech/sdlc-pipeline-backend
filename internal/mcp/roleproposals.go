package mcp

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Тулы предложений версий ролей (CLAB-381, решение 0072 п. 5): те же
// сервисы и права, что у REST. allow ставит уровень-минимум, точное право
// считает workflow.Auth. В белый список токена задачи не входит ни один.

func (s *Server) registerRoleProposalTools() {
	add(s, "sdlc_propose_role_version",
		"Предложить новую версию роли из своего набора (участник уровня agent; роль — из role_keys участия и токена). "+
			"Форма — как у sdlc_put_role: documents [{name, kind, content}] или instructions строкой, "+
			"ссылка на библиотеку — {instruction, instruction_version} без content; поля версии те же. "+
			"changelog обязателен. Проверки те же, что у версии, и идут при подаче. Версия не заводится: "+
			"предложение ждёт сопровождающего (pending), действующей остаётся последняя версия.",
		s.proposeRoleVersion)
	add(s, "sdlc_list_role_proposals",
		"Предложения версий роли: без id — список (state: pending, approved, rejected, withdrawn; пусто — все), "+
			"документы без содержимого; с id — одно предложение с содержимым документов.",
		s.listRoleProposals)
	add(s, "sdlc_approve_role_proposal",
		"Утвердить предложение версии роли (человек уровня maintainer или admin). Одна транзакция: "+
			"база предложения равна последней версии — заводится версия N+1 (автор — автор предложения); "+
			"иначе ошибка stale_role_base, предложение остаётся pending.",
		s.approveRoleProposal)
	add(s, "sdlc_reject_role_proposal",
		"Отклонить предложение версии роли (человек уровня maintainer или admin); note обязательна.",
		s.rejectRoleProposal)
	add(s, "sdlc_withdraw_role_proposal",
		"Отозвать предложение версии роли: автор или сопровождающий; note необязательна.",
		s.withdrawRoleProposal)
}

type proposeRoleInput struct {
	Key            string                        `json:"key" jsonschema:"ключ роли"`
	Instructions   string                        `json:"instructions,omitempty" jsonschema:"инструкция одной строкой (markdown) — станет документом main.md; либо documents"`
	Documents      []workflow.BundleRoleDocument `json:"documents,omitempty" jsonschema:"документы версии по порядку отдачи: name, kind, content; из библиотеки — instruction (ключ) и instruction_version без content; либо instructions"`
	InputKinds     []string                      `json:"input_kinds,omitempty"`
	OutputContract json.RawMessage               `json:"output_contract,omitempty" jsonschema:"[{kind,title,format,required,schema}]"`
	ToolPolicy     json.RawMessage               `json:"tool_policy,omitempty"`
	ModelLevel     string                        `json:"model_level,omitempty" jsonschema:"low, medium, high; пусто — не задан"`
	ModelHint      string                        `json:"model_hint,omitempty" jsonschema:"устарел (решение 0060)"`
	RepoScope      []string                      `json:"repo_scope,omitempty"`
	ContextBudget  json.RawMessage               `json:"context_budget,omitempty"`
	Changelog      string                        `json:"changelog" jsonschema:"что и зачем меняется — обязателен"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (s *Server) proposeRoleVersion(ctx context.Context, in proposeRoleInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberAgent); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	p, err := s.authoring.ProposeRoleVersion(ctx, s.auth, identity(ctx), key, in.Key, workflow.RoleVersionInput{
		Instructions:   in.Instructions,
		Documents:      workflow.BundleDocuments(in.Documents),
		InputKinds:     in.InputKinds,
		OutputContract: in.OutputContract,
		ToolPolicy:     in.ToolPolicy,
		ModelHint:      optional(in.ModelHint),
		ModelLevel:     optional(in.ModelLevel),
		RepoScope:      in.RepoScope,
		ContextBudget:  in.ContextBudget,
		Changelog:      optional(in.Changelog),
	})
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"proposal": p})
}

type listRoleProposalsInput struct {
	Key   string `json:"key" jsonschema:"ключ роли"`
	State string `json:"state,omitempty" jsonschema:"pending, approved, rejected, withdrawn; пусто — все"`
	ID    string `json:"id,omitempty" jsonschema:"идентификатор предложения: одно предложение с содержимым документов"`
}

func (s *Server) listRoleProposals(ctx context.Context, in listRoleProposalsInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	if in.ID != "" {
		id, err := parseProposalID(in.ID)
		if err != nil {
			return fail(err)
		}
		p, err := s.authoring.GetRoleProposal(ctx, key, in.Key, id)
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"proposal": p})
	}
	list, err := s.authoring.ListRoleProposals(ctx, key, in.Key, in.State)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"proposals": list})
}

func parseProposalID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, apperr.InvalidInput("идентификатор предложения не UUID")
	}
	return id, nil
}

type roleProposalInput struct {
	Key  string `json:"key" jsonschema:"ключ роли"`
	ID   string `json:"id" jsonschema:"идентификатор предложения"`
	Note string `json:"note,omitempty" jsonschema:"заметка автору; у отклонения обязательна"`
}

func (s *Server) approveRoleProposal(ctx context.Context, in roleProposalInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberMaintainer); err != nil {
		return fail(err)
	}
	return s.decideRoleProposal(ctx, in, domain.ProposalApproved)
}

func (s *Server) rejectRoleProposal(ctx context.Context, in roleProposalInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberMaintainer); err != nil {
		return fail(err)
	}
	return s.decideRoleProposal(ctx, in, domain.ProposalRejected)
}

func (s *Server) withdrawRoleProposal(ctx context.Context, in roleProposalInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	return s.decideRoleProposal(ctx, in, domain.ProposalWithdrawn)
}

func (s *Server) decideRoleProposal(ctx context.Context, in roleProposalInput, state string) (*sdk.CallToolResult, any, error) {
	key, _ := projectKey(ctx)
	id, err := parseProposalID(in.ID)
	if err != nil {
		return fail(err)
	}
	var p domain.RoleProposal
	switch state {
	case domain.ProposalApproved:
		p, err = s.authoring.ApproveRoleProposal(ctx, s.auth, identity(ctx), key, in.Key, id)
	case domain.ProposalRejected:
		p, err = s.authoring.RejectRoleProposal(ctx, s.auth, identity(ctx), key, in.Key, id, in.Note)
	default:
		p, err = s.authoring.WithdrawRoleProposal(ctx, s.auth, identity(ctx), key, in.Key, id, in.Note)
	}
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"proposal": p})
}

package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Тулы настройки агентов проекта (CLAB-425, решение 0077 п. 4.7): те же
// методы движка и те же права, что у REST. allow ставит порог viewer, кто
// правит — человек уровня maintainer — решает движок по классу актора. Токену
// задачи открыты два тула чтения (executor.go).

func (s *Server) registerAgentSettingsTools() {
	add(s, "sdlc_get_agent_settings",
		"Настройка агентов проекта: какие модели доступны команде у каждого агента, какая модель и "+
			"effort стоят на уровнях low, medium, high и прайс (USD за 1 000 000 токенов, as_of). "+
			"Без version — активная версия ({\"version\": null} — настройки нет), с version — версия "+
			"по номеру, в том числе черновик. Ответ: project, version, state, document, changelog, "+
			"sha256, created_by, created_at, activated_by, activated_at, warnings. Модель своего этапа "+
			"исполнитель получает в execution.models ответа захвата — читать настройку для этого не нужно.",
		s.getAgentSettings)
	add(s, "sdlc_list_agent_settings_versions",
		"Версии настройки агентов проекта без документов: version, state (draft, active, retired), "+
			"changelog, sha256, кто и когда завёл и активировал.",
		s.listAgentSettingsVersions)
	add(s, "sdlc_put_agent_settings_draft",
		"Черновик настройки агентов (человек уровня maintainer или admin; агенту и токену задачи — "+
			"forbidden). Черновика нет — заводит: document, иначе from (active — копия активной, empty — "+
			"пустой; без обоих — копия активной или пустой). Есть — заменяет document целиком и changelog. "+
			"Документ: {agents: {<агент>: {models?: [id], levels: {low|medium|high: {model, effort?}}}}, "+
			"pricing?: {as_of: YYYY-MM-DD, models: {<id>: {input, output, cache_read?, cache_write?}}}}. "+
			"Ошибка — invalid_input с details.path; секретов (token, secret, api_key, password, sdlc_…) "+
			"в документе быть не может. Агент вне набора — предупреждение в warnings.",
		s.putAgentSettingsDraft)
	add(s, "sdlc_discard_agent_settings_draft",
		"Удалить черновик настройки агентов (человек уровня maintainer или admin); not_found — черновика нет.",
		s.discardAgentSettingsDraft)
	add(s, "sdlc_activate_agent_settings",
		"Активировать черновик настройки агентов (человек уровня maintainer или admin): прежняя активная "+
			"версия уходит в retired одной транзакцией, идущие прогоны остаются на своей версии. version — "+
			"номер черновика, который видели: другой — conflict; черновика нет — not_found. Ответ — "+
			"{activated, retired}.",
		s.activateAgentSettings)
}

type getAgentSettingsInput struct {
	Version *int `json:"version,omitempty" jsonschema:"номер версии; без него — активная"`
}

func (s *Server) getAgentSettings(ctx context.Context, in getAgentSettingsInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	key, err := projectKey(ctx)
	if err != nil {
		return fail(err)
	}
	view, err := s.engine.AgentSettings(ctx, key, in.Version)
	if err != nil {
		return fail(err)
	}
	if view == nil {
		return ok(map[string]any{"version": nil})
	}
	return ok(view)
}

type noInput struct{}

func (s *Server) listAgentSettingsVersions(ctx context.Context, _ noInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	key, err := projectKey(ctx)
	if err != nil {
		return fail(err)
	}
	list, err := s.engine.AgentSettingsVersions(ctx, key)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"versions": list})
}

type putAgentSettingsInput struct {
	// any, а не json.RawMessage: схему тула выводит jsonschema-go, и
	// RawMessage для него — массив байтов (как external_ref у sdlc_create_task).
	Document  any     `json:"document,omitempty" jsonschema:"документ настройки целиком: {agents, pricing}"`
	From      string  `json:"from,omitempty" jsonschema:"active или empty — только при заведении без document"`
	Changelog *string `json:"changelog,omitempty" jsonschema:"что и зачем меняется, до 2000 символов"`
}

// agentSettingsActor — порог viewer и актор для права движка.
func (s *Server) agentSettingsActor(ctx context.Context) (string, workflow.Actor, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return "", workflow.Actor{}, err
	}
	key, err := projectKey(ctx)
	if err != nil {
		return "", workflow.Actor{}, err
	}
	who, err := s.stageActor(ctx)
	return key, who, err
}

func (s *Server) putAgentSettingsDraft(ctx context.Context, in putAgentSettingsInput) (*sdk.CallToolResult, any, error) {
	key, who, err := s.agentSettingsActor(ctx)
	if err != nil {
		return fail(err)
	}
	doc, err := rawArgument(in.Document)
	if err != nil {
		return fail(err)
	}
	view, change, created, err := s.engine.UpsertAgentSettingsDraft(ctx, key, who,
		workflow.AgentSettingsDraftInput{From: in.From, Document: doc, Changelog: in.Changelog})
	if err != nil {
		return fail(err)
	}
	setAuditChange(ctx, change)
	return ok(map[string]any{"created": created, "draft": view})
}

func (s *Server) discardAgentSettingsDraft(ctx context.Context, _ noInput) (*sdk.CallToolResult, any, error) {
	key, who, err := s.agentSettingsActor(ctx)
	if err != nil {
		return fail(err)
	}
	change, err := s.engine.DiscardAgentSettingsDraft(ctx, key, who)
	if err != nil {
		return fail(err)
	}
	setAuditChange(ctx, change)
	return ok(map[string]any{"discarded": change.Version})
}

type activateAgentSettingsInput struct {
	Version *int `json:"version,omitempty" jsonschema:"номер черновика, который видели; без него — тот, что есть"`
}

func (s *Server) activateAgentSettings(ctx context.Context, in activateAgentSettingsInput) (*sdk.CallToolResult, any, error) {
	key, who, err := s.agentSettingsActor(ctx)
	if err != nil {
		return fail(err)
	}
	out, change, err := s.engine.ActivateAgentSettings(ctx, key, who, in.Version)
	if err != nil {
		return fail(err)
	}
	setAuditChange(ctx, change)
	return ok(out)
}

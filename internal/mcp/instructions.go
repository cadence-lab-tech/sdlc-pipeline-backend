package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Тулы библиотеки инструкций (решение 0069): те же сервисы и права, что у
// REST. Чтение — viewer и токен задачи, запись — maintainer.

func (s *Server) registerInstructionTools() {
	add(s, "sdlc_list_instructions",
		"Библиотека инструкций проекта: общие правила, которые роли берут версией. "+
			"По каждой — ключ, название, последняя версия, сколько ролей закрепили и сколько отстают.",
		s.listInstructions)
	add(s, "sdlc_get_instruction",
		"Инструкция библиотеки: версии, текст версии (version; по умолчанию последняя) и предыдущей, "+
			"где используется — последние версии ролей с документом <ключ>.md, закреплённым "+
			"(pinned_version) или обычным (null).",
		s.getInstruction)
	add(s, "sdlc_put_instruction",
		"Завести инструкцию библиотеки (версия 1) или её новую версию по ключу. Ключ — имя документа "+
			"в ролях без .md (git → git.md), не меняется. Версия иммутабельна; текст и тип, равные "+
			"последней версии, — ошибка «не изменилось». Роли новая версия не меняет: их переводит "+
			"sdlc_apply_instruction.",
		s.putInstruction)
	add(s, "sdlc_apply_instruction",
		"«Обновить в ролях»: каждой отмеченной роли — новая версия, где документ <ключ>.md закреплён "+
			"за выбранной версией инструкции (version; по умолчанию последняя), остальное как в "+
			"последней версии роли. Одна транзакция: роль без документа <ключ>.md или в архиве — "+
			"ошибка, ничего не заведено; роль уже на этой версии — unchanged; from_version не равен "+
			"последней версии роли — конфликт.",
		s.applyInstruction)
}

type listInstructionsInput struct{}

func (s *Server) listInstructions(ctx context.Context, _ listInstructionsInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	list, err := s.authoring.ListInstructions(ctx, key)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"instructions": list})
}

type getInstructionInput struct {
	Key     string `json:"key" jsonschema:"ключ инструкции"`
	Version int    `json:"version,omitempty" jsonschema:"номер версии; по умолчанию последняя"`
}

func (s *Server) getInstruction(ctx context.Context, in getInstructionInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	out, err := s.authoring.GetInstruction(ctx, key, in.Key, in.Version)
	if err != nil {
		return fail(err)
	}
	return ok(out)
}

type putInstructionInput struct {
	Key       string `json:"key" jsonschema:"ключ: строчные латинские буквы, цифры, _ и -; документ в ролях — <ключ>.md"`
	Name      string `json:"name,omitempty" jsonschema:"название; у новой инструкции по умолчанию ключ"`
	Summary   string `json:"summary,omitempty"`
	Kind      string `json:"kind,omitempty" jsonschema:"тип документа: instructions, checklist, reference…; по умолчанию тип последней версии"`
	Content   string `json:"content" jsonschema:"текст версии (markdown)"`
	Changelog string `json:"changelog,omitempty" jsonschema:"что изменилось"`
}

func (s *Server) putInstruction(ctx context.Context, in putInstructionInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberMaintainer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	var summary *string
	if in.Summary != "" {
		summary = &in.Summary
	}
	var changelog *string
	if in.Changelog != "" {
		changelog = &in.Changelog
	}
	out, err := s.authoring.PutInstruction(ctx, key, actor(ctx).ID, workflow.InstructionInput{
		Key: in.Key, Name: in.Name, Summary: summary, Kind: in.Kind, Content: in.Content, Changelog: changelog,
	})
	if err != nil {
		return fail(err)
	}
	// Ответ читает модель: текст она только что прислала сама.
	out.Version.Content = nil
	return ok(out)
}

type applyInstructionInput struct {
	Key       string               `json:"key" jsonschema:"ключ инструкции"`
	Version   int                  `json:"version,omitempty" jsonschema:"версия инструкции; по умолчанию последняя"`
	Roles     []workflow.ApplyRole `json:"roles" jsonschema:"роли: key и необязательный from_version — номер версии роли, от которой считали"`
	Changelog string               `json:"changelog,omitempty" jsonschema:"допишется к описанию каждой новой версии роли"`
}

func (s *Server) applyInstruction(ctx context.Context, in applyInstructionInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberMaintainer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	out, err := s.authoring.ApplyInstruction(ctx, key, in.Key, actor(ctx).ID, workflow.ApplyInput{
		Version: in.Version, Roles: in.Roles, Changelog: in.Changelog,
	})
	if err != nil {
		return fail(err)
	}
	return ok(out)
}

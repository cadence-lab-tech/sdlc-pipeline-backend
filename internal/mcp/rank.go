package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Перемещение в бэклоге (решение 0075, CLAB-400): тот же порядок проверок,
// что у POST /v1/tasks/:taskKey/rank, — уровень, проект, тело, рамка токена
// задачи, правила места в движке. Ключ места считает только сервер.

type rankTaskInput struct {
	Task     string `json:"task" jsonschema:"ключ задачи, которую двигаем, например SDLC-42"`
	Before   string `json:"before,omitempty" jsonschema:"ключ соседа: поставить над ним (он станет соседом ниже)"`
	After    string `json:"after,omitempty" jsonschema:"ключ соседа: поставить под ним (он станет соседом выше)"`
	Position string `json:"position,omitempty" jsonschema:"top или bottom: край группы"`
}

func (s *Server) rankTask(ctx context.Context, in rankTaskInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberAgent); err != nil {
		return fail(err)
	}
	if err := s.inProject(ctx, in.Task); err != nil {
		return fail(err)
	}
	place, err := workflow.ParseRankPlace(in.Before, in.After, in.Position)
	if err != nil {
		return fail(err)
	}
	if err := s.auth.RequireRankCircle(ctx, identity(ctx), in.Task); err != nil {
		return fail(err)
	}
	res, err := s.engine.RankTask(ctx, in.Task, place, actor(ctx).ID)
	if err != nil {
		return fail(err)
	}
	return ok(res)
}

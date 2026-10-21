package mcp

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// heartbeatTool не пишется в журнал: продление аренды раз в пять минут —
// шум, который заслонил бы действия. Отказ в продлении пишется — это сигнал.
const heartbeatTool = "sdlc_heartbeat"

// recordCall пишет вызов тула в журнал действий: проект подключения, кто и
// каким токеном, параметры, исход. Задача — из аргументов тула.
func (s *Server) recordCall(ctx context.Context, tool string, in any, res *sdk.CallToolResult, callErr error) {
	outcome := toolOutcome(res, callErr)
	if tool == heartbeatTool && !denial(outcome) {
		return
	}
	key, _ := projectKey(ctx)
	args := argsMap(in)
	target := postgres.AuditTarget{
		ProjectKey: key,
		TaskKey:    firstString(args, "task_key", "parent"),
		ArtifactID: uuidArg(args, "artifact_id"),
		SignalID:   uuidArg(args, "signal_id"),
		StageRunID: uuidArg(args, "stage_run_id"),
	}
	s.audit.Record(ctx, workflow.AuditEntry{
		Target:    target,
		Identity:  identity(ctx),
		Channel:   workflow.AuditMCP,
		Action:    tool,
		Params:    args,
		Outcome:   outcome,
		RequestID: requestID(ctx),
	})
}

func denial(outcome string) bool {
	return outcome == string(apperr.CodeForbidden) || outcome == string(apperr.CodeUnauthenticated)
}

// toolOutcome достаёт исход из результата тула. Ошибку домена тул отдаёт не
// ошибкой Go, а результатом с IsError и телом fail — код берётся оттуда.
func toolOutcome(res *sdk.CallToolResult, err error) string {
	if err != nil {
		return string(apperr.As(err).Code)
	}
	if res == nil || !res.IsError {
		return workflow.AuditOK
	}
	for _, c := range res.Content {
		tc, ok := c.(*sdk.TextContent)
		if !ok {
			continue
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(tc.Text), &body) == nil && body.Error.Code != "" {
			return body.Error.Code
		}
	}
	return string(apperr.CodeInternal)
}

// argsMap — аргументы тула в виде JSON-объекта: таким они пришли от агента.
func argsMap(in any) map[string]any {
	raw, err := json.Marshal(in)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if json.Unmarshal(raw, &out) != nil {
		return map[string]any{}
	}
	return out
}

func firstString(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func uuidArg(args map[string]any, key string) *uuid.UUID {
	v, ok := args[key].(string)
	if !ok {
		return nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil
	}
	return &id
}

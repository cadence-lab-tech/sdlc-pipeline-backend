package main

import "github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"

// Прежние имена разбора для тестов раннера: ожидания остались те же, что до
// выноса разбора в agentkit/launch (CLAB-470), — так видно, что поведение
// раннера не поменялось.
func parseUsage(raw []byte) map[string]any { return launch.ParseClaudeJSON(raw, reportedByRunner) }

func parseCodexUsage(raw []byte, model string) map[string]any {
	return launch.ParseCodexJSONL(raw, model, reportedByRunner)
}

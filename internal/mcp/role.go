package mcp

import "github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"

// Роль в ответах sdlc_claim_task и sdlc_get_task (решение 0068, п. 4.2):
// документы с содержимым, без склейки instructions. REST несёт и то и
// другое для clab и раннера прежних версий; модели склейка удвоила бы
// контекст на каждом захвате.
//
// Представление — обёртка, а не копия полей: ключ instructions перекрыт
// полем того же имени на меньшей глубине (encoding/json берёт его, а
// вложенное прячет), nil с omitempty в JSON не попадает вовсе. Новое поле
// RoleView приезжает в MCP само; набор ключей держит TestMCPRoleWithoutInstructions.

type mcpRole struct {
	*workflow.RoleView
	Instructions *struct{} `json:"instructions,omitempty"`
}

func mcpRoleOf(r *workflow.RoleView) *mcpRole {
	if r == nil {
		return nil
	}
	return &mcpRole{RoleView: r}
}

// mcpClaimResult — ответ захвата для MCP: всё как в REST, кроме role.
type mcpClaimResult struct {
	workflow.ClaimResult
	Role *mcpRole `json:"role,omitempty"`
}

func mcpClaim(res workflow.ClaimResult) mcpClaimResult {
	return mcpClaimResult{ClaimResult: res, Role: mcpRoleOf(res.Role)}
}

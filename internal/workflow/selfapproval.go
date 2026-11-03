// Поля журнала: кто, каким правом и «свой» ли в прогоне (CLAB-380, решение
// 0072 п. 4). Самоапрув допустим — человек решил, что agent утверждает
// работу своего агента, — но лента обязана его показывать: «утвердил своё»
// отличается от «утвердил чужое» полем self_approval, а не походом в базу.
//
// Поля только дополняют payload событий gate.decided, task.advanced и
// task.rejected (append-only: старые события не переписываются, клиент
// читает отсутствие ключа как «неизвестно»). Считает их движок рядом с
// правом, под блокировкой задачи; транспорты ключей не пишут.
package workflow

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Вид токена в событиях: «none» — актор назван заголовком, токена нет.
const TokenNone = "none"

// Основания self_approval, в том порядке, в каком их пишет self_basis.
const (
	SelfBasisLease    = "lease"
	SelfBasisArtifact = "artifact"
)

// tokenKindOf — вид токена актора для события; пустой (актор собран мимо
// Auth.Actor: внутренние пути и тесты) — «none».
func tokenKindOf(a Actor) string {
	if a.TokenKind == "" {
		return TokenNone
	}
	return a.TokenKind
}

// selfBasis — основания «свой в прогоне»: аренда и сдача артефактов. Порядок
// фиксирован (lease, artifact); без оснований — пустой массив, а не nil:
// в JSON это [], а не null.
func selfBasis(lease, artifact bool) []string {
	basis := []string{}
	if lease {
		basis = append(basis, SelfBasisLease)
	}
	if artifact {
		basis = append(basis, SelfBasisArtifact)
	}
	return basis
}

// decisionFields — ключи payload gate.decided, добавляемые к прежним (R1).
func decisionFields(actor Actor, v StageVerdict, basis []string) map[string]any {
	return map[string]any{
		"level":         string(actor.Level),
		"via":           v.Via,
		"token_kind":    tokenKindOf(actor),
		"stage_role":    v.StageRole,
		"self_approval": len(basis) > 0,
		"self_basis":    basis,
	}
}

// selfBasisOf читает основания решающего в прогоне из ленты: событий, а не
// claimed_by и artifacts — release и протухание аренды обнуляют первое,
// новая версия артефакта прячет прежнюю во втором, лента хранит всё.
func (e *Engine) selfBasisOf(ctx context.Context, q postgres.Querier, task domain.Task, run domain.StageRun, who uuid.UUID) ([]string, error) {
	lease, artifact, err := e.store.RunSelfBasis(ctx, q, task.ID, run.ID, who, run.EnteredAt)
	if err != nil {
		return nil, err
	}
	return selfBasis(lease, artifact), nil
}

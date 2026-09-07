package workflow

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

// Problem — замечание к версии цикла. Код машиночитаемый, сообщение — для
// человека, который собирает цикл в UI.
type Problem struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Status     string `json:"status,omitempty"`
	Transition string `json:"transition,omitempty"`
	Gate       string `json:"gate,omitempty"`
}

// Validation — вердикт по версии цикла. Проблемы держат активацию,
// предупреждения — нет: цикл с гейтом, который заработает в следующей фазе,
// собран верно, просто ещё не весь исполним.
type Validation struct {
	Valid    bool      `json:"valid"`
	Problems []Problem `json:"problems"`
	Warnings []Problem `json:"warnings"`
}

// implemented — умеет ли движок исполнять гейт такого типа.
func (e *Engine) implemented(gateType string) bool {
	_, ok := e.evaluators[gateType]
	return ok
}

// validateWorkflow проверяет цикл как граф, ничего не зная про смысл статусов.
//
// Схема ловит только локальные ошибки: статус из чужого цикла, дубль ключа,
// два начальных статуса. Тупик в графе или недостижимая колонка — свойство
// цикла целиком, и поймать их можно только здесь.
func validateWorkflow(wf domain.Workflow, implemented func(string) bool, strict bool) Validation {
	v := Validation{Problems: []Problem{}, Warnings: []Problem{}}

	if len(wf.Statuses) == 0 {
		v.Problems = append(v.Problems, Problem{
			Code: "no_statuses", Message: "в цикле нет ни одного статуса"})
		return v
	}

	var initial *domain.Status
	terminal := false
	for i := range wf.Statuses {
		if wf.Statuses[i].IsInitial {
			initial = &wf.Statuses[i]
		}
		if wf.Statuses[i].IsTerminal {
			terminal = true
		}
	}
	if initial == nil {
		v.Problems = append(v.Problems, Problem{
			Code:    "no_initial_status",
			Message: "не отмечен начальный статус — новую задачу некуда поставить"})
	}
	if !terminal {
		v.Problems = append(v.Problems, Problem{
			Code:    "no_terminal_status",
			Message: "нет ни одного терминального статуса — задача не сможет завершиться"})
	}

	// Выходы из статуса. Переход с from_status_id = NULL доступен отовсюду,
	// поэтому считается выходом для каждого статуса.
	anywhere := 0
	outgoing := map[uuid.UUID]int{}
	for _, t := range wf.Transitions {
		if t.FromStatusID == nil {
			anywhere++
			continue
		}
		outgoing[*t.FromStatusID]++
	}

	for _, st := range wf.Statuses {
		if st.IsTerminal {
			continue
		}
		if outgoing[st.ID]+anywhere == 0 {
			v.Problems = append(v.Problems, Problem{
				Code: "dead_end", Status: st.Key,
				Message: fmt.Sprintf("из статуса %q нет ни одного перехода", st.Key)})
		}
	}

	if initial != nil {
		for _, st := range unreachable(wf, initial.ID) {
			v.Problems = append(v.Problems, Problem{
				Code: "unreachable", Status: st,
				Message: fmt.Sprintf("в статус %q нельзя попасть из начального", st)})
		}
	}

	statusByID := map[uuid.UUID]domain.Status{}
	for _, st := range wf.Statuses {
		statusByID[st.ID] = st
	}

	for _, t := range wf.Transitions {
		for _, g := range t.Gates {
			if implemented(g.Type) {
				continue
			}
			p := Problem{
				Code: "gate_not_implemented", Transition: t.Key, Gate: g.Key,
				Message: fmt.Sprintf("тип гейта %q движок пока не исполняет", g.Type)}
			if strict && g.Blocking {
				// При SDLC_STRICT_GATES такой гейт валит переход — это уже не
				// предупреждение, а цикл, по которому задача не пройдёт.
				p.Message += "; при включённом SDLC_STRICT_GATES он заблокирует переход"
				v.Problems = append(v.Problems, p)
				continue
			}
			v.Warnings = append(v.Warnings, p)
		}
	}

	// Перепроверка чужого результата теряет смысл, если проверяет тот же, кто
	// делал. Отловить это можно только на цикле целиком: гейт знает ключ роли,
	// но не знает, кто исполнитель статуса, из которого ведёт переход.
	for _, t := range wf.Transitions {
		for _, g := range t.Gates {
			if g.Type != domain.GateAgentReview {
				continue
			}
			var cfg struct {
				ReviewerRoleKey string `json:"reviewer_role_key"`
			}
			_ = json.Unmarshal(g.Config, &cfg)

			if cfg.ReviewerRoleKey == "" {
				v.Problems = append(v.Problems, Problem{
					Code: "review_role_missing", Transition: t.Key, Gate: g.Key,
					Message: "в гейте перепроверки не указан reviewer_role_key"})
				continue
			}
			if t.FromStatusID == nil {
				continue
			}
			from, ok := statusByID[*t.FromStatusID]
			if !ok {
				continue
			}
			for _, r := range from.Roles {
				if r.IsPrimary && r.RoleKey == cfg.ReviewerRoleKey {
					v.Problems = append(v.Problems, Problem{
						Code: "review_role_is_executor", Transition: t.Key, Gate: g.Key,
						Status: from.Key,
						Message: fmt.Sprintf(
							"роль %q проверяла бы собственный результат: она же исполнитель статуса %q",
							cfg.ReviewerRoleKey, from.Key)})
				}
			}
		}
	}

	// Статус, на котором кто-то должен работать, но исполнитель не назначен:
	// задача войдёт в него и встанет, потому что забирать её будет некому.
	for _, st := range wf.Statuses {
		if st.Category != "active" {
			continue
		}
		primary := false
		for _, r := range st.Roles {
			if r.IsPrimary {
				primary = true
			}
		}
		if !primary {
			v.Warnings = append(v.Warnings, Problem{
				Code: "no_primary_role", Status: st.Key,
				Message: fmt.Sprintf("на рабочем статусе %q не назначен исполнитель", st.Key)})
		}
	}

	v.Valid = len(v.Problems) == 0
	return v
}

// unreachable — статусы, в которые не ведёт ни один путь из начального.
func unreachable(wf domain.Workflow, initial uuid.UUID) []string {
	reached := map[uuid.UUID]bool{initial: true}

	for changed := true; changed; {
		changed = false
		for _, t := range wf.Transitions {
			if reached[t.ToStatusID] {
				continue
			}
			// from_status_id = NULL — переход из любого статуса; раз начальный
			// достижим всегда, такой переход достижим тоже.
			if t.FromStatusID == nil || reached[*t.FromStatusID] {
				reached[t.ToStatusID] = true
				changed = true
			}
		}
	}

	out := []string{}
	for _, st := range wf.Statuses {
		if !reached[st.ID] {
			out = append(out, st.Key)
		}
	}
	return out
}

package postgres_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// CLAB-393: пометка самоапрува на вердикте читается из записанного gate.decided
// того же решения (тот же гейт, прогон и актор decided_by) в самом запросе
// вердиктов. Фикстуры пишут gate_results и события напрямую, чтобы покрыть
// то, что движок сегодня не пишет (решение без поля, решение двух акторов).
func TestGateVerdictsReadSelfApprovalFromDecision(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()

	var taskID, statusID, roleID, rvID, transitionID uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT t.id, s.id, ro.id, rv.id
		FROM tasks t
		JOIN workflow_statuses s ON s.workflow_id = t.workflow_id AND s.key = 'analysis'
		JOIN roles ro ON ro.project_id = t.project_id AND ro.key = 'analyst'
		JOIN role_versions rv ON rv.role_id = ro.id
		WHERE t.key = 'SDLC-1' ORDER BY rv.version DESC LIMIT 1`).Scan(&taskID, &statusID, &roleID, &rvID); err != nil {
		t.Fatal(err)
	}
	var approvalGate, otherGate uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT g.id, g.transition_id FROM gates g
		JOIN workflow_transitions tr ON tr.id = g.transition_id
		JOIN tasks t ON t.workflow_id = tr.workflow_id
		WHERE t.key = 'SDLC-1' AND tr.key = 'analysis_done' AND g.type = $1`, domain.GateHumanApproval).
		Scan(&approvalGate, &transitionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT g.id FROM gates g WHERE g.transition_id = $1 AND g.type NOT IN ($2, $3) LIMIT 1`,
		transitionID, domain.GateHumanApproval, domain.GateChecklist).Scan(&otherGate); err != nil {
		t.Fatal(err)
	}
	var actorA, actorB uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM principals ORDER BY created_at LIMIT 1`).Scan(&actorA); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO principals (kind, handle, display_name) VALUES ('user', 'other-decider', 'other-decider') RETURNING id`).Scan(&actorB); err != nil {
		t.Fatal(err)
	}

	type event struct {
		actor   uuid.UUID
		payload string // gate_id подставляется
	}
	// run создаёт прогон с одним вердиктом и событиями решения по порядку.
	run := func(gate uuid.UUID, decidedBy *uuid.UUID, events ...event) uuid.UUID {
		t.Helper()
		var runID uuid.UUID
		if err := pool.QueryRow(ctx, `
			INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, attempt, state)
			VALUES ($1, $2, $3, $4, (SELECT coalesce(max(attempt), 0) + 1 FROM stage_runs WHERE task_id = $1 AND status_id = $2),
			        'passed') RETURNING id`, taskID, statusID, roleID, rvID).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO gate_results (stage_run_id, gate_id, transition_id, status, decided_by, decided_at)
			VALUES ($1, $2, $3, 'passed', $4, CASE WHEN $4::uuid IS NULL THEN NULL ELSE now() END)`,
			runID, gate, transitionID, decidedBy); err != nil {
			t.Fatal(err)
		}
		for _, e := range events {
			payload := strings.Replace(e.payload, "GATE", gate.String(), 1)
			if _, err := pool.Exec(ctx, `
				INSERT INTO task_events (task_id, stage_run_id, type, actor_id, payload)
				VALUES ($1, $2, 'gate.decided', $3, $4::jsonb)`, taskID, runID, e.actor, payload); err != nil {
				t.Fatal(err)
			}
		}
		return runID
	}
	const (
		leaseArtifact = `{"gate_id":"GATE","status":"passed","self_approval":true,"self_basis":["lease","artifact"]}`
		lease         = `{"gate_id":"GATE","status":"passed","self_approval":true,"self_basis":["lease"]}`
		foreign       = `{"gate_id":"GATE","status":"passed","self_approval":false,"self_basis":[]}`
		legacy        = `{"gate_id":"GATE","status":"passed"}`
	)
	cases := []struct {
		name  string
		runID uuid.UUID
		self  *bool
		basis []string
	}{
		{"аренда и артефакт", run(approvalGate, &actorA, event{actorA, leaseArtifact}), ptr(true), []string{"lease", "artifact"}},
		{"чужое решение", run(approvalGate, &actorA, event{actorA, foreign}), ptr(false), []string{}},
		{"решение пересмотрено: берётся последнее", run(approvalGate, &actorA, event{actorA, lease}, event{actorA, foreign}),
			ptr(false), []string{}},
		{"события другого актора не берутся", run(approvalGate, &actorA, event{actorB, lease}), nil, nil},
		{"decided_by пуст (вердикт движка)", run(approvalGate, nil, event{actorA, lease}), nil, nil},
		{"гейт другого типа", run(otherGate, &actorA, event{actorA, lease}), nil, nil},
		{"решение до CLAB-380, без поля", run(approvalGate, &actorA, event{actorA, legacy}), nil, nil},
		{"события решения нет", run(approvalGate, &actorA), nil, nil},
	}

	verdicts, err := store.GateVerdictsByTask(ctx, pool, taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			vs := verdicts[c.runID]
			if len(vs) != 1 {
				t.Fatalf("вердиктов %d", len(vs))
			}
			v := vs[0]
			raw, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]json.RawMessage
			_ = json.Unmarshal(raw, &keys)
			if c.self == nil {
				if v.SelfApproval != nil || v.SelfBasis != nil {
					t.Fatalf("поля есть: %s", raw)
				}
				if _, ok := keys["self_approval"]; ok {
					t.Fatalf("ключ self_approval в JSON: %s", raw)
				}
				if _, ok := keys["self_basis"]; ok {
					t.Fatalf("ключ self_basis в JSON: %s", raw)
				}
				return
			}
			if v.SelfApproval == nil || *v.SelfApproval != *c.self || v.SelfBasis == nil {
				t.Fatalf("вердикт: %s", raw)
			}
			want, _ := json.Marshal(c.basis)
			if string(keys["self_basis"]) != string(want) {
				t.Fatalf("self_basis %s, ждали %s", keys["self_basis"], want)
			}
			// Прежние ключи на месте (AC-11).
			for _, k := range []string{"gate_key", "gate_name", "gate_type", "transition_key", "status", "details",
				"decided_by_handle", "decided_at"} {
				if _, ok := keys[k]; !ok {
					t.Errorf("пропал ключ %s: %s", k, raw)
				}
			}
		})
	}

	// AC-5 на уровне запроса: событие, пришедшее в ленту после решения
	// (сдача артефакта тем же актором), пометку не меняет.
	submitted := cases[1].runID
	if _, err := pool.Exec(ctx, `
		INSERT INTO task_events (task_id, stage_run_id, type, actor_id, payload)
		VALUES ($1, $2, 'artifact.submitted', $3, '{}'::jsonb)`, taskID, submitted, actorA); err != nil {
		t.Fatal(err)
	}
	after, err := store.GateVerdictsByTask(ctx, pool, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if v := after[submitted][0]; v.SelfApproval == nil || *v.SelfApproval || len(*v.SelfBasis) != 0 {
		t.Fatalf("после сдачи артефакта: %+v", v)
	}
}

func ptr[T any](v T) *T { return &v }

// CLAB-393, AC-8: пометка читается в том же запросе вердиктов — число
// обращений к базе на карточку не зависит от числа решённых вердиктов, а сам
// GateVerdictsByTask делает ровно одно.
func TestCardVerdictsSelfApprovalQueriesDoNotGrow(t *testing.T) {
	store, pool := testdb.Store(t)
	ctx := context.Background()

	var taskID, statusID, roleID, rvID, gateID, transitionID, actor uuid.UUID
	if err := pool.QueryRow(ctx, `
		SELECT t.id, s.id, ro.id, rv.id
		FROM tasks t
		JOIN workflow_statuses s ON s.workflow_id = t.workflow_id AND s.key = 'analysis'
		JOIN roles ro ON ro.project_id = t.project_id AND ro.key = 'analyst'
		JOIN role_versions rv ON rv.role_id = ro.id
		WHERE t.key = 'SDLC-1' ORDER BY rv.version DESC LIMIT 1`).Scan(&taskID, &statusID, &roleID, &rvID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT g.id, g.transition_id FROM gates g
		JOIN workflow_transitions tr ON tr.id = g.transition_id
		JOIN tasks t ON t.workflow_id = tr.workflow_id
		WHERE t.key = 'SDLC-1' AND tr.key = 'analysis_done' AND g.type = $1`, domain.GateHumanApproval).
		Scan(&gateID, &transitionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM principals ORDER BY created_at LIMIT 1`).Scan(&actor); err != nil {
		t.Fatal(err)
	}
	addDecided := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			var runID uuid.UUID
			if err := pool.QueryRow(ctx, `
				INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, attempt, state)
				VALUES ($1, $2, $3, $4, (SELECT coalesce(max(attempt), 0) + 1 FROM stage_runs WHERE task_id = $1 AND status_id = $2),
				        'passed') RETURNING id`, taskID, statusID, roleID, rvID).Scan(&runID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO gate_results (stage_run_id, gate_id, transition_id, status, decided_by, decided_at)
				VALUES ($1, $2, $3, 'passed', $4, now())`, runID, gateID, transitionID, actor); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO task_events (task_id, stage_run_id, type, actor_id, payload)
				VALUES ($1, $2, 'gate.decided', $3, jsonb_build_object('gate_id', $4::text, 'status', 'passed',
				        'self_approval', true, 'self_basis', jsonb_build_array('lease')))`,
				taskID, runID, actor, gateID.String()); err != nil {
				t.Fatal(err)
			}
		}
	}
	measure := func() (cardQueries, verdictQueries, marked int) {
		t.Helper()
		c := &queryCounter{Querier: pool}
		out, err := store.CardStageRuns(ctx, c, taskID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range out {
			for _, g := range r.GateResults {
				if g.SelfApproval != nil {
					marked++
				}
			}
		}
		v := &queryCounter{Querier: pool}
		if _, err := store.GateVerdictsByTask(ctx, v, taskID); err != nil {
			t.Fatal(err)
		}
		return c.n, v.n, marked
	}

	addDecided(1)
	q1, v1, m1 := measure()
	addDecided(6)
	q7, v7, m7 := measure()
	if m1 != 1 || m7 != 7 {
		t.Fatalf("вердиктов с пометкой: %d и %d, ждали 1 и 7", m1, m7)
	}
	if q1 != q7 {
		t.Errorf("обращений к базе на карточку: %d при 1 и %d при 7 решённых вердиктах", q1, q7)
	}
	if v1 != 1 || v7 != 1 {
		t.Errorf("запросов вердиктов: %d и %d, ждали по одному", v1, v7)
	}
}

// CLAB-393, AC-9: запрос вердиктов с полями решения берёт частичный индекс
// task_events_gate_decided при любом режиме плана — тип события в нём
// литерал, а не параметр. Без индекса подзапрос ходил бы по всей ленте.
func TestGateVerdictsUseGateDecidedIndex(t *testing.T) {
	for _, mode := range []string{"auto", "force_custom_plan", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			plan := planOf(t, postgres.GateVerdictsSQL, mode, "'00000000-0000-0000-0000-000000000001'")
			if !strings.Contains(plan, gateDecidedIndex) {
				t.Fatalf("индекс %s не взят при plan_cache_mode=%s:\n%s", gateDecidedIndex, mode, plan)
			}
		})
	}
}

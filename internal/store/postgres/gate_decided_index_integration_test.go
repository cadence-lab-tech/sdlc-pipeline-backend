package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

const gateDecidedIndex = "task_events_gate_decided"

// planOf — план PREPARE/EXPLAIN EXECUTE запроса sql в одной сессии: режим
// плана и enable_seqscan действуют на соединение, поэтому берётся одно.
func planOf(t *testing.T, sql, mode string, params string) string {
	t.Helper()
	ctx := context.Background()
	_, pool := testdb.Store(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, stmt := range []string{
		"SET plan_cache_mode = " + mode,
		"SET enable_seqscan = off",
		"PREPARE q AS " + sql,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	rows, err := conn.Query(ctx, "EXPLAIN EXECUTE q("+params+")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	return strings.Join(plan, "\n")
}

// CLAB-371, AC-3: подсчёт апрувов берёт частичный индекс при любом режиме
// плана, в том числе при общем плане подготовленного выражения pgx — тип
// события в тексте запроса литерал, а не параметр. План строится на тексте
// самого CountApprovals (export_test.go), seqscan выключен: без него на
// пустой тестовой таблице индекс не показывал бы ничего.
func TestCountApprovalsUsesGateDecidedIndex(t *testing.T) {
	const params = "'00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'epoch'"
	for _, mode := range []string{"auto", "force_custom_plan", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			plan := planOf(t, postgres.CountApprovalsSQL, mode, params)
			if !strings.Contains(plan, gateDecidedIndex) {
				t.Fatalf("индекс %s не взят при plan_cache_mode=%s:\n%s", gateDecidedIndex, mode, plan)
			}
		})
	}

	// Контроль, что тест чувствителен: прежний текст с type = $2 в общем
	// плане индекс не берёт. Только лог — поведение планировщика не наше,
	// но если контроль начнёт брать индекс, литерал можно пересмотреть.
	old := strings.Replace(postgres.CountApprovalsSQL, "'"+domain.EventGateDecided+"'", "$4", 1)
	plan := planOf(t, old, "force_generic_plan", params+", 'gate.decided'")
	t.Logf("контроль, type = $4 в общем плане, индекс взят: %v\n%s",
		strings.Contains(plan, gateDecidedIndex), plan)
}

// CLAB-371, AC-1: индекс частичный (по условию на тип события), цикл
// up → down → up проходит, после down индекса нет.
func TestGateDecidedIndexMigrationCycle(t *testing.T) {
	_, pool := testdb.Store(t)
	ctx := context.Background()
	read := func(name string) string {
		raw, err := migrations.Schema.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	def := func() (string, bool) {
		var d string
		err := pool.QueryRow(ctx, `
			SELECT indexdef FROM pg_indexes WHERE tablename = 'task_events' AND indexname = $1`,
			gateDecidedIndex).Scan(&d)
		return d, err == nil
	}
	d, ok := def()
	if !ok || !strings.Contains(d, "(stage_run_id)") || !strings.Contains(d, "WHERE (type = 'gate.decided'::text)") {
		t.Fatalf("после миграций: %q %v", d, ok)
	}
	if _, err := pool.Exec(ctx, read("000042_gate_decided_index.down.sql")); err != nil {
		t.Fatal(err)
	}
	if _, ok := def(); ok {
		t.Fatal("после down индекс остался")
	}
	if _, err := pool.Exec(ctx, read("000042_gate_decided_index.down.sql")); err != nil {
		t.Fatalf("повторный down (IF EXISTS): %v", err)
	}
	if _, err := pool.Exec(ctx, read("000042_gate_decided_index.up.sql")); err != nil {
		t.Fatal(err)
	}
	if _, ok := def(); !ok {
		t.Fatal("после повторного up индекса нет")
	}
}

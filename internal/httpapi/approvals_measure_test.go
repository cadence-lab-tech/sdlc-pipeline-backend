package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Замер цены подсчёта апрувов (CLAB-371, AC-5/6/7) на снимке размера CLAB:
// 400 задач, 150 в работе, ~445 тысяч событий ленты, привязанных к прогонам,
// и решения по гейтам от разных акторов. Это замер, а не проверка: идёт только
// с SDLC_APPROVALS_MEASURE=1 и печатает числа для разбора изменений.
//
// Один снимок, несколько состояний индексов: без индекса, варианты и
// итоговый индекс миграции 000042. Множества awaiting_approval и состав
// /approvals сверяются между всеми состояниями.
func TestApprovalsMeasure(t *testing.T) {
	if os.Getenv("SDLC_APPROVALS_MEASURE") != "1" {
		t.Skip("замер: SDLC_APPROVALS_MEASURE=1")
	}
	f := newAuditFixture(t)
	ctx := context.Background()
	store := postgres.New(f.pool)
	analystTok := f.memberToken("user:ana", domain.MemberAgent, []string{"analyst"})
	developerTok := f.memberToken("user:dev", domain.MemberAgent, []string{"developer"})

	must := func(code int, what string, rec interface{ String() string }) {
		t.Helper()
		if code != http.StatusOK && code != http.StatusCreated {
			t.Fatalf("%s: %d %s", what, code, rec.String())
		}
	}
	post := func(path, token string, body any) {
		t.Helper()
		rec := f.do("POST", path, token, body)
		must(rec.Code, "POST "+path, rec.Body)
	}
	submit := func(key, kind string) {
		post("/v1/tasks/"+key+"/artifacts", f.root,
			map[string]any{"kind": kind, "title": kind, "content": "# " + kind + " " + uuid.NewString()})
	}

	// Задачи: 50 в done, 150 в analysis (в работе), остальные в бэклоге.
	// Среди задач в analysis: нет артефактов, сданы (ждут апрува), апрув
	// поставлен, апрув устарел из-за новой сдачи.
	start := time.Now()
	var inWork []string
	for i := 0; i < 400; i++ {
		rec := f.do("POST", "/v1/projects/SDLC/tasks", f.root, map[string]any{"title": fmt.Sprintf("Замер %03d", i)})
		must(rec.Code, "создание", rec.Body)
		var created struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		key := created.Key
		switch {
		case i%8 == 0:
			if _, err := f.pool.Exec(ctx, `
				UPDATE tasks t SET status_id = ws.id FROM workflow_statuses ws
				WHERE t.key = $1 AND ws.workflow_id = t.workflow_id AND ws.key = 'done'`, key); err != nil {
				t.Fatal(err)
			}
		case i%8 < 4:
			post("/v1/tasks/"+key+"/transitions/start", f.root, map[string]any{})
			inWork = append(inWork, key)
		}
	}
	for j, key := range inWork {
		group := j % 5
		if group == 0 {
			continue
		}
		submit(key, "analysis.brief")
		submit(key, "analysis.acceptance_criteria")
		for _, gate := range []string{"repo_scope", "peer_review"} {
			post("/v1/tasks/"+key+"/transitions/analysis_done/gates/"+gate+"/decision", f.root,
				map[string]any{"status": "waived"})
		}
		if group >= 3 {
			post("/v1/tasks/"+key+"/transitions/analysis_done/gates/human_approval/decision", f.root,
				map[string]any{"status": "passed"})
		}
		if group == 4 {
			submit(key, "analysis.brief") // новая сдача после апрува: апрув устарел
		}
	}

	// История прогонов: на каждый статус цикла, кроме текущего, прогон
	// прошёл (у части — второй заход через другой статус), у прогонов
	// аренды и события — как на проде.
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, attempt, state, entered_at, finished_at, outcome)
		SELECT t.id, ws.id, NULL, NULL, a.n, 'passed',
		       now() - make_interval(secs => (1 + a.n) * 86400 + ws.position * 3600),
		       now() - make_interval(secs => a.n * 86400), 'advanced'
		FROM tasks t
		JOIN workflow_statuses ws ON ws.workflow_id = t.workflow_id AND ws.id <> t.status_id
		CROSS JOIN generate_series(1, 2) a(n)
		WHERE ws.key NOT IN ('new', 'cancelled')`); err != nil {
		t.Fatal(err)
	}
	// Лента: ~94 % lease.extended, остальное stage.claimed, usage.reported и
	// gate.decided (passed/failed, два актора, настоящие гейты human_approval).
	// Все привязаны к прогонам своей задачи.
	if _, err := f.pool.Exec(ctx, `
		WITH r AS (
			SELECT task_id, array_agg(id ORDER BY id) ids FROM stage_runs GROUP BY task_id),
		actors AS (
			SELECT array_agg(id ORDER BY handle) ids FROM principals WHERE handle IN ('root', 'agent:bot')),
		hg AS (
			SELECT array_agg(g.id) ids FROM gates g WHERE g.key = 'human_approval')
		INSERT INTO task_events (task_id, stage_run_id, type, actor_id, payload, created_at)
		SELECT t.id, r.ids[1 + (g * 7) % cardinality(r.ids)],
		       CASE WHEN g % 300 = 0 THEN 'gate.decided'
		            WHEN g % 40 = 0 THEN 'stage.claimed'
		            WHEN g % 33 = 0 THEN 'usage.reported'
		            ELSE 'lease.extended' END,
		       actors.ids[1 + g % 2],
		       CASE WHEN g % 300 = 0 THEN jsonb_build_object('gate_id', hg.ids[1 + g % cardinality(hg.ids)]::text,
		                                                      'status', CASE WHEN g % 600 = 0 THEN 'failed' ELSE 'passed' END)
		            ELSE '{}'::jsonb END,
		       now() - make_interval(secs => g * 60)
		FROM tasks t
		JOIN r ON r.task_id = t.id
		CROSS JOIN actors CROSS JOIN hg
		CROSS JOIN generate_series(1, 1100) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `ANALYZE task_events; ANALYZE tasks; ANALYZE stage_runs; ANALYZE gate_results`); err != nil {
		t.Fatal(err)
	}
	var stat struct {
		events, withRun, decided, runs, tasks, inWork int
	}
	_ = f.pool.QueryRow(ctx, `SELECT count(*), count(stage_run_id), count(*) FILTER (WHERE type = 'gate.decided') FROM task_events`).
		Scan(&stat.events, &stat.withRun, &stat.decided)
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM stage_runs`).Scan(&stat.runs)
	t.Logf("снимок: задач %d, в работе %d, прогонов %d, событий %d (с прогоном %d, gate.decided %d), сборка %s",
		400, len(inWork), stat.runs, stat.events, stat.withRun, stat.decided, time.Since(start).Round(time.Millisecond))

	// Пары (прогон, гейт) для вызовов CountApprovals: текущий прогон каждой
	// задачи в работе и гейт human_approval её перехода analysis_done.
	type pair struct {
		run, gate uuid.UUID
		since     time.Time
	}
	var pairs []pair
	rows, err := f.pool.Query(ctx, `
		SELECT sr.id, g.id, coalesce((SELECT max(a.created_at) FROM artifacts a WHERE a.task_id = t.id), 'epoch'::timestamptz)
		FROM tasks t
		JOIN workflow_statuses ws ON ws.id = t.status_id AND ws.key = 'analysis'
		JOIN stage_runs sr ON sr.task_id = t.id AND sr.status_id = t.status_id
		JOIN workflow_transitions tr ON tr.workflow_id = t.workflow_id AND tr.key = 'analysis_done'
		JOIN gates g ON g.transition_id = tr.id AND g.key = 'human_approval'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.run, &p.gate, &p.since); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if len(pairs) == 0 {
		t.Fatal("нет пар прогон/гейт")
	}

	fmtD := func(d time.Duration) string { return d.Round(10 * time.Microsecond).String() }
	stats := func(times []time.Duration) (med, worst time.Duration) {
		slices.Sort(times)
		return times[len(times)/2], times[len(times)-1]
	}

	type snapshot struct{ awaiting, approvals []string }
	measure := func(state string) snapshot {
		t.Helper()
		// CountApprovals: 7 проходов по прогонам задач в работе. Два вида
		// вызова движка: с границей последней сдачи (первый) и без неё
		// (второй, подсчёт устаревших апрувов, только при нехватке апрувов).
		for _, withSince := range []bool{true, false} {
			var calls []time.Duration
			var sum int
			for pass := 0; pass < 7; pass++ {
				for _, p := range pairs {
					since := p.since
					if !withSince {
						since = time.Time{}
					}
					at := time.Now()
					n, err := store.CountApprovals(ctx, f.pool, p.run, p.gate, since)
					calls = append(calls, time.Since(at))
					if err != nil {
						t.Fatal(err)
					}
					if pass == 0 {
						sum += n
					}
				}
			}
			med, worst := stats(calls)
			t.Logf("[%s] CountApprovals, граница сдачи=%v (%d вызовов, апрувов в сумме %d): медиана %s, худшее %s",
				state, withSince, len(calls), sum, fmtD(med), fmtD(worst))
		}

		var snap snapshot
		getAs := func(name, path, token string) []byte {
			var times []time.Duration
			var body []byte
			f.do("GET", path, token, nil) // прогрев
			for i := 0; i < 7; i++ {
				at := time.Now()
				rec := f.do("GET", path, token, nil)
				times = append(times, time.Since(at))
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
				}
				body = rec.Body.Bytes()
			}
			med, worst := stats(times)
			t.Logf("[%s] %-42s медиана %s, худшее %s", state, name, fmtD(med), fmtD(worst))
			return body
		}
		get := func(name, path string) []byte { return getAs(name, path, f.root) }
		var list struct {
			Tasks []struct {
				Key              string `json:"key"`
				AwaitingApproval bool   `json:"awaiting_approval"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(get("tasks in_work=true limit=200", "/v1/projects/SDLC/tasks?in_work=true&limit=200"), &list); err != nil {
			t.Fatal(err)
		}
		for _, it := range list.Tasks {
			if it.AwaitingApproval {
				snap.awaiting = append(snap.awaiting, it.Key)
			}
		}
		slices.Sort(snap.awaiting)
		var ap struct {
			Approvals []struct {
				Task struct {
					Key string `json:"key"`
				} `json:"task"`
				Transition struct {
					Key string `json:"key"`
				} `json:"transition"`
				Gate struct {
					Key string `json:"key"`
				} `json:"gate"`
			} `json:"approvals"`
		}
		if err := json.Unmarshal(get("approvals", "/v1/projects/SDLC/approvals"), &ap); err != nil {
			t.Fatal(err)
		}
		for _, a := range ap.Approvals {
			snap.approvals = append(snap.approvals, a.Task.Key+" "+a.Transition.Key+"/"+a.Gate.Key)
		}
		slices.Sort(snap.approvals)
		t.Logf("[%s] ждут апрува в списке: %d, в очереди /approvals: %d", state, len(snap.awaiting), len(snap.approvals))

		// Зона agent (CLAB-379, AC-A4): аналитик видит те же гейты этапа
		// analysis, что сопровождающий; разработчик — ни одного, и чужие
		// задачи до гейтов не доходят.
		for _, c := range []struct {
			name, token string
			same        bool
		}{{"approvals agent [analyst]", analystTok, true}, {"approvals agent [developer]", developerTok, false}} {
			var zone struct {
				Approvals []json.RawMessage `json:"approvals"`
			}
			if err := json.Unmarshal(getAs(c.name, "/v1/projects/SDLC/approvals", c.token), &zone); err != nil {
				t.Fatal(err)
			}
			if c.same && len(zone.Approvals) != len(snap.approvals) || !c.same && len(zone.Approvals) != 0 {
				t.Errorf("[%s] %s: %d элементов, у сопровождающего %d", state, c.name, len(zone.Approvals), len(snap.approvals))
			}
		}

		// План одного вызова с гейтом, у которого есть решения.
		var pr, pg uuid.UUID
		var since time.Time
		if err := f.pool.QueryRow(ctx, `
			SELECT e.stage_run_id, (e.payload->>'gate_id')::uuid, 'epoch'::timestamptz
			FROM task_events e WHERE e.type = 'gate.decided' AND e.stage_run_id IS NOT NULL LIMIT 1`).Scan(&pr, &pg, &since); err != nil {
			t.Fatal(err)
		}
		plan, err := f.pool.Query(ctx, `EXPLAIN (ANALYZE, BUFFERS) SELECT count(DISTINCT actor_id)
			FROM task_events WHERE stage_run_id = $1 AND type = 'gate.decided'
			  AND payload->>'gate_id' = $2::text AND payload->>'status' = 'passed' AND created_at > $3`,
			pr, pg.String(), since)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for plan.Next() {
			var l string
			_ = plan.Scan(&l)
			lines = append(lines, l)
		}
		plan.Close()
		t.Logf("[%s] план CountApprovals:\n%s", state, strings.Join(lines, "\n"))
		return snap
	}

	dropIdx := func() {
		if _, err := f.pool.Exec(ctx, `DROP INDEX IF EXISTS task_events_gate_decided`); err != nil {
			t.Fatal(err)
		}
	}
	build := func(label, ddl string) {
		t.Helper()
		at := time.Now()
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, ddl); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		built := time.Since(at)
		if _, err := f.pool.Exec(ctx, `ANALYZE task_events`); err != nil {
			t.Fatal(err)
		}
		var size int64
		_ = f.pool.QueryRow(ctx, `SELECT pg_relation_size('task_events_gate_decided')`).Scan(&size)
		var tbl int64
		_ = f.pool.QueryRow(ctx, `SELECT pg_relation_size('task_events')`).Scan(&tbl)
		t.Logf("[%s] постройка в транзакции %s, размер индекса %d байт (таблица %d байт)", label, fmtD(built), size, tbl)
	}

	dropIdx()
	_, _ = f.pool.Exec(ctx, `ANALYZE task_events`)
	base := measure("без индекса")

	variants := []struct{ name, ddl string }{
		{"(stage_run_id) partial — миграция 000042",
			`CREATE INDEX task_events_gate_decided ON task_events (stage_run_id) WHERE type = 'gate.decided'`},
		{"(stage_run_id, created_at) partial",
			`CREATE INDEX task_events_gate_decided ON task_events (stage_run_id, created_at) WHERE type = 'gate.decided'`},
		{"(stage_run_id) без условия",
			`CREATE INDEX task_events_gate_decided ON task_events (stage_run_id)`},
	}
	for _, v := range variants {
		dropIdx()
		build(v.name, v.ddl)
		snap := measure(v.name)
		if !slices.Equal(snap.awaiting, base.awaiting) {
			t.Errorf("%s: awaiting_approval разошёлся: %v против %v", v.name, snap.awaiting, base.awaiting)
		}
		if !slices.Equal(snap.approvals, base.approvals) {
			t.Errorf("%s: /approvals разошёлся", v.name)
		}
	}
	// Итог — индекс миграции.
	dropIdx()
	build("итог: (stage_run_id) partial", variants[0].ddl)
	var def string
	_ = f.pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'task_events_gate_decided'`).Scan(&def)
	t.Logf("индекс: %s", def)
	// Запись в ленту с индексом: цена вставки gate.decided и lease.extended.
	for _, typ := range []string{"lease.extended", "gate.decided"} {
		at := time.Now()
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO task_events (task_id, stage_run_id, type, payload)
			SELECT task_id, id, $1, '{}' FROM stage_runs LIMIT 3000`, typ); err != nil {
			t.Fatal(err)
		}
		t.Logf("вставка 3000 событий %s с индексом: %s", typ, time.Since(at).Round(time.Millisecond))
	}
}

package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Замер цены строки «Обзора» (CLAB-363, AC-A24) на снимке размера CLAB:
// 400 задач, из них 150 в работе, и лента в 440 тысяч событий, почти все —
// heartbeat'ы (как в оценке CLAB-219). Это замер, а не проверка: идёт только
// с SDLC_OVERVIEW_MEASURE=1 и печатает время, бюджет заявлен в разборе
// изменений.
func TestOverviewMeasure(t *testing.T) {
	if os.Getenv("SDLC_OVERVIEW_MEASURE") != "1" {
		t.Skip("замер: SDLC_OVERVIEW_MEASURE=1")
	}
	f := newOvFixture(t)
	keys := make([]string, 400)
	for i := range keys {
		keys[i] = f.create(fmt.Sprintf("Замер %03d", i), nil)
		switch {
		case i%8 == 0:
			f.setStatus(keys[i], "done")
		case i%8 < 4:
			f.move(keys[i], "start", f.root)
		}
	}
	for i := 1; i+1 < len(keys); i += 10 {
		f.ok("POST", "/v1/tasks/"+keys[i]+"/links", f.root, map[string]any{"task": keys[i+1], "rel": "blocks"})
	}
	start := time.Now()
	if _, err := f.pool.Exec(f.ctx, `
		INSERT INTO task_events (task_id, type, payload, created_at)
		SELECT t.id,
		       CASE WHEN g % 20 = 0 THEN 'stage.claimed' WHEN g % 33 = 0 THEN 'usage.reported' ELSE 'lease.extended' END,
		       '{}', now() - make_interval(secs => g * 60)
		FROM tasks t CROSS JOIN generate_series(1, 1100) g`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(f.ctx, `ANALYZE task_events; ANALYZE tasks; ANALYZE stage_runs`); err != nil {
		t.Fatal(err)
	}
	var events int
	_ = f.pool.QueryRow(f.ctx, `SELECT count(*) FROM task_events`).Scan(&events)
	t.Logf("снимок: 400 задач, событий %d (вставка %s)", events, time.Since(start).Round(time.Millisecond))

	measure := func(name, query string) {
		var times []time.Duration
		for i := 0; i < 7; i++ {
			at := time.Now()
			rec := f.do("GET", "/v1/projects/SDLC/tasks?"+query, f.root, nil)
			times = append(times, time.Since(at))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", name, rec.Code, rec.Body)
			}
		}
		slices.Sort(times)
		t.Logf("%-45s медиана %6s, худшее %6s", name, times[3].Round(100*time.Microsecond), times[6].Round(100*time.Microsecond))
	}
	measure("limit=200 (как clab tasks)", "limit=200")
	measure("in_work=true limit=200", "in_work=true&limit=200")
	measure("зона «Застряло» (фильтр по всей выборке)", "in_work=true&attention_not=awaits_human&attention_except=awaits_human,returned,unclaimed")
	measure("бэклог order=backlog limit=10", "terminal=false&in_work=false&top_level=true&order=backlog&limit=10")

	// План запроса последнего движения на 200 задачах.
	rows, err := f.pool.Query(f.ctx, `
		EXPLAIN (ANALYZE, BUFFERS)
		SELECT t.id, coalesce(lm.at, t.created_at)
		FROM tasks t
		LEFT JOIN LATERAL (
			SELECT e.created_at AS at FROM task_events e
			WHERE e.task_id = t.id AND e.type = ANY($1)
			ORDER BY e.created_at DESC LIMIT 1) lm ON true
		WHERE t.id IN (SELECT id FROM tasks ORDER BY seq LIMIT 200)`, domain.MovementEvents)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for rows.Next() {
		var line string
		_ = rows.Scan(&line)
		plan = append(plan, line)
	}
	rows.Close()
	t.Logf("план last_moved_at:\n%s", strings.Join(plan, "\n"))
}

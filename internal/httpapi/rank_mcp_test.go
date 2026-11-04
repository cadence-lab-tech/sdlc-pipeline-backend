package httpapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Порядок бэклога через MCP (CLAB-400, решение 0075): тул sdlc_rank_task и
// параметры backlog и top_level у sdlc_list_tasks. Номера — критерии К-N
// анализа CLAB-400. Тул и ручка — один движок, поэтому ответы и отказы
// сверяются с REST на одном состоянии.

// mcpErr — код и текст ошибки домена из ответа тула.
func mcpErr(out map[string]any) (code, msg string) {
	e, _ := out["error"].(map[string]any)
	code, _ = e["code"].(string)
	msg, _ = e["message"].(string)
	return code, msg
}

// К-10: те же перемещения и те же ответы, что у REST.
func TestMCPRankMatchesREST(t *testing.T) {
	f := newOvFixture(t)
	a, b, c := f.create("MCP A", nil), f.create("MCP B", nil), f.create("MCP C", nil)
	epic := f.create("MCP эпик", map[string]any{"type": "epic"})
	k1 := f.create("MCP ребёнок 1", map[string]any{"parent_key": epic})
	k2 := f.create("MCP ребёнок 2", map[string]any{"parent_key": epic})

	out, isErr := f.mcp(f.root, "sdlc_rank_task", map[string]any{"task": c, "before": a})
	if isErr {
		t.Fatalf("before: %v", out)
	}
	if out["changed"] != true || out["below"] != a || out["parent"] != nil {
		t.Fatalf("ответ before: %v", out)
	}
	if got := only(f.backlog(""), a, b, c); !slices.Equal(got, []string{c, a, b}) {
		t.Fatalf("порядок после before: %v", got)
	}
	out, isErr = f.mcp(f.root, "sdlc_rank_task", map[string]any{"task": c, "after": b})
	if isErr {
		t.Fatalf("after: %v", out)
	}
	if got := only(f.backlog(""), a, b, c); !slices.Equal(got, []string{a, b, c}) {
		t.Fatalf("порядок после after: %v", got)
	}
	// Край эпика: parent в ответе, событие с актором вызова.
	out, isErr = f.mcp(f.root, "sdlc_rank_task", map[string]any{"task": k2, "position": "top"})
	if isErr {
		t.Fatalf("top: %v", out)
	}
	f.checkRanked(k2, out, epic, "<nil>", k1)
	// То же место: changed=false, новых событий нет.
	events := len(f.rankEvents(k2))
	out, isErr = f.mcp(f.root, "sdlc_rank_task", map[string]any{"task": k2, "position": "top"})
	if isErr || out["changed"] != false || len(f.rankEvents(k2)) != events {
		t.Fatalf("то же место: %v %v", isErr, out)
	}

	// К-10: отказы — тот же код и текст, что у REST, ранги не меняются.
	gone := f.create("MCP снятая", nil)
	f.move(gone, "cancel", f.root)
	for _, tc := range []struct {
		name string
		key  string
		body map[string]any
	}{
		{"без места", a, map[string]any{}},
		{"два места", a, map[string]any{"before": b, "after": b}},
		{"чужой край", a, map[string]any{"position": "middle"}},
		{"нет соседа", a, map[string]any{"before": "SDLC-999"}},
		{"сама задача", a, map[string]any{"after": a}},
		{"другая группа", k1, map[string]any{"before": a}},
		{"задача закрыта", gone, map[string]any{"position": "top"}},
		{"сосед закрыт", a, map[string]any{"before": gone}},
	} {
		ranks, events := f.ranks(), f.eventCount()
		code, rest := f.rank(tc.key, f.root, tc.body)
		restCode, restMsg := mcpErr(rest)
		args := map[string]any{"task": tc.key}
		for k, v := range tc.body {
			args[k] = v
		}
		got, isErr := f.mcp(f.root, "sdlc_rank_task", args)
		gotCode, gotMsg := mcpErr(got)
		if !isErr || code == http.StatusOK || gotCode != restCode || gotMsg != restMsg || gotMsg == "" {
			t.Errorf("%s: MCP %q %q (ошибка %v), REST %d %q %q", tc.name, gotCode, gotMsg, isErr, code, restCode, restMsg)
		}
		if fmt.Sprint(f.ranks()) != fmt.Sprint(ranks) || f.eventCount() != events {
			t.Errorf("%s: что-то записано", tc.name)
		}
	}
	// Задачи нет и задача чужого проекта: not_found, а не ошибка движка.
	if got, isErr := f.mcp(f.root, "sdlc_rank_task", map[string]any{"task": "SDLC-998", "position": "top"}); !isErr {
		t.Errorf("нет задачи: %v", got)
	} else if code, _ := mcpErr(got); code != "not_found" {
		t.Errorf("нет задачи: код %q", code)
	}
}

// К-14, К-13: матрица уровней и токен задачи через MCP; журнал.
func TestMCPRankRightsAndAudit(t *testing.T) {
	f := newOvFixture(t)
	top1, top2 := f.create("MCP права верх 1", nil), f.create("MCP права верх 2", nil)

	viewer := f.tokenFor("user:vera", domain.MemberViewer)
	maint := f.tokenFor("user:mila", domain.MemberMaintainer)
	for _, args := range []map[string]any{
		{"task": top1, "position": "top"}, {"task": top1},
	} {
		out, isErr := f.mcp(viewer, "sdlc_rank_task", args)
		if code, msg := mcpErr(out); !isErr || code != "forbidden" || !strings.Contains(msg, "agent") {
			t.Fatalf("viewer %v: %v", args, out)
		}
	}
	for name, tok := range map[string]string{"agent": f.bot, "maintainer": maint, "admin": f.root} {
		if out, isErr := f.mcp(tok, "sdlc_rank_task", map[string]any{"task": top2, "position": "top"}); isErr {
			t.Fatalf("%s: %v", name, out)
		}
	}

	epic := f.create("MCP права эпик", map[string]any{"type": "epic"})
	own := f.create("MCP права своя", map[string]any{"parent_key": epic})
	brother := f.create("MCP права брат", map[string]any{"parent_key": epic})
	c1 := f.create("MCP права ребёнок 1", map[string]any{"parent_key": own})
	c2 := f.create("MCP права ребёнок 2", map[string]any{"parent_key": own})
	f.move(own, "start", f.root)
	f.ok("POST", "/v1/tasks/"+own+"/claim", f.bot, map[string]any{})
	task := f.ok("POST", "/v1/tasks/"+own+"/task-token", f.bot, nil)["token"].(string)

	for _, args := range []map[string]any{
		{"task": c2, "before": c1}, {"task": c2, "after": c1}, {"task": c2, "position": "top"}, {"task": c2, "position": "bottom"},
	} {
		if out, isErr := f.mcp(task, "sdlc_rank_task", args); isErr {
			t.Fatalf("токен задачи, ребёнок %v: %v", args, out)
		}
	}
	for name, args := range map[string]map[string]any{
		"своя задача":        {"task": own, "position": "top"},
		"брат своей":         {"task": brother, "position": "top"},
		"верхний уровень":    {"task": top1, "before": top2},
		"эпик":               {"task": epic, "position": "top"},
		"чужой ребёнок":      {"task": "SDLC-1", "position": "top"},
		"свой рядом с чужим": {"task": own, "before": brother},
	} {
		ranks := f.ranks()
		out, isErr := f.mcp(task, "sdlc_rank_task", args)
		code, msg := mcpErr(out)
		if !isErr || code != "forbidden" || !strings.HasPrefix(msg, workflow.ExecutorRefusal) ||
			!strings.Contains(msg, "порядок бэклога проекта — решение человека") {
			t.Errorf("%s: %v", name, out)
		}
		if fmt.Sprint(f.ranks()) != fmt.Sprint(ranks) {
			t.Errorf("%s: ранги изменились", name)
		}
	}
	// Сосед из другой группы отбивается движком (400), а не рамкой токена.
	if out, isErr := f.mcp(task, "sdlc_rank_task", map[string]any{"task": c1, "before": brother}); !isErr {
		t.Errorf("сосед другой группы: %v", out)
	} else if code, _ := mcpErr(out); code != "invalid_input" {
		t.Errorf("сосед другой группы: код %q", code)
	}

	// К-13: журнал видит успехи и отказы с видом токена.
	outcomes := map[string]int{}
	for _, r := range f.rows("a.action = 'sdlc_rank_task'") {
		if r.Channel != "mcp" {
			t.Fatalf("канал записи: %s", r.Channel)
		}
		outcomes[s(r.TokenKind)+" "+r.Outcome]++
	}
	if outcomes["task ok"] != 4 || outcomes["task forbidden"] != 6 || outcomes["task invalid_input"] != 1 ||
		outcomes["api forbidden"] != 2 || outcomes["api ok"] != 3 {
		t.Fatalf("журнал sdlc_rank_task: %v", outcomes)
	}
	if !slices.Contains(workflow.ExecutorActions(), "sdlc_rank_task") {
		t.Fatal("тула нет в белом списке токена задачи")
	}
}

// К-11, К-12: sdlc_list_tasks с backlog — та же выборка и порядок, что у
// REST; без backlog ответ прежний.
func TestMCPListBacklog(t *testing.T) {
	f := newOvFixture(t)
	a, b := f.create("Лист A", nil), f.create("Лист B", nil)
	working := f.create("Лист в работе", nil)
	f.move(working, "start", f.root)
	epic := f.create("Лист эпик", map[string]any{"type": "epic"})
	k1 := f.create("Лист ребёнок 1", map[string]any{"parent_key": epic})
	k2 := f.create("Лист ребёнок 2", map[string]any{"parent_key": epic})
	f.ok("POST", "/v1/tasks/"+a+"/links", f.root, map[string]any{"task": b, "rel": "blocks"})
	f.rankOK(b, map[string]any{"position": "top"})
	f.rankOK(k2, map[string]any{"position": "top"})

	for name, c := range map[string]struct {
		mcp  map[string]any
		rest string
	}{
		"верхний уровень": {map[string]any{"backlog": true, "top_level": true, "limit": 500},
			"backlog=true&top_level=true&order=backlog&limit=500"},
		"дети эпика": {map[string]any{"backlog": true, "parent": epic, "limit": 500},
			"backlog=true&parent=" + epic + "&order=backlog&limit=500"},
		"с type": {map[string]any{"backlog": true, "type": "epic"}, "backlog=true&type=epic&order=backlog"},
		"с limit": {map[string]any{"backlog": true, "top_level": true, "limit": 2},
			"backlog=true&top_level=true&order=backlog&limit=2"},
	} {
		restItems, restTotal := f.list(f.root, c.rest)
		out, isErr := f.mcp(f.root, "sdlc_list_tasks", c.mcp)
		if isErr {
			t.Fatalf("%s: %v", name, out)
		}
		raw, _ := out["tasks"].([]any)
		var got []string
		for i, it := range raw {
			m := it.(map[string]any)
			got = append(got, m["key"].(string))
			if i >= len(restItems) {
				continue
			}
			r := restItems[i]
			for _, k := range []string{"title", "priority", "rank", "in_work", "status_category"} {
				if fmt.Sprint(m[k]) != fmt.Sprint(r[k]) {
					t.Errorf("%s %s: %s MCP %v, REST %v", name, m["key"], k, m[k], r[k])
				}
			}
			if fmt.Sprint(m["blocked_by"]) != fmt.Sprint(r["blocked_by"]) || fmt.Sprint(m["child_counts"]) != fmt.Sprint(r["child_counts"]) ||
				fmt.Sprint(m["parent"]) != fmt.Sprint(r["parent_key"]) || m["status"] != r["status_key"] {
				t.Errorf("%s %s: MCP %v, REST %v", name, m["key"], m, r)
			}
		}
		if !slices.Equal(got, taskKeysOf(restItems)) || int(out["total"].(float64)) != restTotal {
			t.Errorf("%s: MCP %v (%v), REST %v (%d)", name, got, out["total"], taskKeysOf(restItems), restTotal)
		}
	}

	// blocked_by у заблокированной и пустой массив у свободной, не null.
	out, _ := f.mcp(f.root, "sdlc_list_tasks", map[string]any{"backlog": true, "top_level": true, "limit": 500})
	byKey := map[string]map[string]any{}
	for _, it := range out["tasks"].([]any) {
		m := it.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if fmt.Sprint(byKey[b]["blocked_by"]) != fmt.Sprint([]any{a}) || fmt.Sprint(byKey[a]["blocked_by"]) != "[]" {
		t.Fatalf("blocked_by: %v %v", byKey[b]["blocked_by"], byKey[a]["blocked_by"])
	}
	if byKey[working] != nil {
		t.Fatalf("задача в работе в бэклоге: %v", byKey[working])
	}
	// Порядок бэклога: b поднята наверх, a ниже.
	if keys := only(f.backlog(""), a, b); !slices.Equal(keys, []string{b, a}) {
		t.Fatalf("порядок: %v", keys)
	}

	// backlog с claimable или role — отказ, а не молчаливая игнорировка.
	for _, args := range []map[string]any{
		{"backlog": true, "claimable": true}, {"backlog": true, "role": "developer"},
	} {
		out, isErr := f.mcp(f.root, "sdlc_list_tasks", args)
		if code, _ := mcpErr(out); !isErr || code != "invalid_input" {
			t.Errorf("%v: %v", args, out)
		}
	}

	// К-12: без backlog — прежние поля и без новых.
	out, isErr := f.mcp(f.root, "sdlc_list_tasks", map[string]any{"parent": epic})
	if isErr {
		t.Fatalf("прежний вызов: %v", out)
	}
	old := out["tasks"].([]any)
	if len(old) != 2 {
		t.Fatalf("прежний вызов: %v", old)
	}
	first := old[0].(map[string]any)
	if first["key"] != k1 || first["status"] == nil || first["priority"] == nil {
		t.Fatalf("прежний порядок и поля: %v", first)
	}
	for _, k := range []string{"rank", "in_work", "blocked_by", "child_counts", "status_category"} {
		if _, ok := first[k]; ok {
			t.Errorf("новое поле %s в прежнем ответе", k)
		}
	}
	if _, ok := out["total"]; ok {
		t.Error("total в прежнем ответе")
	}
	// top_level без backlog — только задачи без родителя.
	out, _ = f.mcp(f.root, "sdlc_list_tasks", map[string]any{"top_level": true, "limit": 500})
	for _, it := range out["tasks"].([]any) {
		if p := it.(map[string]any)["parent"]; p != nil {
			t.Errorf("top_level вернул ребёнка: %v", it)
		}
	}
}

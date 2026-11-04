package httpapi

import (
	"slices"
	"sort"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Согласие «Ждут решения» и списка задач на задачах с объявленной областью
// (CLAB-396): гейт repo_scope здесь НЕ снимается. Прежние тесты согласия
// (overview_test.go, approvals_measure_test.go) снимают его вручную, а
// снятый гейт движок не вычисляет, поэтому незагруженная область
// (ApprovalCandidates, ChildTasks читали задачу без task.Repositories и
// получали «область не объявлена») им не была видна: на проде /approvals
// был пуст при awaits_human в списке. Номера — критерии приёмки анализа.
func TestApprovalsAgreeWithListOnDeclaredScope(t *testing.T) {
	f := newOvFixture(t)
	const backend = "cadence-lab-tech/sdlc-pipeline-backend"
	const docs = "cadence-lab-tech/sdlc-pipeline-docs"

	mk := func(title string, repos []string) string {
		t.Helper()
		key := f.create(title, map[string]any{"repositories": repos})
		f.move(key, "start", f.root)
		return key
	}
	// Всё, кроме апрува и области, снято: ждёт ли задача человека, решает
	// только repo_scope, который вычисляется.
	clearOthers := func(key string) {
		t.Helper()
		for _, gate := range []string{"artifacts", "peer_review", "drift"} {
			f.decide(key, "analysis_done", gate, map[string]any{"status": "waived"})
		}
	}

	waiting := mk("Ждёт апрува, область объявлена", []string{backend})
	clearOthers(waiting)
	approved := mk("Апрув получен", []string{backend})
	clearOthers(approved)
	f.decide(approved, "analysis_done", "human_approval", map[string]any{"status": "passed"})
	outside := mk("Артефакт вне области", []string{backend})
	clearOthers(outside)
	f.ok("POST", "/v1/tasks/"+outside+"/claim", f.bot, map[string]any{})
	f.ok("POST", "/v1/tasks/"+outside+"/artifacts", f.bot, map[string]any{
		"kind": "analysis.brief", "title": "бриф", "content": "# бриф",
		"code_refs": []map[string]any{{"repository": docs, "path": "README.md"}},
	})
	noScope := mk("Область не объявлена", nil)
	clearOthers(noScope)

	pairs := func(token string) map[string][]string {
		t.Helper()
		out := map[string][]string{}
		for _, raw := range f.ok("GET", "/v1/projects/SDLC/approvals", token, nil)["approvals"].([]any) {
			a := raw.(map[string]any)
			key := a["task"].(map[string]any)["key"].(string)
			out[key] = append(out[key], a["transition"].(map[string]any)["key"].(string)+"/"+a["gate"].(map[string]any)["gate_key"].(string))
		}
		for _, v := range out {
			sort.Strings(v)
		}
		return out
	}
	awaits := func() map[string][]string {
		t.Helper()
		out := map[string][]string{}
		items, _ := f.list(f.root, "limit=500")
		for _, it := range items {
			a := attentionOf(it, workflow.AttentionAwaitsHuman)
			if a == nil {
				continue
			}
			key := it["key"].(string)
			for _, raw := range a["gates"].([]any) {
				g := raw.(map[string]any)
				out[key] = append(out[key], g["transition"].(string)+"/"+g["gate"].(string))
			}
			sort.Strings(out[key])
		}
		return out
	}

	// AC-1, AC-5: множества пар (задача, переход, гейт) совпадают, и ждёт
	// ровно задача с объявленной областью, где всё остальное прошло.
	got, want := pairs(f.root), awaits()
	if len(want) == 0 {
		t.Fatalf("список не показал ни одной ждущей задачи: фикстура не ждёт апрува")
	}
	if len(got) != len(want) {
		t.Fatalf("/approvals %v, awaits_human списка %v", got, want)
	}
	for key, w := range want {
		if !slices.Equal(got[key], w) {
			t.Errorf("%s: /approvals %v, awaits_human %v", key, got[key], w)
		}
	}
	if !slices.Equal(got[waiting], []string{"analysis_done/human_approval"}) {
		t.Fatalf("%s ждёт апрува, а /approvals отдал %v (всё: %v)", waiting, got[waiting], got)
	}
	// Остальные состояния: апрув получен, артефакт вне области (repo_scope
	// failed) и область не объявлена (AC-3) — нигде нет.
	for _, key := range []string{approved, outside, noScope} {
		if len(got[key]) != 0 || len(want[key]) != 0 {
			t.Errorf("%s не ждёт апрува: /approvals %v, awaits_human %v", key, got[key], want[key])
		}
	}

	// AC-2: гейт области в карточке задачи — passed; у задачи вне области —
	// failed; тот же вердикт, что и на пути /approvals.
	scopeGate := func(key string) any {
		t.Helper()
		for _, raw := range f.ok("GET", "/v1/tasks/"+key+"/transitions", f.root, nil)["transitions"].([]any) {
			tr := raw.(map[string]any)
			if tr["key"] != "analysis_done" {
				continue
			}
			for _, g := range tr["gates"].([]any) {
				if gate := g.(map[string]any); gate["gate_key"] == "repo_scope" {
					return gate["status"]
				}
			}
		}
		t.Fatalf("%s: гейта repo_scope нет в analysis_done", key)
		return nil
	}
	if s := scopeGate(waiting); s != "passed" {
		t.Errorf("repo_scope %s: %v, want passed", waiting, s)
	}
	if s := scopeGate(outside); s != "failed" {
		t.Errorf("repo_scope %s: %v, want failed", outside, s)
	}
}

// AC-4: признак children[].awaiting_approval карточки эпика совпадает с
// awaiting_approval той же задачи в списке, когда у ребёнка объявлена
// область и repo_scope не снят.
func TestChildrenAwaitingApprovalOnDeclaredScope(t *testing.T) {
	f := newOvFixture(t)
	const backend = "cadence-lab-tech/sdlc-pipeline-backend"
	parent := f.create("Родитель", nil)
	mk := func(title string, repos []string) string {
		t.Helper()
		key := f.create(title, map[string]any{"parent_key": parent, "repositories": repos})
		f.move(key, "start", f.root)
		for _, gate := range []string{"artifacts", "peer_review", "drift"} {
			f.decide(key, "analysis_done", gate, map[string]any{"status": "waived"})
		}
		return key
	}
	withScope := mk("Ребёнок с областью", []string{backend})
	withoutScope := mk("Ребёнок без области", nil)

	card := f.ok("GET", "/v1/tasks/"+parent, f.root, nil)
	seen := 0
	for _, raw := range card["children"].([]any) {
		kid := raw.(map[string]any)
		key := kid["key"].(string)
		exp := f.item(f.root, key)["awaiting_approval"]
		if kid["awaiting_approval"] != exp {
			t.Errorf("%s: карточка эпика %v, список %v", key, kid["awaiting_approval"], exp)
		}
		switch key {
		case withScope:
			seen++
			if kid["awaiting_approval"] != true {
				t.Errorf("%s с областью ждёт апрува, а в карточке эпика %v", key, kid["awaiting_approval"])
			}
		case withoutScope:
			seen++
			if kid["awaiting_approval"] != false {
				t.Errorf("%s без области не ждёт апрува, а в карточке эпика %v", key, kid["awaiting_approval"])
			}
		}
	}
	if seen != 2 {
		t.Fatalf("в карточке %d из 2 детей", seen)
	}
}

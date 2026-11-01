package httpapi

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Отбор документов роли на статусе по REST авторинга (CLAB-348, решение
// 0068, п. 3): document_names в PUT …/roles, отказы формы, снятие отбора
// запросом без поля, отказ у активной версии, предупреждение проверки.
// Номера — критерии приёмки анализа CLAB-348.

// statusRolesBody — тело PUT …/roles из ролей статуса ответа; names —
// отбор по ключу роли (нет ключа — поле не передаётся).
func statusRolesBody(st map[string]any, names map[string]any) map[string]any {
	roles := []map[string]any{}
	for _, raw := range st["roles"].([]any) {
		r := raw.(map[string]any)
		item := map[string]any{"role": r["role_key"], "is_primary": r["is_primary"], "position": r["position"]}
		if sel, ok := names[r["role_key"].(string)]; ok {
			item["document_names"] = sel
		}
		roles = append(roles, item)
	}
	return map[string]any{"roles": roles}
}

func withRoles(t *testing.T, wf map[string]any) map[string]any {
	t.Helper()
	for _, raw := range wf["statuses"].([]any) {
		st := raw.(map[string]any)
		if roles, _ := st["roles"].([]any); len(roles) > 0 {
			return st
		}
	}
	t.Fatalf("в цикле нет статуса с ролью: %v", wf["statuses"])
	return nil
}

// AC-348.2, .3, .4, .9.
func TestStatusRoleDocumentsOverREST(t *testing.T) {
	f := newAuditFixture(t)

	code, active := f.json("GET", "/v1/projects/SDLC/workflow", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("цикл проекта: %d %v", code, active)
	}
	code, draft := f.json("POST", "/v1/projects/SDLC/workflows", f.root, map[string]any{"name": "Отбор"})
	if code != http.StatusCreated {
		t.Fatalf("черновик: %d %v", code, draft)
	}
	base := "/v1/workflows/" + draft["id"].(string)
	code, wf := f.json("GET", base, f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("GET черновика: %d %v", code, wf)
	}
	st := withRoles(t, wf)
	key := st["key"].(string)
	primary := st["roles"].([]any)[0].(map[string]any)
	roleKey := primary["role_key"].(string)
	if _, ok := primary["document_names"]; ok {
		t.Fatalf("без отбора ключа document_names нет: %v", primary)
	}
	url := base + "/statuses/" + key + "/roles"

	code, saved := f.json("PUT", url, f.root, statusRolesBody(st, map[string]any{roleKey: []string{"main.md", "нет-нигде.md"}}))
	if code != http.StatusOK {
		t.Fatalf("PUT с отбором: %d %v", code, saved)
	}
	got := saved["roles"].([]any)[0].(map[string]any)["document_names"]
	if !reflect.DeepEqual(got, []any{"main.md", "нет-нигде.md"}) {
		t.Fatalf("ответ PUT: %v", saved["roles"])
	}
	_, wf = f.json("GET", base, f.root, nil)
	if got := statusByKey(t, wf, key)["roles"].([]any)[0].(map[string]any)["document_names"]; !reflect.DeepEqual(got,
		[]any{"main.md", "нет-нигде.md"}) {
		t.Fatalf("GET версии: %v", got)
	}

	// AC-348.9: имя вне последней версии роли — предупреждение, не проблема.
	code, v := f.json("GET", base+"/validation", f.root, nil)
	if code != http.StatusOK || v["valid"] != true {
		t.Fatalf("проверка: %d %v", code, v)
	}
	var warn []map[string]any
	for _, raw := range v["warnings"].([]any) {
		w := raw.(map[string]any)
		if w["code"] == "role_document_missing" {
			warn = append(warn, w)
		}
	}
	if len(warn) != 1 || warn[0]["status"] != key || !strings.Contains(warn[0]["message"].(string), "нет-нигде.md") {
		t.Fatalf("предупреждения проверки: %v", v["warnings"])
	}

	// AC-348.4: форма отбора — 400, сохранённое не меняется.
	many := make([]string, 51)
	for i := range many {
		many[i] = "d" + strconv.Itoa(i) + ".md"
	}
	for name, sel := range map[string]any{
		"пустой список": []string{}, "пустое имя": []string{""}, "повтор": []string{"a.md", "a.md"}, "больше 50": many,
	} {
		code, body := f.json("PUT", url, f.root, statusRolesBody(st, map[string]any{roleKey: sel}))
		if code != http.StatusBadRequest {
			t.Fatalf("%s: %d %v", name, code, body)
		}
	}
	_, wf = f.json("GET", base, f.root, nil)
	if got := statusByKey(t, wf, key)["roles"].([]any)[0].(map[string]any)["document_names"]; !reflect.DeepEqual(got,
		[]any{"main.md", "нет-нигде.md"}) {
		t.Fatalf("отбор изменился после отказов: %v", got)
	}

	// Запрос без поля снимает отбор.
	code, saved = f.json("PUT", url, f.root, statusRolesBody(st, nil))
	if code != http.StatusOK {
		t.Fatalf("PUT без отбора: %d %v", code, saved)
	}
	if _, ok := saved["roles"].([]any)[0].(map[string]any)["document_names"]; ok {
		t.Fatalf("отбор не снят: %v", saved["roles"])
	}

	// AC-348.3: активная версия — тот же отказ, что у других правок.
	activeURL := "/v1/workflows/" + active["id"].(string) + "/statuses/" + key + "/roles"
	code, body := f.json("PUT", activeURL, f.root, statusRolesBody(withRoles(t, active), map[string]any{roleKey: []string{"main.md"}}))
	codeOther, other := f.json("PATCH", "/v1/workflows/"+active["id"].(string)+"/statuses/"+key, f.root,
		map[string]any{"name": "Другое"})
	if code != http.StatusConflict || codeOther != code {
		t.Fatalf("правка активной версии: %d %v (другая правка: %d %v)", code, body, codeOther, other)
	}
}

package httpapi

import (
	"net/http"
	"testing"
)

// Необязательный этап по REST авторинга (CLAB-314, решение 0066, п. 2).

func statusByKey(t *testing.T, wf map[string]any, key string) map[string]any {
	t.Helper()
	list, _ := wf["statuses"].([]any)
	for _, raw := range list {
		st, _ := raw.(map[string]any)
		if st["key"] == key {
			return st
		}
	}
	t.Fatalf("статуса %q нет в ответе: %v", key, wf["statuses"])
	return nil
}

// AC-2, AC-3, AC-4, AC-6.
func TestOptionalStatusOverREST(t *testing.T) {
	f := newAuditFixture(t)

	code, active := f.json("GET", "/v1/projects/SDLC/workflow", f.root, nil)
	if code != http.StatusOK {
		t.Fatalf("цикл проекта: %d %v", code, active)
	}
	// AC-2: оба поля у каждого статуса, и при нулевых значениях тоже.
	for _, raw := range active["statuses"].([]any) {
		st := raw.(map[string]any)
		if st["is_optional"] != false || st["enabled_by_default"] != true {
			t.Fatalf("у обязательного статуса is_optional=false и enabled_by_default=true: %v", st)
		}
	}
	activeID := active["id"].(string)

	code, draft := f.json("POST", "/v1/projects/SDLC/workflows", f.root, map[string]any{"name": "Опции"})
	if code != http.StatusCreated {
		t.Fatalf("черновик: %d %v", code, draft)
	}
	base := "/v1/workflows/" + draft["id"].(string)

	code, created := f.json("POST", base+"/statuses", f.root, map[string]any{
		"key": "design", "name": "Дизайн", "category": "waiting", "is_optional": true})
	if code != http.StatusCreated || created["is_optional"] != true || created["enabled_by_default"] != true {
		t.Fatalf("новый необязательный по умолчанию включён: %d %v", code, created)
	}
	code, created = f.json("POST", base+"/statuses", f.root, map[string]any{
		"key": "plain", "name": "Обычный", "category": "waiting"})
	if code != http.StatusCreated || created["is_optional"] != false || created["enabled_by_default"] != true {
		t.Fatalf("без is_optional статус обязательный: %d %v", code, created)
	}
	if code, body := f.json("POST", base+"/statuses", f.root, map[string]any{
		"key": "bad", "name": "Плохой", "category": "waiting", "enabled_by_default": false}); code != http.StatusBadRequest {
		t.Fatalf("POST без is_optional с умолчанием «выключен»: %d %v", code, body)
	}

	get := func() map[string]any {
		code, wf := f.json("GET", base, f.root, nil)
		if code != http.StatusOK {
			t.Fatalf("GET версии: %d %v", code, wf)
		}
		return wf
	}

	code, st := f.json("PATCH", base+"/statuses/design", f.root, map[string]any{"enabled_by_default": false})
	if code != http.StatusOK || st["is_optional"] != true || st["enabled_by_default"] != false {
		t.Fatalf("умолчание необязательного: %d %v", code, st)
	}
	if got := statusByKey(t, get(), "design"); got["enabled_by_default"] != false {
		t.Fatalf("GET версии отдаёт умолчание: %v", got)
	}

	for name, body := range map[string]map[string]any{
		"умолчание у обязательного": {"enabled_by_default": false},
		"пара false/false":          {"is_optional": false, "enabled_by_default": false},
	} {
		key := "plain"
		if name == "пара false/false" {
			key = "design"
		}
		before := statusByKey(t, get(), key)
		if code, resp := f.json("PATCH", base+"/statuses/"+key, f.root, body); code != http.StatusBadRequest ||
			resp["error"].(map[string]any)["code"] != "invalid_input" {
			t.Fatalf("%s: ожидался 400 invalid_input, получили %d %v", name, code, resp)
		}
		after := statusByKey(t, get(), key)
		if before["is_optional"] != after["is_optional"] || before["enabled_by_default"] != after["enabled_by_default"] {
			t.Fatalf("%s: после 400 статус изменился: %v → %v", name, before, after)
		}
	}

	// Снятие отметки возвращает умолчание во «включён»; прочие поля как раньше.
	code, st = f.json("PATCH", base+"/statuses/design", f.root, map[string]any{"is_optional": false, "name": "Дизайн 2"})
	if code != http.StatusOK || st["is_optional"] != false || st["enabled_by_default"] != true || st["name"] != "Дизайн 2" {
		t.Fatalf("снятие отметки: %d %v", code, st)
	}

	// AC-6: проверка черновика по REST — коды валидатора. design снова
	// необязательный, без обхода.
	if code, body := f.json("PATCH", base+"/statuses/design", f.root, map[string]any{"is_optional": true}); code != http.StatusOK {
		t.Fatalf("отметка: %d %v", code, body)
	}
	code, v := f.json("GET", base+"/validation", f.root, nil)
	if code != http.StatusOK || v["valid"] != false {
		t.Fatalf("проверка черновика: %d %v", code, v)
	}
	found := false
	for _, raw := range v["problems"].([]any) {
		p := raw.(map[string]any)
		if p["code"] == "optional_no_bypass" && p["status"] == "design" {
			found = true
		}
	}
	if !found {
		t.Fatalf("ожидалась optional_no_bypass у design: %v", v["problems"])
	}

	// AC-4: активная версия — 409; ниже maintainer — 403, как у соседних ручек.
	activeBase := "/v1/workflows/" + activeID
	firstKey := active["statuses"].([]any)[1].(map[string]any)["key"].(string)
	if code, body := f.json("PATCH", activeBase+"/statuses/"+firstKey, f.root, map[string]any{"is_optional": true}); code != http.StatusConflict {
		t.Fatalf("PATCH активной версии: %d %v", code, body)
	}
	if code, body := f.json("POST", activeBase+"/statuses", f.root, map[string]any{
		"key": "opt", "name": "Opt", "category": "waiting", "is_optional": true}); code != http.StatusConflict {
		t.Fatalf("POST в активную версию: %d %v", code, body)
	}
	if code, body := f.json("PATCH", base+"/statuses/design", f.bot, map[string]any{"enabled_by_default": false}); code != http.StatusForbidden {
		t.Fatalf("агент не правит цикл: %d %v", code, body)
	}
}

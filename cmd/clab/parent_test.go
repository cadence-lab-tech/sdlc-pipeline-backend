package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// clab edit --parent (CLAB-196, AC-14): перенос печатает прежнего и нового
// родителя, "" снимает (null в теле), --json отдаёт задачу, перенос идёт
// первым и отказ не применяет остальную правку, сервер без ручки — понятный
// отказ.

const reparented = `{"task":{"key":"CLAB-12","title":"T","parent_key":"CLAB-9"},"from":"CLAB-5","to":"CLAB-9","changed":true}`

func TestEditParentMovesAndPrints(t *testing.T) {
	withJSON(t, false)
	c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, reparented) })
	out, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-12", "--parent", "CLAB-9"})
	})
	if err != nil || out != "CLAB-12: родитель CLAB-5 → CLAB-9\n" {
		t.Fatalf("вывод: %q %v", out, err)
	}
	if len(f.calls) != 1 || f.calls[0] != "PUT /v1/tasks/CLAB-12/parent" || !equalJSONText(f.bodies[0], `{"parent_key":"CLAB-9"}`) {
		t.Fatalf("запрос: %v %v", f.calls, f.bodies)
	}

	// "" — снять: null в теле.
	c, f = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"task":{"key":"CLAB-12"},"from":"CLAB-5","to":null,"changed":true}`)
	})
	out, _, err = capture(t, func() error { return cmdEdit(context.Background(), c, []string{"CLAB-12", "--parent", ""}) })
	if err != nil || out != "CLAB-12: родитель CLAB-5 → —\n" || !equalJSONText(f.bodies[0], `{"parent_key":null}`) {
		t.Fatalf("снятие: %q %v %v", out, err, f.bodies)
	}

	// Повтор: уже там.
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"task":{"key":"CLAB-12"},"from":"CLAB-9","to":"CLAB-9","changed":false}`)
	})
	out, _, _ = capture(t, func() error { return cmdEdit(context.Background(), c, []string{"CLAB-12", "--parent", "CLAB-9"}) })
	if out != "CLAB-12: уже в CLAB-9\n" {
		t.Fatalf("повтор: %q", out)
	}

	// --json — задача.
	withJSON(t, true)
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, reparented) })
	out, _, err = capture(t, func() error { return cmdEdit(context.Background(), c, []string{"CLAB-12", "--parent", "CLAB-9"}) })
	if err != nil || !equalJSONText(out, `{"key":"CLAB-12","title":"T","parent_key":"CLAB-9"}`) {
		t.Fatalf("--json: %q %v", out, err)
	}
}

func TestEditParentFirstAndOldServer(t *testing.T) {
	withJSON(t, false)
	// Вместе с другой правкой: сначала родитель, затем PATCH.
	c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			reply(w, 200, reparented)
			return
		}
		reply(w, 200, editedTask)
	})
	out, _, err := capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--priority", "high", "--parent", "CLAB-9"})
	})
	if err != nil || len(f.calls) != 2 || f.calls[0] != "PUT /v1/tasks/CLAB-58/parent" || f.calls[1] != "PATCH /v1/tasks/CLAB-58" ||
		!strings.Contains(out, "родитель CLAB-5 → CLAB-9") || !strings.Contains(out, "Новый заголовок") {
		t.Fatalf("порядок: %v %q %v", f.calls, out, err)
	}

	// Отказ переноса — остальное не применяется, текст сервера.
	c, f = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 400, `{"error":{"code":"invalid_input","message":"CLAB-9 сама вложена в CLAB-1: вложенность — один уровень"}}`)
	})
	_, _, err = capture(t, func() error {
		return cmdEdit(context.Background(), c, []string{"CLAB-58", "--priority", "high", "--parent", "CLAB-9"})
	})
	if err == nil || !strings.Contains(err.Error(), "вложенность — один уровень") || len(f.calls) != 1 {
		t.Fatalf("отказ: %v %v", err, f.calls)
	}

	// Сервер без ручки: 404 простым текстом.
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	})
	_, _, err = capture(t, func() error { return cmdEdit(context.Background(), c, []string{"CLAB-58", "--parent", "CLAB-9"}) })
	if !errors.Is(err, errParentOldServer) || !strings.Contains(err.Error(), "сервер не поддерживает смену родителя") {
		t.Fatalf("старый сервер: %v", err)
	}
	// «Задачи нет» — конверт с кодом, не старый сервер.
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 404, `{"error":{"code":"not_found","message":"задача \"CLAB-58\" не найдена"}}`)
	})
	_, _, err = capture(t, func() error { return cmdEdit(context.Background(), c, []string{"CLAB-58", "--parent", "CLAB-9"}) })
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
		t.Fatalf("нет задачи: %v", err)
	}
	// Отказ человеческого действия — подсказка human_action.
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 403, `{"error":{"code":"forbidden","message":"смена родителя — решение человека","details":{"reason":"human_action"}}}`)
	})
	_, _, err = capture(t, func() error { return cmdEdit(context.Background(), c, []string{"CLAB-58", "--parent", "CLAB-9"}) })
	if err == nil || !strings.Contains(err.Error(), denialHints["human_action"]) {
		t.Fatalf("отказ человеку: %v", err)
	}
}

func TestEditUsageNamesParent(t *testing.T) {
	for _, cmd := range commands() {
		if cmd.name == "edit" && !strings.Contains(cmd.usage, "--parent") {
			t.Fatalf("usage clab edit без --parent: %s", cmd.usage)
		}
	}
	c, _ := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, `{}`) })
	_, _, err := capture(t, func() error { return cmdEdit(context.Background(), c, []string{"CLAB-58"}) })
	if err == nil || !strings.Contains(err.Error(), "--parent") {
		t.Fatalf("нечего менять: %v", err)
	}
}

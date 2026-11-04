package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// clab move (CLAB-408): K-7 и AC-14 — флаги обязательны, сервер прежней
// версии — понятный отказ, вывод словами; clab wait на переводе выходит
// status_changed.

func TestMoveFlagsRequired(t *testing.T) {
	withJSON(t, false)
	for _, args := range [][]string{
		{"CLAB-12", "--reason", "x"},
		{"CLAB-12", "--to", "analysis"},
		{"CLAB-12", "--to", "analysis", "--reason", "   "},
		{"CLAB-12", "--to", " ", "--reason", "x"},
	} {
		c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) { reply(w, 200, `{}`) })
		_, _, err := capture(t, func() error { return cmdMove(context.Background(), c, args) })
		if err == nil || !strings.Contains(err.Error(), "не указан") {
			t.Errorf("%v: %v", args, err)
		}
		if len(f.calls) != 0 {
			t.Errorf("%v: ушёл запрос %v", args, f.calls)
		}
	}
}

func TestMoveOldServer(t *testing.T) {
	withJSON(t, false)
	// Маршрута нет: gin отвечает простым текстом 404 без кода.
	c, _ := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("404 page not found"))
	})
	_, _, err := capture(t, func() error {
		return cmdMove(context.Background(), c, []string{"CLAB-12", "--to", "analysis", "--reason", "x"})
	})
	if !errors.Is(err, errMoveOldServer) || !strings.Contains(err.Error(), "сервер не знает перевода в статус, обновите сервер") {
		t.Fatalf("старый сервер: %v", err)
	}
	// «Задачи нет» — конверт с кодом, это не старый сервер.
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 404, `{"error":{"code":"not_found","message":"задача \"CLAB-12\" не найдена"}}`)
	})
	_, _, err = capture(t, func() error {
		return cmdMove(context.Background(), c, []string{"CLAB-12", "--to", "analysis", "--reason", "x"})
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
		t.Fatalf("нет задачи: %v", err)
	}
}

func TestMoveRendersResultAndDenial(t *testing.T) {
	withJSON(t, false)
	c, f := newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 200, `{"task":{"key":"CLAB-12","status_key":"analysis"},
			"stage_run":{"status_key":"analysis","attempt":2,"state":"pending"},
			"previous_run":{"status_key":"testing","attempt":1,"state":"abandoned","outcome":"moved"},
			"closed_reviews":1}`)
	})
	out, _, err := capture(t, func() error {
		return cmdMove(context.Background(), c, []string{"CLAB-12", "--to", "analysis", "--reason", " требования неверны "})
	})
	want := "CLAB-12: testing → analysis (перевод, гейты не проверялись), попытка 2\n" +
		"Прогон testing попытки 1: переведён, перепроверок закрыто: 1; аренда снята, токен исполнителя погашен\n"
	if err != nil || out != want {
		t.Fatalf("вывод: %q %v", out, err)
	}
	if f.calls[0] != "POST /v1/tasks/CLAB-12/move" || !equalJSONText(f.bodies[0], `{"to":"analysis","reason":"требования неверны"}`) {
		t.Fatalf("запрос: %v %v", f.calls, f.bodies)
	}

	// Отказ боту — текст сервера и подсказка по human_action.
	c, _ = newFakeBacklog(t, func(w http.ResponseWriter, r *http.Request) {
		reply(w, 403, `{"error":{"code":"forbidden","message":"переводить задачу в статус может человек","details":{"reason":"human_action"}}}`)
	})
	_, _, err = capture(t, func() error {
		return cmdMove(context.Background(), c, []string{"CLAB-12", "--to", "done", "--reason", "x"})
	})
	if err == nil || !strings.Contains(err.Error(), denialHints["human_action"]) {
		t.Fatalf("отказ: %v", err)
	}
	if runOutcomeLabel("moved") != "переведён" || runOutcomeLabel("odd") != "odd" {
		t.Fatal("подпись исхода")
	}
}

// K-7: clab wait на переводе в статус не зависает — status_changed (код 4),
// и в тот же статус (рестарт этапа) тоже: прогон сменился.
func TestWaitOnMove(t *testing.T) {
	w := newWaitWorld(t)
	for _, c := range []struct{ key, to string }{{"SDLC-1", "qa"}, {"SDLC-2", "analysis"}} {
		w.start(c.key)
		ch := w.wait(waitOptions{task: c.key, gate: "human_approval", timeout: time.Minute})
		still(t, ch, 500*time.Millisecond, "перевода ещё нет")
		w.call("POST", "/v1/tasks/"+c.key+"/move", map[string]any{"to": c.to, "reason": "так нужно"}, http.StatusOK)
		r := recv(t, ch, 5*time.Second, "перевод")
		if r.err != nil || r.code != waitStatusChanged || r.res.Outcome != "status_changed" ||
			r.res.Status.From != "analysis" || r.res.Status.To != c.to {
			t.Fatalf("%s → %s: %+v код %d %v", c.key, c.to, r.res, r.code, r.err)
		}
	}
}

package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// CLAB-353 (AC-5, AC-6): PATCH /tasks/:key/scope сливает properties с
// текущими, а не объект — 400 без изменений в базе.
func TestScopePropertiesMergeAndValidation(t *testing.T) {
	f := newAuditFixture(t)
	ctx := context.Background()

	read := func() map[string]any {
		t.Helper()
		var raw json.RawMessage
		if err := f.pool.QueryRow(ctx, `SELECT properties FROM tasks WHERE key = 'SDLC-3'`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if _, err := f.pool.Exec(ctx, `UPDATE tasks SET properties = '{"type":"epic","multi_repo":true}' WHERE key = 'SDLC-3'`); err != nil {
		t.Fatal(err)
	}

	rec := f.do("PATCH", "/v1/tasks/SDLC-3/scope", f.root, map[string]any{
		"properties": map[string]any{"needs_migration": true}, "note": "n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("слияние: %d %s", rec.Code, rec.Body)
	}
	if got := read(); got["type"] != "epic" || got["needs_migration"] != true || got["multi_repo"] != true || len(got) != 3 {
		t.Fatalf("после слияния %v", got)
	}

	for _, bad := range []any{[]string{"multi_repo"}, "multi_repo", 42, true} {
		rec = f.do("PATCH", "/v1/tasks/SDLC-3/scope", f.root, map[string]any{"properties": bad})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("properties=%v: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if got := read(); len(got) != 3 {
		t.Fatalf("отказ изменил свойства: %v", got)
	}

	// properties: null и отсутствие поля — не трогать.
	rec = f.do("PATCH", "/v1/tasks/SDLC-3/scope", f.root, map[string]any{"properties": nil, "note": "n"})
	if rec.Code != http.StatusOK {
		t.Fatalf("null: %d %s", rec.Code, rec.Body)
	}
	if got := read(); len(got) != 3 || got["type"] != "epic" {
		t.Fatalf("null изменил свойства: %v", got)
	}

}

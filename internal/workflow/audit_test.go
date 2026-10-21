package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// Параметры журнала без тел и секретов (CLAB-210, критерии 12–14): секреты
// на любой глубине — [redacted], content — размер и sha256, строка длиннее
// 4 КБ — тоже, запись целиком не больше 16 КБ.
func TestSanitizeAuditParams(t *testing.T) {
	body := "# Разбор\n\nтело артефакта"
	long := strings.Repeat("инструкция ", 600) // больше 4 КБ
	in := map[string]any{
		"task_key": "SDLC-1",
		"content":  body,
		"path":     map[string]any{"code": "ABCD-1234"},
		"body": map[string]any{
			"token":         "clab_inv_secret",
			"Authorization": "Bearer clab_xxx",
			"nested": []any{
				map[string]any{"client_secret": "s1", "password": "p1", "api_key": "k1", "private_key": "pk", "name": "ok"},
			},
			"roles": []any{map[string]any{"key": "analyst", "instructions": long}},
		},
	}
	raw := SanitizeAuditParams(in)
	text := string(raw)
	for _, secret := range []string{"ABCD-1234", "clab_inv_secret", "clab_xxx", `"s1"`, `"p1"`, `"k1"`, `"pk"`, body, "инструкция инструкция"} {
		if strings.Contains(text, secret) {
			t.Fatalf("в параметрах осталось %q: %s", secret, text)
		}
	}
	var out struct {
		TaskKey string `json:"task_key"`
		Content struct {
			Size   int    `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"content"`
		Body struct {
			Nested []map[string]any `json:"nested"`
			Roles  []struct {
				Instructions struct {
					Size   int    `json:"size"`
					SHA256 string `json:"sha256"`
				} `json:"instructions"`
			} `json:"roles"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	if out.TaskKey != "SDLC-1" || out.Content.Size != len(body) || out.Content.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("content и обычные поля: %s", text)
	}
	if out.Body.Nested[0]["name"] != "ok" || out.Body.Nested[0]["password"] != auditRedacted {
		t.Fatalf("вложенные секреты: %s", text)
	}
	if out.Body.Roles[0].Instructions.Size != len(long) || out.Body.Roles[0].Instructions.SHA256 == "" {
		t.Fatalf("длинная строка: %s", text)
	}
}

func TestSanitizeAuditParamsLimit(t *testing.T) {
	// Много коротких строк: каждая меньше 4 КБ, вместе — больше 16 КБ.
	items := make([]any, 0, 200)
	for range 200 {
		items = append(items, strings.Repeat("x", 200))
	}
	raw := SanitizeAuditParams(map[string]any{"task_key": "SDLC-1", "items": items})
	if len(raw) > auditMaxParams {
		t.Fatalf("параметры %d байт", len(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["_truncated"] != true || out["task_key"] != "SDLC-1" {
		t.Fatalf("усечение без пометки или без простых полей: %s", raw)
	}
	if _, ok := out["items"].(map[string]any)["sha256"]; !ok {
		t.Fatalf("составное поле не заменено размером и sha256: %s", raw)
	}

	if got := string(SanitizeAuditParams(nil)); got != "{}" {
		t.Fatalf("пустые параметры: %s", got)
	}
}

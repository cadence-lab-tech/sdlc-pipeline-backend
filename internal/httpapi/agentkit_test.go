package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"testing"
)

type agentKitView struct {
	Version string `json:"version"`
	Files   []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		SHA256  string `json:"sha256"`
	} `json:"files"`
	Skills []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"skills"`
}

// CLAB-232, критерии 3 и 21: набор отдаётся без входа в режиме токенов, его
// версия — та же, что у /v1/instance того же процесса (SHA нашего инстанса
// или vX.Y.Z выпуска), пути относительные, суммы сходятся с содержимым.
func TestAgentKitOpenAndVersioned(t *testing.T) {
	for _, version := range []string{"v0.12.0", "8144824", ""} {
		h := instanceServer(t, instanceSetup{mode: "token", tenancy: "single", creation: "teams", version: version})

		rec := get(t, h, "/v1/agent-kit")
		if rec.Code != http.StatusOK {
			t.Fatalf("%q: /v1/agent-kit без входа: %d %s", version, rec.Code, rec.Body)
		}
		var kit agentKitView
		if err := json.Unmarshal(rec.Body.Bytes(), &kit); err != nil {
			t.Fatalf("тело не JSON: %s", rec.Body)
		}
		if inst := decodeInstance(t, get(t, h, "/v1/instance")); kit.Version != inst.Version {
			t.Fatalf("версия набора %q, у инстанса %q", kit.Version, inst.Version)
		}
		if len(kit.Files) == 0 || len(kit.Skills) == 0 {
			t.Fatalf("пустой набор: %s", rec.Body)
		}
		for _, f := range kit.Files {
			if f.Path == "" || path.IsAbs(f.Path) || strings.Contains(f.Path, "..") || path.Clean(f.Path) != f.Path {
				t.Errorf("путь файла %q", f.Path)
			}
			sum := sha256.Sum256([]byte(f.Content))
			if f.SHA256 != hex.EncodeToString(sum[:]) {
				t.Errorf("%s: sha256 не сходится с содержимым", f.Path)
			}
		}
		names := map[string]string{}
		for _, s := range kit.Skills {
			names[s.Name] = s.Description
		}
		for _, want := range []string{"clab-work", "clab-run", "clab-backlog", "clab-status", "clab-rules", "clab-guide"} {
			if names[want] == "" {
				t.Errorf("скила %s нет или без описания", want)
			}
		}
	}
}

// CLAB-417 (AC-9): ответ дописан полями core и bindings; прежние поля на
// месте, у привязок — все девять полей формата 0077 и файлы с суммами.
func TestAgentKitCoreAndBindings(t *testing.T) {
	h := instanceServer(t, instanceSetup{mode: "token", tenancy: "single", creation: "teams", version: "v0.29.0"})
	rec := get(t, h, "/v1/agent-kit")
	if rec.Code != http.StatusOK {
		t.Fatalf("/v1/agent-kit без входа: %d %s", rec.Code, rec.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("тело не JSON: %v", err)
	}
	for _, f := range []string{"version", "files", "skills", "agents", "core", "bindings"} {
		if _, ok := raw[f]; !ok {
			t.Errorf("нет поля %s", f)
		}
	}
	var core struct {
		Skills []struct{ Path, Content, SHA256 string } `json:"skills"`
		Agents []struct{ Path, Content, SHA256 string } `json:"agents"`
		Guide  struct{ Path, Content, SHA256 string }   `json:"guide"`
	}
	if err := json.Unmarshal(raw["core"], &core); err != nil || len(core.Skills) != 6 || len(core.Agents) != 1 || core.Guide.Content == "" {
		t.Fatalf("core: %v, скилов %d, агентов %d", err, len(core.Skills), len(core.Agents))
	}
	var bindings []map[string]json.RawMessage
	if err := json.Unmarshal(raw["bindings"], &bindings); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range bindings {
		var name string
		_ = json.Unmarshal(b["name"], &name)
		names = append(names, name)
		for _, f := range []string{"kit", "mcp", "run", "executor", "hook", "output", "detect", "levels", "tool_policy", "files"} {
			if _, ok := b[f]; !ok {
				t.Errorf("привязка %s: нет поля %s", name, f)
			}
		}
		var files []struct {
			Path    string `json:"path"`
			Content string `json:"content"`
			SHA256  string `json:"sha256"`
		}
		if err := json.Unmarshal(b["files"], &files); err != nil {
			t.Fatalf("%s.files: %v", name, err)
		}
		for _, f := range files {
			sum := sha256.Sum256([]byte(f.Content))
			if f.SHA256 != hex.EncodeToString(sum[:]) || path.IsAbs(f.Path) || strings.Contains(f.Path, "..") {
				t.Errorf("%s: файл %s", name, f.Path)
			}
		}
	}
	if strings.Join(names, ",") != "claude,codex,generic" {
		t.Errorf("привязки %v — ждали три по имени", names)
	}
	if n := rec.Body.Len(); n > 600*1024 {
		t.Errorf("ответ %d байт — больше предела 600 КБ", n)
	}
}

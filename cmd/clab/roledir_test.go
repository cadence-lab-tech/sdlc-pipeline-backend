package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Каталог роли и role put --dir (CLAB-343, решение 0068, п. 5.4). Номера —
// критерии приёмки анализа CLAB-343.

func writeRoleDir(t *testing.T, manifest string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(dir, "role.json"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const threeDocs = `{"key": "writer", "name": "Техписатель", "model_level": "medium",
  "output_contract": [{"kind": "docs.page", "required": true}],
  "documents": [{"name": "zeta.md"}, {"name": "alpha.md", "kind": "reference"}, {"name": "mid.md", "kind": "checklist"}]}`

var threeFiles = map[string]string{"zeta.md": "Главное.", "alpha.md": "Справка.\n", "mid.md": "- проверить"}

// AC-16: порядок role.json, а не алфавитный; поля роли как есть.
func TestReadRoleDirOrder(t *testing.T) {
	files := map[string]string{"notes.txt": "не документ"}
	for k, v := range threeFiles {
		files[k] = v
	}
	role, err := readRoleDir(writeRoleDir(t, threeDocs, files))
	if err != nil {
		t.Fatal(err)
	}
	docs := role["documents"].([]map[string]any)
	want := []string{"zeta.md", "alpha.md", "mid.md"}
	for i, d := range docs {
		if d["name"] != want[i] || d["content"] != threeFiles[want[i]] {
			t.Fatalf("документ %d: %v", i, d)
		}
	}
	if _, has := docs[0]["kind"]; has || docs[1]["kind"] != "reference" {
		t.Fatalf("типы: %v", docs)
	}
	if roleFieldString(role, "key") != "writer" || roleFieldString(role, "model_level") != "medium" {
		t.Fatalf("поля роли: %v", role)
	}
	if _, has := role["output_contract"]; !has {
		t.Fatal("output_contract потерян")
	}
}

// AC-16: лишний файл, строка без файла, имя за пределами каталога, пустой
// набор, instructions в role.json — ошибка с именем файла.
func TestReadRoleDirRejects(t *testing.T) {
	withDraft := map[string]string{"draft.md": "черновик"}
	for k, v := range threeFiles {
		withDraft[k] = v
	}
	missing := map[string]string{"zeta.md": "x", "alpha.md": "y"}
	cases := []struct {
		name, manifest string
		files          map[string]string
		want           string
	}{
		{"лишний файл", threeDocs, withDraft, "draft.md"},
		{"строка без файла", threeDocs, missing, "mid.md"},
		{"нет role.json", "", threeFiles, "role.json"},
		{"выход за каталог", `{"key":"w","documents":[{"name":"../x.md"}]}`, nil, `"../x.md"`},
		{"подкаталог", `{"key":"w","documents":[{"name":"a/b.md"}]}`, nil, `"a/b.md"`},
		{"дубль", `{"key":"w","documents":[{"name":"a.md"},{"name":"a.md"}]}`, map[string]string{"a.md": "x"}, "дважды"},
		{"пустой набор", `{"key":"w","documents":[]}`, nil, "нет ни одного документа"},
		{"instructions", `{"key":"w","instructions":"x","documents":[{"name":"a.md"}]}`, map[string]string{"a.md": "x"}, "instructions"},
		{"не JSON", `{"key":`, nil, "не разбирается"},
	}
	for _, tc := range cases {
		_, err := readRoleDir(writeRoleDir(t, tc.manifest, tc.files))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, ждали %q", tc.name, err, tc.want)
		}
	}
}

// AC-16: role put --dir шлёт связку формата 3 с документами по порядку;
// ошибка каталога и --dir с --file — без запроса; --file — формат 1 со
// строкой instructions.
func TestRolePutDir(t *testing.T) {
	withJSON(t, false)
	c, rec := newInviteClient(t, http.StatusOK, `{"roles":[{"key":"writer","outcome":"created"}]}`)
	dir := writeRoleDir(t, threeDocs, threeFiles)
	if _, _, err := capture(t, func() error {
		return cmdRole(context.Background(), c, []string{"put", "--project", "CLAB", "--dir", dir, "--changelog", "из каталога"})
	}); err != nil {
		t.Fatal(err)
	}
	if rec.method != "POST" || rec.path != "/v1/projects/CLAB/import" || rec.body["version"] != float64(3) {
		t.Fatalf("%s %s %v", rec.method, rec.path, rec.body["version"])
	}
	role := rec.body["roles"].([]any)[0].(map[string]any)
	if role["key"] != "writer" || role["changelog"] != "из каталога" || role["model_level"] != "medium" {
		t.Fatalf("поля роли: %v", role)
	}
	if _, has := role["instructions"]; has {
		t.Fatalf("instructions рядом с documents: %v", role)
	}
	docs := role["documents"].([]any)
	if len(docs) != 3 || docs[0].(map[string]any)["name"] != "zeta.md" || docs[2].(map[string]any)["kind"] != "checklist" {
		t.Fatalf("документы: %v", docs)
	}

	// Ошибки — до запроса.
	bad := writeRoleDir(t, threeDocs, map[string]string{"zeta.md": "x", "alpha.md": "y", "mid.md": "z", "draft.md": "w"})
	file := filepath.Join(t.TempDir(), "main.md")
	if err := os.WriteFile(file, []byte("Одна строка."), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"лишний файл":    {"put", "--project", "CLAB", "--dir", bad},
		"--dir и --file": {"put", "--project", "CLAB", "--dir", dir, "--file", file},
	} {
		c, rec := newInviteClient(t, http.StatusOK, `{}`)
		if _, _, err := capture(t, func() error { return cmdRole(context.Background(), c, args) }); err == nil {
			t.Fatalf("%s: ошибки нет", name)
		}
		if rec.calls != 0 {
			t.Fatalf("%s: запрос ушёл на сервер", name)
		}
	}

	c, rec = newInviteClient(t, http.StatusOK, `{"roles":[{"key":"writer","outcome":"new_version"}]}`)
	if _, _, err := capture(t, func() error {
		return cmdRole(context.Background(), c, []string{"put", "--project", "CLAB", "--key", "writer", "--file", file})
	}); err != nil {
		t.Fatal(err)
	}
	role = rec.body["roles"].([]any)[0].(map[string]any)
	if rec.body["version"] != float64(1) || role["instructions"] != "Одна строка." || role["documents"] != nil {
		t.Fatalf("--file: %v", rec.body)
	}
}

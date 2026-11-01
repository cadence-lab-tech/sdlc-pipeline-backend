package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// CLAB-348: предупреждения отбора документов на статусе в clab claim и
// clab show; --instructions-dir пишет ровно то, что отдал сервер.
// Номера — критерии приёмки анализа CLAB-348.

// AC-348.8: строки «Отбор:» в сводке claim и show; без ключа — ни одной.
func TestClaimPrintsDocumentWarnings(t *testing.T) {
	withJSON(t, false)
	warnings := []any{
		"документ gone.md из отбора на статусе не найден в версии роли developer v13 — пропущен",
		"документ old.md из отбора на статусе не найден в версии роли developer v13 — пропущен",
	}
	picked := claimDocs[:2]
	reply := withDocs(t, picked, func(role map[string]any) { role["document_warnings"] = warnings })
	out, _, err, _ := runClaim(t, reply)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range warnings {
		if line := lineWith(out, w.(string)); !strings.HasPrefix(strings.TrimSpace(line), "Отбор:") {
			t.Fatalf("claim: нет строки предупреждения %q:\n%s", w, out)
		}
	}
	if i, r := strings.Index(out, "Отбор:"), strings.Index(out, "Роль:"); i < r {
		t.Fatalf("предупреждения выше строки роли:\n%s", out)
	}
	if show := runShow(t, reply); !strings.Contains(show, warnings[0].(string)) {
		t.Fatalf("show без предупреждения:\n%s", show)
	}

	out, _, err, _ = runClaim(t, withDocs(t, picked, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Отбор:") {
		t.Fatalf("строка «Отбор:» без предупреждений:\n%s", out)
	}

	// --instructions-dir — только отобранные документы ответа.
	dir := filepath.Join(t.TempDir(), "role")
	if _, _, err, _ := runClaim(t, reply, "--instructions-dir", dir); err != nil {
		t.Fatal(err)
	}
	files := readDir(t, dir)
	if len(files) != len(picked) {
		t.Fatalf("в каталоге %d файлов, ждали %d: %v", len(files), len(picked), files)
	}
	for _, d := range picked {
		if files[d.name] != d.content {
			t.Fatalf("файл %s не совпадает с документом", d.name)
		}
	}
}

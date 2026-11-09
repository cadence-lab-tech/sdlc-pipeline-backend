package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Настройка агентов в каталоге связки и в импорте CLI (CLAB-428). Номера AC —
// критерии приёмки аналитики задачи.

// exportWithSettings — экспорт формата 3 с активной настройкой агентов:
// числа записаны как 1.0 и 0.10, в changelog и модели стоят <, > и &.
const exportWithSettings = `{
  "version": 3,
  "project": {"key": "CLAB", "name": "Cadence Lab"},
  "roles": [{"key": "analyst", "name": "Аналитик", "documents": [{"name": "main.md", "kind": "instructions", "content": "Требования."}]}],
  "agent_settings": {
    "document": {
      "pricing": {"as_of": "2026-11-01", "models": {"claude-opus-5-5": {"input": 1.0, "output": 0.10}}},
      "agents": {"claude": {"levels": {"high": {"model": "claude-opus-5-5", "effort": "high"}}, "note": "a<b>&c"}}
    },
    "changelog": "цены <ноябрь> & модели"
  }
}`

func settingsFilePath(dir string) string { return filepath.Join(dir, bundleDirAgentSettings) }

// AC-4: файл рядом с bundle.json, объект {document, changelog}, отступ в два
// пробела, перевод строки в конце, без экранирования; в bundle.json поля нет;
// два экспорта — одни байты; без активной версии файла нет.
func TestExportDirAgentSettingsFile(t *testing.T) {
	withJSON(t, false)
	c, _ := newBundleStub(t, exportWithSettings)
	dir := filepath.Join(t.TempDir(), "rules")
	if err := exportDir(t, c, dir); err != nil {
		t.Fatal(err)
	}
	text := dirFileText(t, settingsFilePath(dir))
	if !strings.HasPrefix(text, "{\n  \"changelog\": ") || !strings.HasSuffix(text, "}\n") ||
		!strings.Contains(text, "\n    \"agents\": {") {
		t.Fatalf("раскладка файла:\n%s", text)
	}
	if !strings.Contains(text, "a<b>&c") || !strings.Contains(text, "цены <ноябрь> & модели") || strings.Contains(text, "\\u003c") {
		t.Fatalf("файл экранирован:\n%s", text)
	}
	if !strings.Contains(text, `"input": 1.0`) {
		t.Fatalf("запись числа изменена (файл — прозрачная копия):\n%s", text)
	}
	var file struct {
		Document  map[string]any `json:"document"`
		Changelog string         `json:"changelog"`
	}
	if err := json.Unmarshal([]byte(text), &file); err != nil || file.Document == nil || file.Changelog != "цены <ноябрь> & модели" {
		t.Fatalf("файл не разбирается: %v %s", err, text)
	}
	if manifest := dirFileText(t, filepath.Join(dir, "bundle.json")); strings.Contains(manifest, "agent_settings") {
		t.Fatalf("в bundle.json осталось agent_settings:\n%s", manifest)
	}

	dir2 := filepath.Join(t.TempDir(), "again")
	if err := exportDir(t, c, dir2); err != nil {
		t.Fatal(err)
	}
	if a, b := treeOf(t, dir), treeOf(t, dir2); !reflect.DeepEqual(a, b) || a[bundleDirAgentSettings] == "" {
		t.Fatalf("деревья разошлись или файла нет:\n%v\n%v", a, b)
	}

	// Без активной версии файла нет.
	plain, _ := newBundleStub(t, exportFormat3)
	dir3 := filepath.Join(t.TempDir(), "plain")
	if err := exportDir(t, plain, dir3); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(settingsFilePath(dir3)); !os.IsNotExist(err) {
		t.Fatalf("файл настройки без настройки: %v", err)
	}
}

// AC-5: --force заменяет файл или удаляет устаревший, прочие файлы целы;
// без --force в непустой каталог — отказ, текст называет agent-settings.json.
func TestExportDirAgentSettingsForce(t *testing.T) {
	withJSON(t, false)
	with, _ := newBundleStub(t, exportWithSettings)
	without, _ := newBundleStub(t, exportFormat3)
	dir := t.TempDir()
	if err := exportDir(t, with, dir); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("заметки"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := treeOf(t, dir)

	err := exportDir(t, with, dir)
	if err == nil || !strings.Contains(err.Error(), "agent-settings.json") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("непустой каталог без --force: %v", err)
	}
	if !reflect.DeepEqual(before, treeOf(t, dir)) {
		t.Fatal("каталог изменился без --force")
	}

	// Файл устарел вручную: --force заменяет его целиком.
	if err := os.WriteFile(settingsFilePath(dir), []byte(`{"document":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := exportDir(t, with, dir, "--force"); err != nil {
		t.Fatal(err)
	}
	if after := treeOf(t, dir); after[bundleDirAgentSettings] != before[bundleDirAgentSettings] || after["notes.txt"] != before["notes.txt"] {
		t.Fatalf("замена файла или посторонний файл: %v", after)
	}

	// Проект больше без настройки: прежний файл удаляется, иначе строгий
	// импорт вернул бы устаревшую настройку.
	if err := exportDir(t, without, dir, "--force"); err != nil {
		t.Fatal(err)
	}
	after := treeOf(t, dir)
	if _, ok := after[bundleDirAgentSettings]; ok {
		t.Fatal("устаревший agent-settings.json остался")
	}
	if after["notes.txt"] != before["notes.txt"] {
		t.Fatal("посторонний файл тронут")
	}
	if _, ok := after["bundle.json"]; !ok {
		t.Fatal("bundle.json исчез")
	}
}

func agentSettingsSent(t *testing.T, st *bundleStub, i int) map[string]any {
	t.Helper()
	raw, ok := st.imports[i].body["agent_settings"]
	if !ok {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// AC-6: импорт каталога отправляет файл как agent_settings; каталог прежнего
// clab (поле в bundle.json) принимается; ошибки файла — до запроса.
func TestImportDirAgentSettings(t *testing.T) {
	withJSON(t, false)
	c, st := newBundleStub(t, exportWithSettings)
	dir := t.TempDir()
	if err := exportDir(t, c, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", dir, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	sent := st.imports[0].body["agent_settings"]
	if !strings.Contains(string(sent), `"input":1.0`) && !strings.Contains(string(sent), `"input": 1.0`) {
		t.Fatalf("запись числа потеряна по дороге: %s", sent)
	}
	got := agentSettingsSent(t, st, 0)
	var want map[string]any
	_ = json.Unmarshal([]byte(dirFileText(t, settingsFilePath(dir))), &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("на сервер ушло не то, что в файле:\n%v\n%v", got, want)
	}
	if _, in := st.imports[0].body["roles"]; !in {
		t.Fatal("роли не ушли")
	}

	// Каталог прежнего clab: поле внутри bundle.json, файла нет.
	old := t.TempDir()
	manifest := `{"version": 3, "agent_settings": {"document": {"agents": {}}, "changelog": "старый clab"}}`
	if err := os.WriteFile(filepath.Join(old, "bundle.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", old); err != nil {
		t.Fatalf("каталог прежнего clab: %v", err)
	}
	if as := agentSettingsSent(t, st, 1); as == nil || as["changelog"] != "старый clab" {
		t.Fatalf("поле из bundle.json не дошло: %v", as)
	}

	// Каталог только с файлом настройки — каталог связки.
	only := t.TempDir()
	if err := os.WriteFile(settingsFilePath(only), []byte(`{"document": {"agents": {}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := importDir(t, c, "--project", "CLAB", "--dir", only); err != nil {
		t.Fatalf("только agent-settings.json: %v", err)
	}
	if as := agentSettingsSent(t, st, 2); as == nil {
		t.Fatal("настройка не ушла")
	}

	calls := st.calls
	secondFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(secondFile, "bundle.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsFilePath(secondFile), []byte(`{"document": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"document": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := t.TempDir()
	if err := os.WriteFile(filepath.Join(link, "bundle.json"), []byte(`{"version": 3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, settingsFilePath(link)); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		setup func(d string)
		dir   string
		want  string
	}{
		"не JSON": {setup: func(d string) { _ = os.WriteFile(settingsFilePath(d), []byte(`{document`), 0o644) }, want: "не разбирается"},
		"нет document": {setup: func(d string) { _ = os.WriteFile(settingsFilePath(d), []byte(`{"changelog": "x"}`), 0o644) },
			want: "нет поля document"},
		"document не объект": {setup: func(d string) { _ = os.WriteFile(settingsFilePath(d), []byte(`{"document": [1]}`), 0o644) },
			want: "не JSON-объект"},
		"changelog не строка": {setup: func(d string) {
			_ = os.WriteFile(settingsFilePath(d), []byte(`{"document": {}, "changelog": 5}`), 0o644)
		},
			want: "changelog"},
		"и в bundle.json, и в файле": {dir: secondFile, want: "и в"},
		"символьная ссылка":          {dir: link, want: "символьная ссылка"},
	} {
		d := tc.dir
		if d == "" {
			d = t.TempDir()
			if err := os.WriteFile(filepath.Join(d, "bundle.json"), []byte(`{"version": 3}`), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.setup(d)
		}
		_, err := importDir(t, c, "--project", "CLAB", "--dir", d)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, ждали %q", name, err, tc.want)
		}
	}
	if st.calls != calls {
		t.Fatalf("ошибка каталога дошла до сервера: запросов было %d, стало %d", calls, st.calls)
	}
}

// AC-16: сервер прежней версии теряет agent_settings — отчёт без строки; CLI
// предупреждает (stderr, код 0, warnings в отчёте) по каждому проекту.
func TestImportWarnsWhenServerLostAgentSettings(t *testing.T) {
	c, st := newBundleStub(t, exportWithSettings)
	dir := t.TempDir()
	withJSON(t, false)
	if err := exportDir(t, c, dir); err != nil {
		t.Fatal(err)
	}

	// Один проект, без --json: предупреждение на stderr, отчёт на stdout.
	out, errOut, err := capture(t, func() error {
		return cmdImport(context.Background(), c, []string{"--project", "CLAB", "--dir", dir})
	})
	if err != nil {
		t.Fatalf("код выхода должен быть 0: %v", err)
	}
	if !strings.Contains(errOut, "сервер не применил настройку агентов — обновите сервер") || !strings.Contains(errOut, "CLAB") {
		t.Fatalf("нет предупреждения на stderr: %q", errOut)
	}
	if !strings.Contains(out, `"warnings"`) {
		t.Fatalf("нет warnings в отчёте: %s", out)
	}

	// --json: stderr чист, warnings в отчёте проекта.
	withJSON(t, true)
	out, errOut, err = capture(t, func() error {
		return cmdImport(context.Background(), c, []string{"--project", "CLAB", "--dir", dir})
	})
	var rep struct {
		Warnings []string `json:"warnings"`
	}
	if err != nil || errOut != "" || json.Unmarshal([]byte(out), &rep) != nil || len(rep.Warnings) != 1 {
		t.Fatalf("--json: %v, stderr %q, отчёт %s", err, errOut, out)
	}

	// Новый сервер: строка в отчёте есть — предупреждения нет.
	withJSON(t, false)
	st.reply = `{"dry_run":false,"roles":[],"documents":[],"repositories":[],"agent_settings":{"key":"agent_settings","outcome":"draft_created","detail":"версия 1"}}`
	out, errOut, err = capture(t, func() error {
		return cmdImport(context.Background(), c, []string{"--project", "CLAB", "--dir", dir})
	})
	if err != nil || errOut != "" || strings.Contains(out, "warnings") {
		t.Fatalf("новый сервер: %v, stderr %q, отчёт %s", err, errOut, out)
	}

	// Связка без поля: предупреждения нет, как и раньше.
	st.reply = ""
	plain, _ := newBundleStub(t, exportFormat3)
	plainDir := t.TempDir()
	if err := exportDir(t, plain, plainDir); err != nil {
		t.Fatal(err)
	}
	out, errOut, err = capture(t, func() error {
		return cmdImport(context.Background(), c, []string{"--project", "CLAB", "--dir", plainDir})
	})
	if err != nil || errOut != "" || strings.Contains(out, "warnings") {
		t.Fatalf("связка без настройки: %v, stderr %q, отчёт %s", err, errOut, out)
	}

	// --to двух проектов: предупреждение по каждому, у проекта с ошибкой — нет
	// (отчёта нет); раздача не обрывается.
	st.failing["BAD"] = true
	out, errOut, err = capture(t, func() error {
		return cmdImport(context.Background(), c, []string{"--dir", dir, "--to", "AAA,BAD,BBB"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(errOut, "сервер не применил настройку агентов") != 2 ||
		!strings.Contains(errOut, "AAA") || !strings.Contains(errOut, "BBB") || strings.Contains(errOut, "BAD") {
		t.Fatalf("раздача: %q", errOut)
	}
	var multi struct {
		Imports []struct {
			Project  string   `json:"project"`
			Error    string   `json:"error"`
			Warnings []string `json:"warnings"`
		} `json:"imports"`
	}
	if err := json.Unmarshal([]byte(out), &multi); err != nil || len(multi.Imports) != 3 {
		t.Fatalf("отчёт раздачи: %v %s", err, out)
	}
	for _, r := range multi.Imports {
		if (r.Project == "BAD") != (r.Error != "") || (r.Project != "BAD") != (len(r.Warnings) == 1) {
			t.Fatalf("запись %+v", r)
		}
	}
}

// AC-6/AC-16 на настоящем сервере: круг export → import через каталог даёт
// unchanged; правка файла — черновик, повтор — unchanged, активная не
// меняется; каталог прежнего clab ведёт себя так же.
func TestBundleDirAgentSettingsOnServer(t *testing.T) {
	withJSON(t, false)
	w := newLeaseWorld(t, time.Minute)
	w.call("POST", "/v1/projects", leaseRootToken, map[string]any{"key": "AGS", "name": "Настройка агентов"}, http.StatusCreated)
	c := w.client(leaseRootToken)
	ctx := context.Background()

	doc := map[string]any{"agents": map[string]any{"claude": map[string]any{
		"levels": map[string]any{"high": map[string]any{"model": "claude-opus-5-5"}}}}}
	w.call("POST", "/v1/projects/AGS/agent-settings/draft", leaseRootToken, map[string]any{"document": doc, "changelog": "<первая> & версия"}, http.StatusCreated)
	w.call("POST", "/v1/projects/AGS/agent-settings/draft/activate", leaseRootToken, nil, http.StatusOK)

	dir := t.TempDir()
	if _, _, err := capture(t, func() error {
		return cmdExport(ctx, c, []string{"--project", "AGS", "--dir", dir})
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dirFileText(t, settingsFilePath(dir)), "<первая> & версия") {
		t.Fatalf("файл настройки:\n%s", dirFileText(t, settingsFilePath(dir)))
	}

	run := func(args ...string) map[string]any {
		t.Helper()
		out, _, err := capture(t, func() error { return cmdImport(ctx, c, append([]string{"--project", "AGS"}, args...)) })
		if err != nil {
			t.Fatalf("import %v: %v", args, err)
		}
		var r map[string]any
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("отчёт не JSON: %s", out)
		}
		return r
	}
	outcome := func(r map[string]any) string {
		e, _ := r["agent_settings"].(map[string]any)
		s, _ := e["outcome"].(string)
		return s
	}
	rows := func() int {
		var n int
		if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM agent_settings_versions`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	r := run("--dir", dir)
	if outcome(r) != "unchanged" || r["warnings"] != nil || rows() != 1 {
		t.Fatalf("круг: %v, строк %d", r, rows())
	}

	// Правка файла — черновик; сухой прогон ничего не пишет.
	edited := strings.Replace(dirFileText(t, settingsFilePath(dir)), "claude-opus-5-5", "claude-opus-5-6", 1)
	if err := os.WriteFile(settingsFilePath(dir), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := run("--dir", dir, "--dry-run"); outcome(r) != "draft_created" || rows() != 1 {
		t.Fatalf("сухой прогон: %v, строк %d", r, rows())
	}
	if r := run("--dir", dir); outcome(r) != "draft_created" || rows() != 2 {
		t.Fatalf("правка: %v, строк %d", r, rows())
	}
	if r := run("--dir", dir); outcome(r) != "unchanged" || rows() != 2 {
		t.Fatalf("повтор: %v, строк %d", r, rows())
	}
	var state string
	if err := w.store.DB().QueryRow(ctx, `SELECT state FROM agent_settings_versions WHERE version = 1`).Scan(&state); err != nil || state != "active" {
		t.Fatalf("активная версия изменилась: %s %v", state, err)
	}

	// Раздача: --from проекта AGS в второй проект — через тот же путь.
	w.call("POST", "/v1/projects", leaseRootToken, map[string]any{"key": "AG2", "name": "Второй"}, http.StatusCreated)
	out, _, err := capture(t, func() error {
		return cmdImport(ctx, c, []string{"--from", "AGS", "--to", "AG2,AGS"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var multi struct {
		Imports []struct {
			Project       string `json:"project"`
			AgentSettings struct {
				Outcome string `json:"outcome"`
			} `json:"agent_settings"`
		} `json:"imports"`
	}
	if err := json.Unmarshal([]byte(out), &multi); err != nil || len(multi.Imports) != 2 {
		t.Fatalf("раздача: %v %s", err, out)
	}
	if multi.Imports[0].AgentSettings.Outcome != "draft_created" || multi.Imports[1].AgentSettings.Outcome != "unchanged" {
		t.Fatalf("исходы раздачи: %+v", multi.Imports)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Каталог роли (решение 0068, п. 5.4) — формат CLI, а не API: на сервер
// уходит роль связки формата 3 с documents. Его читают `clab role put --dir`
// и (CLAB-345) `clab import --dir` для каждого roles/<ключ роли>/.
//
//	D/role.json   — поля роли связки без содержимого документов:
//	                {"key", "name", "summary", "input_kinds", "output_contract",
//	                 "tool_policy", "model_level", "repo_scope", "context_budget",
//	                 "changelog", …, "documents": [{"name": "main.md", "kind": "instructions"}, …]}
//	D/<имя>.md    — содержимое документа <имя>.md
//
// Порядок и тип задаёт role.json: в каталоге порядка нет. Сверка строгая в
// обе стороны: .md без строки в role.json (черновик, бэкап редактора) и
// строка без файла — ошибка с именем файла, на сервер ничего не уходит.
// Файлы не на .md (role.json, .DS_Store) документами не считаются.
//
// Каталог мог прийти из чужого репозитория или архива, поэтому читается
// только описанное (CLAB-345): неизвестное поле role.json — ошибка (опечатка
// в model_levl молча потеряла бы уровень), подкаталог — ошибка, символьная
// ссылка и не-обычный файл не читаются (иначе каталог вынес бы на сервер
// произвольный локальный файл).

// roleDirManifest — имя файла с полями роли в каталоге роли.
const roleDirManifest = "role.json"

// roleDirName — форма имени документа, та же, что у сервера (CHECK в
// 000038): здесь она ещё и не даёт строке role.json выйти за каталог
// («../x.md», «a/b.md»).
var roleDirName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,60}\.md$`)

// roleDirDocument — строка documents в role.json.
type roleDirDocument struct {
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"`
	// Instruction — ключ инструкции библиотеки, за версией которой
	// закреплён документ (решение 0069); файл <ключ>.md роли остаётся, и
	// сервер находит версию по его sha.
	Instruction string `json:"instruction,omitempty"`
}

// roleManifest — role.json: все поля роли связки (workflow.BundleRole), кроме
// instructions и содержимого документов. Порядок полей — порядок в файле,
// который пишет clab export --dir. Поле, которого здесь нет, экспорт
// каталогом потерял бы, и перенос перестал бы быть идемпотентным, поэтому
// набор сверяет с workflow.BundleRole тест (TestRoleManifestCoversBundleRole).
// Поля-JSON — any, а не json.RawMessage: так экспорт пишет их с тем же
// отступом и без экранирования <, > и &, а сервер сравнивает их по смыслу.
type roleManifest struct {
	Key            string            `json:"key"`
	Name           string            `json:"name"`
	Summary        string            `json:"summary,omitempty"`
	InputKinds     []string          `json:"input_kinds,omitempty"`
	OutputContract any               `json:"output_contract,omitempty"`
	ToolPolicy     any               `json:"tool_policy,omitempty"`
	ModelHint      string            `json:"model_hint,omitempty"`
	ModelLevel     string            `json:"model_level,omitempty"`
	RepoScope      []string          `json:"repo_scope,omitempty"`
	ContextBudget  any               `json:"context_budget,omitempty"`
	Changelog      string            `json:"changelog,omitempty"`
	Documents      []roleDirDocument `json:"documents"`
}

// roleManifestFields — имена полей role.json по порядку.
var roleManifestFields = func() []string {
	t := reflect.TypeFor[roleManifest]()
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		out = append(out, name)
	}
	return out
}()

// knownRoleField — поле есть в role.json.
func knownRoleField(name string) bool {
	for _, f := range roleManifestFields {
		if f == name {
			return true
		}
	}
	return false
}

// readRegularFile читает обычный файл и ничего другого: символьная ссылка,
// каталог, устройство или FIFO — ошибка без чтения. Сверка Lstat с Stat
// открытого файла закрывает подмену файла ссылкой между проверкой и чтением.
func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		what := "не обычный файл"
		if info.Mode()&os.ModeSymlink != 0 {
			what = "символьная ссылка"
		}
		return nil, fmt.Errorf("%s — %s: такие файлы не читаются, положите на его место сам файл", path, what)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s подменили во время чтения", path)
	}
	return io.ReadAll(f)
}

// readRoleDir читает каталог роли и отдаёт роль связки формата 3: поля
// role.json как есть и documents с содержимым в порядке role.json.
func readRoleDir(dir string) (map[string]any, error) {
	manifestPath := filepath.Join(dir, roleDirManifest)
	raw, err := readRegularFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("каталог роли %s: %w", dir, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("%s не разбирается как объект JSON: %w", manifestPath, err)
	}
	if _, ok := fields["instructions"]; ok {
		return nil, fmt.Errorf("%s: instructions в каталоге роли не бывает — текст роли лежит документами: "+
			"файлами <имя>.md, а их порядок задаёт documents", manifestPath)
	}
	var unknown []string
	for k := range fields {
		if !knownRoleField(k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: неизвестное поле %s — в role.json бывают только %s",
			manifestPath, strings.Join(unknown, ", "), strings.Join(roleManifestFields, ", "))
	}
	var docs []roleDirDocument
	if rawDocs, ok := fields["documents"]; ok {
		// Строка с content или опечаткой в kind не должна пройти молча:
		// содержимое лежит файлом, а тип пропал бы.
		dec := json.NewDecoder(bytes.NewReader(rawDocs))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&docs); err != nil {
			return nil, fmt.Errorf("%s: documents — список [{\"name\": \"main.md\", \"kind\": \"instructions\", \"instruction\": \"<ключ>\"}]: %w",
				manifestPath, err)
		}
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("%s: в documents нет ни одного документа", manifestPath)
	}

	listed := map[string]bool{}
	for i, d := range docs {
		if !roleDirName.MatchString(d.Name) {
			return nil, fmt.Errorf("%s: documents[%d] %q — имя вида main.md: строчные латинские буквы, "+
				"цифры, «.», «_», «-», не длиннее 64 символов", manifestPath, i, d.Name)
		}
		if listed[d.Name] {
			return nil, fmt.Errorf("%s: документ %q указан дважды", manifestPath, d.Name)
		}
		listed[d.Name] = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("каталог роли %s: %w", dir, err)
	}
	present := map[string]bool{}
	var extra []string
	for _, e := range entries {
		if e.IsDir() {
			return nil, fmt.Errorf("подкаталог %s в каталоге роли: документы лежат прямо в нём, вложенных каталогов не бывает",
				filepath.Join(dir, e.Name()))
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("%s — символьная ссылка или не обычный файл: такие файлы не читаются",
				filepath.Join(dir, e.Name()))
		}
		present[e.Name()] = true
		if !listed[e.Name()] {
			extra = append(extra, filepath.Join(dir, e.Name()))
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("файл без строки в %s: %s — допишите его в documents или уберите из каталога",
			roleDirManifest, strings.Join(extra, ", "))
	}

	out := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		path := filepath.Join(dir, d.Name)
		if !present[d.Name] {
			return nil, fmt.Errorf("%s: документ %q указан, а файла %s нет", manifestPath, d.Name, path)
		}
		content, err := readRegularFile(path)
		if err != nil {
			return nil, fmt.Errorf("документ %s: %w", path, err)
		}
		// JSON невалидный UTF-8 не несёт: кодировщик молча заменил бы такие
		// байты на U+FFFD, и сервер принял бы испорченный текст новой версией.
		if !utf8.Valid(content) {
			return nil, fmt.Errorf("документ %s: содержимое — не UTF-8", path)
		}
		doc := map[string]any{"name": d.Name, "content": string(content)}
		if d.Kind != "" {
			doc["kind"] = d.Kind
		}
		if d.Instruction != "" {
			doc["instruction"] = d.Instruction
		}
		out = append(out, doc)
	}

	role := map[string]any{}
	for k, v := range fields {
		if k == "documents" {
			continue
		}
		role[k] = v
	}
	role["documents"] = out
	return role, nil
}

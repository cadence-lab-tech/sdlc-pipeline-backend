package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
}

// readRoleDir читает каталог роли и отдаёт роль связки формата 3: поля
// role.json как есть и documents с содержимым в порядке role.json.
func readRoleDir(dir string) (map[string]any, error) {
	manifestPath := filepath.Join(dir, roleDirManifest)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("каталог роли %s: %w", dir, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("%s не разбирается как объект JSON: %w", manifestPath, err)
	}
	if _, ok := fields["instructions"]; ok {
		return nil, fmt.Errorf("%s: instructions в каталоге роли не бывает — содержимое лежит файлами <имя>.md, "+
			"а их порядок задаёт documents", manifestPath)
	}
	var docs []roleDirDocument
	if rawDocs, ok := fields["documents"]; ok {
		if err := json.Unmarshal(rawDocs, &docs); err != nil {
			return nil, fmt.Errorf("%s: documents — список [{\"name\": \"main.md\", \"kind\": \"instructions\"}]: %w",
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
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
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
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("документ %s: %w", path, err)
		}
		doc := map[string]any{"name": d.Name, "content": string(content)}
		if d.Kind != "" {
			doc["kind"] = d.Kind
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

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Каталог связки (CLAB-345, решение 0068, п. 5.4) — формат CLI, а не API:
// на сервер уходит та же JSON-связка, что у --file, поэтому клиент и сервер
// разных версий работают так же, как с JSON.
//
//	D/bundle.json                  связка без ролей: version, project,
//	                               repositories, documents, workflows
//	D/roles/<ключ роли>/role.json  поля роли без содержимого документов
//	D/roles/<ключ роли>/<имя>.md   содержимое документа, байт в байт
//
// bundle.json — прозрачная копия ответа сервера без roles: CLI не разбирает
// цикл и документы проекта по типам, и поле, которого он ещё не знает,
// проходит круг экспорт → импорт нетронутым. Роли читает читатель каталога
// роли (readRoleDir) — тот же, что у clab role put --dir.

const (
	bundleDirManifest = "bundle.json"
	bundleDirRoles    = "roles"
	// bundleDirFormat — формат связки, с которого роль несёт documents:
	// каталог роли без них не выражается.
	bundleDirFormat = 3
)

// bundleDirTopOrder — порядок ключей bundle.json; остальные идут за ними по
// алфавиту.
var bundleDirTopOrder = []string{"version", "project", "repositories", "documents", "workflows"}

// bundleDirEntry — файл каталога связки, собранный в памяти.
type bundleDirEntry struct {
	path string // относительно корня каталога, через «/»
	body []byte
}

// bundleDirPlan — каталог связки целиком, до первой записи на диск.
type bundleDirPlan struct {
	files     []bundleDirEntry
	roles     int
	documents int
}

// planBundleDir раскладывает ответ экспорта по файлам. Всё проверяется
// здесь, до записи: ошибка не оставляет на диске половины каталога.
func planBundleDir(raw []byte) (bundleDirPlan, error) {
	var plan bundleDirPlan
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return plan, fmt.Errorf("ответ экспорта не разбирается как объект JSON: %w", err)
	}
	version, err := bundleVersion(top["version"])
	if err != nil {
		return plan, err
	}
	if version < bundleDirFormat {
		return plan, errOldBundleFormat(version)
	}

	rawRoles, ok := top["roles"].([]any)
	if !ok && top["roles"] != nil {
		return plan, fmt.Errorf("ответ экспорта: roles — не список")
	}
	delete(top, "roles")
	type plannedRole struct {
		key      string
		manifest roleManifest
		docs     []bundleDirEntry
	}
	roles := make([]plannedRole, 0, len(rawRoles))
	for i, item := range rawRoles {
		fields, ok := item.(map[string]any)
		if !ok {
			return plan, fmt.Errorf("ответ экспорта: roles[%d] — не объект", i)
		}
		key, _ := fields["key"].(string)
		if _, has := fields["instructions"]; has {
			return plan, errOldBundleFormat(version)
		}
		var unknown []string
		for k := range fields {
			if !knownRoleField(k) {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return plan, fmt.Errorf("роль %s: сервер отдал поле %s, которого этот clab не знает — каталог его потерял бы; "+
				"обновите clab или выгрузите JSON (export без --dir)", key, strings.Join(unknown, ", "))
		}
		manifest, docs, err := planRole(key, fields)
		if err != nil {
			return plan, err
		}
		roles = append(roles, plannedRole{key: key, manifest: manifest, docs: docs})
	}

	keys := make([]string, len(roles))
	for i, r := range roles {
		keys[i] = r.key
	}
	if err := checkRoleDirKeys(keys); err != nil {
		return plan, err
	}
	sort.Slice(roles, func(i, j int) bool { return roles[i].key < roles[j].key })

	body, err := encodeBundleTop(top)
	if err != nil {
		return plan, err
	}
	plan.files = append(plan.files, bundleDirEntry{path: bundleDirManifest, body: body})
	for _, r := range roles {
		manifest, err := encodeIndented(r.manifest)
		if err != nil {
			return plan, fmt.Errorf("роль %s: %w", r.key, err)
		}
		plan.files = append(plan.files, bundleDirEntry{path: bundleDirRoles + "/" + r.key + "/" + roleDirManifest, body: manifest})
		plan.files = append(plan.files, r.docs...)
		plan.documents += len(r.docs)
	}
	plan.roles = len(roles)
	return plan, nil
}

// planRole делит роль экспорта на role.json и файлы документов.
func planRole(key string, fields map[string]any) (roleManifest, []bundleDirEntry, error) {
	var manifest roleManifest
	rawDocs, _ := fields["documents"].([]any)
	if len(rawDocs) == 0 {
		// Так бывает у версии, заведённой в обход сервиса (сид демо-проекта):
		// каталог роли без документов не выражается, а молча пропущенная роль
		// хуже отказа.
		return manifest, nil, fmt.Errorf("роль %s: у последней версии нет ни одного документа — каталог её не выразит; "+
			"заведите версию с документами (clab role put) или выгрузите JSON (export без --dir)", key)
	}
	rest := map[string]any{}
	for k, v := range fields {
		if k != "documents" {
			rest[k] = v
		}
	}
	encoded, err := json.Marshal(rest)
	if err != nil {
		return manifest, nil, fmt.Errorf("роль %s: %w", key, err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	if err := dec.Decode(&manifest); err != nil {
		return manifest, nil, fmt.Errorf("роль %s: поля роли не разбираются: %w", key, err)
	}

	seen := map[string]bool{}
	docs := make([]bundleDirEntry, 0, len(rawDocs))
	manifest.Documents = make([]roleDirDocument, 0, len(rawDocs))
	for i, item := range rawDocs {
		d, ok := item.(map[string]any)
		if !ok {
			return manifest, nil, fmt.Errorf("роль %s: documents[%d] — не объект", key, i)
		}
		name, _ := d["name"].(string)
		kind, _ := d["kind"].(string)
		content, ok := d["content"].(string)
		// Имя станет именем файла: форма сервера не даёт ему выйти за каталог.
		if !roleDirName.MatchString(name) {
			return manifest, nil, fmt.Errorf("роль %s: документ %q — имя не годится для файла каталога", key, name)
		}
		if seen[name] {
			return manifest, nil, fmt.Errorf("роль %s: документ %q повторяется", key, name)
		}
		if !ok {
			return manifest, nil, fmt.Errorf("роль %s: у документа %s нет содержимого", key, name)
		}
		seen[name] = true
		manifest.Documents = append(manifest.Documents, roleDirDocument{Name: name, Kind: kind})
		docs = append(docs, bundleDirEntry{path: bundleDirRoles + "/" + key + "/" + name, body: []byte(content)})
	}
	return manifest, docs, nil
}

// checkRoleDirKeys проверяет, что ключи ролей годятся в имена каталогов.
// Сервер формат ключа не проверяет, поэтому «../x» без этой проверки
// записал бы файлы вне каталога, а Dev и dev на macOS и Windows — в один.
func checkRoleDirKeys(keys []string) error {
	var bad []string
	folded := map[string]string{}
	for _, k := range keys {
		if reason := badRoleDirKey(k); reason != "" {
			bad = append(bad, fmt.Sprintf("%q (%s)", k, reason))
			continue
		}
		lower := strings.ToLower(k)
		if prev, ok := folded[lower]; ok {
			bad = append(bad, fmt.Sprintf("%q и %q (различаются только регистром — на macOS и Windows это один каталог)", prev, k))
			continue
		}
		folded[lower] = k
	}
	if len(bad) > 0 {
		return fmt.Errorf("ключи ролей не годятся в имена каталогов: %s — ничего не записано; "+
			"переименуйте роли или выгрузите JSON (export без --dir)", strings.Join(bad, ", "))
	}
	return nil
}

func badRoleDirKey(k string) string {
	switch {
	case k == "":
		return "пустой"
	case k == "." || k == "..":
		return "служебное имя"
	case strings.ContainsAny(k, `/\`):
		return "содержит разделитель пути"
	case strings.HasPrefix(k, "."):
		return "начинается с точки"
	case strings.ContainsRune(k, 0):
		return "содержит NUL"
	}
	return ""
}

func errOldBundleFormat(version int) error {
	return fmt.Errorf("сервер отдал связку формата %d, каталог связки требует формата %d: "+
		"обновите сервер или выгрузите JSON (export без --dir)", version, bundleDirFormat)
}

// bundleVersion — номер формата из поля version; нет поля — 0.
func bundleVersion(v any) (int, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("version связки — не целое число: %s", n)
		}
		return int(i), nil
	}
	return 0, fmt.Errorf("version связки — не число: %v", v)
}

// encodeIndented — JSON с отступом в два пробела и переводом строки в
// конце, без экранирования <, > и &: файл читают люди и git diff.
func encodeIndented(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeBundleTop пишет bundle.json с постоянным порядком ключей верхнего
// уровня: version первым, остальное — как в связке.
func encodeBundleTop(top map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(top))
	for _, k := range bundleDirTopOrder {
		if _, ok := top[k]; ok {
			keys = append(keys, k)
		}
	}
	var rest []string
	for k := range top {
		known := false
		for _, o := range bundleDirTopOrder {
			known = known || o == k
		}
		if !known {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	var buf bytes.Buffer
	buf.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			buf.WriteString(",")
		}
		name, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		var value bytes.Buffer
		enc := json.NewEncoder(&value)
		enc.SetEscapeHTML(false)
		enc.SetIndent("  ", "  ")
		if err := enc.Encode(top[k]); err != nil {
			return nil, err
		}
		buf.WriteString("\n  ")
		buf.Write(name)
		buf.WriteString(": ")
		buf.Write(bytes.TrimRight(value.Bytes(), "\n"))
	}
	if len(keys) > 0 {
		buf.WriteString("\n")
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

// writeBundleDir кладёт план на диск. Каталог должен не существовать или
// быть пустым; с force в непустом заменяются bundle.json и roles/ целиком
// (устаревшие роли и документы исчезают, иначе строгий импорт споткнулся бы
// о них), остальные файлы каталога не трогаются. Новое сначала пишется во
// временный подкаталог и встаёт на место переименованием: сбой записи не
// оставляет прежний каталог без ролей.
func writeBundleDir(dir string, plan bundleDirPlan, force bool) error {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("каталог связки: %w", err)
		}
	case err != nil:
		return fmt.Errorf("каталог связки: %w", err)
	case !info.IsDir():
		return fmt.Errorf("%s — не каталог", dir)
	default:
		entries, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("каталог связки: %w", err)
		}
		if len(entries) > 0 && !force {
			return fmt.Errorf("каталог %s не пуст: --force заменит в нём bundle.json и roles/ целиком, "+
				"остальные файлы не тронет", dir)
		}
	}

	staging, err := os.MkdirTemp(dir, ".clab-export-")
	if err != nil {
		return fmt.Errorf("каталог связки: %w", err)
	}
	defer os.RemoveAll(staging)
	for _, f := range plan.files {
		path := filepath.Join(staging, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("каталог связки: %w", err)
		}
		if err := os.WriteFile(path, f.body, 0o644); err != nil {
			return fmt.Errorf("каталог связки: %w", err)
		}
	}

	rolesPath := filepath.Join(dir, bundleDirRoles)
	if err := os.RemoveAll(rolesPath); err != nil {
		return fmt.Errorf("каталог связки: прежний %s: %w", rolesPath, err)
	}
	if err := os.Rename(filepath.Join(staging, bundleDirRoles), rolesPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("каталог связки: %w", err)
	}
	if err := os.Rename(filepath.Join(staging, bundleDirManifest), filepath.Join(dir, bundleDirManifest)); err != nil {
		return fmt.Errorf("каталог связки: %w", err)
	}
	return nil
}

// readBundleDir собирает из каталога одну связку формата 3 — ту, что уходит
// на сервер тем же путём, что --file. Всё читается до первого запроса:
// ошибка каталога не доходит до сервера.
func readBundleDir(dir string) (map[string]any, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("каталог связки: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s — не каталог", dir)
	}

	bundle := map[string]any{}
	version := 0
	manifestPath := filepath.Join(dir, bundleDirManifest)
	_, statErr := os.Lstat(manifestPath)
	hasManifest := statErr == nil
	switch {
	case hasManifest:
		raw, err := readRegularFile(manifestPath)
		if err != nil {
			return nil, err
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, fmt.Errorf("%s не разбирается как объект JSON: %w", manifestPath, err)
		}
		if _, ok := top["roles"]; ok {
			return nil, fmt.Errorf("%s: поля roles в каталоге связки не бывает — роли лежат в %s/<ключ>/ (role.json и документы)",
				manifestPath, bundleDirRoles)
		}
		if v, ok := top["version"]; ok {
			if err := json.Unmarshal(v, &version); err != nil {
				return nil, fmt.Errorf("%s: version — не целое число: %s", manifestPath, v)
			}
		}
		for k, v := range top {
			bundle[k] = v
		}
	case !errors.Is(statErr, fs.ErrNotExist):
		return nil, fmt.Errorf("каталог связки: %w", statErr)
	}
	// Роли каталога — documents, то есть формат 3. Номер выше оставляем как
	// есть: сервер, который его не знает, откажет своим текстом.
	bundle["version"] = max(version, bundleDirFormat)

	rolesPath := filepath.Join(dir, bundleDirRoles)
	rolesInfo, err := os.Lstat(rolesPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !hasManifest {
			return nil, fmt.Errorf("%s не похоже на каталог связки: нет ни %s, ни %s/", dir, bundleDirManifest, bundleDirRoles)
		}
		return bundle, nil
	case err != nil:
		return nil, fmt.Errorf("каталог связки: %w", err)
	case !rolesInfo.IsDir():
		return nil, fmt.Errorf("%s — не каталог (символьная ссылка не читается): роли лежат в %s/<ключ>/", rolesPath, bundleDirRoles)
	}
	entries, err := os.ReadDir(rolesPath)
	if err != nil {
		return nil, fmt.Errorf("каталог связки: %w", err)
	}
	var roles []any
	for _, e := range entries {
		name := e.Name()
		// Ключ роли с точки не начинается: .DS_Store и .git — не роли.
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(rolesPath, name)
		if !e.IsDir() {
			return nil, fmt.Errorf("%s: в %s/ лежат только каталоги ролей (символьная ссылка не читается)", path, bundleDirRoles)
		}
		role, err := readRoleDir(path)
		if err != nil {
			return nil, fmt.Errorf("роль %s: %w", name, err)
		}
		if raw, ok := role["key"]; ok {
			if key := roleFieldString(role, "key"); key != name {
				return nil, fmt.Errorf("роль %s: key %s в %s не равен имени каталога %s/%s — каталог и есть ключ роли",
					name, raw, filepath.Join(path, roleDirManifest), bundleDirRoles, name)
			}
		} else {
			role["key"] = name
		}
		roles = append(roles, role)
	}
	if len(roles) > 0 {
		bundle["roles"] = roles
	}
	return bundle, nil
}

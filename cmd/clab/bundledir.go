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
//	D/agent-settings.json          настройка агентов (CLAB-428, решение
//	                               0077): {document, changelog}; файла нет,
//	                               если у проекта нет активной версии
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
	// bundleDirAgentSettings — настройка агентов: поле agent_settings связки
	// лежит отдельным файлом рядом с ролями и циклами, а не внутри
	// bundle.json (решение 0077, п. 4.10).
	bundleDirAgentSettings = "agent-settings.json"
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
	files        []bundleDirEntry
	roles        int
	documents    int
	instructions int
	// agentSettings — каталог несёт agent-settings.json.
	agentSettings bool
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

	instFiles, instCount, err := planInstructions(top["instructions"])
	if err != nil {
		return plan, err
	}
	delete(top, "instructions")

	var settingsFile *bundleDirEntry
	if raw, ok := top["agent_settings"]; ok {
		body, err := planAgentSettings(raw)
		if err != nil {
			return plan, err
		}
		settingsFile = &bundleDirEntry{path: bundleDirAgentSettings, body: body}
		delete(top, "agent_settings")
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
	if settingsFile != nil {
		plan.files = append(plan.files, *settingsFile)
		plan.agentSettings = true
	}
	plan.files = append(plan.files, instFiles...)
	plan.instructions = instCount
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

// planAgentSettings — тело agent-settings.json из поля agent_settings
// ответа экспорта: объект как есть (document и changelog; прочее, что сервер
// добавит, проходит нетронутым, как в bundle.json), отступ два пробела.
func planAgentSettings(raw any) ([]byte, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("ответ экспорта: agent_settings — не объект")
	}
	if _, ok := obj["document"].(map[string]any); !ok {
		return nil, fmt.Errorf("ответ экспорта: у agent_settings нет document-объекта")
	}
	return encodeIndented(obj)
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
		instruction, _ := d["instruction"].(string)
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
		manifest.Documents = append(manifest.Documents, roleDirDocument{Name: name, Kind: kind, Instruction: instruction})
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
// быть пустым; с force в непустом заменяются bundle.json, roles/, instructions/
// и agent-settings.json целиком
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
			return fmt.Errorf("каталог %s не пуст: --force заменит в нём bundle.json, roles/, instructions/ и agent-settings.json целиком, "+
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

	for _, sub := range []string{bundleDirRoles, bundleDirInstructions} {
		path := filepath.Join(dir, sub)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("каталог связки: прежний %s: %w", path, err)
		}
		if err := os.Rename(filepath.Join(staging, sub), path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("каталог связки: %w", err)
		}
	}
	// Файл настройки заменяется или, если у проекта её уже нет, удаляется:
	// устаревший файл вернул бы импорт настройки, которой в проекте больше
	// нет.
	settingsPath := filepath.Join(dir, bundleDirAgentSettings)
	if err := os.RemoveAll(settingsPath); err != nil {
		return fmt.Errorf("каталог связки: прежний %s: %w", settingsPath, err)
	}
	if plan.agentSettings {
		if err := os.Rename(filepath.Join(staging, bundleDirAgentSettings), settingsPath); err != nil {
			return fmt.Errorf("каталог связки: %w", err)
		}
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
	settingsInManifest := false
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
		if _, ok := top["instructions"]; ok {
			return nil, fmt.Errorf("%s: поля instructions в каталоге связки не бывает — инструкции лежат в %s/<ключ>/ (%s и v<N>.md)",
				manifestPath, bundleDirInstructions, instructionDirManifest)
		}
		if _, ok := top["agent_settings"]; ok {
			// Поле в bundle.json — каталог прежнего clab: принимается, если
			// файла рядом нет (проверка ниже); оба сразу — какой из них
			// правда, решает человек.
			settingsInManifest = true
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

	settingsPath := filepath.Join(dir, bundleDirAgentSettings)
	_, settingsErr := os.Lstat(settingsPath)
	hasSettings := settingsErr == nil
	switch {
	case hasSettings:
		if settingsInManifest {
			return nil, fmt.Errorf("agent_settings есть и в %s, и в %s: оставьте одно — файл %s, поле в bundle.json "+
				"оставил прежний clab", manifestPath, settingsPath, bundleDirAgentSettings)
		}
		raw, err := readRegularFile(settingsPath)
		if err != nil {
			return nil, err
		}
		if err := checkAgentSettingsFile(settingsPath, raw); err != nil {
			return nil, err
		}
		bundle["agent_settings"] = json.RawMessage(raw)
	case !errors.Is(settingsErr, fs.ErrNotExist):
		return nil, fmt.Errorf("каталог связки: %w", settingsErr)
	}

	instructions, err := readInstructionsDir(filepath.Join(dir, bundleDirInstructions))
	if err != nil {
		return nil, err
	}
	if len(instructions) > 0 {
		bundle["instructions"] = instructions
	}

	rolesPath := filepath.Join(dir, bundleDirRoles)
	rolesInfo, err := os.Lstat(rolesPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if !hasManifest && len(instructions) == 0 && !hasSettings {
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

// checkAgentSettingsFile — agent-settings.json разбирается как объект с
// document-объектом: ошибка файла не доходит до сервера. Содержимое
// документа проверяет сервер (ValidateAgentSettings), клиент его не
// разбирает.
func checkAgentSettingsFile(path string, raw []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("%s не разбирается как объект JSON: %w", path, err)
	}
	doc, ok := top["document"]
	if !ok {
		return fmt.Errorf("%s: нет поля document — файл настройки агентов это {\"document\": {…}, \"changelog\": \"…\"}", path)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(doc, &obj); err != nil || obj == nil {
		return fmt.Errorf("%s: document — не JSON-объект", path)
	}
	if c, ok := top["changelog"]; ok {
		var text string
		if err := json.Unmarshal(c, &text); err != nil {
			return fmt.Errorf("%s: changelog — не строка", path)
		}
	}
	return nil
}

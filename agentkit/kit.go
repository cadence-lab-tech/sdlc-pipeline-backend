// Package agentkit — набор агента, который инстанс раздаёт пользователям
// (CLAB-232, решение 0050): нейтральное ядро и привязки к агентам (CLAB-417,
// решение 0077, п. 3).
//
// Ядро — тексты, годные любому агенту: core/skills/<имя>/SKILL.md,
// core/agents/<имя>.md (во фронтматтере только name и description) и
// core/guide.md — справка, она же тело скила clab-guide. Ядро не называет
// провайдера, его модели, тулы и пути: это проверяет kit_test.go.
//
// Привязка — файл bindings/<имя>.json: как ядро ложится в конкретного
// агента. Девять полей формата 0077 (kit, mcp, run, executor, hook, output,
// detect, levels, tool_policy) плюс name; пустое значение — явный null.
// Рядом, при надобности, каталог bindings/<имя>/ с фрагментами:
// skills/<скил>.md и agents/<агент>.md дописывают к файлу ядра свои ключи
// фронтматтера и заметки в конец тела, agents/<агент>.toml делает файл
// агента TOML-таблицей (name, description, developer_instructions). Сервер
// собирает готовые файлы каждой привязки сам, клиенту остаётся записать их.
// Новая привязка на известных форматах — только файлом, без правки кода.
//
// Набор лежит исходниками рядом с кодом и встраивается в бинарник api, как
// migrations: его версия — версия сборки сервера. Отдаёт его открытая ручка
// GET /v1/agent-kit, ставит — clab setup. Поле Files — раскладка привязки
// claude с путями относительно .claude/ (skills/<имя>/SKILL.md,
// agents/<имя>.md): из него ставит набор clab прежних версий.
package agentkit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/pelletier/go-toml/v2"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"
)

//go:embed core bindings
var source embed.FS

// File — файл набора с содержимым и его sha256: по сумме clab setup узнаёт,
// правил ли файл человек после установки.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

// Entry — скил или саб-агент набора, как его видит человек на странице
// /docs: имя и описание из фронтматтера.
type Entry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Core — нейтральные тексты набора. Пути — относительно core/:
// skills/<имя>/SKILL.md, agents/<имя>.md, guide.md. Тело скила clab-guide —
// справка guide.md: источник один.
type Core struct {
	Skills []File `json:"skills"`
	Agents []File `json:"agents"`
	Guide  File   `json:"guide"`
}

// KitLayout — где набор привязки лежит у агента: пути от корня проекта
// (при установке в домашний каталог — от него).
type KitLayout struct {
	Manifest  string `json:"manifest"`
	SkillsDir string `json:"skills_dir"`
	AgentsDir string `json:"agents_dir"`
}

// DetectRule — одно условие признака агента в окружении: переменная env с
// префиксом, равная значению или непустая. Условия привязки — «или».
type DetectRule struct {
	Env      string  `json:"env"`
	Prefix   *string `json:"prefix,omitempty"`
	Equals   *string `json:"equals,omitempty"`
	NonEmpty bool    `json:"non_empty,omitempty"`
}

// Levels — таблица уровней по умолчанию: модель агента на уровень, когда в
// проекте нет настройки агентов (решение 0077, п. 7, шаг 3).
type Levels struct {
	Low    string `json:"low"`
	Medium string `json:"medium"`
	High   string `json:"high"`
}

// ToolPolicy — флаги агента, в которые раннер переводит role.tool_policy.
type ToolPolicy struct {
	Allow string `json:"allow"`
	Deny  string `json:"deny"`
}

// Binding — привязка в ответе: поля файла bindings/<имя>.json и готовые
// файлы набора с путями от корня проекта. Поля mcp и hook отдаются как
// записаны: у разных способов свои ключи.
type Binding struct {
	Name       string          `json:"name"`
	Kit        *KitLayout      `json:"kit"`
	MCP        json.RawMessage `json:"mcp"`
	Run        []string        `json:"run"`
	Executor   string          `json:"executor"`
	Hook       json.RawMessage `json:"hook"`
	Output     *string         `json:"output"`
	Detect     []DetectRule    `json:"detect"`
	Levels     *Levels         `json:"levels"`
	ToolPolicy *ToolPolicy     `json:"tool_policy"`
	Files      []File          `json:"files"`
}

// Kit — ответ GET /v1/agent-kit. Объект, а не массив: новые сведения ложатся
// новыми полями, не ломая clab прошлой версии. Version, Files, Skills и
// Agents — прежние по форме и смыслу; Core и Bindings — с CLAB-417, их
// отсутствие в ответе значит «сервер прежней версии, привязок нет».
type Kit struct {
	Version  string    `json:"version"`
	Files    []File    `json:"files"`
	Skills   []Entry   `json:"skills"`
	Agents   []Entry   `json:"agents"`
	Core     Core      `json:"core"`
	Bindings []Binding `json:"bindings"`
}

// Имена, которые знает сборка набора. Привязка, из которой собирается
// прежнее поле files, и скил, телом которого служит справка.
const (
	legacyBinding = "claude"
	guideSkill    = "clab-guide"
)

// Поля файла привязки (решение 0077, п. 3) — все обязательны.
var bindingFields = []string{"kit", "mcp", "run", "executor", "hook", "output", "detect", "levels", "tool_policy"}

// Известные значения: новый формат вывода, способ MCP или место хука — это
// код клиента и раннера, отдельная задача, а не файл набора.
var (
	knownExecutors  = []string{"subagent", "process"}
	knownOutputs    = []string{"claude-json", "codex-jsonl"}
	knownMCPMethods = []string{"claude-mcp-add", "codex-config-toml"}
	knownPlacements = []string{"agent-frontmatter", "launch"}
	knownRunSlots   = launch.Slots
	runSlot         = regexp.MustCompile(`\{[a-z_]+\}`)
)

var (
	loadOnce sync.Once
	loaded   Kit
	loadErr  error
)

// Load собирает набор из встроенных файлов один раз на процесс: содержимое
// не меняется до следующей сборки. Версию подставляет вызывающий — набор её
// не знает.
func Load(version string) (Kit, error) {
	loadOnce.Do(func() { loaded, loadErr = build(source) })
	if loadErr != nil {
		return Kit{}, loadErr
	}
	k := loaded
	k.Version = version
	return k, nil
}

// doc — markdown-файл, разобранный на фронтматтер (строки между «---», как
// записаны) и тело после закрывающей «---».
type doc struct {
	front string
	body  string
}

func parseDoc(content string) (doc, bool) {
	if !strings.HasPrefix(content, "---\n") {
		return doc{body: content}, false
	}
	rest := content[len("---\n"):]
	if strings.HasPrefix(rest, "---\n") {
		return doc{body: rest[len("---\n"):]}, true
	}
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return doc{body: content}, false
	}
	return doc{front: rest[:end+1], body: rest[end+len("\n---\n"):]}, true
}

func (d doc) render() string {
	return "---\n" + d.front + "---\n" + d.body
}

// Body — тело файла набора без фронтматтера и пустых строк перед ним: то,
// что читает агент, и то, что ложится в developer_instructions агента TOML.
func Body(content string) string {
	d, _ := parseDoc(content)
	return strings.TrimLeft(d.body, "\n")
}

func newFile(p, content string) File {
	sum := sha256.Sum256([]byte(content))
	return File{Path: p, Content: content, SHA256: hex.EncodeToString(sum[:])}
}

// coreEntry — скил или агент ядра: разобранный файл и его имя.
type coreEntry struct {
	name string
	doc  doc
}

func build(fsys fs.FS) (Kit, error) {
	k, err := buildKit(fsys)
	if err != nil {
		return Kit{}, fmt.Errorf("agentkit: %w", err)
	}
	return k, nil
}

func buildKit(fsys fs.FS) (Kit, error) {
	skills, agents, guide, err := readCore(fsys)
	if err != nil {
		return Kit{}, err
	}
	k := Kit{Files: []File{}, Skills: []Entry{}, Agents: []Entry{}, Bindings: []Binding{}}
	k.Core = Core{Skills: []File{}, Agents: []File{}, Guide: newFile("guide.md", guide)}
	for _, s := range skills {
		k.Core.Skills = append(k.Core.Skills, newFile("skills/"+s.name+"/SKILL.md", s.doc.render()))
		meta := Frontmatter(s.doc.render())
		k.Skills = append(k.Skills, Entry{Name: meta["name"], Description: meta["description"]})
	}
	for _, a := range agents {
		k.Core.Agents = append(k.Core.Agents, newFile("agents/"+a.name+".md", a.doc.render()))
		meta := Frontmatter(a.doc.render())
		k.Agents = append(k.Agents, Entry{Name: meta["name"], Description: meta["description"]})
	}

	names, err := bindingNames(fsys)
	if err != nil {
		return Kit{}, err
	}
	var legacy *Binding
	for _, name := range names {
		b, err := readBinding(fsys, name, skills, agents)
		if err != nil {
			return Kit{}, fmt.Errorf("привязка %s: %w", name, err)
		}
		k.Bindings = append(k.Bindings, b)
		if name == legacyBinding {
			legacy = &k.Bindings[len(k.Bindings)-1]
		}
	}
	if legacy == nil {
		return Kit{}, fmt.Errorf("нет привязки %s — из неё собирается поле files для clab прежних версий", legacyBinding)
	}
	if err := launchHooks(k.Bindings); err != nil {
		return Kit{}, err
	}
	if k.Files, err = legacyFiles(*legacy); err != nil {
		return Kit{}, fmt.Errorf("привязка %s: %w", legacyBinding, err)
	}
	return k, nil
}

// readCore читает ядро: скилы и агентов (во фронтматтере только name и
// description, name совпадает с именем файла) и справку. Скил clab-guide —
// только фронтматтер: его тело — guide.md.
func readCore(fsys fs.FS) (skills, agents []coreEntry, guide string, err error) {
	raw, err := fs.ReadFile(fsys, "core/guide.md")
	if err != nil {
		return nil, nil, "", fmt.Errorf("нет справки core/guide.md: %w", err)
	}
	guide = string(raw)
	err = fs.WalkDir(fsys, "core", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "core/guide.md" {
			return err
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, "core/")
		var name string
		switch dir, file := path.Split(rel); {
		case file == "SKILL.md" && strings.Count(rel, "/") == 2 && strings.HasPrefix(rel, "skills/"):
			name = path.Base(dir)
		case strings.Count(rel, "/") == 1 && strings.HasPrefix(rel, "agents/") && path.Ext(file) == ".md":
			name = strings.TrimSuffix(file, ".md")
		default:
			return fmt.Errorf("%s: файл вне skills/<имя>/SKILL.md, agents/<имя>.md и guide.md", p)
		}
		d0, ok := parseDoc(string(raw))
		if !ok {
			return fmt.Errorf("%s: нет фронтматтера", p)
		}
		meta := Frontmatter(string(raw))
		for key := range meta {
			if key != "name" && key != "description" {
				return fmt.Errorf("%s: во фронтматтере ядра лишний ключ %q — только name и description", p, key)
			}
		}
		if meta["name"] != name || strings.TrimSpace(meta["description"]) == "" {
			return fmt.Errorf("%s: name %q не совпадает с именем файла или пустой description", p, meta["name"])
		}
		e := coreEntry{name: name, doc: d0}
		if strings.HasPrefix(rel, "skills/") {
			if name == guideSkill {
				if strings.TrimSpace(d0.body) != "" {
					return fmt.Errorf("%s: тело скила — справка core/guide.md, в файле только фронтматтер", p)
				}
				e.doc.body = "\n" + guide
			} else if strings.TrimSpace(d0.body) == "" {
				return fmt.Errorf("%s: пустое тело", p)
			}
			skills = append(skills, e)
		} else {
			agents = append(agents, e)
		}
		return nil
	})
	if err != nil {
		return nil, nil, "", err
	}
	if len(skills) == 0 {
		return nil, nil, "", errors.New("в ядре нет скилов")
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].name < skills[j].name })
	sort.Slice(agents, func(i, j int) bool { return agents[i].name < agents[j].name })
	return skills, agents, guide, nil
}

// bindingNames — имена привязок по файлам bindings/<имя>.json. Каталог
// bindings/<имя>/ без файла привязки — ошибка: фрагменты ничьи.
func bindingNames(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, "bindings")
	if err != nil {
		return nil, fmt.Errorf("нет каталога привязок: %w", err)
	}
	files, dirs := map[string]bool{}, []string{}
	var names []string
	for _, e := range entries {
		switch {
		case e.IsDir():
			dirs = append(dirs, e.Name())
		case path.Ext(e.Name()) == ".json":
			name := strings.TrimSuffix(e.Name(), ".json")
			files[name] = true
			names = append(names, name)
		default:
			return nil, fmt.Errorf("bindings/%s: не файл привязки .json и не каталог фрагментов", e.Name())
		}
	}
	for _, d := range dirs {
		if !files[d] {
			return nil, fmt.Errorf("bindings/%s/: каталог фрагментов без файла bindings/%s.json", d, d)
		}
	}
	sort.Strings(names)
	return names, nil
}

func readBinding(fsys fs.FS, name string, skills, agents []coreEntry) (Binding, error) {
	file := "bindings/" + name + ".json"
	raw, err := fs.ReadFile(fsys, file)
	if err != nil {
		return Binding{}, err
	}
	if err := noCyrillic(file, string(raw)); err != nil {
		return Binding{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Binding{}, fmt.Errorf("%s: не JSON-объект: %w", file, err)
	}
	for key := range fields {
		if key != "name" && !contains(bindingFields, key) {
			return Binding{}, fmt.Errorf("%s: поле %q не из формата привязки (%s)", file, key, strings.Join(bindingFields, ", "))
		}
	}
	for _, key := range append([]string{"name"}, bindingFields...) {
		if _, ok := fields[key]; !ok {
			return Binding{}, fmt.Errorf("%s: нет поля %q — пустое значение пишется явным null", file, key)
		}
	}
	var b Binding
	if err := json.Unmarshal(fields["name"], &b.Name); err != nil || b.Name != name {
		return Binding{}, fmt.Errorf("%s: поле name %s не совпадает с именем файла", file, fields["name"])
	}
	if err := decodeFields(fields, &b); err != nil {
		return Binding{}, fmt.Errorf("%s: %w", file, err)
	}
	frags, err := readFragments(fsys, name, skills, agents)
	if err != nil {
		return Binding{}, err
	}
	if err := checkHook(b, frags); err != nil {
		return Binding{}, fmt.Errorf("%s: %w", file, err)
	}
	if b.Files, err = bindingFiles(b, frags, skills, agents); err != nil {
		return Binding{}, err
	}
	return b, nil
}

// decodeFields разбирает и проверяет поля привязки по формам решения 0077
// (AC-12): у каждого поля — null или объект известной формы.
func decodeFields(fields map[string]json.RawMessage, b *Binding) error {
	if err := strict(fields["kit"], &b.Kit); err != nil {
		return fmt.Errorf("поле kit: %w", err)
	}
	if b.Kit != nil {
		for key, p := range map[string]string{"manifest": b.Kit.Manifest, "skills_dir": b.Kit.SkillsDir, "agents_dir": b.Kit.AgentsDir} {
			if !relPath(p) {
				return fmt.Errorf("поле kit: %s %q — нужен относительный путь без ..", key, p)
			}
		}
	}
	if err := strict(fields["executor"], &b.Executor); err != nil || !contains(knownExecutors, b.Executor) {
		return fmt.Errorf("поле executor: %s — известны %s", fields["executor"], strings.Join(knownExecutors, ", "))
	}
	if err := strict(fields["output"], &b.Output); err != nil || b.Output != nil && !contains(knownOutputs, *b.Output) {
		return fmt.Errorf("поле output: %s — известны %s и null", fields["output"], strings.Join(knownOutputs, ", "))
	}
	if err := strict(fields["run"], &b.Run); err != nil || b.Run != nil && len(b.Run) == 0 {
		return fmt.Errorf("поле run: нужен null или непустой массив строк")
	}
	for _, arg := range b.Run {
		for _, slot := range runSlot.FindAllString(arg, -1) {
			if !contains(knownRunSlots, slot) {
				return fmt.Errorf("поле run: подстановка %s — известны %s", slot, strings.Join(knownRunSlots, " "))
			}
		}
	}
	if err := strict(fields["detect"], &b.Detect); err != nil || b.Detect != nil && len(b.Detect) == 0 {
		return fmt.Errorf("поле detect: нужен null или непустой массив условий")
	}
	for _, r := range b.Detect {
		conds := 0
		for _, set := range []bool{r.Prefix != nil, r.Equals != nil, r.NonEmpty} {
			if set {
				conds++
			}
		}
		if r.Env == "" || conds != 1 {
			return fmt.Errorf("поле detect: условие %+v — нужны env и ровно одно из prefix, equals, non_empty", r)
		}
	}
	if err := strict(fields["levels"], &b.Levels); err != nil {
		return fmt.Errorf("поле levels: %w", err)
	}
	if b.Levels != nil && (b.Levels.Low == "" || b.Levels.Medium == "" || b.Levels.High == "") {
		return errors.New("поле levels: нужны модели low, medium и high")
	}
	if err := strict(fields["tool_policy"], &b.ToolPolicy); err != nil {
		return fmt.Errorf("поле tool_policy: %w", err)
	}
	if b.ToolPolicy != nil && (b.ToolPolicy.Allow == "" || b.ToolPolicy.Deny == "") {
		return errors.New("поле tool_policy: нужны флаги allow и deny")
	}
	var err error
	if b.MCP, err = keyedObject(fields["mcp"], "method", knownMCPMethods); err != nil {
		return fmt.Errorf("поле mcp: %w", err)
	}
	if b.Hook, err = keyedObject(fields["hook"], "placement", knownPlacements); err != nil {
		return fmt.Errorf("поле hook: %w", err)
	}
	return nil
}

// strict разбирает значение поля без неизвестных ключей.
func strict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// keyedObject проверяет null или объект, у которого ключ key — одно из
// известных значений; остальные ключи способа отдаются как записаны.
func keyedObject(raw json.RawMessage, key string, known []string) (json.RawMessage, error) {
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errors.New("нужен null или объект")
	}
	if v, _ := obj[key].(string); !contains(known, v) {
		return nil, fmt.Errorf("%s %v — известны %s", key, obj[key], strings.Join(known, ", "))
	}
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// fragments — дополнения привязки к файлам ядра: markdown к скилам и
// агентам, TOML — признак, что агент привязки — TOML-таблица.
type fragments struct {
	skills     map[string]doc
	agents     map[string]doc
	tomlAgents map[string]string
}

func readFragments(fsys fs.FS, name string, skills, agents []coreEntry) (fragments, error) {
	f := fragments{skills: map[string]doc{}, agents: map[string]doc{}, tomlAgents: map[string]string{}}
	root := "bindings/" + name
	if _, err := fs.Stat(fsys, root); errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		if err := noCyrillic(p, string(raw)); err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		dir, file := path.Split(rel)
		ext := path.Ext(file)
		base := strings.TrimSuffix(file, ext)
		switch {
		case dir == "skills/" && ext == ".md":
			if !hasEntry(skills, base) {
				return fmt.Errorf("%s: в ядре нет скила %s", p, base)
			}
			frag, err := fragmentDoc(p, string(raw))
			if err != nil {
				return err
			}
			f.skills[base] = frag
		case dir == "agents/" && (ext == ".md" || ext == ".toml"):
			if !hasEntry(agents, base) {
				return fmt.Errorf("%s: в ядре нет агента %s", p, base)
			}
			if _, dup := f.agents[base]; dup {
				return fmt.Errorf("%s: у агента %s два фрагмента", p, base)
			}
			if _, dup := f.tomlAgents[base]; dup {
				return fmt.Errorf("%s: у агента %s два фрагмента", p, base)
			}
			if ext == ".toml" {
				f.tomlAgents[base] = string(raw)
				return nil
			}
			frag, err := fragmentDoc(p, string(raw))
			if err != nil {
				return err
			}
			f.agents[base] = frag
		default:
			return fmt.Errorf("%s: фрагмент вне skills/<скил>.md, agents/<агент>.md и agents/<агент>.toml", p)
		}
		return nil
	})
	return f, err
}

// fragmentDoc разбирает markdown-фрагмент: его фронтматтер дописывается к
// фронтматтеру ядра и не повторяет name и description, тело — в конец тела.
func fragmentDoc(p, content string) (doc, error) {
	d, _ := parseDoc(content)
	for key := range Frontmatter(d.render()) {
		if key == "name" || key == "description" {
			return doc{}, fmt.Errorf("%s: ключ %s берётся из ядра", p, key)
		}
	}
	return d, nil
}

// checkHook сверяет хук во фронтматтере агента с файлом фрагмента: место
// хука agent-frontmatter требует фрагмент агента с этим событием.
func checkHook(b Binding, f fragments) error {
	if b.Hook == nil {
		return nil
	}
	var h struct {
		Placement string `json:"placement"`
		Agent     string `json:"agent"`
		Event     string `json:"event"`
	}
	if err := json.Unmarshal(b.Hook, &h); err != nil {
		return fmt.Errorf("поле hook: %w", err)
	}
	if h.Placement != "agent-frontmatter" {
		return nil
	}
	frag, ok := f.agents[h.Agent]
	if !ok {
		return fmt.Errorf("поле hook: хук во фронтматтере агента %q, а файла bindings/%s/agents/%s.md нет", h.Agent, b.Name, h.Agent)
	}
	if h.Event == "" || !strings.Contains(frag.front, h.Event+":") {
		return fmt.Errorf("поле hook: во фронтматтере bindings/%s/agents/%s.md нет события %q", b.Name, h.Agent, h.Event)
	}
	return nil
}

// launchHooks дописывает хуку, который крепится к запуску (placement
// launch), сценарий: поле command — тело хука того же агента из
// фронтматтера привязки с placement agent-frontmatter (CLAB-419, решение
// 0077 п. 10). Сценарий один на всех агентов, и корпус hook_test.go
// проверяет его один раз; вторая копия в файле привязки разошлась бы с ним.
// Шаблон run с {hook_script} без сценария — ошибка сборки.
func launchHooks(bindings []Binding) error {
	scripts := map[string]string{}
	for _, b := range bindings {
		h, ok := hookOf(b)
		if !ok || h.Placement != "agent-frontmatter" || b.Kit == nil {
			continue
		}
		for _, f := range b.Files {
			if f.Path == path.Join(b.Kit.AgentsDir, h.Agent+".md") {
				script, err := launch.HookCommand(f.Content)
				if err != nil {
					return fmt.Errorf("привязка %s: агент %s: %w", b.Name, h.Agent, err)
				}
				scripts[h.Agent] = script
			}
		}
	}
	for i := range bindings {
		b := &bindings[i]
		h, ok := hookOf(*b)
		usesScript := launch.Uses(b.Run, launch.SlotHookScript)
		if !ok || h.Placement != "launch" || h.Agent == "" {
			if usesScript {
				return fmt.Errorf("привязка %s: в run есть %s, а хука с placement launch и agent нет", b.Name, launch.SlotHookScript)
			}
			continue
		}
		script, found := scripts[h.Agent]
		if !found {
			return fmt.Errorf("привязка %s: хук агента %s при запуске, а сценария хука этого агента во фронтматтере ни одной привязки нет", b.Name, h.Agent)
		}
		var obj map[string]any
		if err := json.Unmarshal(b.Hook, &obj); err != nil {
			return fmt.Errorf("привязка %s: поле hook: %w", b.Name, err)
		}
		if _, set := obj["command"]; set {
			return fmt.Errorf("привязка %s: поле hook.command собирается из фронтматтера агента %s — в файле его не пишут", b.Name, h.Agent)
		}
		obj["command"] = script
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(obj); err != nil {
			return err
		}
		b.Hook = bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	}
	return nil
}

type hookFields struct {
	Placement string `json:"placement"`
	Agent     string `json:"agent"`
}

func hookOf(b Binding) (hookFields, bool) {
	var h hookFields
	if b.Hook == nil || json.Unmarshal(b.Hook, &h) != nil {
		return hookFields{}, false
	}
	return h, true
}

// bindingFiles собирает файлы привязки: скилы и агенты ядра с фрагментами,
// по путям kit. Без kit файлов нет, и фрагменты тогда ничьи.
func bindingFiles(b Binding, f fragments, skills, agents []coreEntry) ([]File, error) {
	files := []File{}
	if b.Kit == nil {
		if len(f.skills)+len(f.agents)+len(f.tomlAgents) > 0 {
			return nil, fmt.Errorf("bindings/%s/: у привязки без kit нет файлов набора — фрагменты ничьи", b.Name)
		}
		return files, nil
	}
	for _, s := range skills {
		d := s.doc
		if frag, ok := f.skills[s.name]; ok {
			d.front += frag.front
			d.body += frag.body
		}
		files = append(files, newFile(path.Join(b.Kit.SkillsDir, s.name, "SKILL.md"), d.render()))
	}
	for _, a := range agents {
		if extra, ok := f.tomlAgents[a.name]; ok {
			content, err := tomlAgent(a, extra)
			if err != nil {
				return nil, fmt.Errorf("bindings/%s/agents/%s.toml: %w", b.Name, a.name, err)
			}
			files = append(files, newFile(path.Join(b.Kit.AgentsDir, a.name+".toml"), content))
			continue
		}
		d := a.doc
		if frag, ok := f.agents[a.name]; ok {
			d.front += frag.front
			d.body += frag.body
		}
		files = append(files, newFile(path.Join(b.Kit.AgentsDir, a.name+".md"), d.render()))
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// tomlAgent — агент как TOML-таблица: name и description из фронтматтера
// ядра, ключи фрагмента, developer_instructions — тело ядра. Собранный файл
// разбирается обратно: тело должно вернуться байт в байт.
func tomlAgent(a coreEntry, extra string) (string, error) {
	meta := Frontmatter(a.doc.render())
	body := Body(a.doc.render())
	var sb strings.Builder
	sb.WriteString("name = " + tomlBasic(meta["name"]) + "\n")
	sb.WriteString("description = " + tomlBasic(meta["description"]) + "\n")
	if strings.TrimSpace(extra) != "" {
		sb.WriteString(strings.TrimRight(extra, "\n") + "\n")
	}
	sb.WriteString("developer_instructions = " + tomlMultiline(body) + "\n")
	var back map[string]any
	if err := toml.Unmarshal([]byte(sb.String()), &back); err != nil {
		return "", fmt.Errorf("собранный TOML не разбирается: %w", err)
	}
	if back["developer_instructions"] != body || back["name"] != meta["name"] || back["description"] != meta["description"] {
		return "", errors.New("собранный TOML не возвращает имя, описание или тело ядра")
	}
	return sb.String(), nil
}

// tomlBasic — однострочная базовая строка TOML.
func tomlBasic(s string) string {
	return `"` + tomlEscape(s, false) + `"`
}

// tomlMultiline — многострочная строка: литеральная (три одинарные
// кавычки), если тело её допускает — в нём нет трёх одинарных кавычек
// подряд и управляющих символов, иначе базовая (три двойные) с
// экранированием. Перевод строки сразу после открывающих кавычек TOML
// отбрасывает, поэтому значение — тело как есть.
func tomlMultiline(s string) string {
	literal := !strings.Contains(s, "'''") && !strings.HasSuffix(s, "'")
	for _, r := range s {
		if r != '\n' && r != '\t' && (r < 0x20 || r == 0x7f) {
			literal = false
		}
	}
	if literal {
		return "'''\n" + s + "'''"
	}
	return `"""` + "\n" + tomlEscape(s, true) + `"""`
}

func tomlEscape(s string, multiline bool) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case r == '\\':
			sb.WriteString(`\\`)
		case r == '"':
			sb.WriteString(`\"`)
		case r == '\n' && multiline:
			sb.WriteByte('\n')
		case r == '\t':
			sb.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&sb, `\u%04X`, r)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// legacyFiles — прежнее поле files: файлы привязки claude с путями
// относительно её каталога (.claude/), как их ставит clab прежних версий.
func legacyFiles(b Binding) ([]File, error) {
	if b.Kit == nil {
		return nil, errors.New("у привязки нет kit — files не из чего собрать")
	}
	root := path.Dir(b.Kit.SkillsDir)
	if path.Base(b.Kit.SkillsDir) != "skills" || path.Base(b.Kit.AgentsDir) != "agents" || path.Dir(b.Kit.AgentsDir) != root {
		return nil, fmt.Errorf("kit %+v: для files нужны <каталог>/skills и <каталог>/agents", *b.Kit)
	}
	files := make([]File, 0, len(b.Files))
	for _, f := range b.Files {
		rel := strings.TrimPrefix(f.Path, root+"/")
		files = append(files, File{Path: rel, Content: f.Content, SHA256: f.SHA256})
	}
	return files, nil
}

func noCyrillic(p, s string) error {
	for i, line := range strings.Split(s, "\n") {
		for _, r := range line {
			if unicode.Is(unicode.Cyrillic, r) {
				return fmt.Errorf("%s:%d: кириллица — набор на английском", p, i+1)
			}
		}
	}
	return nil
}

func relPath(p string) bool {
	return p != "" && !path.IsAbs(p) && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.Contains(p, `\`)
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

func hasEntry(list []coreEntry, name string) bool {
	for _, e := range list {
		if e.name == name {
			return true
		}
	}
	return false
}

// Frontmatter читает верхнеуровневые поля «ключ: значение» между первыми
// строками «---». Вложенные блоки (hooks у агента) пропускаются: набору
// нужны только имя, описание и перечень тулов, а YAML-библиотека ради трёх
// строк в зависимости не идёт.
func Frontmatter(content string) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return out
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			return out
		}
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		out[strings.TrimSpace(key)] = value
	}
	// Фронтматтер не закрыт — его нет.
	return map[string]string{}
}

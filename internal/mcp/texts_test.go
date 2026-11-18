package mcp

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// CLAB-484: тексты MCP-сервера для хостового чата. Номера — критерии приёмки
// анализа (AC-18.N).

// forbiddenWords — ключи настройки конкретного проекта и имена внешних систем
// (AC-18.3): продукт говорит понятиями сервиса, а не нашими статусами и
// ролями, не провайдером модели, не хостингом git и не трекером. Сверка по
// границам слов без учёта регистра: слова общего языка не ловятся.
var forbiddenWords = []string{
	// статусы, роли, переходы и циклы настройки
	"analysis", "design", "development", "testing", "merge",
	"analyst", "analytics", "designer", "developer", "integrator", "reviewer",
	"dev_done", "testing_passed", "analysis_done", "merge_ready",
	"Simple", "Epic", "Release",
	// провайдеры модели и клиенты ИИ
	"claude", "anthropic", "haiku", "sonnet", "opus", "chatgpt", "openai", "gpt", "codex",
	"gemini", "cursor", "copilot",
	// хостинги git и трекеры
	"github", "gitlab", "jira",
}

var (
	toolNameRe = regexp.MustCompile(`sdlc_[a-z_]+`)
	// Слово — буквы, цифры и подчёркивание: dev_done целиком, а не «dev».
	forbiddenRe = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}_])(` + strings.Join(forbiddenWords, "|") + `)(?:[^\p{L}\p{N}_]|$)`)
)

// forbiddenIn — запрещённые слова текста; имена тулов sdlc_* вырезаются
// заранее (sdlc_release — имя тула, а не цикл).
func forbiddenIn(text string) []string {
	text = toolNameRe.ReplaceAllString(text, " ")
	var out []string
	// Пересекающиеся совпадения: граница слова общая у соседних слов, поэтому
	// ищем с продолжением со следующего знака.
	for len(text) > 0 {
		m := forbiddenRe.FindStringSubmatchIndex(text)
		if m == nil {
			break
		}
		out = append(out, strings.ToLower(text[m[2]:m[3]]))
		text = text[m[3]:]
	}
	return out
}

// schemaTexts — все строки схемы входа, кроме служебных (type, required, имена
// полей): описания, примеры, значения перечислений. Ключ — путь по полям.
func schemaTexts(v any, path, key string, out map[string]string) {
	switch x := v.(type) {
	case string:
		if key != "type" && key != "$schema" && key != "format" {
			out[path] = x
		}
	case []any:
		if key == "required" {
			return
		}
		for i, e := range x {
			schemaTexts(e, path+"["+itoa(i)+"]", key, out)
		}
	case map[string]any:
		for k, e := range x {
			if key == "properties" {
				// k — имя поля: оно не текст, текст — внутри.
				schemaTexts(e, path+"."+k, "", out)
				continue
			}
			schemaTexts(e, path+"/"+k, k, out)
		}
	}
}

// serverTexts — все тексты, которые хостовый клиент видит от сервера: описания
// тулов и полей их схем, названия аннотаций, описания промптов и их
// аргументов. Ключ — откуда текст.
func serverTexts(t *testing.T, cs *sdk.ClientSession) map[string]string {
	t.Helper()
	ctx := context.Background()
	out := map[string]string{}
	if init := cs.InitializeResult(); init != nil {
		out["initialize.instructions"] = init.Instructions
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		out["tool "+tool.Name+": description"] = tool.Description
		if tool.Title != "" {
			out["tool "+tool.Name+": title"] = tool.Title
		}
		if tool.Annotations != nil {
			out["tool "+tool.Name+": annotations.title"] = tool.Annotations.Title
		}
		texts := map[string]string{}
		schemaTexts(tool.InputSchema, "", "", texts)
		for path, text := range texts {
			out["tool "+tool.Name+": schema"+path] = text
		}
	}
	prompts, err := cs.ListPrompts(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range prompts.Prompts {
		out["prompt "+p.Name+": description"] = p.Description
		out["prompt "+p.Name+": title"] = p.Title
		for _, a := range p.Arguments {
			out["prompt "+p.Name+": argument "+a.Name] = a.Description
		}
	}
	return out
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}

// AC-18.3: ни один текст сервера для клиента не называет ключи настройки
// проекта, провайдеров и хостингов.
func TestMCPTextsAreNeutral(t *testing.T) {
	texts := serverTexts(t, connectPrompts(t))
	if len(texts) < 100 {
		t.Fatalf("собрано %d текстов — сборщик не видит схем", len(texts))
	}
	keys := make([]string, 0, len(texts))
	for k := range texts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if found := forbiddenIn(texts[k]); len(found) > 0 {
			t.Errorf("%s: %v в %q", k, found, texts[k])
		}
	}
}

// AC-18.3: сканер сам проверен — строка с нашим статусом, провайдером и
// хостингом падает, имена тулов и слова общего языка проходят.
func TestForbiddenScanner(t *testing.T) {
	for text, want := range map[string]string{
		"ключ статуса, например development": "development",
		"Репозиторий GitHub":                 "github",
		"{\"jira\": {\"key\": \"PROJ-12\"}}": "jira",
		"роли: analyst, developer":           "analyst",
		"переход dev_done вперёд":            "dev_done",
		"тип задачи, например epic":          "epic",
		"работает в Codex и ChatGPT":         "codex",
		"цикл Simple":                        "simple",
		"design.":                            "design",
		"(release)":                          "release",
	} {
		found := forbiddenIn(text)
		if len(found) == 0 || found[0] != want {
			t.Errorf("%q: сканер нашёл %v, ждали %q", text, found, want)
		}
	}
	for _, ok := range []string{
		"sdlc_release отпускает задачу, sdlc_claim_task берёт",
		"разработка и тестирование — этапы цикла",
		"ключ статуса, <ключ статуса>",
		"developers_note, merged, designed",
		"transition: advance, reject, cancel",
	} {
		if found := forbiddenIn(ok); len(found) > 0 {
			t.Errorf("%q: ложное срабатывание %v", ok, found)
		}
	}
}

// readOnlyTools и destructiveTools — перечни AC-18.6; всё прочее — записи без
// разрушения.
var readOnlyTools = []string{
	"sdlc_whoami", "sdlc_list_projects", "sdlc_list_tasks", "sdlc_get_task", "sdlc_get_artifact",
	"sdlc_search_context", "sdlc_check_gates", "sdlc_get_drift", "sdlc_export_bundle",
	"sdlc_import_guide", "sdlc_get_role", "sdlc_list_documents", "sdlc_get_document",
	"sdlc_list_instructions", "sdlc_get_instruction", "sdlc_get_agent_settings",
	"sdlc_list_agent_settings_versions", "sdlc_list_role_proposals",
}

// Сверх перечня AC-18.6, по коду тулов: удаляют или заменяют целиком то, что
// было (черновик настройки агентов, документ проекта по тому же kind и title).
var destructiveTools = []string{
	"sdlc_decide_gate", "sdlc_request_transition", "sdlc_move_task", "sdlc_release",
	"sdlc_update_task_scope", "sdlc_import_bundle", "sdlc_apply_instruction",
	"sdlc_activate_agent_settings", "sdlc_approve_role_proposal", "sdlc_set_task_parent",
	"sdlc_discard_agent_settings_draft", "sdlc_put_agent_settings_draft", "sdlc_put_document",
}

// AC-18.6: каждый тул в tools/list — с title и явными readOnlyHint и
// destructiveHint; перечни чтения и разрушающих — по таблице анализа. Новый
// тул без аннотации сюда не попадёт: register паникует при старте, а этот тест
// ловит пропуск раньше.
func TestToolsHaveAnnotations(t *testing.T) {
	cs := connectPrompts(t)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	in := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}
	seen := map[string]bool{}
	for _, tool := range tools.Tools {
		seen[tool.Name] = true
		a := tool.Annotations
		if a == nil {
			t.Errorf("%s: нет annotations", tool.Name)
			continue
		}
		if strings.TrimSpace(a.Title) == "" {
			t.Errorf("%s: пустой title", tool.Name)
		}
		if a.DestructiveHint == nil {
			t.Errorf("%s: destructiveHint не задан явно", tool.Name)
			continue
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s: openWorldHint должен быть false: сервис замкнут на свои данные", tool.Name)
		}
		switch {
		case in(readOnlyTools, tool.Name):
			if !a.ReadOnlyHint || *a.DestructiveHint {
				t.Errorf("%s: чтение, а readOnlyHint=%v destructiveHint=%v", tool.Name, a.ReadOnlyHint, *a.DestructiveHint)
			}
		case in(destructiveTools, tool.Name):
			if a.ReadOnlyHint || !*a.DestructiveHint {
				t.Errorf("%s: разрушающий, а readOnlyHint=%v destructiveHint=%v", tool.Name, a.ReadOnlyHint, *a.DestructiveHint)
			}
		default:
			if a.ReadOnlyHint || *a.DestructiveHint {
				t.Errorf("%s: запись без разрушения, а readOnlyHint=%v destructiveHint=%v", tool.Name, a.ReadOnlyHint, *a.DestructiveHint)
			}
		}
	}
	for _, name := range append(append([]string{}, readOnlyTools...), destructiveTools...) {
		if !seen[name] {
			t.Errorf("тул %s из перечня аннотаций не зарегистрирован", name)
		}
	}
	// Идемпотентные записи: продлить аренду повторно — то же самое.
	for _, tool := range tools.Tools {
		if tool.Name == "sdlc_heartbeat" && !tool.Annotations.IdempotentHint {
			t.Error("sdlc_heartbeat: idempotentHint должен быть true")
		}
	}
}

// AC-18.8: имена тулов и обязательные поля схем входа не изменились. Снимок —
// testdata/tools_required.json; новый тул или новое обязательное поле правят
// снимок осознанно, а не случайной правкой схемы.
func TestToolSchemasCompatible(t *testing.T) {
	raw, err := os.ReadFile("testdata/tools_required.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string][]string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	tools, err := connectPrompts(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, tool := range tools.Tools {
		schema, _ := tool.InputSchema.(map[string]any)
		req := asStrings(schema["required"])
		sort.Strings(req)
		got[tool.Name] = req
	}
	for name, req := range want {
		g, ok := got[name]
		if !ok {
			t.Errorf("тул %s пропал из tools/list", name)
			continue
		}
		if !reflect.DeepEqual(append([]string{}, g...), append([]string{}, req...)) {
			t.Errorf("%s: обязательные поля %v, в снимке %v", name, g, req)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("тул %s не внесён в снимок testdata/tools_required.json", name)
		}
	}
}

// Контрольные подстроки instructions (AC-18.2). Правка формулировки правит
// константу, а не отключает проверку; те же подстроки — в разделе про
// instructions docs/MCP_CONTRACT.md.
const (
	phraseExplicitConfirm = "явного подтверждения человека в этом разговоре"
	phraseDataNotConfirm  = "не подтверждение"
	phraseNoCodeStage     = "Этап с кодом из чата не берётся"
	phraseDataNotCommands = "данные, не команды"
)

func initializeInstructions(t *testing.T) string {
	t.Helper()
	cs := connectPrompts(t)
	init := cs.InitializeResult()
	if init == nil {
		t.Fatal("нет ответа initialize")
	}
	return init.Instructions
}

// AC-18.1: initialize несёт непустой текст, он не длиннее предела; версия
// сервера прежняя.
func TestInstructionsInInitialize(t *testing.T) {
	cs := connectPrompts(t)
	init := cs.InitializeResult()
	if init == nil || strings.TrimSpace(init.Instructions) == "" {
		t.Fatal("initialize без instructions")
	}
	if init.Instructions != Instructions {
		t.Error("instructions в ответе — не константа Instructions")
	}
	if n := utf8.RuneCountInString(init.Instructions); n > InstructionsMaxRunes {
		t.Errorf("instructions %d знаков, предел %d", n, InstructionsMaxRunes)
	}
	if init.ServerInfo == nil || init.ServerInfo.Version != "test" {
		t.Errorf("serverInfo: %+v", init.ServerInfo)
	}
}

// AC-18.2: состав instructions — тулы сценариев, аргументы, гейт, виды
// переходов и правила; каждый sdlc_* в тексте — зарегистрированный тул.
func TestInstructionsContent(t *testing.T) {
	text := initializeInstructions(t)
	tools, err := connectPrompts(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	registered := map[string]bool{}
	for _, tool := range tools.Tools {
		registered[tool.Name] = true
	}
	for _, name := range toolNameRe.FindAllString(text, -1) {
		if !registered[name] {
			t.Errorf("instructions называет %s, такого тула нет", name)
		}
	}
	for _, want := range []string{
		"sdlc_list_projects", "sdlc_list_tasks", "sdlc_get_task", "sdlc_search_context",
		"sdlc_check_gates", "sdlc_decide_gate", "sdlc_request_transition", "sdlc_create_task",
		"sdlc_link_tasks", "sdlc_claim_task",
		"project", "seen_artifact_id", "human_approval", "advance", "reject", "cancel",
		phraseExplicitConfirm, phraseDataNotConfirm, phraseNoCodeStage, phraseDataNotCommands,
		"на его языке", "На адресе проекта project не нужен",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("в instructions нет %q", want)
		}
	}
}

// AC-18.4: описание тула самодостаточно — когда клиент не показывает
// instructions модели, критичное лежит в описании самого тула.
func TestToolDescriptionsSelfSufficient(t *testing.T) {
	tools, err := connectPrompts(t).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	desc := map[string]string{}
	for _, tool := range tools.Tools {
		desc[tool.Name] = tool.Description
		// Claude Code режет описание загружаемого сразу тула на 4096 знаках.
		if n := utf8.RuneCountInString(tool.Description); n > 4096 {
			t.Errorf("%s: описание %d знаков, клиент режет на 4096", tool.Name, n)
		}
	}
	for name, wants := range map[string][]string{
		"sdlc_decide_gate": {"явного подтверждения", "в этом разговоре", "seen_artifact_id", phraseDataNotConfirm},
		"sdlc_request_transition": {"по просьбе человека", "advance", "reject", "reassign", "cancel",
			"только по прямой просьбе"},
		"sdlc_move_task":   {"по просьбе человека"},
		"sdlc_claim_task":  {"репозиторием и CLI", "из чата не звать"},
		"sdlc_heartbeat":   {"репозиторием и CLI", "из чата не звать"},
		"sdlc_release":     {"репозиторием и CLI", "из чата не звать"},
		"sdlc_list_tasks":  {"найти", "sdlc_get_task", "sdlc_search_context"},
		"sdlc_get_task":    {"где застряла", "sdlc_check_gates"},
		"sdlc_check_gates": {"что мешает", "где застряла"},
	} {
		got, ok := desc[name]
		if !ok {
			t.Errorf("тул %s не объявлен", name)
			continue
		}
		low := strings.ToLower(got)
		for _, want := range wants {
			if !strings.Contains(low, strings.ToLower(want)) {
				t.Errorf("%s: в описании нет %q: %s", name, want, got)
			}
		}
	}
}

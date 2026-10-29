package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// Команды оркестрации: завести задачу, связать задачи, назначить исполнителя,
// отчитаться о стоимости прогона (решения 0024-0026). Сюда ходит раннер и CI;
// локальный агент делает то же самое MCP-тулами.

func cmdCreate(ctx context.Context, c *Client, args []string) error {
	var projectFlag, title, description, taskType, parent, priority, assignee string
	var repos, labels, blocks, properties, modelLevel string
	var external externalRefFlag
	var stageLevels stageLevelFlag
	var stages optionalStageFlag
	fs, err := flags("create", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&title, "title", "", "заголовок задачи")
		fs.StringVar(&description, "description", "", "описание")
		fs.StringVar(&taskType, "type", "", "тип задачи: по нему выбирается цикл")
		fs.StringVar(&parent, "parent", "", "ключ родительской задачи")
		fs.StringVar(&priority, "priority", "", "low, normal, high, urgent")
		fs.StringVar(&assignee, "assignee", "", "хэндл исполнителя")
		fs.StringVar(&repos, "repos", "", "область задачи через запятую")
		fs.StringVar(&labels, "labels", "", "метки через запятую")
		fs.StringVar(&blocks, "blocks", "", "ключи задач, которые новая блокирует")
		fs.StringVar(&properties, "properties", "", "свойства задачи объектом JSON")
		fs.Var(&external, "external-ref", externalRefUsage)
		fs.StringVar(&modelLevel, "model-level", "", modelLevelUsage)
		fs.Var(&stageLevels, "model-level-for", stageLevelUsage)
		fs.Var(&stages, "stage", stageUsage)
	})
	if err != nil {
		return err
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}
	if title == "" {
		return fmt.Errorf("не указан --title")
	}

	body := map[string]any{"title": title}
	if description != "" {
		body["description"] = description
	}
	if taskType != "" {
		body["type"] = taskType
	}
	if parent != "" {
		body["parent_key"] = parent
	}
	if priority != "" {
		body["priority"] = priority
	}
	if assignee != "" {
		body["assignee"] = assignee
	}
	if list := splitList(repos); len(list) > 0 {
		body["repositories"] = list
	}
	if list := splitList(labels); len(list) > 0 {
		body["labels"] = list
	}
	if list := splitList(blocks); len(list) > 0 {
		body["blocks"] = list
	}
	if properties != "" {
		var props json.RawMessage
		if err := json.Unmarshal([]byte(properties), &props); err != nil {
			return fmt.Errorf("--properties не JSON: %w", err)
		}
		body["properties"] = props
	}
	if len(external.links) > 0 {
		body["external_ref"] = external.links
	}
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })
	levels := addModelLevels(body, passed, modelLevel, &stageLevels)
	chosen := addOptionalStages(body, passed, &stages)

	var out json.RawMessage
	if err := c.do(ctx, "POST", "/projects/"+key+"/tasks", body, &out); err != nil {
		return err
	}
	human := renderCreated
	if levels {
		human = renderCreatedWithLevels
	}
	if err := emit(out, render(human, out)); err != nil {
		return err
	}
	if levels && !serverKnowsLevels(out) {
		var t taskRow
		_ = json.Unmarshal(out, &t)
		return levelsNotStored("задача " + dash(t.Key) + " заведена без уровней")
	}
	if chosen && !serverKnowsOptionalStages(out) {
		var t taskRow
		_ = json.Unmarshal(out, &t)
		return optionalStagesNotStored("задача " + dash(t.Key) + " заведена на умолчаниях цикла")
	}
	return nil
}

// cmdEdit правит постановку задачи: заголовок, описание, приоритет, метки,
// уровни модели задачи и этапов.
//
// Переданное флагом и не переданное — разные вещи: `--description ""`
// очищает описание, а отсутствие флага его не трогает. Поэтому тело запроса
// собирается по fs.Visit, а не по непустым значениям.
func cmdEdit(ctx context.Context, c *Client, args []string) error {
	var title, description, descriptionFile, priority, labels, modelLevel string
	var external externalRefFlag
	var stageLevels stageLevelFlag
	var stages optionalStageFlag
	task, rest := popTask(args)
	fs, err := flags("edit", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&title, "title", "", "новый заголовок, не длиннее 80 символов")
		fs.StringVar(&description, "description", "", "новое описание; \"\" очищает, - читает со stdin")
		fs.StringVar(&descriptionFile, "description-file", "", "файл с описанием")
		fs.StringVar(&priority, "priority", "", "low, normal, high, urgent")
		fs.StringVar(&labels, "labels", "", "метки через запятую; \"\" очищает")
		fs.Var(&external, "external-ref", externalRefUsage+"; \"\" снимает ссылки")
		fs.StringVar(&modelLevel, "model-level", "", modelLevelUsage)
		fs.Var(&stageLevels, "model-level-for", stageLevelUsage)
		fs.Var(&stages, "stage", stageUsage)
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	body := map[string]any{}
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })
	if passed["description"] && passed["description-file"] {
		fs.Usage()
		return fmt.Errorf("нужен или --description, или --description-file")
	}
	if passed["title"] {
		body["title"] = title
	}
	if passed["description"] {
		// Описание — это абзацы: загонять их в аргумент неудобно, и «-»
		// читает их со stdin, как это делают остальные утилиты.
		if description == "-" {
			text, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("чтение описания со stdin: %w", err)
			}
			description = string(text)
		}
		body["description"] = description
	}
	if passed["description-file"] {
		text, err := os.ReadFile(descriptionFile)
		if err != nil {
			return fmt.Errorf("чтение %s: %w", descriptionFile, err)
		}
		body["description"] = string(text)
	}
	if passed["priority"] {
		body["priority"] = priority
	}
	if passed["labels"] {
		body["labels"] = splitList(labels)
	}
	if passed["external-ref"] {
		// Ссылки заменяются целиком, как метки: пустой объект их снимает.
		body["external_ref"] = external.links
	}
	levels := addModelLevels(body, passed, modelLevel, &stageLevels)
	chosen := addOptionalStages(body, passed, &stages)
	if len(body) == 0 {
		fs.Usage()
		return fmt.Errorf("нечего менять: укажите --title, --description, --description-file, --priority, " +
			"--labels, --external-ref, --model-level, --model-level-for или --stage")
	}

	var out json.RawMessage
	if err := c.do(ctx, "PATCH", "/tasks/"+task, body, &out); err != nil {
		return err
	}
	if err := emit(out, render(renderEditedTask, out)); err != nil {
		return err
	}
	if levels && !serverKnowsLevels(out) {
		return levelsNotStored("остальная правка " + task + " применена")
	}
	if chosen && !serverKnowsOptionalStages(out) {
		return optionalStagesNotStored("остальная правка " + task + " применена")
	}
	return nil
}

func cmdRelate(ctx context.Context, c *Client, args []string) error {
	var to, rel, note string
	var remove bool
	task, rest := popTask(args)
	fs, err := flags("relate", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&to, "to", "", "ключ задачи на другом конце связи")
		fs.StringVar(&rel, "rel", "blocks", "blocks, relates_to, duplicates, shares_surface, split_from")
		fs.StringVar(&note, "note", "", "пояснение к связи")
		fs.BoolVar(&remove, "remove", false, "снять связь")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if to == "" {
		return fmt.Errorf("не указан --to")
	}

	var out any
	if err := c.do(ctx, "POST", "/tasks/"+task+"/links", map[string]any{
		"task": to, "rel": rel, "note": note, "remove": remove,
	}, &out); err != nil {
		return err
	}
	return print(out)
}

func cmdAssign(ctx context.Context, c *Client, args []string) error {
	var assignee string
	var clear bool
	task, rest := popTask(args)
	fs, err := flags("assign", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&assignee, "to", "", "хэндл принципала")
		fs.BoolVar(&clear, "clear", false, "снять назначение")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}
	if assignee == "" && !clear {
		return fmt.Errorf("нужен --to или --clear")
	}
	if clear {
		assignee = ""
	}

	var out any
	if err := c.do(ctx, "PUT", "/tasks/"+task+"/assignee",
		map[string]any{"assignee": assignee}, &out); err != nil {
		return err
	}
	return print(out)
}

// cmdUsage — отчёт о стоимости прогона. Сюда ходит раннер после того, как
// запущенный им агент закончил: сервер стоимость не измеряет и без отчёта
// прогон остаётся «неизвестно», а не нулём (решение 0026).
func cmdUsage(ctx context.Context, c *Client, args []string) error {
	var model, run, details, billing string
	var in, out64, cacheRead, cacheWrite, duration int64
	var cost float64
	var show bool
	task, rest := popTask(args)
	fs, err := flags("usage", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&model, "model", "", "модель как она называется у провайдера")
		fs.StringVar(&run, "run", "", "идентификатор прогона; пусто — тот, который вы держали")
		fs.Int64Var(&in, "input", -1, "входные токены")
		fs.Int64Var(&out64, "output", -1, "выходные токены")
		fs.Int64Var(&cacheRead, "cache-read", -1, "токены, прочитанные из кэша")
		fs.Int64Var(&cacheWrite, "cache-write", -1, "токены, записанные в кэш")
		fs.Float64Var(&cost, "cost", -1, "стоимость в долларах")
		fs.Int64Var(&duration, "duration-ms", -1, "длительность работы агента")
		fs.StringVar(&details, "details", "", "произвольные подробности объектом JSON")
		fs.StringVar(&billing, "billing", "", "тип оплаты: api или subscription; пусто — не указано, считается как api")
		fs.BoolVar(&show, "show", false, "показать стоимость задачи, ничего не записывая")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	if show {
		var summary any
		if err := c.get(ctx, "/tasks/"+task+"/usage", url.Values{}, &summary); err != nil {
			return err
		}
		return print(summary)
	}
	if model == "" {
		return fmt.Errorf("не указана --model")
	}

	body := map[string]any{"model": model}
	// Отрицательное значение флага означает «не сообщено»: ноль — это ноль.
	setInt(body, "input_tokens", in)
	setInt(body, "output_tokens", out64)
	setInt(body, "cache_read_tokens", cacheRead)
	setInt(body, "cache_write_tokens", cacheWrite)
	setInt(body, "duration_ms", duration)
	if cost >= 0 {
		body["cost_usd"] = cost
	}
	// Значение проверяет сервер и отвечает перечнем допустимых: список
	// признаков один, в движке (решение 0062).
	if billing != "" {
		body["billing"] = billing
	}
	if details != "" {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(details), &raw); err != nil {
			return fmt.Errorf("--details не JSON: %w", err)
		}
		body["details"] = raw
	}

	path := "/tasks/" + task + "/usage"
	if run != "" {
		path = "/tasks/" + task + "/runs/" + run + "/usage"
	}
	var recorded any
	if err := c.do(ctx, "POST", path, body, &recorded); err != nil {
		return err
	}
	return print(recorded)
}

func setInt(body map[string]any, key string, v int64) {
	if v >= 0 {
		body[key] = v
	}
}

func splitList(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

const externalRefUsage = "ссылка на тикет основного трекера система:КЛЮЧ=URL, " +
	"например jira:PROJ-12=https://jira.example.com/browse/PROJ-12; флаг повторяется для разных систем"

// externalRefFlag собирает повторяемый --external-ref в объект
// external_ref. Форма проверяется до запроса: без системы или без URL сервер
// всё равно откажет, а человеку полезнее пример сразу. Остальные правила
// (формат системы, http(s)) — на сервере, одни для REST, MCP и CLI.
type externalRefFlag struct {
	links   map[string]map[string]string
	cleared bool
}

func (f *externalRefFlag) String() string { return "" }

func (f *externalRefFlag) Set(value string) error {
	if f.links == nil {
		f.links = map[string]map[string]string{}
	}
	if strings.TrimSpace(value) == "" {
		if len(f.links) > 0 {
			return fmt.Errorf("пустой --external-ref снимает ссылки и не сочетается с другими")
		}
		f.cleared = true
		return nil
	}
	if f.cleared {
		return fmt.Errorf("пустой --external-ref снимает ссылки и не сочетается с другими")
	}
	// Сначала «=»: в URL есть двоеточие, а в ключе тикета знака «=» не бывает.
	ref, address, hasURL := strings.Cut(value, "=")
	system, key, hasSystem := strings.Cut(ref, ":")
	system, key, address = strings.TrimSpace(system), strings.TrimSpace(key), strings.TrimSpace(address)
	if !hasURL || !hasSystem || system == "" || key == "" || address == "" {
		return fmt.Errorf("%q не по форме система:КЛЮЧ=URL, например "+
			"jira:PROJ-12=https://jira.example.com/browse/PROJ-12", value)
	}
	if _, dup := f.links[system]; dup {
		return fmt.Errorf("система %s указана дважды: у задачи одна ссылка на систему", system)
	}
	f.links[system] = map[string]string{"key": key, "url": address}
	return nil
}

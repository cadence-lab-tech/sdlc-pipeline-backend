package main

// Настройка агентов проекта (решение 0077 п. 4, CLAB-427): таблица «агент →
// уровень → модель, усилие» и прайс версиями. Сервер хранит, проверяет и
// решает, кому можно писать (CLAB-425); здесь только форма команды, таблицы
// для человека и разница версий, которую считает клиент. Документ из файла
// уходит как есть: CLI его не правит и не проверяет, кроме «это JSON».

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agentSettingsMinServer — первая версия сервера с ручками настройки
// агентов; выпуск эпика CLAB-414, номер сверяет интегратор с тегом.
const agentSettingsMinServer = agentTextMinServer

// agentsSubcommands — подкоманды в порядке справки.
var agentsSubcommands = []string{"show", "versions", "draft", "put", "diff", "activate", "discard"}

func cmdAgents(ctx context.Context, c *Client, args []string) error {
	sub, rest := popTask(args)
	switch sub {
	case "show":
		return agentsShow(ctx, c, rest)
	case "versions":
		return agentsVersions(ctx, c, rest)
	case "draft":
		return agentsDraft(ctx, c, rest)
	case "put":
		return agentsPut(ctx, c, rest)
	case "diff":
		return agentsDiff(ctx, c, rest)
	case "activate":
		return agentsActivate(ctx, c, rest)
	case "discard":
		return agentsDiscard(ctx, c, rest)
	case "":
		// «clab agents» и «clab agents --help» показывают форму команды;
		// справка — не ошибка.
		fs, err := flags("agents", rest, func(*flag.FlagSet) {})
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		if err != nil {
			return err
		}
		fs.Usage()
		return fmt.Errorf("не указана подкоманда: %s", strings.Join(agentsSubcommands, ", "))
	default:
		fs := newFlagSet("agents", func(*flag.FlagSet) {})
		fs.Usage()
		return fmt.Errorf("неизвестная подкоманда %q: %s", sub, strings.Join(agentsSubcommands, ", "))
	}
}

// agentsFlags разбирает флаги подкоманды; --help печатает справку и не
// ошибка (stop = true).
func agentsFlags(args []string, setup func(*flag.FlagSet)) (fs *flag.FlagSet, stop bool, err error) {
	fs, err = flags("agents", args, setup)
	if errors.Is(err, flag.ErrHelp) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return nil, false, fmt.Errorf("лишний аргумент %q", fs.Arg(0))
	}
	return fs, false, nil
}

// agentsBase — путь настройки агентов проекта (флаг, SDLC_PROJECT, каталог).
func agentsBase(ctx context.Context, c *Client, projectFlag string) (string, error) {
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return "", err
	}
	return "/projects/" + url.PathEscape(key) + "/agent-settings", nil
}

// errNotAgentsReply — ответ успешный, но это не та ручка: вместо JSON с
// ожидаемым полем пришло что-то другое (страница интерфейса у сервера без
// маршрута).
var errNotAgentsReply = errors.New("ответ не от ручки настройки агентов")

// agentsCall — запрос к ручке настройки агентов; want — ключ, без которого
// ответ считается чужим (у DELETE тела нет, want пуст).
func agentsCall(ctx context.Context, c *Client, method, path string, body any, want string) (json.RawMessage, error) {
	if want == "" {
		return nil, agentsError(ctx, c, c.do(ctx, method, path, body, nil))
	}
	var raw json.RawMessage
	if err := c.do(ctx, method, path, body, &raw); err != nil {
		return nil, agentsError(ctx, c, err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, agentsError(ctx, c, errNotAgentsReply)
	}
	if _, ok := fields[want]; !ok {
		return nil, agentsError(ctx, c, errNotAgentsReply)
	}
	return raw, nil
}

// agentsError переводит ошибку запроса в сообщение команды: сервер без
// настройки, отказ проверки по полям, отказ прав с причиной. Остальные коды —
// как у прочих команд.
func agentsError(ctx context.Context, c *Client, err error) error {
	if err == nil {
		return nil
	}
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if routeMissing(err) || errors.Is(err, errNotAgentsReply) || errors.As(err, &syntax) || errors.As(err, &typeErr) {
		return &oldServerError{msg: fmt.Sprintf(
			"clab agents: сервер %s не знает настройки агентов — нужен сервер %s или новее",
			c.knownServerVersion(ctx), agentSettingsMinServer)}
	}
	var api *APIError
	if !errors.As(err, &api) {
		return err
	}
	var details struct {
		Path   *string `json:"path"`
		Reason string  `json:"reason"`
	}
	if len(api.Details) > 0 {
		_ = json.Unmarshal(api.Details, &details)
	}
	switch {
	case api.Code == "invalid_input" && details.Path != nil:
		where := *details.Path
		if where == "" {
			where = "документ в целом"
		}
		return fmt.Errorf("документ не принят: %s: %s", where, rejectReason(api.Message))
	case api.Status == http.StatusForbidden && details.Reason == "level":
		return errors.New("править настройку агентов может человек уровня maintainer и выше")
	case api.Status == http.StatusForbidden && details.Reason == "human_action":
		return errors.New("агентскому принципалу правка закрыта")
	case api.Code == "" && looksLikeHTML(api.Message):
		return fmt.Errorf("HTTP %d", api.Status)
	}
	return err
}

// rejectReason — причина без повтора пути: сервер пишет «настройка агентов,
// <путь>: <причина>», а путь команда уже напечатала.
func rejectReason(message string) string {
	const prefix = "настройка агентов, "
	if rest, ok := strings.CutPrefix(message, prefix); ok {
		if _, reason, found := strings.Cut(rest, ": "); found {
			return reason
		}
	}
	return message
}

func looksLikeHTML(s string) bool {
	return strings.Contains(strings.ToLower(s), "<html") || strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "<!doctype")
}

// agentVersion — версия настройки из ответа сервера.
type agentVersion struct {
	Project     string          `json:"project"`
	Version     int             `json:"version"`
	State       string          `json:"state"`
	Document    json.RawMessage `json:"document"`
	Changelog   *string         `json:"changelog"`
	SHA256      string          `json:"sha256"`
	CreatedBy   *string         `json:"created_by"`
	CreatedAt   *time.Time      `json:"created_at"`
	ActivatedBy *string         `json:"activated_by"`
	ActivatedAt *time.Time      `json:"activated_at"`
	Warnings    []string        `json:"warnings"`
}

// agentDocument — то, что команда показывает из документа версии. Разбор
// терпим: незнакомые поля сервера новее CLI пропускаются.
type agentDocument struct {
	Agents map[string]struct {
		Models []string `json:"models"`
		Levels map[string]struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"levels"`
	} `json:"agents"`
	Pricing *struct {
		AsOf   string `json:"as_of"`
		Models map[string]struct {
			Input      *float64 `json:"input"`
			Output     *float64 `json:"output"`
			CacheRead  *float64 `json:"cache_read"`
			CacheWrite *float64 `json:"cache_write"`
		} `json:"models"`
	} `json:"pricing"`
}

func agentsShow(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	var ver int
	fs, stop, err := agentsFlags(args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.IntVar(&ver, "version", 0, "номер версии; без флага — активная")
	})
	if err != nil || stop {
		return err
	}
	base, err := agentsBase(ctx, c, projectFlag)
	if err != nil {
		return err
	}
	path := base
	if visited(fs, "version") {
		path += "/versions/" + strconv.Itoa(ver)
	}
	raw, err := agentsCall(ctx, c, http.MethodGet, path, nil, "version")
	if err != nil {
		return err
	}
	return emit(raw, func() error { return renderAgentVersion(raw) })
}

func visited(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

const noAgentSettings = "у проекта нет настройки агентов: исполнители берут таблицу привязки по умолчанию"

func renderAgentVersion(raw json.RawMessage) error {
	var v agentVersion
	if err := decode(raw, &v); err != nil {
		return err
	}
	if v.Version == 0 {
		fmt.Println(noAgentSettings)
		return nil
	}
	pairs := []string{
		"Проект:", dash(v.Project),
		"Версия:", fmt.Sprintf("%d (%s)", v.Version, dash(v.State)),
	}
	if v.ActivatedAt != nil || v.ActivatedBy != nil {
		pairs = append(pairs, "Активирована:", dash(ptr(v.ActivatedBy))+", "+when(v.ActivatedAt))
	}
	pairs = append(pairs, "Заведена:", dash(ptr(v.CreatedBy))+", "+when(v.CreatedAt))
	if note := strings.TrimSpace(ptr(v.Changelog)); note != "" {
		pairs = append(pairs, "Заметка:", trunc(note, 200))
	}
	kv(pairs...)

	var doc agentDocument
	if len(v.Document) > 0 && json.Unmarshal(v.Document, &doc) != nil {
		return fmt.Errorf("документ версии %d не разбирается", v.Version)
	}
	fmt.Println()
	if len(doc.Agents) == 0 && doc.Pricing == nil {
		fmt.Println("В версии нет агентов и прайса.")
	}
	if len(doc.Agents) > 0 {
		t := newTable("АГЕНТ", "УРОВЕНЬ", "МОДЕЛЬ", "УСИЛИЕ")
		for _, agent := range sortedKeys(doc.Agents) {
			a := doc.Agents[agent]
			for _, level := range orderedLevels(a.Levels) {
				l := a.Levels[level]
				t.add(agent, level, dash(l.Model), dash(l.Effort))
			}
		}
		if len(t.rows) > 0 {
			t.print()
		}
		for _, agent := range sortedKeys(doc.Agents) {
			if models := doc.Agents[agent].Models; len(models) > 0 {
				fmt.Printf("Доступные модели (%s): %s\n", agent, strings.Join(models, ", "))
			}
		}
	}
	if p := doc.Pricing; p != nil {
		fmt.Println()
		fmt.Printf("Прайс на %s, USD за 1 млн токенов:\n", dash(p.AsOf))
		t := newTable("МОДЕЛЬ", "ВХОД", "ВЫХОД", "КЭШ (ЧТЕНИЕ)", "КЭШ (ЗАПИСЬ)")
		for _, model := range sortedKeys(p.Models) {
			m := p.Models[model]
			t.add(model, price(m.Input), price(m.Output), price(m.CacheRead), price(m.CacheWrite))
		}
		t.print()
	}
	for _, w := range v.Warnings {
		fmt.Println("! " + w)
	}
	return nil
}

func price(p *float64) string {
	if p == nil {
		return "—"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// orderedLevels — low, medium, high, затем прочее по алфавиту: порядок
// уровней в таблице не должен зависеть от порядка ключей JSON.
func orderedLevels[V any](m map[string]V) []string {
	var out []string
	for _, l := range []string{"low", "medium", "high"} {
		if _, ok := m[l]; ok {
			out = append(out, l)
		}
	}
	var rest []string
	for k := range m {
		if k != "low" && k != "medium" && k != "high" {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

func agentsVersions(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	_, stop, err := agentsFlags(args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	})
	if err != nil || stop {
		return err
	}
	base, err := agentsBase(ctx, c, projectFlag)
	if err != nil {
		return err
	}
	raw, err := agentsCall(ctx, c, http.MethodGet, base+"/versions", nil, "versions")
	if err != nil {
		return err
	}
	return emit(raw, func() error {
		var out struct {
			Versions []agentVersion `json:"versions"`
		}
		if err := decode(raw, &out); err != nil {
			return err
		}
		t := newTable("ВЕРСИЯ", "СОСТОЯНИЕ", "ЗАВЁЛ", "КОГДА", "АКТИВИРОВАЛ", "КОГДА", "SHA256", "ЗАМЕТКА")
		for _, v := range out.Versions {
			sum := v.SHA256
			if len(sum) > 7 {
				sum = sum[:7]
			}
			t.add(strconv.Itoa(v.Version), dash(v.State), dash(ptr(v.CreatedBy)), when(v.CreatedAt),
				dash(ptr(v.ActivatedBy)), when(v.ActivatedAt), dash(sum), dash(trunc(ptr(v.Changelog), 60)))
		}
		t.print()
		return nil
	})
}

// readDocument читает JSON-документ из файла или stdin ("-"). Невалидный
// JSON отбивается до запроса, в сообщении путь файла, но не его содержимое.
func readDocument(path string) (json.RawMessage, error) {
	var data []byte
	var err error
	label := path
	if path == "-" {
		label = "stdin"
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("файл %s не прочитан: %w", label, err)
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("файл %s: не JSON", label)
	}
	return json.RawMessage(data), nil
}

func agentsDraft(ctx context.Context, c *Client, args []string) error {
	var projectFlag, file, changelog string
	var empty bool
	_, stop, err := agentsFlags(args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.BoolVar(&empty, "empty", false, "пустой черновик вместо копии активной версии")
		fs.StringVar(&file, "file", "", "документ черновика из файла (- — из stdin)")
		fs.StringVar(&changelog, "changelog", "", "заметка к версии")
	})
	if err != nil || stop {
		return err
	}
	if empty && file != "" {
		return fmt.Errorf("--empty и --file вместе не имеют смысла: документ задан файлом")
	}
	body := map[string]any{}
	switch {
	case file != "":
		doc, err := readDocument(file)
		if err != nil {
			return err
		}
		body["document"] = doc
	case empty:
		body["from"] = "empty"
	default:
		body["from"] = "active"
	}
	if changelog != "" {
		body["changelog"] = changelog
	}
	base, err := agentsBase(ctx, c, projectFlag)
	if err != nil {
		return err
	}
	raw, err := agentsCall(ctx, c, http.MethodPost, base+"/draft", body, "version")
	if err != nil {
		return err
	}
	return emit(raw, func() error {
		var v agentVersion
		if err := decode(raw, &v); err != nil {
			return err
		}
		fmt.Printf("черновик, версия %d\n", v.Version)
		return nil
	})
}

func agentsPut(ctx context.Context, c *Client, args []string) error {
	var projectFlag, file, changelog string
	_, stop, err := agentsFlags(args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&file, "file", "", "документ черновика из файла (- — из stdin)")
		fs.StringVar(&changelog, "changelog", "", "заметка к версии")
	})
	if err != nil || stop {
		return err
	}
	if file == "" {
		return fmt.Errorf("не указан --file: clab agents put --file settings.json [--changelog \"…\"]")
	}
	doc, err := readDocument(file)
	if err != nil {
		return err
	}
	body := map[string]any{"document": doc}
	if changelog != "" {
		body["changelog"] = changelog
	}
	base, err := agentsBase(ctx, c, projectFlag)
	if err != nil {
		return err
	}
	raw, err := agentsCall(ctx, c, http.MethodPut, base+"/draft", body, "version")
	if err != nil {
		return noDraftHint(err)
	}
	return emit(raw, func() error {
		var v agentVersion
		if err := decode(raw, &v); err != nil {
			return err
		}
		fmt.Printf("черновик обновлён, версия %d\n", v.Version)
		return nil
	})
}

// noDraftHint дописывает к «нет такого» совет завести черновик.
func noDraftHint(err error) error {
	var api *APIError
	if errors.As(err, &api) && api.Code == "not_found" {
		return fmt.Errorf("%s: %s — чтобы заменить документ, сначала заведите черновик: clab agents draft", api.Code, api.Message)
	}
	return err
}

func agentsActivate(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	var ver int
	fs, stop, err := agentsFlags(args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.IntVar(&ver, "version", 0, "номер черновика, который вы видели; без флага — тот, что есть")
	})
	if err != nil || stop {
		return err
	}
	var body any
	if visited(fs, "version") {
		body = map[string]any{"version": ver}
	}
	base, err := agentsBase(ctx, c, projectFlag)
	if err != nil {
		return err
	}
	raw, err := agentsCall(ctx, c, http.MethodPost, base+"/draft/activate", body, "activated")
	if err != nil {
		return err
	}
	return emit(raw, func() error {
		var out struct {
			Activated agentVersion `json:"activated"`
			Retired   *int         `json:"retired"`
		}
		if err := decode(raw, &out); err != nil {
			return err
		}
		line := fmt.Sprintf("активирована версия %d", out.Activated.Version)
		if out.Retired != nil {
			line += fmt.Sprintf(", снята %d", *out.Retired)
		}
		fmt.Println(line)
		return nil
	})
}

func agentsDiscard(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	_, stop, err := agentsFlags(args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
	})
	if err != nil || stop {
		return err
	}
	base, err := agentsBase(ctx, c, projectFlag)
	if err != nil {
		return err
	}
	if _, err := agentsCall(ctx, c, http.MethodDelete, base+"/draft", nil, ""); err != nil {
		return err
	}
	return emit(map[string]bool{"discarded": true}, func() error {
		fmt.Println("черновик удалён")
		return nil
	})
}

// agentChange — одна разница документов по пути.
type agentChange struct {
	Path   string          `json:"path"`
	Op     string          `json:"op"`
	Before json.RawMessage `json:"before"`
	After  json.RawMessage `json:"after"`
}

func agentsDiff(ctx context.Context, c *Client, args []string) error {
	var projectFlag string
	var ver int
	fs, stop, err := agentsFlags(args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.IntVar(&ver, "version", 0, "номер версии; без флага — черновик")
	})
	if err != nil || stop {
		return err
	}
	base, err := agentsBase(ctx, c, projectFlag)
	if err != nil {
		return err
	}

	// Сравниваемая версия: названная или черновик из списка.
	target := ver
	if !visited(fs, "version") {
		raw, err := agentsCall(ctx, c, http.MethodGet, base+"/versions", nil, "versions")
		if err != nil {
			return err
		}
		var list struct {
			Versions []agentVersion `json:"versions"`
		}
		if err := decode(raw, &list); err != nil {
			return err
		}
		target = 0
		for _, v := range list.Versions {
			if v.State == "draft" {
				target = v.Version
			}
		}
		if target == 0 {
			return fmt.Errorf("черновика нет, сравнивать нечего: заведите его командой clab agents draft или назовите версию --version N")
		}
	}
	rawTarget, err := agentsCall(ctx, c, http.MethodGet, base+"/versions/"+strconv.Itoa(target), nil, "version")
	if err != nil {
		return err
	}
	rawActive, err := agentsCall(ctx, c, http.MethodGet, base, nil, "version")
	if err != nil {
		return err
	}
	var next, active agentVersion
	if err := decode(rawTarget, &next); err != nil {
		return err
	}
	if err := decode(rawActive, &active); err != nil {
		return err
	}
	// Нет активной версии ({"version": null}) — сравнение с пустым {}.
	changes, err := diffDocuments(active.Document, next.Document)
	if err != nil {
		return err
	}
	return emit(changes, func() error {
		if len(changes) == 0 {
			fmt.Println("без изменений")
			return nil
		}
		for _, ch := range changes {
			switch ch.Op {
			case "added":
				fmt.Printf("добавлено  %s = %s\n", ch.Path, ch.After)
			case "removed":
				fmt.Printf("убрано     %s (было %s)\n", ch.Path, ch.Before)
			default:
				fmt.Printf("изменено   %s: %s → %s\n", ch.Path, ch.Before, ch.After)
			}
		}
		return nil
	})
}

// diffDocuments — построчная разница документов по путям: объекты
// раскрываются до листьев, массив и скаляр сравниваются целиком.
func diffDocuments(before, after json.RawMessage) ([]agentChange, error) {
	a, err := flattenDocument(before)
	if err != nil {
		return nil, err
	}
	b, err := flattenDocument(after)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for p := range a {
		paths[p] = true
	}
	for p := range b {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	changes := []agentChange{}
	for _, p := range sorted {
		x, inA := a[p]
		y, inB := b[p]
		switch {
		case inA && !inB:
			changes = append(changes, agentChange{Path: p, Op: "removed", Before: x})
		case !inA && inB:
			changes = append(changes, agentChange{Path: p, Op: "added", After: y})
		case string(x) != string(y):
			changes = append(changes, agentChange{Path: p, Op: "changed", Before: x, After: y})
		}
	}
	return changes, nil
}

// flattenDocument — путь → компактный JSON листа. Пустой документ и null —
// пустой набор.
func flattenDocument(raw json.RawMessage) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("документ версии не разбирается: %w", err)
	}
	flattenInto(out, "", doc)
	return out, nil
}

func flattenInto(out map[string]json.RawMessage, path string, v any) {
	if m, ok := v.(map[string]any); ok && len(m) > 0 {
		for k, child := range m {
			p := k
			if path != "" {
				p = path + "." + k
			}
			flattenInto(out, p, child)
		}
		return
	}
	if path == "" {
		return // пустой объект верхнего уровня: путей нет
	}
	leaf, _ := json.Marshal(v)
	out[path] = leaf
}

package main

// Порядок бэклога в CLI (CLAB-400, эпик CLAB-368, решение 0075): clab rank
// ставит задачу рядом с соседом или к краю её группы, clab backlog показывает
// бэклог в порядке сервера и отвечает, что делать следующим. Ранг и порядок
// считает сервер: CLI называет соседа, ключ не строит и не пересортировывает
// строки. --next — выбор строки из уже отданного порядка по полям blocked_by
// и in_work; статусов, ролей и категорий в клиенте нет.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// backlogMinServer — первая версия сервера с порядком бэклога (ручка rank,
// параметр backlog и поле rank у задач).
const backlogMinServer = "v0.25.0"

// errBacklogOldServer — сервер прежней версии: ручки rank нет, а параметр
// backlog он молча проигнорировал.
var errBacklogOldServer = fmt.Errorf("сервер не знает порядка бэклога: обновите сервер до %s или новее", backlogMinServer)

const (
	backlogDefaultLimit = 50
	backlogMaxLimit     = 500
	// Сколько заблокированных строк называть в строке «пропущены» и в
	// пометке «ждёт».
	backlogNamedKeys = 3
)

// ── clab rank ───────────────────────────────────────────────────────────────

func cmdRank(ctx context.Context, c *Client, args []string) error {
	var before, after string
	var top, bottom bool
	task, rest := popTask(args)
	fs, err := flags("rank", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&before, "before", "", "поставить над этой задачей (она станет соседом ниже)")
		fs.StringVar(&after, "after", "", "поставить под этой задачей (она станет соседом выше)")
		fs.BoolVar(&top, "top", false, "в начало группы: верхнего уровня или эпика")
		fs.BoolVar(&bottom, "bottom", false, "в конец группы")
	})
	if err != nil {
		return err
	}
	if err := requireTask(fs, task); err != nil {
		return err
	}

	// Ровно одно место — до запроса: сервер проверил бы то же, но лишний
	// круг ради опечатки в команде не нужен.
	body := map[string]string{}
	places := 0
	if before != "" {
		body["before"], places = before, places+1
	}
	if after != "" {
		body["after"], places = after, places+1
	}
	if top {
		body["position"], places = "top", places+1
	}
	if bottom {
		body["position"], places = "bottom", places+1
	}
	if places != 1 {
		return fmt.Errorf("укажите ровно одно место: --before <ключ>, --after <ключ>, --top или --bottom")
	}

	var out json.RawMessage
	if err := c.post(ctx, "/tasks/"+url.PathEscape(task)+"/rank", body, &out); err != nil {
		var apiErr *APIError
		// Plain-text 404 без кода ошибки — маршрута нет: сервер прежней
		// версии. «Задачи нет» приходит конвертом с кодом not_found.
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Code == "" {
			return errBacklogOldServer
		}
		return err
	}
	return emit(out, func() error { return renderRanked(out, task, body) })
}

// renderRanked — одна строка исхода: что и куда поставлено. Соседа называет
// запрос, край группы — ответ (parent: эпик или null).
func renderRanked(raw json.RawMessage, task string, place map[string]string) error {
	var res struct {
		Changed bool    `json:"changed"`
		Parent  *string `json:"parent"`
	}
	if err := decode(raw, &res); err != nil {
		return err
	}
	if !res.Changed {
		fmt.Printf("%s уже на этом месте.\n", task)
		return nil
	}
	group := "бэклога"
	if res.Parent != nil && *res.Parent != "" {
		group = "эпика " + *res.Parent
	}
	switch {
	case place["before"] != "":
		fmt.Printf("%s поставлена над %s.\n", task, place["before"])
	case place["after"] != "":
		fmt.Printf("%s поставлена под %s.\n", task, place["after"])
	case place["position"] == "top":
		fmt.Printf("%s — в начале %s.\n", task, group)
	default:
		fmt.Printf("%s — в конце %s.\n", task, group)
	}
	return nil
}

// ── clab backlog ────────────────────────────────────────────────────────────

// backlogRow — строка списка бэклога: те поля, которые CLI показывает и по
// которым выбирает следующую. Rank нужен только как признак сервера, который
// порядок знает.
type backlogRow struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	Priority    string          `json:"priority"`
	Properties  json.RawMessage `json:"properties"`
	Rank        json.RawMessage `json:"rank"`
	InWork      bool            `json:"in_work"`
	BlockedBy   []string        `json:"blocked_by"`
	ChildCounts map[string]int  `json:"child_counts"`
}

// backlogPage — страница списка: строки типизированно и сырыми (для --json
// и --next --json) и total выборки.
type backlogPage struct {
	Rows  []backlogRow
	Raw   []json.RawMessage
	Total int
	Body  json.RawMessage
}

// fetchBacklog читает страницу бэклога: верхний уровень или дети parent, в
// порядке сервера. Сервер прежней версии узнаётся по форме ответа: у его
// строк нет поля rank, а пустой ответ отличает версия из заголовка.
func fetchBacklog(ctx context.Context, c *Client, project, parent string, limit, offset int) (backlogPage, error) {
	q := url.Values{}
	q.Set("backlog", "true")
	q.Set("order", "backlog")
	if parent != "" {
		q.Set("parent", parent)
	} else {
		q.Set("top_level", "true")
	}
	q.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}

	var page backlogPage
	if err := c.get(ctx, "/projects/"+url.PathEscape(project)+"/tasks", q, &page.Body); err != nil {
		return page, err
	}
	var reply struct {
		Tasks []json.RawMessage `json:"tasks"`
		Total int               `json:"total"`
	}
	if err := decode(page.Body, &reply); err != nil {
		return page, err
	}
	page.Raw, page.Total = reply.Tasks, reply.Total
	for _, raw := range reply.Tasks {
		var row backlogRow
		if err := decode(raw, &row); err != nil {
			return page, err
		}
		page.Rows = append(page.Rows, row)
	}
	if err := checkBacklogServer(c, page.Rows); err != nil {
		return page, err
	}
	return page, nil
}

// checkBacklogServer — сервер знает порядок бэклога? Есть строки — по полю
// rank у первой; пустой ответ старого сервера неотличим от пустого бэклога
// по форме, поэтому смотрим версию из заголовка (сборки dev и SHA не
// сравниваются: для них верим форме ответа).
func checkBacklogServer(c *Client, rows []backlogRow) error {
	if len(rows) > 0 {
		if len(rows[0].Rank) == 0 {
			return errBacklogOldServer
		}
		return nil
	}
	if v, _ := c.serverVersion.Load().(string); v != "" {
		if got, ok := parseRelease(v); ok {
			min, _ := parseRelease(backlogMinServer)
			if got.less(min) {
				return errBacklogOldServer
			}
		}
	}
	return nil
}

func cmdBacklog(ctx context.Context, c *Client, args []string) error {
	var projectFlag, parent string
	limit := backlogDefaultLimit
	var next bool
	_, err := flags("backlog", args, func(fs *flag.FlagSet) {
		fs.StringVar(&projectFlag, "project", "", "ключ проекта")
		fs.StringVar(&parent, "parent", "", "бэклог эпика: его дети в порядке бэклога")
		fs.IntVar(&limit, "limit", backlogDefaultLimit, "сколько строк показать, 1…500")
		fs.BoolVar(&next, "next", false, "только следующая задача: первая без блокирующих; --limit не действует")
	})
	if err != nil {
		return err
	}
	if !next && (limit < 1 || limit > backlogMaxLimit) {
		return fmt.Errorf("--limit: нужно от 1 до %d, пришло %d", backlogMaxLimit, limit)
	}
	key, err := c.project(ctx, projectFlag)
	if err != nil {
		return err
	}

	if next {
		return backlogNext(ctx, c, key, parent)
	}

	// Заголовок эпика берётся до списка: нет такого эпика — отказ сервера
	// с его текстом, а не пустая таблица.
	epicTitle := ""
	if parent != "" {
		if epicTitle, err = taskTitle(ctx, c, parent); err != nil {
			return err
		}
	}
	page, err := fetchBacklog(ctx, c, key, parent, limit, 0)
	if err != nil {
		return err
	}
	return emit(page.Body, func() error {
		renderBacklog(page, parent, epicTitle)
		return nil
	})
}

// taskTitle — заголовок задачи по GET /tasks/:key.
func taskTitle(ctx context.Context, c *Client, key string) (string, error) {
	var t struct {
		Task struct {
			Title string `json:"title"`
		} `json:"task"`
	}
	if err := c.get(ctx, "/tasks/"+url.PathEscape(key), nil, &t); err != nil {
		return "", err
	}
	return t.Task.Title, nil
}

func renderBacklog(page backlogPage, parent, epicTitle string) {
	if parent != "" {
		fmt.Printf("Бэклог эпика %s · %s\n", parent, epicTitle)
	}
	if len(page.Rows) == 0 {
		if parent != "" {
			fmt.Printf("В эпике %s нет задач в бэклоге.\n", parent)
		} else {
			fmt.Println("Бэклог пуст.")
		}
		return
	}
	t := newTable("№", "КЛЮЧ", "ТИП", "ПРИОРИТЕТ", "ЗАДАЧ", "ПОМЕТКИ", "ЗАГОЛОВОК")
	for i, r := range page.Rows {
		t.add(strconv.Itoa(i+1), r.Key, taskType(r.Properties), dash(r.Priority),
			childrenInBacklog(r), dash(backlogMarks(r)), trunc(r.Title, 60))
	}
	t.print()
	if page.Total > len(page.Rows) {
		more := min(page.Total, backlogMaxLimit)
		if more > len(page.Rows) {
			fmt.Printf("Показано %d из %d. Дальше: clab backlog%s --limit %d\n",
				len(page.Rows), page.Total, parentFlag(parent), more)
		} else {
			fmt.Printf("Показано %d из %d.\n", len(page.Rows), page.Total)
		}
	}
}

func parentFlag(parent string) string {
	if parent == "" {
		return ""
	}
	return " --parent " + parent
}

// childrenInBacklog — счёт детей в бэклоге у строки с детьми: категория
// backlog из child_counts, как её считает сервер; без детей — прочерк.
func childrenInBacklog(r backlogRow) string {
	if len(r.ChildCounts) == 0 {
		return "—"
	}
	return strconv.Itoa(r.ChildCounts["backlog"])
}

// backlogMarks — пометки строки через « · »: «в работе» и «ждёт <ключи>».
func backlogMarks(r backlogRow) string {
	var marks []string
	if r.InWork {
		marks = append(marks, "в работе")
	}
	if len(r.BlockedBy) > 0 {
		marks = append(marks, "ждёт "+keyList(r.BlockedBy))
	}
	return strings.Join(marks, " · ")
}

// keyList — ключи через запятую, не больше трёх; остальные счётом.
func keyList(keys []string) string {
	if len(keys) <= backlogNamedKeys {
		return strings.Join(keys, ", ")
	}
	return fmt.Sprintf("%s и ещё %d", strings.Join(keys[:backlogNamedKeys], ", "), len(keys)-backlogNamedKeys)
}

// ── clab backlog --next ─────────────────────────────────────────────────────

// nextResult — исход выбора следующей задачи.
type nextResult struct {
	// Выбранная строка и эпик, в котором она найдена (nil — верхний уровень).
	Row  *backlogRow
	Epic *backlogRow
	// Заблокированные, пройденные до выбранной (или все, если свободной нет).
	Skipped []backlogRow
	// Первая заблокированная по порядку — для ответа «свободной нет».
	FirstBlocked *backlogRow
	// Сырые строки для --json.
	rawRow, rawEpic, rawBlocked json.RawMessage
}

// readAllBacklog читает группу целиком страницами по 500: --next идёт по
// всему бэклогу, а не по --limit.
func readAllBacklog(ctx context.Context, c *Client, project, parent string, visit func(backlogRow, json.RawMessage) (stop bool)) error {
	for offset := 0; ; {
		page, err := fetchBacklog(ctx, c, project, parent, backlogMaxLimit, offset)
		if err != nil {
			return err
		}
		for i, r := range page.Rows {
			if visit(r, page.Raw[i]) {
				return nil
			}
		}
		offset += len(page.Rows)
		if len(page.Rows) == 0 || offset >= page.Total {
			return nil
		}
	}
}

// pickNext — правило выбора: первая строка без blocked_by. Эпик в работе
// (in_work) без блокировок открывается: берётся первый свободный ребёнок
// из его бэклога; у эпика все дети заблокированы — идём дальше. Заблокированная
// строка, в том числе эпик, пропускается целиком.
func pickNext(ctx context.Context, c *Client, project, parent string) (nextResult, error) {
	var res nextResult
	noteBlocked := func(r backlogRow, raw json.RawMessage) {
		res.Skipped = append(res.Skipped, r)
		if res.FirstBlocked == nil {
			cp := r
			res.FirstBlocked, res.rawBlocked = &cp, raw
		}
	}
	var visitErr error
	err := readAllBacklog(ctx, c, project, parent, func(r backlogRow, raw json.RawMessage) bool {
		switch {
		case len(r.BlockedBy) > 0:
			noteBlocked(r, raw)
			return false
		case !r.InWork:
			cp := r
			res.Row, res.rawRow = &cp, raw
			return true
		}
		// Эпик в работе, сам не заблокирован: ищем среди его детей.
		epic := r
		var skippedHere []backlogRow
		var firstHere backlogRow
		var firstHereRaw json.RawMessage
		found := false
		visitErr = readAllBacklog(ctx, c, project, epic.Key, func(ch backlogRow, chRaw json.RawMessage) bool {
			if len(ch.BlockedBy) > 0 {
				if len(skippedHere) == 0 {
					firstHere, firstHereRaw = ch, chRaw
				}
				skippedHere = append(skippedHere, ch)
				return false
			}
			cp, cpe := ch, epic
			res.Row, res.rawRow, res.Epic, res.rawEpic = &cp, chRaw, &cpe, raw
			found = true
			return true
		})
		if visitErr != nil {
			return true
		}
		res.Skipped = append(res.Skipped, skippedHere...)
		if res.FirstBlocked == nil && len(skippedHere) > 0 {
			res.FirstBlocked, res.rawBlocked = &firstHere, firstHereRaw
		}
		return found
	})
	if err == nil {
		err = visitErr
	}
	return res, err
}

func backlogNext(ctx context.Context, c *Client, project, parent string) error {
	res, err := pickNext(ctx, c, project, parent)
	if err != nil {
		return err
	}
	return emit(res.jsonView(), func() error {
		res.print()
		return nil
	})
}

// jsonView — тело для --next --json: выбранная строка как её отдал сервер
// (или null), эпик, в котором она найдена, пропущенные ключи и первая
// заблокированная.
func (r nextResult) jsonView() map[string]any {
	raw := func(m json.RawMessage) any {
		if m == nil {
			return nil
		}
		return m
	}
	skipped := make([]string, 0, len(r.Skipped))
	for _, s := range r.Skipped {
		skipped = append(skipped, s.Key)
	}
	return map[string]any{
		"next": raw(r.rawRow), "epic": raw(r.rawEpic),
		"skipped": skipped, "first_blocked": raw(r.rawBlocked),
	}
}

func (r nextResult) print() {
	if r.Row == nil {
		if r.FirstBlocked == nil {
			fmt.Println("Бэклог пуст.")
			return
		}
		fmt.Printf("Свободной задачи нет: все в бэклоге ждут блокирующих. Первая по порядку — %s, ждёт %s.\n",
			r.FirstBlocked.Key, strings.Join(r.FirstBlocked.BlockedBy, ", "))
		return
	}
	fmt.Printf("Следующая: %s  %s  %s\n", r.Row.Key, dash(r.Row.Priority), r.Row.Title)
	if r.Epic != nil {
		fmt.Printf("в эпике %s · %s\n", r.Epic.Key, r.Epic.Title)
	}
	if len(r.Skipped) > 0 {
		keys := make([]string, len(r.Skipped))
		for i, s := range r.Skipped {
			keys[i] = s.Key
		}
		fmt.Printf("пропущены заблокированные: %s\n", keyList(keys))
	}
}

package main

// Вывод для человека и для скрипта. Команды чтения показывают таблицу или
// сводку, --json отдаёт ответ сервера как есть; команды без своего
// представления печатают JSON всегда. Скрипты и агенты зовут clab с --json
// и не зависят от того, как выглядит таблица.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// jsonOut выставляется глобальным --json.
var jsonOut bool

func print(v any) error {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(body))
	return nil
}

// emit печатает ответ человеку, если есть чем, и JSON — если попросили или
// представления нет.
func emit(v any, human func() error) error {
	if jsonOut || human == nil {
		return print(v)
	}
	return human()
}

// render связывает рендер с сырым ответом для emit.
func render(fn func(json.RawMessage) error, raw json.RawMessage) func() error {
	return func() error { return fn(raw) }
}

// decode перекладывает ответ сервера в типизированный вид для рендера, не
// теряя исходный JSON для --json.
func decode(raw json.RawMessage, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ответ сервера не разбирается: %w", err)
	}
	return nil
}

type table struct {
	headers []string
	rows    [][]string
}

func newTable(headers ...string) *table { return &table{headers: headers} }

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *table) print() {
	if len(t.rows) == 0 {
		fmt.Println("пусто")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(t.headers, "\t"))
	for _, row := range t.rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	w.Flush()
}

// kv печатает пары «подпись: значение» одной колонкой подписей.
func kv(pairs ...string) {
	for _, line := range kvLines(pairs...) {
		fmt.Println(line)
	}
}

// kvLines — те же пары строками, по строке на пару: так между ними можно
// вставить строку, не выровненную по колонке.
func kvLines(pairs ...string) []string {
	var buf strings.Builder
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	for i := 0; i+1 < len(pairs); i += 2 {
		fmt.Fprintf(w, "%s\t%s\n", pairs[i], pairs[i+1])
	}
	w.Flush()
	if buf.Len() == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func ptr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func yesNo(b bool) string {
	if b {
		return "да"
	}
	return "нет"
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func when(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// ago — «3 мин назад» для последних отметок: точная дата тут не нужна.
func ago(t *time.Time) string {
	if t == nil {
		return "—"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "только что"
	case d < time.Hour:
		return fmt.Sprintf("%d мин назад", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d ч назад", int(d.Hours()))
	default:
		return fmt.Sprintf("%d дн назад", int(d.Hours()/24))
	}
}

func joinOr(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	return strings.Join(items, ", ")
}

// taskType достаёт тип задачи из её свойств: сервер отдаёт их сырым JSON.
func taskType(properties json.RawMessage) string {
	var props struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(properties, &props)
	if props.Type == "" {
		return "task"
	}
	return props.Type
}

package main

// Учёт стоимости clab exec по выводу привязки (CLAB-470). Самоотчёт агента
// занижает токены в разы, поэтому при привязке с разбором вывода
// (поле output) отчитывается сам clab exec — тем же общим разбором, что
// раннер (agentkit/launch), одной строкой на прогон аренды.
//
// Токен задачи гаснет на переходе, который агент делает сам, и после
// выхода агента им уже не отчитаться. Строку шлёт клиент запустившего —
// его окружение и конфиг, как у любой команды clab, — и только после
// выхода процесса агента: агент этих учётных данных не видит ни в
// окружении, ни в файлах, ни в argv. Клиент ходит только на тот же хост,
// что у токена задачи; иначе, как и при любом другом сбое, тело строки
// ложится в <каталог захода>/usage.json с готовой командой clab usage, а
// код выхода остаётся кодом агента.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"
)

// execReportedBy — details.reported_by строки учёта clab exec.
const execReportedBy = "clab exec"

// execCaptureLimit — предел копии вывода агента для разбора. Вывод Codex с
// выводом команд бывает десятками мегабайт; сверх предела агент не
// обрывается и stdout идёт целиком, но учёт не отправляется.
var execCaptureLimit = 64 << 20

// execUsageTimeout — предел отправки строки: агент уже отработал, и ждать
// сервер дольше незачем.
var execUsageTimeout = 30 * time.Second

// launcherClient — клиент запустившего clab exec: окружение и конфиг
// процесса clab, как у любой команды. Зовётся только после выхода агента.
var launcherClient = func() (*Client, error) { return NewClient(globals{}) }

// usagePlan — кто отчитается о стоимости: решается до запуска, от этого
// зависит инструкция.
type usagePlan struct {
	reports bool
	// format — значение output привязки; model — модель, которую агент
	// получил на самом деле (в команде есть {model}).
	format, model string
	agent         string
	line          string
	warning       string
}

// planUsage — решение об учёте. Ветвление — по формату вывода привязки, а
// не по имени агента: привязка с любым именем и полем output разбирается
// так же.
func planUsage(agent string, binding *agentBinding, noBindings bool, template []string, wrapped bool, model string) usagePlan {
	u := usagePlan{agent: agent}
	if binding != nil {
		u.format = binding.Output
	}
	if launch.Uses(template, launch.SlotModel) {
		u.model = model
	}
	self := "учёт: о стоимости отчитывается агент сам по инструкции — "
	switch {
	case noBindings:
		u.line = self + "набор инстанса не отдал привязок, формат вывода неизвестен"
	case u.format == "":
		u.line = self + "у привязки " + agent + " нет разбора вывода"
	case wrapped && !launch.EmitsFormat(u.format, template):
		u.line = self + "в своей команде нет флага формата вывода " + u.format
		u.warning = "своя команда не включает формат вывода " + u.format + " (" + launch.FormatFlagHint(u.format) +
			"): clab exec её вывод не разберёт, о стоимости отчитается агент"
	case !launch.LauncherReports(u.format, u.model):
		u.line = self + "в выводе " + u.format + " модели нет, а агенту она не передана (" + launch.SlotModel + " в команде и выбранная модель)"
	default:
		u.reports = true
		u.line = "учёт: о стоимости отчитается clab exec по выводу " + u.format + " после выхода агента; инструкция без самоотчёта (usage=runner)"
	}
	return u
}

// capturedOutput — stdout агента как есть плюс копия для разбора до
// предела. Запись в w не зависит от копии: байты запустившего те же, что
// без перехвата.
type capturedOutput struct {
	w        io.Writer
	limit    int
	mu       sync.Mutex
	buf      []byte
	overflow bool
}

func (c *capturedOutput) Write(p []byte) (int, error) {
	c.mu.Lock()
	if !c.overflow {
		if len(c.buf)+len(p) > c.limit {
			c.overflow, c.buf = true, nil
		} else {
			c.buf = append(c.buf, p...)
		}
	}
	c.mu.Unlock()
	return c.w.Write(p)
}

func (c *capturedOutput) bytes() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf, c.overflow
}

// reportExecUsage разбирает вывод и шлёт одну строку учёта за прогон runID.
// Нечего отправить — предупреждение и ничего больше: стоимость неизвестна,
// а не ноль (решение 0026). Отправить не вышло — тело в usage.json.
func reportExecUsage(ctx context.Context, stderr io.Writer, td taskDir, task, runID string, plan *launchPlan, capture *capturedOutput) {
	var secrets []string
	say := func(format string, a ...any) {
		s := fmt.Sprintf(format, a...)
		for _, tok := range append([]string{td.token}, secrets...) {
			if tok != "" {
				s = strings.ReplaceAll(s, tok, "<токен>")
			}
		}
		fmt.Fprintln(stderr, "clab exec: "+s)
	}
	raw, overflow := capture.bytes()
	u := plan.usage
	if overflow {
		say("предупреждение: учёт не отправлен — вывод агента больше предела перехвата %s; стоимость неизвестна", sizeLabel(execCaptureLimit))
		return
	}
	report := launch.ParseOutput(u.format, raw, u.model, execReportedBy)
	if report == nil {
		say("предупреждение: учёт не отправлен — вывод агента не разобран как %s; стоимость неизвестна, строки нет", u.format)
		return
	}
	// Какой привязкой разобран вывод — рядом с reported_by, как у раннера.
	if details, ok := report["details"].(map[string]any); ok {
		details["agent"] = u.agent
	}

	sent, reason, secret := sendExecUsage(ctx, td, task, runID, report)
	secrets = append(secrets, secret)
	if reason == "" {
		say("учёт: строка записана — %s", usageSummary(report))
		if line := costSourceLine(sent); line != "" {
			fmt.Fprintln(stderr, line)
		}
		return
	}
	path := filepath.Join(td.runDir, "usage.json")
	body, _ := json.MarshalIndent(report, "", "  ")
	if err := writePrivate(path, append(body, '\n')); err != nil {
		say("предупреждение: учёт не отправлен — %s; сохранить %s не удалось: %v", reason, path, err)
	} else {
		say("предупреждение: учёт не отправлен — %s; строка сохранена в %s", reason, path)
	}
	say("сдать вручную: %s", usageCommand(td.origin, task, runID, report))
}

// sendExecUsage — POST строки клиентом запустившего. reason пуст при
// успехе; secret — токен клиента, чтобы вычистить его из текста.
func sendExecUsage(ctx context.Context, td taskDir, task, runID string, report map[string]any) (sent any, reason, secret string) {
	if runID == "" {
		return nil, "прогон аренды неизвестен", ""
	}
	c, err := launcherClient()
	if err != nil {
		return nil, "учётных данных запустившего нет: " + err.Error(), ""
	}
	if c == nil || c.token == "" || c.origin == "" {
		return nil, "учётных данных запустившего нет (clab auth login --host " + td.origin + ")", ""
	}
	secret = c.token
	_, theirs, err1 := normalizeHost(c.origin)
	_, ours, err2 := normalizeHost(td.origin)
	if err1 != nil || err2 != nil || theirs != ours {
		return nil, "хост учётных данных запустившего " + c.origin + " не тот, что у токена задачи " + td.origin, secret
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), execUsageTimeout)
	defer cancel()
	path := "/tasks/" + task + "/runs/" + runID + "/usage"
	if err := c.do(ctx, "POST", path, report, &sent); err != nil {
		return nil, "сервер не принял строку: " + err.Error(), secret
	}
	return sent, "", secret
}

// usageSummary — строка учёта словами, для stderr.
func usageSummary(r map[string]any) string {
	parts := []string{"модель " + fmt.Sprint(r["model"])}
	for _, f := range []struct{ key, name string }{
		{"input_tokens", "вход"}, {"output_tokens", "выход"},
		{"cache_read_tokens", "чтение кэша"}, {"cache_write_tokens", "запись в кэш"},
		{"cost_usd", "сумма агента, USD"}, {"duration_ms", "мс"},
	} {
		if v, ok := r[f.key]; ok {
			parts = append(parts, f.name+" "+fmt.Sprint(v))
		}
	}
	return strings.Join(parts, ", ")
}

// usageCommand — та же строка командой clab usage: оркестратор сдаёт её
// своими учётными данными, когда разберётся, почему не вышло.
func usageCommand(origin, task, runID string, r map[string]any) string {
	words := []string{"clab", "--host", origin, "usage", task, "--run", runID, "--model", fmt.Sprint(r["model"])}
	for _, f := range []struct{ key, flag string }{
		{"input_tokens", "--input"}, {"output_tokens", "--output"},
		{"cache_read_tokens", "--cache-read"}, {"cache_write_tokens", "--cache-write"},
		{"cost_usd", "--cost"}, {"duration_ms", "--duration-ms"},
	} {
		if v, ok := r[f.key]; ok {
			words = append(words, f.flag, fmt.Sprint(v))
		}
	}
	if d, ok := r["details"].(map[string]any); ok && len(d) > 0 {
		raw, _ := json.Marshal(d) // ключи map — по алфавиту
		words = append(words, "--details", string(raw))
	}
	for i, w := range words {
		words[i] = shellWord(w)
	}
	return strings.Join(words, " ")
}

// shellWord — слово для sh: без особых знаков как есть, иначе в одинарных
// кавычках.
func shellWord(w string) string {
	if w != "" && !strings.ContainsAny(w, " \t\n\"'$`\\;&|<>(){}*?[]#~!") {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}

func sizeLabel(n int) string {
	if n >= 1<<20 && n%(1<<20) == 0 {
		return fmt.Sprintf("%d МиБ", n>>20)
	}
	return fmt.Sprintf("%d байт", n)
}

// writePrivate — файл только для владельца: в каталоге захода лежит и
// конфиг токена задачи.
func writePrivate(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

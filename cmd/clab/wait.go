package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// clab wait (CLAB-323, решение 0067 п. 4–6): ждёт, пока гейт на переходах из
// текущего статуса не перестанет быть pending или задача не сменит статус
// либо прогон этапа, и выходит кодом по исходу. Ждёт событий долгим опросом
// GET /v1/tasks/:key/events; событие только будит, исход решает перечитанная
// карточка GET /v1/tasks/:key. Ключей статусов, переходов и ролей здесь нет:
// различаются тип и ключ гейта из флага, вид перехода (advance, reject,
// cancel, reassign), статус гейта и id прогона.
//
// Пока команда ждёт, она не печатает ничего и не будит того, кто её
// запустил: исход — одна строка (или один объект --json) в конце.

// Коды выхода clab wait. 1 — ошибка (exit), 2 — неверный вызов.
const (
	waitPassed        = 0
	waitUsage         = 2
	waitReturned      = 3
	waitStatusChanged = 4
	waitTimeout       = 5
	// Прерывание сигналом: исхода нет, код не из 0/3/4/5 (128 + SIGINT).
	waitInterrupted = 130
)

// waitTiming — сроки ожидания; в тестах короче.
type waitTiming struct {
	// Потолок одного долгого опроса; сервер урезает его сам, но лишнего не
	// просим.
	longPoll time.Duration
	// Пауза между кругами долгого опроса: круг без событий не повторяется
	// впритык.
	gap time.Duration
	// Опрос карточки на сервере без ручки событий (CLAB-148, 0067 п. 6).
	poll time.Duration
	// Отступ повтора при сбое сети и 502/503/504: от first вдвое до max.
	retryFirst, retryMax time.Duration
	// Карточка перечитывается после ответа с событиями и после каждого
	// recheck-го пустого круга: событие с ранней меткой, зафиксированное
	// позже соседнего, курсор мог перешагнуть, и пустой круг тогда
	// единственный способ его заметить. Каждый пустой круг — лишний поход
	// за карточкой на каждое ожидание, а такой пропуск редок.
	recheck int
}

var defaultWaitTiming = waitTiming{
	longPoll: 25 * time.Second, gap: time.Second, poll: 15 * time.Second,
	retryFirst: time.Second, retryMax: 30 * time.Second, recheck: 4,
}

type waitOptions struct {
	task, gate, transition string
	timeout                time.Duration
	timing                 waitTiming
}

// waitResult — то, что печатает --json. Поля добавляются только дополнением:
// его читают скилы набора агента.
type waitResult struct {
	Outcome string `json:"outcome"`
	Task    string `json:"task"`
	Status  struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"status"`
	Transition     string      `json:"transition"`
	Gate           *waitGateID `json:"gate"`
	TransitionDone bool        `json:"transition_done"`
	Allowed        bool        `json:"allowed"`
	Reason         string      `json:"reason"`
	Actor          *string     `json:"actor"`
	DecidedAt      *time.Time  `json:"decided_at"`
	Mode           string      `json:"mode"`
}

type waitGateID struct {
	Key    string `json:"key"`
	Type   string `json:"type"`
	Status string `json:"status"`
}

// waitCard — то из карточки задачи, по чему решается исход.
type waitCard struct {
	Task struct {
		Key       string `json:"key"`
		StatusKey string `json:"status_key"`
	} `json:"task"`
	StageRuns []struct {
		ID          string     `json:"id"`
		Kind        string     `json:"kind"`
		FinishedAt  *time.Time `json:"finished_at"`
		GateResults []struct {
			GateKey         string     `json:"gate_key"`
			TransitionKey   string     `json:"transition_key"`
			DecidedByHandle *string    `json:"decided_by_handle"`
			DecidedAt       *time.Time `json:"decided_at"`
		} `json:"gate_results"`
		Exit *struct {
			Kind       string    `json:"kind"`
			Transition string    `json:"transition"`
			ToStatus   string    `json:"to_status"`
			Comment    *string   `json:"comment"`
			ByHandle   *string   `json:"by_handle"`
			At         time.Time `json:"at"`
		} `json:"exit"`
	} `json:"stage_runs"`
	Transitions []struct {
		Key     string `json:"key"`
		Kind    string `json:"kind"`
		Allowed bool   `json:"allowed"`
		Gates   []struct {
			GateKey string         `json:"gate_key"`
			Type    string         `json:"type"`
			Status  string         `json:"status"`
			Reason  string         `json:"reason"`
			Details map[string]any `json:"details"`
		} `json:"gates"`
	} `json:"transitions"`
}

// currentRun — id открытого прогона этапа (kind=stage); перепроверка этапом
// не считается: она живёт внутри родителя.
func (c waitCard) currentRun() string {
	for i := len(c.StageRuns) - 1; i >= 0; i-- {
		if r := c.StageRuns[i]; r.Kind == "stage" && r.FinishedAt == nil {
			return r.ID
		}
	}
	return ""
}

// waitStart — с чего начали: статус, прогон, виды переходов из статуса и
// какие из них ждём.
type waitStart struct {
	status, run string
	kinds       map[string]string
	awaited     map[string]bool
}

func (o waitOptions) gateMatches(key, typ string) bool {
	return o.gate != "" && (o.gate == typ || o.gate == key)
}

// begin проверяет, есть ли чего ждать, и запоминает начало. Нечего —
// ошибка с перечнем переходов (код 2).
func (o waitOptions) begin(card waitCard) (waitStart, error) {
	s := waitStart{status: card.Task.StatusKey, run: card.currentRun(),
		kinds: map[string]string{}, awaited: map[string]bool{}}
	var list []string
	found := false
	for _, t := range card.Transitions {
		s.kinds[t.Key] = t.Kind
		var gates []string
		has := false
		for _, g := range t.Gates {
			gates = append(gates, g.GateKey+"("+g.Type+")")
			has = has || o.gateMatches(g.GateKey, g.Type)
		}
		list = append(list, fmt.Sprintf("%s [%s]", t.Key, strings.Join(gates, ", ")))
		if o.transition != "" && t.Key != o.transition {
			continue
		}
		found = true
		if o.gate == "" || has {
			s.awaited[t.Key] = true
		}
	}
	listing := strings.Join(list, "; ")
	if listing == "" {
		listing = "переходов нет"
	}
	switch {
	case o.transition != "" && !found:
		return s, fmt.Errorf("ждать нечего: перехода %q нет из статуса %s; переходы: %s", o.transition, s.status, listing)
	case o.gate != "" && len(s.awaited) == 0:
		where := "у переходов"
		if o.transition != "" {
			where = "у перехода " + o.transition
		}
		return s, fmt.Errorf("ждать нечего: %s из статуса %s нет гейта %q (тип или ключ); переходы: %s", where, s.status, o.gate, listing)
	}
	return s, nil
}

// decide — исход по перечитанной карточке; nil — ждать дальше.
func (o waitOptions) decide(s waitStart, card waitCard) (*waitResult, int) {
	res := &waitResult{Task: card.Task.Key}
	res.Status.From, res.Status.To = s.status, card.Task.StatusKey

	if card.Task.StatusKey != s.status || card.currentRun() != s.run {
		for _, r := range card.StageRuns {
			if r.ID != s.run || r.Exit == nil {
				continue
			}
			x := r.Exit
			res.Transition, res.Actor = x.Transition, x.ByHandle
			at := x.At
			res.DecidedAt = &at
			if x.Comment != nil {
				res.Reason = *x.Comment
			}
			kind := s.kinds[x.Transition]
			if kind == "" {
				// Перехода не было в превью на старте (цикл знает его, а
				// превью — нет): вид — по событию выхода.
				kind = map[string]string{"advanced": "advance", "rejected": "reject", "cancelled": "cancel"}[x.Kind]
			}
			switch {
			case kind == "reject":
				res.Outcome = "returned"
				return res, waitReturned
			case kind == "advance" && (len(s.awaited) == 0 && o.gate == "" || s.awaited[x.Transition]):
				res.Outcome, res.TransitionDone, res.Allowed = "passed", true, true
				return res, waitPassed
			}
			break
		}
		res.Outcome = "status_changed"
		return res, waitStatusChanged
	}
	if o.gate == "" {
		return nil, 0
	}

	// Тот же прогон: судит состояние гейтов, а не события. failed — раньше
	// passed: возврат с причиной важнее соседнего перехода.
	var passed *waitResult
	for _, t := range card.Transitions {
		if !s.awaited[t.Key] {
			continue
		}
		for _, g := range t.Gates {
			if !o.gateMatches(g.GateKey, g.Type) || g.Status == "pending" {
				continue
			}
			r := *res
			r.Transition, r.Allowed = t.Key, t.Allowed
			r.Gate = &waitGateID{Key: g.GateKey, Type: g.Type, Status: g.Status}
			r.Actor, r.DecidedAt = card.decider(s.run, t.Key, g.GateKey)
			if g.Status == "failed" {
				r.Outcome, r.Reason = "returned", gateReason(g.Reason, g.Details)
				return &r, waitReturned
			}
			if passed == nil {
				r.Outcome = "passed"
				passed = &r
			}
		}
	}
	if passed != nil {
		return passed, waitPassed
	}
	return nil, 0
}

// decider — кто и когда вынес вердикт гейта на прогоне (gate_results).
func (c waitCard) decider(run, transition, gate string) (*string, *time.Time) {
	for _, r := range c.StageRuns {
		if r.ID != run {
			continue
		}
		for _, g := range r.GateResults {
			if g.GateKey == gate && g.TransitionKey == transition {
				return g.DecidedByHandle, g.DecidedAt
			}
		}
	}
	return nil, nil
}

// gateReason — причина отклонения: reason из решения человека, иначе
// причина гейта.
func gateReason(reason string, details map[string]any) string {
	if d, ok := details["decision"].(map[string]any); ok {
		if r, ok := d["reason"].(string); ok {
			return r
		}
		return ""
	}
	return reason
}

func (r waitResult) line() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s", r.Task, r.Outcome)
	switch {
	case r.Outcome == "timeout":
		fmt.Fprintf(&b, ": решения нет, статус %s", r.Status.From)
	case r.Gate != nil:
		fmt.Fprintf(&b, ": %s · %s (%s) %s", r.Transition, r.Gate.Key, r.Gate.Type, r.Gate.Status)
	case r.Transition != "":
		fmt.Fprintf(&b, ": %s → %s (%s)", r.Status.From, r.Status.To, r.Transition)
	default:
		fmt.Fprintf(&b, ": %s → %s", r.Status.From, r.Status.To)
	}
	if r.Actor != nil {
		fmt.Fprintf(&b, " — %s", *r.Actor)
	}
	if r.DecidedAt != nil {
		fmt.Fprintf(&b, " %s", r.DecidedAt.Local().Format("2006-01-02 15:04:05"))
	}
	if r.Outcome == "returned" {
		if r.Reason == "" {
			b.WriteString("; причина не указана")
		} else {
			fmt.Fprintf(&b, "; причина: %s", trunc(r.Reason, 300))
		}
	}
	return b.String()
}

func cmdWait(ctx context.Context, c *Client, args []string) error {
	o := waitOptions{timing: defaultWaitTiming}
	task, rest := popTask(args)
	fs, err := flags("wait", rest, func(fs *flag.FlagSet) {
		fs.StringVar(&o.gate, "gate", "", "тип или ключ гейта: human_approval ловит апрув человека на любом переходе")
		fs.StringVar(&o.transition, "transition", "", "ждать только этого перехода")
		fs.DurationVar(&o.timeout, "timeout", 0, "срок ожидания (30m, 8h); без него — без срока")
	})
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		os.Exit(waitUsage)
	}
	if task == "" || fs.NArg() > 0 || o.timeout < 0 {
		fs.Usage()
		fmt.Fprintln(os.Stderr, "нужен ключ задачи; --timeout не меньше нуля")
		os.Exit(waitUsage)
	}
	o.task = task

	res, code, err := runWait(ctx, c, o)
	var usageErr waitUsageError
	switch {
	case errors.As(err, &usageErr):
		fmt.Fprintln(os.Stderr, usageErr.Error())
		os.Exit(waitUsage)
	case ctx.Err() != nil:
		os.Exit(waitInterrupted)
	case err != nil:
		return err
	}
	if jsonOut {
		body, _ := json.Marshal(res)
		fmt.Println(string(body))
	} else {
		fmt.Println(res.line())
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

// waitUsageError — «ждать нечего»: код 2, а не 1.
type waitUsageError struct{ error }

// runWait — само ожидание: рукопожатие, карточка, долгий опрос или опрос
// карточки на старом сервере. Ошибка — код 1 (кроме waitUsageError);
// прерывание видно по ctx.Err() вызывающего.
func runWait(ctx context.Context, c *Client, o waitOptions) (waitResult, int, error) {
	w := &waiter{ctx: ctx, c: c, o: o, t: o.timing, backoff: o.timing.retryFirst,
		path: "/tasks/" + url.PathEscape(o.task), mode: "events"}
	if o.timeout > 0 {
		w.deadline = time.Now().Add(o.timeout)
	}

	// Рукопожатие — до первого чтения карточки: событие между ними иначе
	// осталось бы за курсором.
	for {
		var feed struct {
			Cursor string `json:"cursor"`
		}
		err := c.get(ctx, w.path+"/events", nil, &feed)
		if err == nil && feed.Cursor != "" {
			w.cursor = feed.Cursor
			break
		}
		if err == nil || waitStatus(err) == http.StatusNotFound {
			// Маршрута нет (сервер старше ручки) или задачи нет — различит
			// карточка ниже.
			w.mode = "poll"
			break
		}
		if !w.transient(err) {
			return waitResult{}, 1, err
		}
		if !w.retry() {
			return w.timeout(""), waitTimeout, nil
		}
	}

	card, err := w.card()
	if errors.Is(err, errWaitDeadline) {
		return w.timeout(""), waitTimeout, nil
	}
	if err != nil {
		return waitResult{}, 1, err
	}
	start, err := o.begin(card)
	if err != nil {
		return waitResult{}, waitUsage, waitUsageError{err}
	}
	if res, code := o.decide(start, card); res != nil {
		res.Mode = w.mode
		return *res, code, nil
	}

	empty := 0
	for {
		if ctx.Err() != nil {
			return waitResult{}, 1, ctx.Err()
		}
		if w.remaining() <= 0 {
			return w.timeout(start.status), waitTimeout, nil
		}
		if w.mode == "events" {
			got, err := w.longPoll()
			switch {
			case err == nil:
			case waitStatus(err) == http.StatusNotFound:
				// Ручку убрали (откат сервера) — дальше опросом.
				w.mode = "poll"
				continue
			case w.transient(err):
				w.retry()
				continue
			default:
				return waitResult{}, 1, err
			}
			if got == 0 {
				empty++
				if empty%max(w.t.recheck, 1) != 0 {
					w.pause(w.t.gap)
					continue
				}
			}
		} else if !w.pause(w.t.poll) {
			continue
		}

		card, err := w.card()
		if errors.Is(err, errWaitDeadline) {
			continue
		}
		if err != nil {
			return waitResult{}, 1, err
		}
		if res, code := o.decide(start, card); res != nil {
			res.Mode = w.mode
			return *res, code, nil
		}
		if w.mode == "events" {
			w.pause(w.t.gap)
		}
	}
}

var errWaitDeadline = errors.New("срок ожидания вышел")

// waiter — состояние одного ожидания: срок, курсор, режим и отступ повтора.
type waiter struct {
	ctx      context.Context
	c        *Client
	o        waitOptions
	t        waitTiming
	deadline time.Time
	backoff  time.Duration
	path     string
	mode     string
	cursor   string
}

func (w *waiter) remaining() time.Duration {
	if w.deadline.IsZero() {
		return time.Duration(1<<63 - 1)
	}
	return time.Until(w.deadline)
}

// pause ждёт d, но не дольше срока; false — срок вышел или прервали.
func (w *waiter) pause(d time.Duration) bool {
	d = min(d, w.remaining())
	if d <= 0 {
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-w.ctx.Done():
		return false
	case <-timer.C:
		return w.remaining() > 0
	}
}

// retry — пауза перед повтором после сбоя: отступ растёт вдвое до retryMax.
func (w *waiter) retry() bool {
	ok := w.pause(w.backoff)
	w.backoff = min(w.backoff*2, w.t.retryMax)
	return ok
}

// longPoll — один круг долгого опроса; число пришедших событий.
func (w *waiter) longPoll() (int, error) {
	secs := max(int(min(w.t.longPoll, w.remaining())/time.Second), 1)
	var feed struct {
		Events []json.RawMessage `json:"events"`
		Cursor string            `json:"cursor"`
	}
	if err := w.c.get(w.ctx, w.path+"/events", query("after", w.cursor, "wait", fmt.Sprint(secs)), &feed); err != nil {
		return 0, err
	}
	w.backoff = w.t.retryFirst
	if feed.Cursor != "" {
		w.cursor = feed.Cursor
	}
	return len(feed.Events), nil
}

// card читает карточку с повторами на сбоях; 404 — задачи нет (код 1).
func (w *waiter) card() (waitCard, error) {
	for {
		var card waitCard
		err := w.c.get(w.ctx, w.path, nil, &card)
		if err == nil {
			w.backoff = w.t.retryFirst
			return card, nil
		}
		if waitStatus(err) == http.StatusNotFound {
			return card, fmt.Errorf("задача %s не найдена: %w", w.o.task, err)
		}
		if !w.transient(err) {
			return card, err
		}
		if !w.retry() {
			if w.ctx.Err() != nil {
				return card, w.ctx.Err()
			}
			return card, errWaitDeadline
		}
	}
}

func (w *waiter) timeout(status string) waitResult {
	r := waitResult{Outcome: "timeout", Task: w.o.task, Mode: w.mode, Transition: w.o.transition}
	r.Status.From, r.Status.To = status, status
	return r
}

// waitStatus — HTTP-код ошибки сервера; 0, если ответа не было.
func waitStatus(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// transient — сбой, который переживается повтором: сеть, обрыв ответа
// прокси, шлюз без апстрима (перезапуск API). Прерывание — не сбой.
func (w *waiter) transient(err error) bool {
	if w.ctx.Err() != nil {
		return false
	}
	switch waitStatus(err) {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	case 0:
	default:
		return false
	}
	// Сеть (отказ соединения, обрыв, DNS) и срок ответа — да; ошибка
	// сертификата или адреса — нет: повтор её не исправит.
	var op *net.OpError
	var ne net.Error
	return errors.As(err, &op) || (errors.As(err, &ne) && ne.Timeout()) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		strings.Contains(err.Error(), "connection reset")
}

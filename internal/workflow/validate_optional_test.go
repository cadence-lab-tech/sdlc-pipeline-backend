package workflow

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Правила необязательных этапов (CLAB-314, решение 0066, п. 4-6).

// opt — необязательный рабочий статус с исполнителем; on — умолчание.
func opt(key string, on bool) domain.Status {
	st := status(key, false, false)
	st.IsOptional, st.EnabledByDefault = true, on
	st.Roles = []domain.StatusRole{{IsPrimary: true}}
	return st
}

// mandatory — обязательный статус с исполнителем, чтобы no_primary_role не
// шумел в предупреждениях.
func mandatory(key string, initial, terminal bool) domain.Status {
	st := status(key, initial, terminal)
	st.EnabledByDefault = true
	st.Roles = []domain.StatusRole{{IsPrimary: true}}
	return st
}

func kinded(kind string, t domain.Transition) domain.Transition {
	t.Kind = kind
	return t
}

// positioned расставляет position по порядку: свёртка и порядок проблем
// идут по position, а не по порядку в срезе.
func positioned(statuses []domain.Status, transitions []domain.Transition) domain.Workflow {
	for i := range statuses {
		statuses[i].Position = i + 1
	}
	for i := range transitions {
		transitions[i].Position = i + 1
	}
	return domain.Workflow{Statuses: statuses, Transitions: transitions}
}

func byCode(problems []Problem, code string) []Problem {
	var out []Problem
	for _, p := range problems {
		if p.Code == code {
			out = append(out, p)
		}
	}
	return out
}

func hasOptionalCode(problems []Problem) bool {
	for _, p := range problems {
		if strings.HasPrefix(p.Code, "optional_") {
			return true
		}
	}
	return false
}

// AC-6: начальный и терминальный необязательными быть не могут — одна
// проблема на статус, обход у них не ищется, свёртка не идёт (Q1).
func TestOptionalInitialAndTerminal(t *testing.T) {
	newSt := mandatory("new", true, false)
	newSt.IsOptional = true
	work := mandatory("work", false, false)
	done := mandatory("done", false, true)
	done.IsOptional = true

	v := validateWorkflow(positioned([]domain.Status{newSt, work, done}, []domain.Transition{
		transition("start", &newSt, work),
		transition("finish", &work, done),
	}), allImplemented, false)

	if v.Valid {
		t.Fatal("необязательный начальный и терминальный — проблемы")
	}
	if got := byCode(v.Problems, "optional_initial"); len(got) != 1 || got[0].Status != "new" {
		t.Fatalf("optional_initial: %+v", v.Problems)
	}
	// У терминального нет исходящего advance, но optional_no_bypass не
	// появляется: проблема у статуса одна.
	if got := byCode(v.Problems, "optional_terminal"); len(got) != 1 || got[0].Status != "done" {
		t.Fatalf("optional_terminal: %+v", v.Problems)
	}
	if len(v.Problems) != 2 {
		t.Fatalf("ровно две проблемы, без обхода и свёртки: %+v", v.Problems)
	}
}

// AC-6: нет обхода. Единственный advance — «из любого статуса», и обходным
// он не считается.
func TestOptionalNoBypass(t *testing.T) {
	newSt := mandatory("new", true, false)
	design := opt("design", true)
	done := mandatory("done", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, design, done}, []domain.Transition{
		transition("start", &newSt, design),
		transition("finish", nil, done),
	}), allImplemented, false)

	got := byCode(v.Problems, "optional_no_bypass")
	if v.Valid || len(got) != 1 || got[0].Status != "design" {
		t.Fatalf("ожидалась optional_no_bypass у design: %+v", v.Problems)
	}
	if len(byCode(v.Problems, "optional_breaks_path")) != 0 {
		t.Fatalf("без обхода свёртка не идёт: %+v", v.Problems)
	}
}

// AC-6: два advance из необязательного — неоднозначный обход, transition —
// второй по position.
func TestOptionalBypassAmbiguous(t *testing.T) {
	newSt := mandatory("new", true, false)
	design := opt("design", true)
	dev := mandatory("dev", false, false)
	done := mandatory("done", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, design, dev, done}, []domain.Transition{
		transition("start", &newSt, design),
		transition("to_dev", &design, dev),
		transition("shortcut", &design, done),
		transition("finish", &dev, done),
	}), allImplemented, false)

	got := byCode(v.Problems, "optional_bypass_ambiguous")
	if v.Valid || len(got) != 1 || got[0].Status != "design" || got[0].Transition != "shortcut" {
		t.Fatalf("ожидалась optional_bypass_ambiguous design/shortcut: %+v", v.Problems)
	}
}

// AC-6: кольцо обходов — status первый по position, в сообщении кольцо;
// отдельно — переход в себя.
func TestOptionalBypassCycle(t *testing.T) {
	newSt := mandatory("new", true, false)
	a := opt("a", true)
	b := opt("b", true)
	dev := mandatory("dev", false, false)
	done := mandatory("done", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, a, b, dev, done}, []domain.Transition{
		transition("start", &newSt, b),
		transition("b_to_a", &b, a),
		transition("a_to_b", &a, b),
		kinded("reassign", transition("a_out", &a, dev)),
		transition("finish", &dev, done),
	}), allImplemented, false)

	got := byCode(v.Problems, "optional_bypass_cycle")
	if v.Valid || len(got) != 1 || got[0].Status != "a" || !strings.Contains(got[0].Message, "a → b → a") {
		t.Fatalf("ожидалось одно кольцо a → b → a: %+v", v.Problems)
	}
	if len(byCode(v.Problems, "optional_breaks_path")) != 0 {
		t.Fatalf("при кольце свёртка не идёт: %+v", v.Problems)
	}

	x := opt("x", true)
	newSt2 := mandatory("new", true, false)
	done2 := mandatory("done", false, true)
	v = validateWorkflow(positioned([]domain.Status{newSt2, x, done2}, []domain.Transition{
		transition("start", &newSt2, x),
		transition("again", &x, x),
		kinded("reassign", transition("out", &x, done2)),
	}), allImplemented, false)
	got = byCode(v.Problems, "optional_bypass_cycle")
	if v.Valid || len(got) != 1 || got[0].Status != "x" || !strings.Contains(got[0].Message, "x → x") {
		t.Fatalf("переход в себя — кольцо x → x: %+v", v.Problems)
	}
}

// chain собирает new → s1 → … → sN → done, где каждый s — необязательный, а
// обход каждого — его advance в следующий.
func chain(n int) domain.Workflow {
	newSt := mandatory("new", true, false)
	done := mandatory("done", false, true)
	statuses := []domain.Status{newSt}
	for i := 1; i <= n; i++ {
		statuses = append(statuses, opt(fmt.Sprintf("s%d", i), i%2 == 0))
	}
	statuses = append(statuses, done)
	var transitions []domain.Transition
	for i := 0; i+1 < len(statuses); i++ {
		transitions = append(transitions,
			transition(fmt.Sprintf("t%d", i), &statuses[i], statuses[i+1]))
	}
	return positioned(statuses, transitions)
}

// AC-9: десять необязательных — полный перебор, валиден; одиннадцать —
// optional_too_many без свёртки.
func TestOptionalLimit(t *testing.T) {
	if maxOptionalStatuses != 10 {
		t.Fatalf("предел из решения 0066 — 10, в коде %d", maxOptionalStatuses)
	}
	v := validateWorkflow(chain(10), allImplemented, false)
	if !v.Valid || hasOptionalCode(v.Warnings) {
		t.Fatalf("цепочка из десяти необязательных валидна: %+v %+v", v.Problems, v.Warnings)
	}

	v = validateWorkflow(chain(11), allImplemented, false)
	got := byCode(v.Problems, "optional_too_many")
	if v.Valid || len(got) != 1 || got[0].Status != "" {
		t.Fatalf("ожидалась optional_too_many без статуса: %+v", v.Problems)
	}
	if len(v.Problems) != 1 {
		t.Fatalf("сверх предела — только optional_too_many: %+v", v.Problems)
	}
}

// AC-9: свёртка не идёт, если у какого-то необязательного обход не
// определён, — даже когда при выключении другого цикл сломан бы был.
func TestOptionalFoldSkippedOnStructuralProblem(t *testing.T) {
	newSt := mandatory("new", true, false)
	design := opt("design", true)
	docs := opt("docs", true)
	hold := mandatory("hold", false, false)
	done := mandatory("done", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, design, hold, docs, done}, []domain.Transition{
		transition("start", &newSt, design),
		transition("design_done", &design, hold),
		kinded("reject", transition("back", &hold, design)), // при выключенном design — тупик
		kinded("reassign", transition("hold_docs", &hold, docs)),
		kinded("reassign", transition("docs_out", &docs, done)), // у docs нет advance
	}), allImplemented, false)

	if len(byCode(v.Problems, "optional_no_bypass")) != 1 {
		t.Fatalf("ожидалась optional_no_bypass у docs: %+v", v.Problems)
	}
	for _, p := range v.Problems {
		if p.Code != "optional_no_bypass" {
			t.Fatalf("свёртка не должна была идти: %+v", v.Problems)
		}
	}
}

// AC-7, п. 1: терминальный недостижим при наборе — одна проблема без статуса,
// в сообщении набор.
func TestOptionalBreaksPathTerminalUnreachable(t *testing.T) {
	newSt := mandatory("new", true, false)
	design := opt("design", true)
	hold := mandatory("hold", false, false)
	done := mandatory("done", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, design, hold, done}, []domain.Transition{
		transition("start", &newSt, design),
		transition("park", &design, hold),
		kinded("reassign", transition("ship", &design, done)),
		kinded("reject", transition("back", &hold, design)),
	}), allImplemented, false)

	got := byCode(v.Problems, "optional_breaks_path")
	if v.Valid || len(got) != 2 {
		t.Fatalf("ожидались две optional_breaks_path (терминал и тупик hold): %+v", v.Problems)
	}
	if got[0].Status != "" || !strings.Contains(got[0].Message, "[design]") {
		t.Fatalf("первой — проблема без статуса с набором [design]: %+v", got[0])
	}
	if got[1].Status != "hold" {
		t.Fatalf("второй — тупик hold: %+v", got[1])
	}
}

// AC-7, п. 2 и 5: статус, чей единственный выход — reject в выключаемый
// этап, — тупик; с другим выходом цикл валиден.
func TestOptionalBreaksPathRejectOnlyExit(t *testing.T) {
	build := func(extraExit bool) domain.Workflow {
		newSt := mandatory("new", true, false)
		design := opt("design", false)
		dev := mandatory("dev", false, false)
		blocked := mandatory("blocked", false, false)
		done := mandatory("done", false, true)
		ts := []domain.Transition{
			transition("start", &newSt, design),
			transition("design_done", &design, dev),
			transition("finish", &dev, done),
			kinded("reassign", transition("block", &dev, blocked)),
			kinded("reject", transition("redesign", &blocked, design)),
		}
		if extraExit {
			ts = append(ts, kinded("reject", transition("unblock", &blocked, dev)))
		}
		return positioned([]domain.Status{newSt, design, dev, blocked, done}, ts)
	}

	v := validateWorkflow(build(false), allImplemented, false)
	got := byCode(v.Problems, "optional_breaks_path")
	if v.Valid || len(got) != 1 || got[0].Status != "blocked" || !strings.Contains(got[0].Message, "[design]") {
		t.Fatalf("ожидался тупик blocked при [design]: %+v", v.Problems)
	}

	v = validateWorkflow(build(true), allImplemented, false)
	if !v.Valid {
		t.Fatalf("с выходом в обязательный статус цикл валиден: %+v", v.Problems)
	}
	// reject в необязательный — только предупреждение.
	if w := byCode(v.Warnings, "optional_reject_target"); len(w) != 1 || w[0].Transition != "redesign" || w[0].Status != "design" {
		t.Fatalf("ожидалось optional_reject_target redesign/design: %+v", v.Warnings)
	}
}

// AC-7, п. 2: не больше одной проблемы на статус-тупик, набор — первый по
// размеру, внутри — по position.
func TestOptionalBreaksPathFirstSetOnly(t *testing.T) {
	newSt := mandatory("new", true, false)
	a := opt("a", true)
	b := opt("b", true)
	dev := mandatory("dev", false, false)
	blocked := mandatory("blocked", false, false)
	done := mandatory("done", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, a, b, dev, blocked, done}, []domain.Transition{
		transition("start", &newSt, a),
		transition("a_done", &a, b),
		transition("b_done", &b, dev),
		transition("finish", &dev, done),
		kinded("reassign", transition("block", &dev, blocked)),
		kinded("reject", transition("back_b", &blocked, b)),
		kinded("reject", transition("back_a", &blocked, a)),
	}), allImplemented, false)

	got := byCode(v.Problems, "optional_breaks_path")
	// blocked ломается только при выключенных обоих: первый такой набор — [a, b].
	if len(got) != 1 || got[0].Status != "blocked" || !strings.Contains(got[0].Message, "[a, b]") {
		t.Fatalf("ожидалась одна проблема blocked при [a, b]: %+v", v.Problems)
	}
}

// AC-7, п. 3: переход «из любого статуса» остаётся выходом каждого статуса.
func TestOptionalBreaksPathAnywhereIsExit(t *testing.T) {
	newSt := mandatory("new", true, false)
	design := opt("design", true)
	dev := mandatory("dev", false, false)
	blocked := mandatory("blocked", false, false)
	done := mandatory("done", false, true)
	cancelled := mandatory("cancelled", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, design, dev, blocked, done, cancelled}, []domain.Transition{
		transition("start", &newSt, design),
		transition("design_done", &design, dev),
		transition("finish", &dev, done),
		kinded("reassign", transition("block", &dev, blocked)),
		kinded("reject", transition("redesign", &blocked, design)),
		kinded("cancel", transition("cancel", nil, cancelled)),
	}), allImplemented, false)

	if !v.Valid {
		t.Fatalf("cancel из любого статуса — выход blocked: %+v", v.Problems)
	}
}

// AC-7, п. 4: статус, в который при наборе не попасть, проблемой не
// считается, если терминальный достижим.
func TestOptionalUnreachableStatusIsFine(t *testing.T) {
	newSt := mandatory("new", true, false)
	design := opt("design", false)
	extra := mandatory("extra", false, false)
	done := mandatory("done", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, design, extra, done}, []domain.Transition{
		transition("start", &newSt, design),
		transition("design_done", &design, done),
		kinded("reassign", transition("to_extra", &design, extra)),
		transition("extra_done", &extra, done),
	}), allImplemented, false)

	if !v.Valid {
		t.Fatalf("недостижимый при [design] статус extra — не проблема: %+v", v.Problems)
	}
}

// AC-7, п. 6: цепочка из двух выключенных — advance в первый ведёт в конец
// цепочки.
func TestOptionalChainRedirect(t *testing.T) {
	newSt := mandatory("new", true, false)
	a := opt("a", true)
	b := opt("b", true)
	dev := mandatory("dev", false, false)
	done := mandatory("done", false, true)
	wf := positioned([]domain.Status{newSt, a, b, dev, done}, []domain.Transition{
		transition("start", &newSt, a),
		transition("a_done", &a, b),
		transition("b_done", &b, dev),
		transition("finish", &dev, done),
	})

	bypass := map[uuid.UUID]domain.Transition{
		wf.Statuses[1].ID: wf.Transitions[1],
		wf.Statuses[2].ID: wf.Transitions[2],
	}
	disabled := map[uuid.UUID]bool{wf.Statuses[1].ID: true, wf.Statuses[2].ID: true}
	if end := chainEnd(wf.Statuses[1].ID, disabled, bypass); end != wf.Statuses[3].ID {
		t.Fatalf("конец цепочки a → b → dev — dev, получили %v", end)
	}
	delete(disabled, wf.Statuses[2].ID)
	if end := chainEnd(wf.Statuses[1].ID, disabled, bypass); end != wf.Statuses[2].ID {
		t.Fatalf("при включённом b цепочка a кончается на b, получили %v", end)
	}

	if v := validateWorkflow(wf, allImplemented, false); !v.Valid {
		t.Fatalf("цепочка необязательных валидна: %+v", v.Problems)
	}
}

// Наборы перебираются по возрастанию размера, внутри — по position.
func TestOptionalSetsOrder(t *testing.T) {
	got := fmt.Sprint(optionalSets(3))
	want := "[[] [0] [1] [2] [0 1] [0 2] [1 2] [0 1 2]]"
	if got != want {
		t.Fatalf("порядок наборов: %s, ожидали %s", got, want)
	}
	if n := len(optionalSets(maxOptionalStatuses)); n != 1<<maxOptionalStatuses {
		t.Fatalf("наборов при пределе — %d", n)
	}
}

// AC-8: гейты на обходном — предупреждение; оба предупреждения не держат
// активацию.
func TestOptionalWarnings(t *testing.T) {
	newSt := mandatory("new", true, false)
	design := opt("design", true)
	dev := mandatory("dev", false, false)
	done := mandatory("done", false, true)

	v := validateWorkflow(positioned([]domain.Status{newSt, design, dev, done}, []domain.Transition{
		transition("start", &newSt, design),
		transition("design_done", &design, dev,
			domain.Gate{Key: "approve", Type: domain.GateHumanApproval, Blocking: true}),
		transition("finish", &dev, done),
		kinded("reassign", transition("redo", &dev, design)),
	}), allImplemented, false)

	if !v.Valid {
		t.Fatalf("предупреждения не держат: %+v", v.Problems)
	}
	if w := byCode(v.Warnings, "optional_bypass_gates"); len(w) != 1 || w[0].Transition != "design_done" || w[0].Status != "design" {
		t.Fatalf("ожидалось optional_bypass_gates design_done/design: %+v", v.Warnings)
	}
	if w := byCode(v.Warnings, "optional_reject_target"); len(w) != 1 || w[0].Transition != "redo" || w[0].Status != "design" {
		t.Fatalf("reassign в необязательный — optional_reject_target: %+v", v.Warnings)
	}
}

// AC-11: цикл без необязательных статусов новых кодов не получает, даже
// сломанный.
func TestOptionalAbsentKeepsVerdict(t *testing.T) {
	newSt := mandatory("new", true, false)
	work := mandatory("work", false, false)
	done := mandatory("done", false, true)
	v := validateWorkflow(positioned([]domain.Status{newSt, work, done}, []domain.Transition{
		transition("start", &newSt, work),
		kinded("reject", transition("back", &work, newSt)),
	}), allImplemented, false)
	if hasOptionalCode(v.Problems) || hasOptionalCode(v.Warnings) {
		t.Fatalf("без необязательных — без новых кодов: %+v %+v", v.Problems, v.Warnings)
	}
}

// Поломка исходного графа отдаётся базовыми правилами и свёрткой на пустом
// наборе (п. 5 решения: наборы — включая пустой).
func TestOptionalFoldReportsBaseBreakOnEmptySet(t *testing.T) {
	newSt := mandatory("new", true, false)
	design := opt("design", true)
	stuck := mandatory("stuck", false, false)
	done := mandatory("done", false, true)
	v := validateWorkflow(positioned([]domain.Status{newSt, design, stuck, done}, []domain.Transition{
		transition("start", &newSt, design),
		transition("design_done", &design, done),
		kinded("reassign", transition("to_stuck", &newSt, stuck)),
	}), allImplemented, false)

	if len(byCode(v.Problems, "dead_end")) != 1 {
		t.Fatalf("тупик stuck — dead_end: %+v", v.Problems)
	}
	got := byCode(v.Problems, "optional_breaks_path")
	if len(got) != 1 || got[0].Status != "stuck" || !strings.Contains(got[0].Message, "[]") {
		t.Fatalf("свёртка называет тупик на пустом наборе один раз: %+v", v.Problems)
	}
}

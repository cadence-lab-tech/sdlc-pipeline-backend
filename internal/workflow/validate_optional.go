package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// maxOptionalStatuses — предел необязательных статусов в версии цикла
// (решение 0066, п. 6). Свёртка перебирает все 2^N наборов выключенных: при
// десяти это 1024 прохода по графу, и проверка на каждой правке черновика
// остаётся мгновенной. Это константа кода, а не настройка: предел держит
// время проверки, а не вкус проекта.
const maxOptionalStatuses = 10

// validateOptional — правила необязательных этапов (решение 0066, п. 4-6):
// задача должна пройти цикл при любом наборе выключенных этапов.
//
// Движок, который будет пропускать этап (часть 3), идёт по обходному
// переходу — единственному исходящему advance из самого статуса. Поэтому
// сначала проверяется, что у каждого необязательного статуса обход
// определён однозначно и цепочки обходов не замыкаются, и только потом —
// свёртка графа по всем наборам: без однозначной цепочки сворачивать нечего.
//
// Цикл без необязательных статусов отсюда не получает ничего: его вердикт
// тот же, что до появления отметки.
func validateOptional(wf domain.Workflow) (problems, warnings []Problem) {
	statuses := byPosition(wf.Statuses)
	transitions := transitionsByPosition(wf.Transitions)

	var optional []domain.Status
	for _, st := range statuses {
		if st.IsOptional {
			optional = append(optional, st)
		}
	}
	if len(optional) == 0 {
		return nil, nil
	}

	statusByID := map[uuid.UUID]domain.Status{}
	for _, st := range statuses {
		statusByID[st.ID] = st
	}

	// Возврат или переназначение в необязательный этап недоступны, пока он у
	// задачи выключен (п. 13): собрать так можно, но автор должен это видеть.
	for _, t := range transitions {
		if t.Kind != "reject" && t.Kind != "reassign" {
			continue
		}
		if to, ok := statusByID[t.ToStatusID]; ok && to.IsOptional {
			warnings = append(warnings, Problem{
				Code: "optional_reject_target", Transition: t.Key, Status: to.Key,
				Message: fmt.Sprintf(
					"переход %q ведёт в необязательный этап %q: пока этап у задачи выключен, переход недоступен",
					t.Key, to.Key)})
		}
	}

	// bypass — обходной переход каждого необязательного статуса, у которого
	// он определён однозначно. Статус без записи свёртку останавливает.
	bypass := map[uuid.UUID]domain.Transition{}
	undefined := false
	for _, st := range optional {
		// Начальный и терминальный необязательными быть не могут: задача
		// начинает и заканчивает путь в них, пропускать нечего. Одна проблема
		// на статус — обход у такого статуса не ищется.
		if st.IsInitial {
			problems = append(problems, Problem{
				Code: "optional_initial", Status: st.Key,
				Message: fmt.Sprintf("начальный статус %q не может быть необязательным: с него начинается путь задачи", st.Key)})
			undefined = true
			continue
		}
		if st.IsTerminal {
			problems = append(problems, Problem{
				Code: "optional_terminal", Status: st.Key,
				Message: fmt.Sprintf("терминальный статус %q не может быть необязательным: в нём путь задачи заканчивается", st.Key)})
			undefined = true
			continue
		}

		// Переход «из любого статуса» обходным не считается: он не про этот
		// этап, а про любой (п. 4).
		var out []domain.Transition
		for _, t := range transitions {
			if t.Kind == "advance" && t.FromStatusID != nil && *t.FromStatusID == st.ID {
				out = append(out, t)
			}
		}
		switch len(out) {
		case 0:
			problems = append(problems, Problem{
				Code: "optional_no_bypass", Status: st.Key,
				Message: fmt.Sprintf(
					"у необязательного этапа %q нет перехода вида advance из него самого: выключенный этап нечем обойти", st.Key)})
			undefined = true
		case 1:
			bypass[st.ID] = out[0]
			if len(out[0].Gates) > 0 {
				warnings = append(warnings, Problem{
					Code: "optional_bypass_gates", Transition: out[0].Key, Status: st.Key,
					Message: fmt.Sprintf(
						"гейты обходного перехода %q не проверяются, когда этап %q у задачи выключен: они действуют только для задач с включённым этапом",
						out[0].Key, st.Key)})
			}
		default:
			problems = append(problems, Problem{
				Code: "optional_bypass_ambiguous", Status: st.Key, Transition: out[1].Key,
				Message: fmt.Sprintf(
					"у необязательного этапа %q несколько переходов вида advance (%q, %q…): обход выключенного этапа неоднозначен",
					st.Key, out[0].Key, out[1].Key)})
			undefined = true
		}
	}

	if ring := bypassRings(optional, bypass, statusByID); len(ring) > 0 {
		problems = append(problems, ring...)
		undefined = true
	}

	if len(optional) > maxOptionalStatuses {
		problems = append(problems, Problem{
			Code: "optional_too_many",
			Message: fmt.Sprintf(
				"необязательных этапов %d, больше предела %d: проверить все наборы выключенных этапов нельзя",
				len(optional), maxOptionalStatuses)})
		undefined = true
	}

	if undefined {
		return problems, warnings
	}
	return append(problems, foldProblems(statuses, transitions, optional, bypass)...), warnings
}

// bypassRings находит кольца цепочек обходов: выключи все статусы кольца — и
// пропуск никогда не кончится. Одно кольцо — одна проблема на первом по
// position статусе кольца; переход в себя — кольцо из одного статуса.
func bypassRings(optional []domain.Status, bypass map[uuid.UUID]domain.Transition, statusByID map[uuid.UUID]domain.Status) []Problem {
	var out []Problem
	reported := map[uuid.UUID]bool{}
	for _, start := range optional {
		path := []uuid.UUID{}
		index := map[uuid.UUID]int{}
		for id := start.ID; ; {
			if i, seen := index[id]; seen {
				ring := path[i:]
				if !reported[ring[0]] {
					out = append(out, ringProblem(ring, statusByID))
					for _, r := range ring {
						reported[r] = true
					}
				}
				break
			}
			if reported[id] {
				break
			}
			t, ok := bypass[id]
			if !ok {
				break
			}
			index[id] = len(path)
			path = append(path, id)
			id = t.ToStatusID
		}
	}
	return out
}

func ringProblem(ring []uuid.UUID, statusByID map[uuid.UUID]domain.Status) Problem {
	// Кольцо называется с первого по position статуса: тот же цикл, найденный
	// с другого конца, даёт ту же проблему.
	first := 0
	for i, id := range ring {
		if statusByID[id].Position < statusByID[ring[first]].Position {
			first = i
		}
	}
	keys := make([]string, 0, len(ring)+1)
	for i := range ring {
		keys = append(keys, statusByID[ring[(first+i)%len(ring)]].Key)
	}
	keys = append(keys, keys[0])
	return Problem{
		Code: "optional_bypass_cycle", Status: keys[0],
		Message: fmt.Sprintf(
			"цепочка обходов замкнулась на необязательных этапах: %s — выключи их все, и задача не выйдет из пропуска",
			strings.Join(keys, " → "))}
}

// foldProblems — свёртка графа (п. 5) по всем наборам выключенных этапов по
// возрастанию размера, внутри размера — по position. На каждом наборе
// advance в выключенный статус ведёт в конец его цепочки обходов, иные виды
// переходов в него удаляются, сам статус уходит из графа; затем — те же
// правила, что у dead_end и unreachable: терминальный достижим из
// начального, у каждого достижимого нетерминального статуса есть выход.
//
// Пустой набор — исходный граф (п. 5 решения: «все 2^N наборов, включая
// пустой»). Его поломку базовые правила уже отдали как dead_end или
// unreachable; свёртка называет её ещё раз своим кодом с пустым набором,
// чтобы автор видел: ломается цикл сам, а не выключение этапов.
func foldProblems(statuses []domain.Status, transitions []domain.Transition, optional []domain.Status, bypass map[uuid.UUID]domain.Transition) []Problem {
	var initial *domain.Status
	for i := range statuses {
		if statuses[i].IsInitial {
			initial = &statuses[i]
		}
	}
	if initial == nil {
		return nil
	}

	broken := map[uuid.UUID]bool{}
	terminalBroken := false
	var deadEnds []Problem
	var terminalProblem *Problem

	for _, set := range optionalSets(len(optional)) {
		disabled := map[uuid.UUID]bool{}
		for _, i := range set {
			disabled[optional[i].ID] = true
		}
		dead, terminalReached := foldOnce(statuses, transitions, initial.ID, disabled, bypass)

		names := make([]string, 0, len(set))
		for _, i := range set {
			names = append(names, optional[i].Key)
		}
		setText := "при выключенных этапах [" + strings.Join(names, ", ") + "]"
		if len(set) == 0 {
			setText = "даже без выключенных этапов []"
		}

		if !terminalReached && !terminalBroken {
			terminalBroken = true
			terminalProblem = &Problem{
				Code:    "optional_breaks_path",
				Message: setText + " задача не дойдёт до терминального статуса"}
		}
		for _, st := range dead {
			if broken[st.ID] {
				continue
			}
			broken[st.ID] = true
			deadEnds = append(deadEnds, Problem{
				Code: "optional_breaks_path", Status: st.Key,
				Message: fmt.Sprintf("%s у статуса %q нет выхода: задача в нём застрянет", setText, st.Key)})
		}
	}

	// Порядок — по position статусов-тупиков; проблема без статуса — первой.
	sort.SliceStable(deadEnds, func(i, j int) bool {
		return positionOf(statuses, deadEnds[i].Status) < positionOf(statuses, deadEnds[j].Status)
	})
	var out []Problem
	if terminalProblem != nil {
		out = append(out, *terminalProblem)
	}
	return append(out, deadEnds...)
}

// foldOnce сворачивает граф для одного набора выключенных и отдаёт
// достижимые статусы без выхода (по position) и достижим ли терминальный.
func foldOnce(statuses []domain.Status, transitions []domain.Transition, initial uuid.UUID,
	disabled map[uuid.UUID]bool, bypass map[uuid.UUID]domain.Transition) ([]domain.Status, bool) {
	type edge struct {
		from *uuid.UUID
		to   uuid.UUID
	}
	var edges []edge
	for _, t := range transitions {
		if t.FromStatusID != nil && disabled[*t.FromStatusID] {
			continue
		}
		to := t.ToStatusID
		if disabled[to] {
			if t.Kind != "advance" {
				continue
			}
			to = chainEnd(to, disabled, bypass)
		}
		edges = append(edges, edge{from: t.FromStatusID, to: to})
	}

	reached := map[uuid.UUID]bool{initial: true}
	for changed := true; changed; {
		changed = false
		for _, e := range edges {
			if reached[e.to] {
				continue
			}
			// Переход «из любого статуса» достижим, раз достижим начальный.
			if e.from == nil || reached[*e.from] {
				reached[e.to] = true
				changed = true
			}
		}
	}

	anywhere := false
	exits := map[uuid.UUID]bool{}
	for _, e := range edges {
		if e.from == nil {
			anywhere = true
			continue
		}
		exits[*e.from] = true
	}

	terminal := false
	var dead []domain.Status
	for _, st := range statuses {
		if !reached[st.ID] || disabled[st.ID] {
			continue
		}
		if st.IsTerminal {
			terminal = true
			continue
		}
		if !exits[st.ID] && !anywhere {
			dead = append(dead, st)
		}
	}
	return dead, terminal
}

// chainEnd — конец цепочки обходов выключенного статуса: первый статус,
// который в наборе не выключен. Кольца отсеяны до свёртки, поэтому цепочка
// конечна.
func chainEnd(id uuid.UUID, disabled map[uuid.UUID]bool, bypass map[uuid.UUID]domain.Transition) uuid.UUID {
	for disabled[id] {
		id = bypass[id].ToStatusID
	}
	return id
}

// optionalSets — все наборы индексов из n по возрастанию размера, внутри
// размера — лексикографически, то есть по position необязательных статусов.
func optionalSets(n int) [][]int {
	out := [][]int{{}}
	for k := 1; k <= n; k++ {
		comb := make([]int, k)
		for i := range comb {
			comb[i] = i
		}
		for {
			out = append(out, append([]int(nil), comb...))
			i := k - 1
			for i >= 0 && comb[i] == n-k+i {
				i--
			}
			if i < 0 {
				break
			}
			comb[i]++
			for j := i + 1; j < k; j++ {
				comb[j] = comb[j-1] + 1
			}
		}
	}
	return out
}

func byPosition(in []domain.Status) []domain.Status {
	out := append([]domain.Status(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

func transitionsByPosition(in []domain.Transition) []domain.Transition {
	out := append([]domain.Transition(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out
}

func positionOf(statuses []domain.Status, key string) int {
	for i, st := range statuses {
		if st.Key == key {
			return i
		}
	}
	return len(statuses)
}

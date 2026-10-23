package main

// Представления ответов для человека. Каждый рендер читает сырой JSON
// сервера в ровно те поля, что показывает: CLI по-прежнему не знает домена,
// он только раскладывает ответ в таблицу.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/trust"
)

type taskRow struct {
	Key            string          `json:"key"`
	Title          string          `json:"title"`
	StatusKey      string          `json:"status_key"`
	Priority       string          `json:"priority"`
	AssigneeHandle *string         `json:"assignee_handle"`
	ParentKey      *string         `json:"parent_key"`
	Properties     json.RawMessage `json:"properties"`
	UpdatedAt      *time.Time      `json:"updated_at"`
}

func renderTasks(raw json.RawMessage) error {
	var out struct {
		Tasks []taskRow `json:"tasks"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("КЛЮЧ", "СТАТУС", "ТИП", "ЗАГОЛОВОК", "ИСПОЛНИТЕЛЬ", "ЭПИК", "ОБНОВЛЕНА")
	for _, task := range out.Tasks {
		t.add(task.Key, task.StatusKey, taskType(task.Properties), trunc(task.Title, 48),
			dash(ptr(task.AssigneeHandle)), dash(ptr(task.ParentKey)), ago(task.UpdatedAt))
	}
	t.print()
	return nil
}

func renderQueue(raw json.RawMessage) error {
	var out struct {
		Queue []struct {
			Task taskRow `json:"task"`
			Run  struct {
				StatusKey     string     `json:"status_key"`
				State         string     `json:"state"`
				Attempt       int        `json:"attempt"`
				ClaimedHandle *string    `json:"claimed_by_handle"`
				LeaseExpires  *time.Time `json:"lease_expires_at"`
			} `json:"stage_run"`
			RoleKey   string `json:"role_key"`
			Claimable bool   `json:"claimable"`
		} `json:"queue"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("ЗАДАЧА", "СТАТУС", "РОЛЬ", "СОСТОЯНИЕ", "МОЖНО ЗАБРАТЬ", "ЗАГОЛОВОК")
	for _, item := range out.Queue {
		state := item.Run.State
		if h := ptr(item.Run.ClaimedHandle); h != "" {
			state += " (" + h + ")"
		}
		t.add(item.Task.Key, item.Run.StatusKey, dash(item.RoleKey), state, yesNo(item.Claimable), trunc(item.Task.Title, 48))
	}
	t.print()
	return nil
}

// stateView — ответ claim и show: задача глазами агента.
type stateView struct {
	StageRunID string `json:"stage_run_id"`
	Kind       string `json:"kind"`
	Task       struct {
		Key         string          `json:"key"`
		Title       string          `json:"title"`
		Status      string          `json:"status"`
		StatusName  string          `json:"status_name"`
		Attempt     int             `json:"attempt"`
		Priority    string          `json:"priority"`
		Type        string          `json:"type"`
		Parent      *string         `json:"parent"`
		Description *string         `json:"description"`
		ExternalRef json.RawMessage `json:"external_ref"`
		taskLevels
	} `json:"task"`
	// Рекомендация прогона (решение 0060). Сервер прежней версии её не
	// отдаёт — тогда строк уровня в сводке нет.
	Execution *executionView `json:"execution"`
	// Граница правил и данных (CLAB-209). Старый сервер её не отдаёт —
	// тогда подписи секций те же, а правило берётся своё.
	Boundary *struct {
		Note string `json:"note"`
	} `json:"boundary"`
	Role *struct {
		Key          string `json:"key"`
		Name         string `json:"name"`
		Version      int    `json:"version"`
		Instructions string `json:"instructions"`
		// Сырым: форму контракта проверяет сервер, а show не должен падать
		// из-за поля, которое он не печатает.
		OutputContract json.RawMessage `json:"output_contract"`
	} `json:"role"`
	Lease *struct {
		ExpiresAt time.Time `json:"expires_at"`
		Heartbeat int       `json:"heartbeat_interval_seconds"`
	} `json:"lease"`
	Context struct {
		Repositories []struct {
			Owner    string  `json:"owner"`
			Name     string  `json:"name"`
			RoleHint *string `json:"role_hint"`
			Primary  bool    `json:"primary"`
		} `json:"repositories"`
		PriorArtifacts []artifactRef     `json:"prior_artifacts"`
		Drift          []json.RawMessage `json:"drift"`
		Rejection      *struct {
			FromStage               string  `json:"from_stage"`
			Reason                  string  `json:"reason"`
			ProblemReportArtifactID *string `json:"problem_report_artifact_id"`
		} `json:"rejection"`
		Review *struct {
			Gate             string        `json:"gate"`
			Transition       string        `json:"transition"`
			SubjectRole      string        `json:"subject_role"`
			SubjectArtifacts []artifactRef `json:"subject_artifacts"`
		} `json:"review"`
	} `json:"context"`
	GatesAhead []struct {
		Transition string `json:"transition"`
		ToStatus   string `json:"to_status"`
		Gates      []struct {
			Key      string `json:"key"`
			Blocking bool   `json:"blocking"`
		} `json:"gates"`
	} `json:"gates_ahead"`
}

// artifactRef — ссылка на артефакт в контексте этапа: входы и то, что
// проверяет ревьюер. Содержимого в захвате нет, только координаты.
type artifactRef struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Stage   string `json:"stage"`
	Status  string `json:"status"`
	Version int    `json:"version"`
}

func renderState(raw json.RawMessage) error {
	var s stateView
	if err := decode(raw, &s); err != nil {
		return err
	}
	printSummary(s, true)
	return nil
}

// printSummary — сводка задачи, общая для show и claim. Причину возврата
// show показывает одной усечённой строкой, claim — целиком отдельной
// секцией, поэтому здесь она по флагу.
func printSummary(s stateView, rejectionLine bool) {
	fmt.Printf("%s  %s\n", s.Task.Key, s.Task.Title)
	pairs := []string{
		"Статус:", fmt.Sprintf("%s (%s), попытка %d, тип %s, приоритет %s",
			s.Task.Status, s.Task.StatusName, s.Task.Attempt, dash(s.Task.Type), dash(s.Task.Priority)),
	}
	if s.Task.Parent != nil {
		pairs = append(pairs, "Эпик:", *s.Task.Parent)
	}
	for _, link := range externalLinks(s.Task.ExternalRef) {
		pairs = append(pairs, "Трекер:", link)
	}
	if s.Role != nil {
		pairs = append(pairs, "Роль:", fmt.Sprintf("%s (%s) v%d", s.Role.Key, s.Role.Name, s.Role.Version))
	}
	run := dash(short(s.StageRunID))
	if s.Kind != "" {
		run += " " + s.Kind
	}
	if s.Lease != nil {
		run += fmt.Sprintf(", аренда до %s, heartbeat каждые %d с", when(&s.Lease.ExpiresAt), s.Lease.Heartbeat)
	}
	pairs = append(pairs, "Прогон:", run)
	// Строки уровня идут после «Прогон» без выравнивания по колонке подписей:
	// длинная подпись иначе сдвинула бы всю сводку, а скилы ищут строку
	// «Рекомендованный уровень: …» как есть.
	levelsAfter := len(pairs) / 2
	var repos []string
	for _, r := range s.Context.Repositories {
		item := r.Owner + "/" + r.Name
		var notes []string
		if h := ptr(r.RoleHint); h != "" {
			notes = append(notes, h)
		}
		if r.Primary {
			notes = append(notes, "основной")
		}
		if len(notes) > 0 {
			item += " (" + strings.Join(notes, ", ") + ")"
		}
		repos = append(repos, item)
	}
	pairs = append(pairs, "Репозитории:", joinOr(repos, "—"))
	pairs = append(pairs, "Контекст:", fmt.Sprintf("артефактов ранее %d, сигналов дрейфа %d",
		len(s.Context.PriorArtifacts), len(s.Context.Drift)))
	if s.Context.Rejection != nil && rejectionLine {
		pairs = append(pairs, "Возврат:", trunc(s.Context.Rejection.Reason, 120))
	}
	lines := kvLines(pairs...)
	if s.Execution != nil {
		levelsAfter = min(levelsAfter, len(lines))
		levels := []string{s.Execution.recommendedLine(), s.Task.taskLevels.summaryLine()}
		lines = append(lines[:levelsAfter], append(levels, lines[levelsAfter:]...)...)
	}
	for _, line := range lines {
		fmt.Println(line)
	}
	if len(s.GatesAhead) > 0 {
		fmt.Println("Переходы:")
		for _, tr := range s.GatesAhead {
			var gates []string
			for _, g := range tr.Gates {
				name := g.Key
				if !g.Blocking {
					name += "?"
				}
				gates = append(gates, name)
			}
			fmt.Printf("  %s → %s   гейты: %s\n", tr.Transition, tr.ToStatus, joinOr(gates, "нет"))
		}
	}
}

// renderClaim — ответ захвата: сводка, как у show, и всё, что нужно агенту
// без MCP, — что сдать, входы, возврат или перепроверка и инструкция роли
// целиком последней, чтобы длинный текст не прятал остальное. С файлом
// инструкция пишется в него, и только после того, как всё прочее
// напечатано: аренда уже взята, и id прогона агенту нужен даже при ошибке.
func renderClaim(raw json.RawMessage, instructionsFile string) error {
	var s stateView
	if err := decode(raw, &s); err != nil {
		return err
	}
	printSummary(s, false)

	// Правила и данные — разными секциями (CLAB-209): агент без MCP видит
	// границу так же, как по полю boundary. Инструкция роли — тоже правило,
	// но печатается последней (CLAB-146), поэтому секция правил разрезана.
	fmt.Println()
	fmt.Println(rulesHeading)
	if s.Role != nil {
		printContract(s.Role.OutputContract)
	}
	fmt.Println("Область и гейты: «Репозитории» и «Переходы» в сводке выше.")

	fmt.Println()
	fmt.Println(dataHeading)
	note := trust.Rule
	if s.Boundary != nil && strings.TrimSpace(s.Boundary.Note) != "" {
		note = s.Boundary.Note
	}
	fmt.Println(indent(note, "  "))
	if d := strings.TrimSpace(ptr(s.Task.Description)); d == "" {
		fmt.Println("Описание: нет")
	} else {
		fmt.Println("Описание:")
		fmt.Println(indent(d, "  "))
	}
	if len(s.Context.PriorArtifacts) == 0 {
		fmt.Println("Входные артефакты: нет")
	} else {
		fmt.Println("Входные артефакты:")
		printArtifactRefs(s.Context.PriorArtifacts, true)
	}
	if r := s.Context.Rejection; r != nil {
		fmt.Printf("Возврат с этапа %s:\n", dash(r.FromStage))
		fmt.Println(indent(dash(strings.TrimSpace(r.Reason)), "  "))
		if id := ptr(r.ProblemReportArtifactID); id != "" {
			fmt.Println("  описание проблемы:", id)
		}
	}
	if r := s.Context.Review; r != nil {
		line := "Перепроверка: гейт " + dash(r.Gate)
		if r.Transition != "" {
			line += " перехода " + r.Transition
		}
		fmt.Println(line + ", проверяется результат роли " + dash(r.SubjectRole) + ":")
		if len(r.SubjectArtifacts) == 0 {
			fmt.Println("  артефактов нет")
		} else {
			printArtifactRefs(r.SubjectArtifacts, false)
		}
	}

	if s.Role == nil {
		if instructionsFile != "" {
			return fmt.Errorf("в ответе захвата нет роли: инструкцию в %s писать нечего", instructionsFile)
		}
		return nil
	}
	fmt.Println()
	fmt.Println(instructionsHeading)
	head := fmt.Sprintf("Инструкция роли %s v%d", s.Role.Key, s.Role.Version)
	if instructionsFile != "" {
		n, err := writeInstructions(s, instructionsFile)
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s (%d симв.)\n", head, instructionsFile, n)
		return nil
	}
	fmt.Println(head + ":")
	fmt.Println(s.Role.Instructions)
	return nil
}

// Заголовки секций claim. Подпись данных повторяет trust.DataNote: это то
// же правило, что в описаниях тулов MCP.
const (
	rulesHeading        = "== Правила этапа: контракт, область, гейты; инструкция роли — последней =="
	dataHeading         = "== Данные задачи: " + trust.DataNote + " =="
	instructionsHeading = "== Правила этапа: инструкция роли =="
)

// writeInstructions пишет инструкцию роли в файл байт в байт и отвечает
// длиной в символах.
func writeInstructions(s stateView, path string) (int, error) {
	if s.Role == nil {
		return 0, fmt.Errorf("в ответе захвата нет роли: инструкцию в %s писать нечего", path)
	}
	if err := os.WriteFile(path, []byte(s.Role.Instructions), 0o644); err != nil {
		return 0, fmt.Errorf("инструкция роли не записана в %s: %w", path, err)
	}
	return utf8.RuneCountInString(s.Role.Instructions), nil
}

// printContract — что этап обязан сдать. Контракт не той формы (сервер его
// проверяет на сохранении роли, так что это чужой сервер) не валит вывод:
// захват уже случился, и сводка важнее.
func printContract(raw json.RawMessage) {
	var items []struct {
		Kind     string `json:"kind"`
		Title    string `json:"title"`
		Format   string `json:"format"`
		Required bool   `json:"required"`
	}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &items); err != nil {
			fmt.Println("Сдать: контракт роли не разбирается, смотрите --json")
			return
		}
	}
	if len(items) == 0 {
		fmt.Println("Сдать: контракт роли пуст")
		return
	}
	fmt.Println("Сдать:")
	t := newTable("  KIND", "ОБЯЗАТЕЛЬНО", "ФОРМАТ", "ЗАГОЛОВОК")
	for _, it := range items {
		t.add("  "+it.Kind, yesNo(it.Required), dash(it.Format), dash(it.Title))
	}
	t.print()
}

func printArtifactRefs(refs []artifactRef, withStage bool) {
	headers := []string{"  KIND", "ВЕРСИЯ"}
	if withStage {
		headers = append(headers, "ЭТАП")
	}
	t := newTable(append(headers, "ЗАГОЛОВОК", "ID")...)
	for _, a := range refs {
		row := []string{"  " + a.Kind, "v" + strconv.Itoa(a.Version)}
		if withStage {
			row = append(row, dash(a.Stage))
		}
		t.add(append(row, dash(a.Title), a.ID)...)
	}
	t.print()
}

func indent(text, prefix string) string {
	return prefix + strings.ReplaceAll(text, "\n", "\n"+prefix)
}

type gateOutcome struct {
	GateKey  string `json:"gate_key"`
	Type     string `json:"type"`
	Blocking bool   `json:"blocking"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
}

type transitionPreview struct {
	Key      string        `json:"key"`
	Name     string        `json:"name"`
	ToStatus string        `json:"to_status"`
	Allowed  bool          `json:"allowed"`
	Gates    []gateOutcome `json:"gates"`
}

func renderTransitions(items []transitionPreview) error {
	if len(items) == 0 {
		fmt.Println("переходов из текущего статуса нет")
		return nil
	}
	for _, tr := range items {
		mark := "✗"
		if tr.Allowed {
			mark = "✓"
		}
		fmt.Printf("%s %s → %s (%s)\n", mark, tr.Key, tr.ToStatus, tr.Name)
		printGates(tr.Gates)
	}
	return nil
}

func printGates(gates []gateOutcome) {
	for _, g := range gates {
		kind := g.Type
		if !g.Blocking {
			kind += ", не блокирует"
		}
		line := fmt.Sprintf("    [%s] %s (%s)", g.Status, g.GateKey, kind)
		if g.Reason != "" {
			line += " — " + trunc(g.Reason, 100)
		}
		fmt.Println(line)
	}
}

func renderTransitionResult(raw json.RawMessage) error {
	var out struct {
		Task struct {
			Key       string `json:"key"`
			StatusKey string `json:"status_key"`
		} `json:"task"`
		Blocked bool          `json:"blocked"`
		Gates   []gateOutcome `json:"gates"`
		FromRun struct {
			StatusKey string `json:"status_key"`
		} `json:"from_stage_run"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	if out.Blocked {
		fmt.Printf("%s остаётся в %s: гейты не пройдены\n", out.Task.Key, out.Task.StatusKey)
	} else {
		fmt.Printf("%s: %s → %s\n", out.Task.Key, dash(out.FromRun.StatusKey), out.Task.StatusKey)
	}
	printGates(out.Gates)
	return nil
}

func renderTask(raw json.RawMessage) error {
	var t taskRow
	if err := decode(raw, &t); err != nil {
		return err
	}
	fmt.Printf("%s  %s  [%s, %s]\n", t.Key, t.Title, taskType(t.Properties), t.StatusKey)
	return nil
}

// renderEditedTask показывает постановку после правки целиком: иначе у
// правки приоритета или меток не видно, что она применилась.
func renderEditedTask(raw json.RawMessage) error {
	var t struct {
		taskRow
		Labels      []string        `json:"labels"`
		Description *string         `json:"description"`
		ExternalRef json.RawMessage `json:"external_ref"`
	}
	if err := decode(raw, &t); err != nil {
		return err
	}
	fmt.Printf("%s  %s  [%s, %s]\n", t.Key, t.Title, taskType(t.Properties), t.StatusKey)
	fmt.Printf("Приоритет: %s   Метки: %s   Описание: %s\n",
		t.Priority, dash(strings.Join(t.Labels, ", ")), dash(describeLength(t.Description)))
	fmt.Printf("Трекер: %s\n", joinOr(externalLinks(t.ExternalRef), "—"))
	// Сервер прежней версии уровней не знает: строки нет, вывод прежний.
	if serverKnowsLevels(raw) {
		var levels taskLevels
		if err := decode(raw, &levels); err != nil {
			return err
		}
		fmt.Println(levels.editLine())
	}
	return nil
}

// renderCreatedWithLevels — строка заведённой задачи и её уровни: create с
// --model-level или --model-level-for показывает, что они записались.
func renderCreatedWithLevels(raw json.RawMessage) error {
	if err := renderTask(raw); err != nil {
		return err
	}
	if serverKnowsLevels(raw) {
		var levels taskLevels
		if err := decode(raw, &levels); err != nil {
			return err
		}
		fmt.Println(levels.editLine())
	}
	return nil
}

// externalLinks раскладывает ссылки на основной трекер в строки «система
// КЛЮЧ URL» по порядку систем. Записи чужой формы — legacy_epic_key у эпиков,
// перенесённых миграцией 000011, — пропускаются молча: это не ссылка, и
// ломать из-за неё вывод незачем.
func externalLinks(raw json.RawMessage) []string {
	var entries map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	systems := make([]string, 0, len(entries))
	for system := range entries {
		systems = append(systems, system)
	}
	sort.Strings(systems)
	var out []string
	for _, system := range systems {
		var link struct {
			Key string `json:"key"`
			URL string `json:"url"`
		}
		if json.Unmarshal(entries[system], &link) != nil || link.Key == "" {
			continue
		}
		out = append(out, strings.TrimSpace(system+" "+link.Key+" "+link.URL))
	}
	return out
}

// describeLength не печатает описание целиком: это абзацы, и в сводке правки
// важно, что оно есть или очищено, а не его текст.
func describeLength(description *string) string {
	if description == nil || *description == "" {
		return ""
	}
	return fmt.Sprintf("%d символов", utf8.RuneCountInString(*description))
}

func renderArtifact(raw json.RawMessage) error {
	var a struct {
		ID      string `json:"id"`
		Kind    string `json:"kind"`
		Title   string `json:"title"`
		Version int    `json:"version"`
		Size    int64  `json:"size_bytes"`
	}
	if err := decode(raw, &a); err != nil {
		return err
	}
	fmt.Printf("Сдан %s v%d «%s», %d байт (%s)\n", a.Kind, a.Version, a.Title, a.Size, short(a.ID))
	return nil
}

func renderDrift(raw json.RawMessage) error {
	var out struct {
		Signals []struct {
			ID          string     `json:"id"`
			Kind        string     `json:"kind"`
			Severity    string     `json:"severity"`
			State       string     `json:"state"`
			Repository  *string    `json:"repository"`
			FromCommit  *string    `json:"from_commit"`
			ToCommit    *string    `json:"to_commit"`
			RelatedTask *string    `json:"related_task"`
			DetectedAt  *time.Time `json:"detected_at"`
		} `json:"signals"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("УРОВЕНЬ", "ВИД", "СОСТОЯНИЕ", "РЕПОЗИТОРИЙ", "КОММИТЫ", "ЗАДАЧА", "НАЙДЕН", "ID")
	for _, s := range out.Signals {
		commits := ""
		if s.FromCommit != nil || s.ToCommit != nil {
			commits = short(ptr(s.FromCommit)) + "→" + short(ptr(s.ToCommit))
		}
		t.add(s.Severity, s.Kind, s.State, dash(ptr(s.Repository)), dash(commits),
			dash(ptr(s.RelatedTask)), ago(s.DetectedAt), s.ID)
	}
	t.print()
	return nil
}

func renderSearch(raw json.RawMessage) error {
	var out struct {
		Mode string `json:"mode"`
		Hits []struct {
			Scope      string  `json:"scope"`
			Score      float64 `json:"score"`
			Repository string  `json:"repository"`
			Path       string  `json:"path"`
			StartLine  int     `json:"start_line"`
			EndLine    int     `json:"end_line"`
			SymbolFQN  string  `json:"symbol_fqn"`
			TaskKey    string  `json:"task"`
			Kind       string  `json:"kind"`
			Title      string  `json:"title"`
			Text       string  `json:"text"`
		} `json:"results"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	if len(out.Hits) == 0 {
		fmt.Printf("ничего не найдено (режим %s)\n", out.Mode)
		return nil
	}
	fmt.Printf("режим поиска: %s\n", out.Mode)
	for i, h := range out.Hits {
		var where string
		switch h.Scope {
		case "code":
			where = fmt.Sprintf("%s:%s:%d-%d", h.Repository, h.Path, h.StartLine, h.EndLine)
			if h.SymbolFQN != "" {
				where += " " + h.SymbolFQN
			}
		case "artifacts":
			where = fmt.Sprintf("%s %s «%s»", h.TaskKey, h.Kind, h.Title)
		default:
			where = fmt.Sprintf("%s «%s»", h.Kind, h.Title)
		}
		fmt.Printf("\n%d. [%s %.2f] %s\n", i+1, h.Scope, h.Score, where)
		for _, line := range strings.SplitN(strings.TrimSpace(h.Text), "\n", 4)[:min(3, len(strings.SplitN(strings.TrimSpace(h.Text), "\n", 4)))] {
			fmt.Printf("   %s\n", trunc(line, 110))
		}
	}
	return nil
}

func renderIndex(raw json.RawMessage) error {
	var out struct {
		Repositories []struct {
			Repository    string     `json:"repository"`
			Enabled       bool       `json:"index_enabled"`
			Branch        *string    `json:"branch"`
			Commit        *string    `json:"commit"`
			State         *string    `json:"state"`
			FilesIndexed  *int       `json:"files_indexed"`
			FilesReused   *int       `json:"files_reused"`
			ChunksIndexed *int       `json:"chunks_indexed"`
			Error         *string    `json:"error"`
			LastIndexedAt *time.Time `json:"last_indexed_at"`
			// Failure — репозиторий не читается повторно (CLAB-79). Сервер
			// старше CLI поля не отдаёт, и вывод остаётся прежним.
			Failure *struct {
				Since  *time.Time `json:"since"`
				Error  string     `json:"error"`
				Source string     `json:"source"`
			} `json:"failure"`
		} `json:"repositories"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("РЕПОЗИТОРИЙ", "ИНДЕКС", "ВЕТКА", "КОММИТ", "ПРОГОН", "ФАЙЛЫ", "КУСКИ", "ПОСЛЕДНИЙ")
	for _, r := range out.Repositories {
		files := "—"
		if r.FilesIndexed != nil {
			files = fmt.Sprintf("%d+%d", *r.FilesIndexed, deref(r.FilesReused))
		}
		chunks := "—"
		if r.ChunksIndexed != nil {
			chunks = fmt.Sprint(*r.ChunksIndexed)
		}
		state := dash(ptr(r.State))
		if e := ptr(r.Error); e != "" {
			state += ": " + trunc(e, 40)
		}
		enabled := "вкл"
		if !r.Enabled {
			enabled = "выкл"
		}
		t.add(r.Repository, enabled, dash(ptr(r.Branch)), short(ptr(r.Commit)), state, files, chunks, ago(r.LastIndexedAt))
	}
	t.print()
	// Колонка прогона режет ошибку, чтобы таблица не разъезжалась; по ней
	// однажды не заметили восемнадцать дней стоявшей индексации. Репозиторий,
	// который не читается, назван отдельно — с полным текстом и временем.
	for _, r := range out.Repositories {
		if r.Failure == nil {
			continue
		}
		what := "прогон индексации не проходит"
		if r.Failure.Source == "head" {
			what = "голова ветки не читается"
		}
		fmt.Printf("\n%s не читается с %s (%s): %s\n  %s\n",
			r.Repository, when(r.Failure.Since), ago(r.Failure.Since), what,
			strings.TrimSpace(r.Failure.Error))
	}
	return nil
}

func deref(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}

// renderGitHubCheck — проверка доступа к репозиториям проекта: есть ли он и
// чем проверяли (решение 0034).
func renderGitHubCheck(raw json.RawMessage) error {
	var out struct {
		AppConfigured bool           `json:"app_configured"`
		App           *appStatusView `json:"app"`
		Repositories  []struct {
			Repository     string `json:"repository"`
			OK             bool   `json:"ok"`
			Error          string `json:"error"`
			Source         string `json:"source"`
			InstallationID *int64 `json:"installation_id"`
			CredentialRef  string `json:"credential_ref"`
		} `json:"repositories"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	printAppStatus(out.App, out.AppConfigured)
	t := newTable("РЕПОЗИТОРИЙ", "ДОСТУП", "ЧЕМ ПРОВЕРЯЛИ", "ЧТО НЕ ТАК")
	for _, r := range out.Repositories {
		access := "есть"
		if !r.OK {
			access = "нет"
		}
		t.add(r.Repository, access, credentialSource(r.Source, r.InstallationID, r.CredentialRef), dash(r.Error))
	}
	t.print()
	return nil
}

// appStatusView — состояние приложения GitHub у провайдера, каким его отдают
// ручки настроек (CLAB-79).
type appStatusView struct {
	State              string     `json:"state"`
	Error              string     `json:"error"`
	CheckedAt          *time.Time `json:"checked_at"`
	ClockOffsetSeconds *int       `json:"clock_offset_seconds"`
}

const appNotConfiguredLine = "Приложение у инстанса не настроено: установки не выписывают токенов."

// appStatusLines — состояние приложения словами. «Не удалось проверить» не
// называет ключ непринятым: недоступный GitHub — не отозванный ключ.
// Сервер старше CLI поля app не отдаёт — тогда остаётся прежняя строка про
// app_configured и ничего сверх.
func appStatusLines(app *appStatusView, configured bool) []string {
	if app == nil {
		if !configured {
			return []string{appNotConfiguredLine}
		}
		return nil
	}
	var lines []string
	switch app.State {
	case "ok":
		lines = append(lines, "Приложение GitHub: ключ принят"+checkedSuffix(app.CheckedAt)+".")
	case "rejected":
		lines = append(lines,
			"Приложение GitHub: ключ не принят провайдером"+checkedSuffix(app.CheckedAt)+": "+dash(app.Error),
			"Это настройка инстанса, чинит её администратор инстанса. Что проверить: ключ приложения "+
				"(SDLC_GITHUB_APP_KEY_FILE), идентификатор приложения (SDLC_GITHUB_APP_ID), часы машины.")
	case "unknown":
		lines = append(lines, "Приложение GitHub: не удалось проверить"+checkedSuffix(app.CheckedAt)+": "+dash(app.Error))
	case "not_configured":
		lines = append(lines, appNotConfiguredLine)
	default:
		// Состояние от сервера новее CLI показывается как есть.
		lines = append(lines, "Приложение GitHub: "+dash(app.State))
	}
	if app.ClockOffsetSeconds != nil {
		offset := time.Duration(*app.ClockOffsetSeconds) * time.Second
		direction := "отстают от времени провайдера"
		if offset < 0 {
			direction = "спешат относительно времени провайдера"
		}
		lines = append(lines, fmt.Sprintf("Часы машины %s на %s — проверьте синхронизацию времени.",
			direction, offset.Abs()))
	}
	return lines
}

func checkedSuffix(at *time.Time) string {
	if at == nil {
		return ""
	}
	return " (проверено " + when(at) + ")"
}

func printAppStatus(app *appStatusView, configured bool) {
	for _, line := range appStatusLines(app, configured) {
		fmt.Println(line)
	}
}

// renderTeamGitHub — установки приложения на команде и состояние самого
// приложения: живая установка при непринятом ключе ничего не читает.
func renderTeamGitHub(raw json.RawMessage) error {
	var out struct {
		App           *appStatusView `json:"app"`
		Installations []struct {
			InstallationID int64      `json:"installation_id"`
			AccountLogin   string     `json:"account_login"`
			AccountType    string     `json:"account_type"`
			CreatedAt      *time.Time `json:"created_at"`
			SuspendedAt    *time.Time `json:"suspended_at"`
			RemovedAt      *time.Time `json:"removed_at"`
		} `json:"installations"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	// Без поля app сервер старше CLI: о приложении сказать нечего.
	printAppStatus(out.App, true)
	t := newTable("УСТАНОВКА", "АККАУНТ", "ТИП", "СОСТОЯНИЕ", "ПОДКЛЮЧЕНА")
	for _, i := range out.Installations {
		state := "работает"
		switch {
		case i.RemovedAt != nil:
			state = "снята"
		case i.SuspendedAt != nil:
			state = "приостановлена"
		case out.App != nil && out.App.State == "rejected":
			state = "ключ приложения не принят"
		}
		t.add(fmt.Sprint(i.InstallationID), i.AccountLogin, dash(i.AccountType), state, when(i.CreatedAt))
	}
	t.print()
	return nil
}

// credentialSource — подпись источника учётных данных, как в настройках
// проекта. Незнакомое значение показывается как есть: сервер новее CLI.
func credentialSource(source string, installation *int64, ref string) string {
	id := ""
	if installation != nil {
		id = fmt.Sprint(*installation)
	}
	switch source {
	case "repository_secret":
		return "секрет репозитория " + ref
	case "repository_installation":
		return "установка репозитория " + id
	case "project_installation":
		return "установка проекта " + id
	case "project_secret":
		return "секрет проекта " + ref
	case "instance":
		return "токен инстанса"
	}
	return dash(source)
}

func renderMe(raw json.RawMessage) error {
	var me struct {
		Mode          string `json:"mode"`
		Authenticated bool   `json:"authenticated"`
		Principal     struct {
			Kind        string `json:"kind"`
			Handle      string `json:"handle"`
			DisplayName string `json:"display_name"`
			IsAdmin     bool   `json:"is_admin"`
		} `json:"principal"`
		Token *struct {
			Name       string     `json:"name"`
			Prefix     string     `json:"prefix"`
			ProjectKey *string    `json:"project_key"`
			RoleKeys   []string   `json:"role_keys"`
			ExpiresAt  *time.Time `json:"expires_at"`
		} `json:"token"`
		Memberships []struct {
			ProjectKey string   `json:"project_key"`
			Role       string   `json:"role"`
			RoleKeys   []string `json:"role_keys"`
		} `json:"memberships"`
		Teams []struct {
			TeamKey string `json:"team_key"`
			Role    string `json:"role"`
		} `json:"teams"`
		CanCreateProjects bool `json:"can_create_projects"`
	}
	if err := decode(raw, &me); err != nil {
		return err
	}
	who := me.Principal.Kind + ":" + me.Principal.Handle
	if me.Principal.DisplayName != "" && me.Principal.DisplayName != me.Principal.Handle {
		who += " (" + me.Principal.DisplayName + ")"
	}
	if me.Principal.IsAdmin {
		who += ", администратор системы"
	}
	pairs := []string{"Кто:", who, "Режим:", me.Mode}
	if me.Token != nil {
		tok := fmt.Sprintf("%s (%s…)", me.Token.Name, me.Token.Prefix)
		if me.Token.ProjectKey != nil {
			tok += ", проект " + *me.Token.ProjectKey
		}
		if len(me.Token.RoleKeys) > 0 {
			tok += ", роли " + strings.Join(me.Token.RoleKeys, ", ")
		}
		if me.Token.ExpiresAt != nil {
			tok += ", до " + when(me.Token.ExpiresAt)
		}
		pairs = append(pairs, "Токен:", tok)
	} else if me.Mode == "header" {
		pairs = append(pairs, "Токен:", "нет: актор из заголовка")
	}
	pairs = append(pairs, "Заводить проекты:", yesNo(me.CanCreateProjects))
	kv(pairs...)
	if len(me.Memberships) > 0 {
		fmt.Println("Проекты:")
		t := newTable("  ПРОЕКТ", "УРОВЕНЬ", "РОЛИ")
		for _, m := range me.Memberships {
			t.add("  "+m.ProjectKey, m.Role, joinOr(m.RoleKeys, "любые"))
		}
		t.print()
	}
	if len(me.Teams) > 0 {
		fmt.Println("Команды:")
		for _, tm := range me.Teams {
			fmt.Printf("  %s (%s)\n", tm.TeamKey, tm.Role)
		}
	}
	return nil
}

func renderTokens(raw json.RawMessage) error {
	var out struct {
		Tokens []struct {
			ID         string     `json:"id"`
			Name       string     `json:"name"`
			Handle     string     `json:"handle"`
			Kind       string     `json:"kind"`
			TokenKind  string     `json:"token_kind"`
			Prefix     string     `json:"prefix"`
			ProjectKey *string    `json:"project_key"`
			RoleKeys   []string   `json:"role_keys"`
			ExpiresAt  *time.Time `json:"expires_at"`
			LastUsedAt *time.Time `json:"last_used_at"`
			RevokedAt  *time.Time `json:"revoked_at"`
		} `json:"tokens"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("ИМЯ", "КОМУ", "ВИД", "НАЧАЛО", "ПРОЕКТ", "РОЛИ", "ИСТЕКАЕТ", "ИСПОЛЬЗОВАН", "ID")
	for _, tk := range out.Tokens {
		expires := "бессрочно"
		if tk.RevokedAt != nil {
			expires = "отозван " + when(tk.RevokedAt)
		} else if tk.ExpiresAt != nil {
			expires = when(tk.ExpiresAt)
		}
		t.add(trunc(tk.Name, 28), tk.Kind+":"+tk.Handle, tk.TokenKind, tk.Prefix, dash(ptr(tk.ProjectKey)),
			joinOr(tk.RoleKeys, "любые"), expires, ago(tk.LastUsedAt), tk.ID)
	}
	t.print()
	return nil
}

func renderMembers(raw json.RawMessage) error {
	var out struct {
		Members []struct {
			Handle      string   `json:"handle"`
			Kind        string   `json:"kind"`
			DisplayName string   `json:"display_name"`
			Role        string   `json:"role"`
			RoleKeys    []string `json:"role_keys"`
		} `json:"members"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("УЧАСТНИК", "УРОВЕНЬ", "РОЛИ", "ИМЯ")
	for _, m := range out.Members {
		t.add(m.Kind+":"+m.Handle, m.Role, joinOr(m.RoleKeys, "любые"), dash(m.DisplayName))
	}
	t.print()
	return nil
}

// inviteRow — приглашение из ответа сервера (решение 0035). Токена в нём нет:
// его сервер отдаёт один раз рядом, в ответе на выдачу.
type inviteRow struct {
	ID               string     `json:"id"`
	TargetKind       string     `json:"target_kind"`
	TeamKey          *string    `json:"team_key"`
	ProjectKey       *string    `json:"project_key"`
	TargetName       string     `json:"target_name"`
	TeamRole         *string    `json:"team_role"`
	Level            *string    `json:"level"`
	RoleKeys         []string   `json:"role_keys"`
	Status           string     `json:"status"`
	CreatedByHandle  string     `json:"created_by_handle"`
	ExpiresAt        *time.Time `json:"expires_at"`
	AcceptedByHandle *string    `json:"accepted_by_handle"`
	AcceptedAt       *time.Time `json:"accepted_at"`
	RevokedByHandle  *string    `json:"revoked_by_handle"`
	RevokedAt        *time.Time `json:"revoked_at"`
}

func (i inviteRow) isProject() bool { return i.ProjectKey != nil || i.TargetKind == "project" }

// target — «team acme — Acme» или «project CLAB — Название».
func (i inviteRow) target() string {
	key := ptr(i.TeamKey)
	if i.isProject() {
		key = ptr(i.ProjectKey)
	}
	s := strings.TrimSpace(i.TargetKind + " " + key)
	if i.TargetName != "" {
		s += " — " + i.TargetName
	}
	return s
}

// role — роль в команде у командного приглашения, уровень у проектного.
func (i inviteRow) role() string {
	if i.isProject() {
		return dash(ptr(i.Level))
	}
	return dash(ptr(i.TeamRole))
}

// roles — роли специалистов проектного приглашения; nil — любые.
func (i inviteRow) roles() string {
	if !i.isProject() {
		return "—"
	}
	return joinOr(i.RoleKeys, "любые")
}

// outcome — чем кончилось приглашение: кто принял или кто отозвал.
func (i inviteRow) outcome() string {
	switch {
	case i.AcceptedByHandle != nil:
		return *i.AcceptedByHandle + " " + when(i.AcceptedAt)
	case i.RevokedByHandle != nil:
		return "отозвал " + *i.RevokedByHandle + " " + when(i.RevokedAt)
	}
	return "—"
}

// inviteStatus — подпись статуса приглашения. Незнакомое значение
// показывается как есть: сервер новее CLI.
func inviteStatus(s string) string {
	switch s {
	case "pending":
		return "выдан"
	case "accepted":
		return "принят"
	case "expired":
		return "протух"
	case "revoked":
		return "отозван"
	}
	return dash(s)
}

// renderInvites — список приглашений цели в порядке сервера, свежие сверху.
// ID полностью: отзыв требует UUID целиком.
func renderInvites(raw json.RawMessage) error {
	var out struct {
		Invites []inviteRow `json:"invites"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("ID", "СТАТУС", "РОЛЬ", "РОЛИ", "ВЫДАЛ", "ИСТЕКАЕТ", "ПРИНЯЛ")
	for _, inv := range out.Invites {
		t.add(inv.ID, inviteStatus(inv.Status), inv.role(), inv.roles(), dash(inv.CreatedByHandle),
			when(inv.ExpiresAt), inv.outcome())
	}
	t.print()
	return nil
}

func renderInviteRevoked(raw json.RawMessage) error {
	var inv inviteRow
	if err := decode(raw, &inv); err != nil {
		return err
	}
	kv("ID:", inv.ID, "Куда:", inv.target(), "Статус:", inviteStatus(inv.Status),
		"Отозвал:", dash(ptr(inv.RevokedByHandle))+" "+when(inv.RevokedAt))
	return nil
}

// renderRepoRemoved печатает цену отвязки: что ушло вместе с репозиторием.
func renderRepoRemoved(raw json.RawMessage) error {
	var out struct {
		Repository struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		} `json:"repository"`
		Removed struct {
			IndexRuns  int `json:"index_runs"`
			CodeFiles  int `json:"code_files"`
			CodeChunks int `json:"code_chunks"`
			Documents  int `json:"documents"`
			Drift      int `json:"drift_signals_dismissed"`
		} `json:"removed"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	kv("Отвязан:", out.Repository.Owner+"/"+out.Repository.Name,
		"Прогонов индексации:", strconv.Itoa(out.Removed.IndexRuns),
		"Файлов кода:", strconv.Itoa(out.Removed.CodeFiles),
		"Чанков:", strconv.Itoa(out.Removed.CodeChunks),
		"Документов:", strconv.Itoa(out.Removed.Documents),
		"Сигналов дрейфа закрыто:", strconv.Itoa(out.Removed.Drift))
	fmt.Fprintln(os.Stderr, "Отвязка необратима: знание репозитория собирается заново при повторной привязке.")
	return nil
}

// renderProjectDeleted печатает цену удаления: что ушло вместе с проектом.
func renderProjectDeleted(raw json.RawMessage) error {
	var out struct {
		Project struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"project"`
		Removed struct {
			Tasks        int     `json:"tasks"`
			StageRuns    int     `json:"stage_runs"`
			Artifacts    int     `json:"artifacts"`
			Repositories int     `json:"repositories"`
			IndexRuns    int     `json:"index_runs"`
			CodeFiles    int     `json:"code_files"`
			CodeChunks   int     `json:"code_chunks"`
			Documents    int     `json:"documents"`
			Members      int     `json:"members"`
			Tokens       int     `json:"tokens"`
			Roles        int     `json:"roles"`
			Workflows    int     `json:"workflows"`
			Invites      int     `json:"invites"`
			CostUSD      float64 `json:"cost_usd"`
		} `json:"removed"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	kv("Удалён:", out.Project.Key+"  "+out.Project.Name,
		"Задач:", strconv.Itoa(out.Removed.Tasks),
		"Прогонов этапов:", strconv.Itoa(out.Removed.StageRuns),
		"Артефактов:", strconv.Itoa(out.Removed.Artifacts),
		"Репозиториев:", strconv.Itoa(out.Removed.Repositories),
		"Прогонов индексации:", strconv.Itoa(out.Removed.IndexRuns),
		"Файлов кода:", strconv.Itoa(out.Removed.CodeFiles),
		"Чанков:", strconv.Itoa(out.Removed.CodeChunks),
		"Документов:", strconv.Itoa(out.Removed.Documents),
		"Участников:", strconv.Itoa(out.Removed.Members),
		"Токенов проекта:", strconv.Itoa(out.Removed.Tokens),
		"Ролей:", strconv.Itoa(out.Removed.Roles),
		"Версий цикла:", strconv.Itoa(out.Removed.Workflows),
		"Приглашений:", strconv.Itoa(out.Removed.Invites),
		"Учтено стоимости:", fmt.Sprintf("$%.2f", out.Removed.CostUSD))
	fmt.Fprintln(os.Stderr, "Удаление необратимо: восстановить проект нечем, место в блобах освободит сборка мусора.")
	return nil
}

func renderProjects(raw json.RawMessage) error {
	var out struct {
		Projects []struct {
			Key          string  `json:"key"`
			Name         string  `json:"name"`
			OwnerTeamKey *string `json:"owner_team_key"`
			Installation *int64  `json:"github_installation_id"`
		} `json:"projects"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("КЛЮЧ", "НАЗВАНИЕ", "КОМАНДА", "GITHUB APP")
	for _, p := range out.Projects {
		inst := "—"
		if p.Installation != nil {
			inst = fmt.Sprintf("установка %d", *p.Installation)
		}
		t.add(p.Key, trunc(p.Name, 40), dash(ptr(p.OwnerTeamKey)), inst)
	}
	t.print()
	return nil
}

// ── версии цикла, решения, команды ──────────────────────────────────────────

type workflowRow struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Version   int      `json:"version"`
	State     string   `json:"state"`
	TaskTypes []string `json:"task_types"`
}

// renderWorkflows — таблица версий цикла. Идентификатор печатается целиком:
// его копируют в clab workflow activate.
func renderWorkflows(raw json.RawMessage) error {
	var out struct {
		Workflows []workflowRow `json:"workflows"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	t := newTable("ID", "ЦИКЛ", "ВЕРСИЯ", "СОСТОЯНИЕ", "ТИПЫ ЗАДАЧ")
	for _, w := range out.Workflows {
		t.add(w.ID, w.Name, fmt.Sprint(w.Version), w.State, joinOr(w.TaskTypes, "task"))
	}
	t.print()
	return nil
}

func renderActivation(raw json.RawMessage) error {
	var out struct {
		Workflow      workflowRow `json:"workflow"`
		Retired       []string    `json:"retired_workflow_ids"`
		PreviousTotal int         `json:"tasks_on_previous_version_total"`
		MovedTotal    int         `json:"tasks_moved_total"`
		Warnings      []struct {
			Message string `json:"message"`
		} `json:"warnings"`
	}
	if err := decode(raw, &out); err != nil {
		return err
	}
	w := out.Workflow
	fmt.Printf("активирован: %s, версия %d (%s)\n", w.Name, w.Version, joinOr(w.TaskTypes, "task"))
	if len(out.Retired) > 0 {
		fmt.Printf("  ушло на покой версий: %d\n", len(out.Retired))
	}
	if out.PreviousTotal > 0 {
		fmt.Printf("  задач доходит по прежней версии: %d\n", out.PreviousTotal)
	}
	if out.MovedTotal > 0 {
		fmt.Printf("  задач бэклога переехало на новую: %d\n", out.MovedTotal)
	}
	for _, p := range out.Warnings {
		fmt.Printf("  ! %s\n", p.Message)
	}
	return nil
}

// renderDecision — записанное решение по гейту в той же строке, что у
// clab gates.
func renderDecision(raw json.RawMessage) error {
	var g gateOutcome
	if err := decode(raw, &g); err != nil {
		return err
	}
	fmt.Println("решение записано:")
	printGates([]gateOutcome{g})
	return nil
}

func renderTeamCreated(raw json.RawMessage) error {
	var team struct {
		Key               string `json:"key"`
		Name              string `json:"name"`
		CanCreateProjects bool   `json:"can_create_projects"`
	}
	if err := decode(raw, &team); err != nil {
		return err
	}
	fmt.Printf("команда %s «%s» создана; может заводить проекты: %s\n",
		team.Key, team.Name, yesNo(team.CanCreateProjects))
	return nil
}

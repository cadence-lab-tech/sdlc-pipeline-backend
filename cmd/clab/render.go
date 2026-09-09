package main

// Представления ответов для человека. Каждый рендер читает сырой JSON
// сервера в ровно те поля, что показывает: CLI по-прежнему не знает домена,
// он только раскладывает ответ в таблицу.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
		Key        string  `json:"key"`
		Title      string  `json:"title"`
		Status     string  `json:"status"`
		StatusName string  `json:"status_name"`
		Attempt    int     `json:"attempt"`
		Priority   string  `json:"priority"`
		Type       string  `json:"type"`
		Parent     *string `json:"parent"`
	} `json:"task"`
	Role *struct {
		Key     string `json:"key"`
		Name    string `json:"name"`
		Version int    `json:"version"`
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
		PriorArtifacts []json.RawMessage `json:"prior_artifacts"`
		Drift          []json.RawMessage `json:"drift"`
		Rejection      *struct {
			Reason string `json:"reason"`
		} `json:"rejection"`
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

func renderState(raw json.RawMessage) error {
	var s stateView
	if err := decode(raw, &s); err != nil {
		return err
	}
	fmt.Printf("%s  %s\n", s.Task.Key, s.Task.Title)
	pairs := []string{
		"Статус:", fmt.Sprintf("%s (%s), попытка %d, тип %s, приоритет %s",
			s.Task.Status, s.Task.StatusName, s.Task.Attempt, dash(s.Task.Type), dash(s.Task.Priority)),
	}
	if s.Task.Parent != nil {
		pairs = append(pairs, "Эпик:", *s.Task.Parent)
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
	if s.Context.Rejection != nil {
		pairs = append(pairs, "Возврат:", trunc(s.Context.Rejection.Reason, 120))
	}
	kv(pairs...)
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
	return nil
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
	return nil
}

func deref(n *int) int {
	if n == nil {
		return 0
	}
	return *n
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

package launch

import (
	"reflect"
	"strings"
	"testing"
)

// CLAB-420, AC-2: имя программы первого слова команды.
func TestProgram(t *testing.T) {
	for word, want := range map[string]string{
		"claude": "claude", "/opt/bin/codex": "codex", "codex.exe": "codex", "Codex.EXE": "codex",
		`C:\tools\codex.exe`: "codex", "aider": "aider", "env": "env", "node": "node",
	} {
		if got := Program(word); got != want {
			t.Errorf("%q → %q, ждали %q", word, got, want)
		}
	}
}

// CLAB-420, AC-5 и AC-14: шаги 2, 3 и 5 порядка 0077 п. 7 — одна функция
// на clab exec и раннер.
func TestPickModel(t *testing.T) {
	snap := map[string]ModelChoice{"codex": {Model: "gpt-x", Effort: "high"}, "claude": {Model: ""}}
	levels := map[string]string{"high": "opus"}
	cases := []struct {
		name, level, agent string
		snap               map[string]ModelChoice
		levels             map[string]string
		want               Pick
	}{
		{"настройка", "high", "codex", snap, levels, Pick{Agent: "codex", Model: "gpt-x", Effort: "high", Source: SourceAgentSettings}},
		{"пустая запись — таблица", "high", "claude", snap, levels, Pick{Agent: "claude", Model: "opus", Source: SourceBinding}},
		{"нет уровня в таблице", "low", "claude", nil, levels, Pick{Agent: "claude", Source: SourceAgentDefault}},
		{"без уровня", "", "claude", nil, levels, Pick{Agent: "claude", Source: SourceAgentDefault}},
		{"без таблицы", "high", "generic", nil, nil, Pick{Agent: "generic", Source: SourceAgentDefault}},
	}
	for _, c := range cases {
		if got := PickModel(c.level, c.agent, c.snap, c.levels); got != c.want {
			t.Errorf("%s: %+v, ждали %+v", c.name, got, c.want)
		}
	}
}

// CLAB-420, AC-9 и решение человека после проверки CLAB-419: allow и deny
// проверяются порознь, ни одна часть политики не теряется молча.
func TestPolicyWarnings(t *testing.T) {
	both := []string{"claude", "--allowedTools", SlotAllowedTools, "--disallowedTools", SlotDisallowedTools}
	allowOnly := []string{"claude", "--allowedTools", SlotAllowedTools}
	allow, deny := []string{"Read", "Grep"}, []string{"Bash"}

	if w := PolicyWarnings("claude", true, both, allow, deny); w != nil {
		t.Errorf("обе подстановки на месте, а предупреждение: %q", w)
	}
	if w := PolicyWarnings("codex", false, nil, nil, nil); w != nil {
		t.Errorf("пустая политика, а предупреждение: %q", w)
	}
	w := PolicyWarnings("claude", true, allowOnly, allow, deny)
	if len(w) != 1 || !strings.Contains(w[0], "deny не передан") || !strings.Contains(w[0], SlotDisallowedTools) || !strings.Contains(w[0], "deny=Bash") {
		t.Errorf("deny без подстановки: %q", w)
	}
	if w := PolicyWarnings("claude", true, allowOnly, allow, nil); w != nil {
		t.Errorf("deny пуст, подстановка не нужна: %q", w)
	}
	w = PolicyWarnings("claude", true, []string{"claude", "{prompt}"}, allow, deny)
	if len(w) != 2 || !strings.Contains(w[0], "allow не передан") || !strings.Contains(w[1], "deny не передан") {
		t.Errorf("ни одной подстановки: %q", w)
	}
	// Своя команда привязки без перевода с подстановкой — перевод
	// администратора: части проверяются так же.
	if w := PolicyWarnings("generic", false, both, allow, deny); w != nil {
		t.Errorf("своя команда с обеими подстановками: %q", w)
	}
	if w := PolicyWarnings("generic", false, allowOnly, allow, deny); len(w) != 1 || !strings.Contains(w[0], "deny не передан привязке generic") {
		t.Errorf("своя команда без {disallowed_tools}: %q", w)
	}
	w = PolicyWarnings("codex", false, []string{"codex", "exec", "{prompt}"}, allow, deny)
	want := []string{"tool_policy роли не переведён для привязки codex: allow=Read,Grep, deny=Bash; агент запущен без ограничений по тулам"}
	if !reflect.DeepEqual(w, want) {
		t.Errorf("привязка без перевода: %q", w)
	}
	if w := PolicyWarnings("generic", false, nil, nil, deny); len(w) != 1 || !strings.Contains(w[0], "allow=—, deny=Bash") {
		t.Errorf("только deny: %q", w)
	}
}

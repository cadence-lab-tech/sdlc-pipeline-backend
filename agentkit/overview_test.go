package agentkit

import (
	"strings"
	"testing"
)

// CLAB-377: доску сменил «Обзор» (CLAB-365, решение 0070). Скилы называют
// адрес /p/<KEY>/overview, а старый /board — только редирект интерфейса, и
// ни один файл набора его не советует.
func TestKitPointsToOverview(t *testing.T) {
	k, err := Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range k.Files {
		if strings.Contains(f.Content, "/board") {
			t.Errorf("%s: старый адрес доски /board", f.Path)
		}
		if f.Path == "skills/clab-status/SKILL.md" {
			found = true
			if !strings.Contains(f.Content, "https://<instance>/p/<KEY>/overview") {
				t.Errorf("%s: нет адреса «Обзора» /p/<KEY>/overview", f.Path)
			}
		}
	}
	if !found {
		t.Fatal("в наборе нет skills/clab-status/SKILL.md")
	}
}

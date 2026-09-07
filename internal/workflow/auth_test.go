package workflow

import (
	"strings"
	"testing"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
)

func TestNewTokenShape(t *testing.T) {
	token, hash, prefix, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		t.Fatalf("токен без префикса: %q", token)
	}
	if len(token) < 30 {
		t.Fatalf("токен слишком короткий: %q", token)
	}
	if hash != HashToken(token) || len(hash) != 64 {
		t.Fatalf("хеш не sha256 токена")
	}
	if !strings.HasPrefix(token, prefix) || len(prefix) != 12 {
		t.Fatalf("префикс %q не из токена %q", prefix, token)
	}
	// Два вызова подряд не должны совпасть — иначе это не секрет.
	other, _, _, _ := NewToken()
	if other == token {
		t.Fatal("два токена подряд совпали")
	}
}

func TestSplitHandle(t *testing.T) {
	cases := []struct {
		raw   string
		def   domain.PrincipalKind
		kind  domain.PrincipalKind
		value string
	}{
		{"agent:bot", domain.PrincipalUser, domain.PrincipalAgent, "bot"},
		{"user:alice", domain.PrincipalAgent, domain.PrincipalUser, "alice"},
		{"alice", domain.PrincipalUser, domain.PrincipalUser, "alice"},
		{"alice", domain.PrincipalAgent, domain.PrincipalAgent, "alice"},
		// Неизвестный префикс — часть хэндла, как human:ci в e2e-сценарии.
		{"human:ci", domain.PrincipalUser, domain.PrincipalUser, "human:ci"},
		{"  agent:bot ", domain.PrincipalUser, domain.PrincipalAgent, "bot"},
	}
	for _, tc := range cases {
		kind, handle := SplitHandle(tc.raw, tc.def)
		if kind != tc.kind || handle != tc.value {
			t.Errorf("%q: получили (%s, %q), ожидали (%s, %q)", tc.raw, kind, handle, tc.kind, tc.value)
		}
	}
}

func TestMemberRoleCovers(t *testing.T) {
	if !domain.MemberAdmin.Covers(domain.MemberViewer) {
		t.Fatal("администратор обязан покрывать наблюдателя")
	}
	if domain.MemberViewer.Covers(domain.MemberAgent) {
		t.Fatal("наблюдатель не должен покрывать агента")
	}
	if domain.MemberRole("owner").Valid() {
		t.Fatal("неизвестный уровень не должен считаться допустимым")
	}
	if domain.MemberRole("").Covers(domain.MemberViewer) {
		t.Fatal("не участник не покрывает ничего")
	}
}

func TestRoleAllowed(t *testing.T) {
	run := domain.StageRun{RoleKey: "reviewer"}
	if err := roleAllowed(run, nil); err != nil {
		t.Fatalf("без ограничения захват разрешён: %v", err)
	}
	if err := roleAllowed(run, []string{"reviewer", "qa"}); err != nil {
		t.Fatalf("роль в списке: %v", err)
	}
	if err := roleAllowed(run, []string{"developer"}); err == nil {
		t.Fatal("роль вне списка обязана отбиваться")
	}
	if err := roleAllowed(run, []string{}); err == nil {
		t.Fatal("пустой, но заданный список запрещает всё")
	}
}

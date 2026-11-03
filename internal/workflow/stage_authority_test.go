package workflow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// AC-A2 (CLAB-379): право этапа — чистая функция; таблица уровень × набор
// ролей × роль статуса × действие (R3 целиком), оба значения from (свой
// статус, чужой) и глобальный переход. Без базы.
func TestStageAuthorityTable(t *testing.T) {
	type act struct {
		name string
		a    StageAction
		// Что положено agent на своём этапе (R3).
		agentOwn bool
	}
	tr := func(kind string) StageAction {
		return StageAction{Transition: "t_" + kind, Kind: kind, FromCurrent: true}
	}
	gate := func(typ, verdict string) StageAction {
		return StageAction{Transition: "t", Kind: "advance", FromCurrent: true, Gate: "g", GateType: typ, Verdict: verdict}
	}
	acts := []act{
		{"advance", tr("advance"), true},
		{"reject", tr("reject"), true},
		{"cancel", tr("cancel"), false},
		{"reassign", tr("reassign"), false},
		{"human_approval passed", gate(domain.GateHumanApproval, domain.GateResultPassed), true},
		{"human_approval failed", gate(domain.GateHumanApproval, domain.GateResultFailed), true},
		{"human_approval waived", gate(domain.GateHumanApproval, domain.GateResultWaived), false},
		{"checklist passed", gate(domain.GateChecklist, domain.GateResultPassed), true},
		{"checklist failed", gate(domain.GateChecklist, domain.GateResultFailed), false},
		{"checklist waived", gate(domain.GateChecklist, domain.GateResultWaived), false},
	}
	for _, typ := range []string{domain.GateAttemptsLimit, domain.GateStageTimeLimit, domain.GateAgentReview,
		domain.GateArtifactsRequired, domain.GateRepoScope, domain.GateDriftClear, domain.GateExternalCheck,
		domain.GateRepoRefRequired, domain.GateSchemaValidation, domain.GateDependenciesClosed} {
		for _, v := range []string{domain.GateResultPassed, domain.GateResultFailed, domain.GateResultWaived} {
			acts = append(acts, act{typ + " " + v, gate(typ, v), false})
		}
	}

	type stage struct {
		name string
		role string // основная роль статуса
	}
	stages := []stage{{"свой", "developer"}, {"чужой", "analyst"}, {"без исполнителя", ""}}
	roleSets := []struct {
		name  string
		roles []string
	}{{"NULL", nil}, {"[]", []string{}}, {"[developer]", []string{"developer"}}, {"[analyst developer]", []string{"analyst", "developer"}}}
	levels := []domain.MemberRole{"", domain.MemberViewer, domain.MemberAgent, domain.MemberMaintainer, domain.MemberAdmin}

	for _, level := range levels {
		for _, rs := range roleSets {
			for _, st := range stages {
				for _, ac := range acts {
					for _, from := range []string{"свой статус", "чужой статус", "глобальный"} {
						a := ac.a
						switch from {
						case "чужой статус":
							a.FromCurrent = false
						case "глобальный":
							a.FromCurrent, a.Global = false, true
						}
						v := StageAuthority(level, rs.roles, StageFacts{StatusKey: "s", StageRole: st.role}, a)
						name := strings.Join([]string{string(level), rs.name, st.name, ac.name, from}, " / ")

						var want bool
						var reason string
						switch {
						case level.Covers(domain.MemberMaintainer):
							want = true
						case !level.Covers(domain.MemberAgent):
							reason = DenyLevel
						case from != "свой статус", st.role == "":
							reason = DenyNoStageRole
						case !hasRole(rs.roles, st.role):
							reason = DenyStageRole
						case !ac.agentOwn:
							reason = DenyMaintainerAction
						default:
							want = true
						}
						if v.Allowed != want || v.Reason != reason || v.StageRole != st.role {
							t.Fatalf("%s: %+v, ждали allowed=%v reason=%q", name, v, want, reason)
						}
						if want {
							via := ViaStageRole
							if level.Covers(domain.MemberMaintainer) {
								via = ViaLevel
							}
							if v.Via != via || v.Err() != nil {
								t.Fatalf("%s: основание %q, ошибка %v", name, v.Via, v.Err())
							}
							continue
						}
						if v.Message == "" || v.Err() == nil {
							t.Fatalf("%s: отказ без текста или ошибки: %+v", name, v)
						}
					}
				}
			}
		}
	}

	// Тексты отказа называют роль статуса и роли актора (R4).
	v := StageAuthority(domain.MemberAgent, []string{"developer"}, StageFacts{StatusKey: "analysis", StageRole: "analyst"}, tr("advance"))
	for _, s := range []string{`этап "analysis"`, "роли analyst", "[developer]"} {
		if !strings.Contains(v.Message, s) {
			t.Fatalf("в отказе нет %q: %s", s, v.Message)
		}
	}
	// Неизвестный уровень (будущий reporter, CLAB-302) ниже agent — ничего.
	if v := StageAuthority("reporter", nil, StageFacts{StageRole: "developer"}, tr("advance")); v.Allowed || v.Reason != DenyLevel {
		t.Fatalf("уровень ниже agent: %+v", v)
	}
}

// AC-A2: транспорты не могут обойти право этапа. Обёртки без права
// (Execute, ExecuteBound, DecideGate, DecideGateSeen, UncheckedActor) —
// внутренние пути движка и тесты; вне тестов их зовёт только сам пакет
// workflow в своих обёртках. Новый вызов вне перечня валит тест.
func TestTransportsUseStageAuthority(t *testing.T) {
	// Имя → число аргументов: по нему Execute движка не путается с
	// template.Execute и подобными.
	unchecked := map[string]int{"Execute": 5, "ExecuteBound": 6, "DecideGate": 8,
		"DecideGateSeen": 9, "UncheckedActor": 2}

	root := filepath.Join("..", "..")
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "devops", "deploy":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			case *ast.Ident:
				name = fn.Name
			}
			if n, ok := unchecked[name]; ok && n == len(call.Args) {
				found = append(found, rel+": "+name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	// Единственные вызовы вне тестов — четыре обёртки движка, каждая зовёт
	// UncheckedActor.
	want := []string{
		"internal/workflow/engine.go: UncheckedActor",
		"internal/workflow/engine.go: UncheckedActor",
		"internal/workflow/engine.go: UncheckedActor",
		"internal/workflow/engine.go: UncheckedActor",
	}
	if !slices.Equal(found, want) {
		t.Fatalf("вызовы без права этапа вне тестов:\n%s\nждали:\n%s", strings.Join(found, "\n"), strings.Join(want, "\n"))
	}
}

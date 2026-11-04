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

// AC-A2 (CLAB-379), AC-17 (CLAB-410): право этапа — чистая функция; таблица
// класс актора (уровень × вид принципала) × набор ролей × роль статуса ×
// действие (виды переходов, гейты каждого типа и вердикты), оба значения
// from (свой статус, чужой) и глобальный переход. Без базы.
func TestStageAuthorityTable(t *testing.T) {
	type act struct {
		name string
		a    StageAction
		// Что положено боту на своём этапе (R3 CLAB-379).
		botOwn bool
		// Что положено сотруднику на любом этапе (таблица В-1, CLAB-410).
		employee bool
	}
	tr := func(kind string) StageAction {
		return StageAction{Transition: "t_" + kind, Kind: kind, FromCurrent: true}
	}
	gate := func(typ, verdict string, limit bool) StageAction {
		return StageAction{Transition: "t", Kind: "advance", FromCurrent: true, Gate: "g", GateType: typ, Verdict: verdict, Limit: limit}
	}
	acts := []act{
		{"advance", tr("advance"), true, true},
		{"reject", tr("reject"), true, true},
		{"cancel", tr("cancel"), false, true},
		{"reassign", tr("reassign"), false, true},
		{"human_approval passed", gate(domain.GateHumanApproval, domain.GateResultPassed, false), true, true},
		{"human_approval failed", gate(domain.GateHumanApproval, domain.GateResultFailed, false), true, true},
		{"human_approval waived", gate(domain.GateHumanApproval, domain.GateResultWaived, false), false, false},
		{"checklist passed", gate(domain.GateChecklist, domain.GateResultPassed, false), true, true},
		{"checklist failed", gate(domain.GateChecklist, domain.GateResultFailed, false), false, true},
		{"checklist waived", gate(domain.GateChecklist, domain.GateResultWaived, false), false, false},
	}
	// Пределы — только waived сотрудником; agent_review — и с пределом, и без.
	for _, l := range []struct {
		typ   string
		limit bool
	}{{domain.GateAttemptsLimit, true}, {domain.GateStageTimeLimit, true}, {domain.GateAgentReview, true}} {
		for _, v := range []string{domain.GateResultPassed, domain.GateResultFailed, domain.GateResultWaived} {
			acts = append(acts, act{l.typ + " с пределом " + v, gate(l.typ, v, l.limit), false, v == domain.GateResultWaived})
		}
	}
	for _, typ := range []string{domain.GateAgentReview,
		domain.GateArtifactsRequired, domain.GateRepoScope, domain.GateDriftClear, domain.GateExternalCheck,
		domain.GateRepoRefRequired, domain.GateSchemaValidation, domain.GateDependenciesClosed} {
		for _, v := range []string{domain.GateResultPassed, domain.GateResultFailed, domain.GateResultWaived} {
			acts = append(acts, act{typ + " " + v, gate(typ, v, false), false, false})
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
	kinds := []domain.PrincipalKind{domain.PrincipalUser, domain.PrincipalAgent, ""}

	checked := map[ActorClass]int{}
	for _, level := range levels {
		for _, kind := range kinds {
			for _, rs := range roleSets {
				actor := Actor{Principal: domain.Principal{Kind: kind}, Level: level, Roles: rs.roles}
				class := actor.Class()
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
							v := StageAuthority(actor, StageFacts{StatusKey: "s", StageRole: st.role}, a)
							name := strings.Join([]string{string(level), string(kind), rs.name, st.name, ac.name, from}, " / ")

							var want bool
							var reason, via string
							switch class {
							case ClassMaintainer:
								want, via = true, ViaLevel
							case ClassEmployee:
								via = ViaLevel
								if st.role != "" && hasRole(rs.roles, st.role) {
									via = ViaStageRole
								}
								// Откуда переход, сотруднику не важно (UC-3, 4а; снятие
								// stage_time_limit — на переходе входа).
								switch {
								case a.Gate == "", ac.employee:
									want = true
								default:
									reason = DenyMaintainerAction
								}
							case ClassBot:
								switch {
								case from != "свой статус", st.role == "":
									reason = DenyNoStageRole
								case !hasRole(rs.roles, st.role):
									reason = DenyStageRole
								case !ac.botOwn:
									reason = DenyMaintainerAction
								default:
									want, via = true, ViaStageRole
								}
							default:
								reason = DenyLevel
							}
							if v.Allowed != want || v.Reason != reason || v.StageRole != st.role {
								t.Fatalf("%s (%s): %+v, ждали allowed=%v reason=%q", name, class, v, want, reason)
							}
							checked[class]++
							if want {
								if v.Via != via || v.Err() != nil {
									t.Fatalf("%s (%s): основание %q, ждали %q, ошибка %v", name, class, v.Via, via, v.Err())
								}
								continue
							}
							if v.Message == "" || v.Err() == nil || v.Via != "" {
								t.Fatalf("%s (%s): отказ без текста или ошибки, или с основанием: %+v", name, class, v)
							}
						}
					}
				}
			}
		}
	}
	for _, c := range []ActorClass{ClassUnchecked, ClassOutsider, ClassBot, ClassEmployee, ClassMaintainer} {
		if checked[c] == 0 {
			t.Fatalf("класс %s не проверен: %v", c, checked)
		}
	}

	// Тексты отказа называют роль статуса и роли актора (R4).
	bot := Actor{Principal: domain.Principal{Kind: domain.PrincipalAgent}, Level: domain.MemberAgent, Roles: []string{"developer"}}
	v := StageAuthority(bot, StageFacts{StatusKey: "analysis", StageRole: "analyst"}, tr("advance"))
	for _, s := range []string{`этап "analysis"`, "роли analyst", "[developer]"} {
		if !strings.Contains(v.Message, s) {
			t.Fatalf("в отказе нет %q: %s", s, v.Message)
		}
	}
	// Неизвестный уровень (будущий reporter, CLAB-302) ниже agent — ничего,
	// и человеку тоже.
	for _, kind := range kinds {
		reporter := Actor{Principal: domain.Principal{Kind: kind}, Level: "reporter"}
		if v := StageAuthority(reporter, StageFacts{StageRole: "developer"}, tr("advance")); v.Allowed || v.Reason != DenyLevel {
			t.Fatalf("уровень ниже agent (%s): %+v", kind, v)
		}
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

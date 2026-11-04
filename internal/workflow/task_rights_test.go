package workflow

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// CLAB-411, R-1: класс актора — одна функция; порядок — токен задачи раньше
// уровня, уровень сопровождающего раньше вида принципала, вид не user — бот.
func TestActorClass(t *testing.T) {
	bound := &TaskBinding{TaskID: uuid.New(), TaskKey: "SDLC-1"}
	user, agent := domain.Principal{Kind: domain.PrincipalUser}, domain.Principal{Kind: domain.PrincipalAgent}
	for _, c := range []struct {
		a    Actor
		want ActorClass
	}{
		{Actor{Principal: user, Level: domain.MemberAdmin, Bound: bound}, ClassTaskToken},
		{Actor{Principal: user, Level: domain.MemberAgent, Bound: bound}, ClassTaskToken},
		{UncheckedActor(uuid.New(), bound), ClassTaskToken},
		{UncheckedActor(uuid.New(), nil), ClassMaintainer},
		{Actor{}, ClassUnchecked},
		{Actor{Principal: agent, Level: domain.MemberMaintainer}, ClassMaintainer},
		{Actor{Principal: user, Level: domain.MemberAdmin}, ClassMaintainer},
		{Actor{Principal: user, Level: domain.MemberAgent}, ClassEmployee},
		{Actor{Principal: agent, Level: domain.MemberAgent}, ClassBot},
		{Actor{Principal: domain.Principal{Kind: domain.PrincipalSystem}, Level: domain.MemberAgent}, ClassBot},
		{Actor{Level: domain.MemberAgent}, ClassBot},
		{Actor{Principal: user, Level: domain.MemberViewer}, ClassOutsider},
		// Будущий уровень ниже agent (CLAB-302) — ничего, а не права agent.
		{Actor{Principal: user, Level: "reporter"}, ClassOutsider},
	} {
		if got := c.a.Class(); got != c.want {
			t.Errorf("%+v: класс %s, ждали %s", c.a, got, c.want)
		}
	}
}

// K-3 на уровне функции: каждое действие для каждого класса — разрешение или
// отказ с кодом; прогноз карточки и движок зовут её же.
func TestTaskRightTable(t *testing.T) {
	ownID, parentID, otherID := uuid.New(), uuid.New(), uuid.New()
	bound := &TaskBinding{TaskID: ownID, TaskKey: "SDLC-1"}
	closed := time.Now()
	own := domain.Task{ID: ownID, Key: "SDLC-1"}
	kid := domain.Task{ID: uuid.New(), Key: "SDLC-2", ParentID: &ownID}
	foreign := domain.Task{ID: otherID, Key: "SDLC-3", ParentID: &parentID}
	done := domain.Task{ID: uuid.New(), Key: "SDLC-4", ClosedAt: &closed}

	user, agent := domain.Principal{Kind: domain.PrincipalUser}, domain.Principal{Kind: domain.PrincipalAgent}
	employee := Actor{Principal: user, Level: domain.MemberAgent}
	maintainer := Actor{Principal: user, Level: domain.MemberMaintainer}
	bot := Actor{Principal: agent, Level: domain.MemberAgent}
	token := Actor{Principal: user, Level: domain.MemberMaintainer, Bound: bound}
	viewer := Actor{Principal: user, Level: domain.MemberViewer}

	const yes, forbidden, conflict = apperr.Code(""), apperr.CodeForbidden, apperr.CodeConflict
	for _, c := range []struct {
		name string
		a    Actor
		act  TaskAct
		f    TaskFacts
		want apperr.Code
	}{
		{"сотрудник правит чужую", employee, ActEdit, TaskFacts{Task: foreign}, yes},
		{"сотрудник правит закрытую", employee, ActEdit, TaskFacts{Task: done}, yes},
		{"сотрудник: выбор этапов закрытой", employee, ActStageChoice, TaskFacts{Task: done}, conflict},
		{"сопровождающий: выбор этапов закрытой", maintainer, ActStageChoice, TaskFacts{Task: done}, conflict},
		{"сотрудник заводит без parent", employee, ActCreate, TaskFacts{}, yes},
		{"сотрудник: ребёнок чужой", employee, ActCreateChild, TaskFacts{Task: foreign}, yes},
		{"сотрудник: область", employee, ActScope, TaskFacts{}, yes},
		{"сотрудник: сужение", employee, ActScope, TaskFacts{Narrow: true}, yes},
		{"бот без аренды правит", bot, ActEdit, TaskFacts{Task: foreign}, forbidden},
		{"бот с арендой правит", bot, ActEdit, TaskFacts{Task: foreign, Held: true}, yes},
		{"бот с арендой родителя правит", bot, ActEdit, TaskFacts{Task: foreign, ParentHeld: true}, yes},
		{"бот: выбор этапов без аренды", bot, ActStageChoice, TaskFacts{Task: foreign}, forbidden},
		{"бот с арендой: выбор этапов закрытой", bot, ActStageChoice, TaskFacts{Task: done, Held: true}, conflict},
		{"бот без parent", bot, ActCreate, TaskFacts{}, forbidden},
		{"бот: ребёнок без аренды", bot, ActCreateChild, TaskFacts{Task: foreign}, forbidden},
		{"бот: ребёнок с арендой", bot, ActCreateChild, TaskFacts{Task: foreign, Held: true}, yes},
		{"бот назначает", bot, ActAssign, TaskFacts{Task: foreign}, yes},
		{"бот связывает", bot, ActLink, TaskFacts{Task: foreign}, yes},
		{"бот: область", bot, ActScope, TaskFacts{}, forbidden},
		{"бот: сужение", bot, ActScope, TaskFacts{Narrow: true}, forbidden},
		{"бот: свойства", bot, ActProperties, TaskFacts{}, yes},
		{"токен правит свою", token, ActEdit, TaskFacts{Task: own}, yes},
		{"токен правит ребёнка", token, ActEdit, TaskFacts{Task: kid}, yes},
		{"токен правит чужую", token, ActEdit, TaskFacts{Task: foreign}, forbidden},
		{"токен: выбор этапов своей", token, ActStageChoice, TaskFacts{Task: own}, forbidden},
		{"токен: выбор этапов ребёнка", token, ActStageChoice, TaskFacts{Task: kid}, yes},
		{"токен без parent", token, ActCreate, TaskFacts{}, forbidden},
		{"токен: ребёнок своей", token, ActCreateChild, TaskFacts{Task: own}, yes},
		{"токен: ребёнок ребёнка", token, ActCreateChild, TaskFacts{Task: kid}, forbidden},
		{"токен назначает чужую", token, ActAssign, TaskFacts{Task: foreign}, forbidden},
		{"токен: своя blocks чужую", token, ActLink, TaskFacts{Task: own, Other: &foreign, Rel: domain.TaskLinkBlocks}, yes},
		{"токен: чужая blocks свою", token, ActLink, TaskFacts{Task: foreign, Other: &own, Rel: domain.TaskLinkBlocks}, forbidden},
		{"токен: чужая relates_to свою", token, ActLink, TaskFacts{Task: foreign, Other: &own, Rel: domain.TaskLinkRelatesTo}, yes},
		{"токен: прогноз связи чужой", token, ActLink, TaskFacts{Task: foreign}, forbidden},
		{"токен: область", token, ActScope, TaskFacts{}, forbidden},
		{"токен: свойства", token, ActProperties, TaskFacts{}, forbidden},
		{"viewer правит", viewer, ActEdit, TaskFacts{Task: foreign}, forbidden},
		{"viewer: свойства", viewer, ActProperties, TaskFacts{}, forbidden},
		{"без уровня: всё", Actor{}, ActScope, TaskFacts{}, yes},
	} {
		err := TaskRight(c.a, c.act, c.f)
		if c.want == yes {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil || apperr.As(err).Code != c.want {
			t.Errorf("%s: %v, ждали %s", c.name, err, c.want)
		}
	}

	// Тексты: токену задачи — отказ исполнителю, боту — кто может (R-6).
	if err := TaskRight(token, ActScope, TaskFacts{}); !strings.HasPrefix(apperr.As(err).Message, ExecutorRefusal) {
		t.Fatalf("область токеном задачи: %v", err)
	}
	for _, narrow := range []bool{false, true} {
		msg := apperr.As(TaskRight(bot, ActScope, TaskFacts{Narrow: narrow})).Message
		if !strings.Contains(msg, "сопровождающий") || !strings.Contains(msg, "сотрудник") || !strings.Contains(msg, "решение человека") {
			t.Fatalf("отказ боту (сужение %v): %q", narrow, msg)
		}
	}
}

// AC-4, AC-15 на движке: сотрудник связывает задачи только одного проекта
// (400, как раньше); токен задачи держателя-сопровождающего не заводит
// ребёнка своей задачи вне её области, внутренний вызов без уровня — как
// прежде без ограничений.
func TestEngineTaskRightsBoundaries(t *testing.T) {
	ctx := context.Background()
	store, engine, _, _, system := setupPhase4(t, AuthHeader)
	anna, err := store.ResolveActor(ctx, store.DB(), "anna", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	employee := Actor{Principal: anna, Level: domain.MemberAgent}
	foreign, _ := testdb.ForeignTask(t, store)
	if _, err := engine.LinkTasks(ctx, "SDLC-1", foreign.Key, domain.TaskLinkRelatesTo, "", employee, false); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("связь между проектами: %v", err)
	}
	if _, err := engine.LinkTasks(ctx, "SDLC-1", "SDLC-2", domain.TaskLinkRelatesTo, "", employee, false); err != nil {
		t.Fatalf("связь сотрудником: %v", err)
	}

	own, err := store.GetTaskByKey(ctx, store.DB(), "SDLC-1")
	if err != nil {
		t.Fatal(err)
	}
	bound := &TaskBinding{TaskID: own.ID, TaskKey: own.Key}
	child := func(level domain.MemberRole, b *TaskBinding, repo string) error {
		_, err := engine.CreateTask(ctx, "SDLC", system.ID, CreateTaskInput{
			CreateTaskInput: postgres.CreateTaskInput{Title: "Ребёнок", ParentKey: own.Key, Repositories: []string{repo}},
			Level:           level, Kind: domain.PrincipalUser, Bound: b,
		})
		return err
	}
	if err := child(domain.MemberMaintainer, bound, "frontend"); apperr.As(err).Code != apperr.CodeScopeExpansionForbidden {
		t.Fatalf("токен задачи сопровождающего вне области: %v", err)
	}
	if err := child(domain.MemberMaintainer, bound, "backend"); err != nil {
		t.Fatalf("токен задачи сопровождающего в области: %v", err)
	}
	if err := child("", nil, "frontend"); err != nil {
		t.Fatalf("внутренний вызов вне области: %v", err)
	}
	if err := child(domain.MemberAgent, nil, "frontend"); err != nil {
		t.Fatalf("сотрудник вне области: %v", err)
	}
}

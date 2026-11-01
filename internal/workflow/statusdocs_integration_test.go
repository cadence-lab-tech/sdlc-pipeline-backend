package workflow

import (
	"context"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Отбор документов роли на статусе цикла (CLAB-348, решение 0068, п. 3) по
// настоящей схеме: захват, правка черновика, проверка, связка, миграция.
// Номера — критерии приёмки анализа CLAB-348.

var (
	makerDocs = []RoleDocumentInput{
		{Name: "main.md", Content: "Ты исполнитель.\n"},
		{Name: "git.md", Kind: "reference", Content: "Ветки clab/<ключ>."},
		{Name: "release.md", Kind: "checklist", Content: "- выкладка проверена — ёж 🦔"},
	}
	checkerDocs = []RoleDocumentInput{
		{Name: "main.md", Content: "Ты проверяющий.\n"},
		{Name: "review.md", Content: "Вердикт без перевода строки"},
	}
)

const statusDocsCycle = "StatusDocs"

// statusDocsBundle — цикл, где у основной роли maker отбор на development
// (порядок отбора обратный версии), testing (одно имя нет в версии), verify
// (ни одного имени в версии); на review отбора у maker нет, а у
// проверяющего checker — один документ.
func statusDocsBundle() Bundle {
	yes := true
	return Bundle{
		Version: BundleVersion,
		Roles: []BundleRole{
			{Key: "maker", Name: "Исполнитель", Documents: bundleDocs(makerDocs),
				OutputContract: json.RawMessage(`[{"kind":"dev.note","required":true}]`)},
			{Key: "checker", Name: "Проверяющий", Documents: bundleDocs(checkerDocs),
				OutputContract: json.RawMessage(`[{"kind":"review.verdict","required":true}]`)},
		},
		Workflows: []BundleWorkflow{{
			Name: statusDocsCycle, TaskTypes: []string{"task"},
			Statuses: []BundleStatus{
				{Key: "new", Name: "New", Category: "backlog", IsInitial: true},
				{Key: "development", Name: "Development", Category: "active", Role: "maker",
					RoleDocuments: []string{"release.md", "main.md"}},
				{Key: "testing", Name: "Testing", Category: "active", Role: "maker",
					RoleDocuments: []string{"main.md", "gone.md"}},
				{Key: "verify", Name: "Verify", Category: "active", Role: "maker",
					RoleDocuments: []string{"gone.md"}},
				{Key: "review", Name: "Review", Category: "active", Role: "maker",
					Reviewers: []string{"checker"}, ReviewerDocuments: map[string][]string{"checker": {"review.md"}}},
				{Key: "done", Name: "Done", Category: "done", IsTerminal: true},
				{Key: "cancelled", Name: "Cancelled", Category: "cancelled", IsTerminal: true},
			},
			Transitions: []BundleTransition{
				{Key: "start", Name: "Start", From: "new", To: "development", Kind: "advance"},
				{Key: "to_testing", Name: "To testing", From: "development", To: "testing", Kind: "advance"},
				{Key: "to_verify", Name: "To verify", From: "testing", To: "verify", Kind: "advance"},
				{Key: "to_review", Name: "To review", From: "verify", To: "review", Kind: "advance"},
				{Key: "finish", Name: "Finish", From: "review", To: "done", Kind: "advance",
					Gates: []BundleGate{{Key: "peer_review", Name: "Перепроверка", Type: domain.GateAgentReview,
						Blocking: &yes, Config: json.RawMessage(`{"reviewer_role_key":"checker"}`)}}},
				{Key: "cancel", Name: "Cancel", To: "cancelled", Kind: "cancel"},
			},
		}},
	}
}

func bundleDocs(in []RoleDocumentInput) []BundleRoleDocument {
	out := make([]BundleRoleDocument, len(in))
	for i, d := range in {
		out[i] = BundleRoleDocument{Name: d.Name, Kind: d.Kind, Content: d.Content}
	}
	return out
}

// docsByName — документы входа по именам, в порядке входа: из них
// собирается ожидаемая склейка.
func docsByName(in []RoleDocumentInput, names ...string) []domain.RoleDocument {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	var picked []RoleDocumentInput
	for _, d := range in {
		if want[d.Name] {
			picked = append(picked, d)
		}
	}
	return documentsOf(picked)
}

func missingWarnings(v Validation) []Problem {
	var out []Problem
	for _, w := range v.Warnings {
		if w.Code == codeRoleDocumentMissing {
			out = append(out, w)
		}
	}
	return out
}

type statusDocsWorld struct {
	store     *postgres.Store
	engine    *Engine
	authoring *Authoring
	system    domain.Principal
	active    uuid.UUID
	report    ImportReport
	activated ActivationResult
}

func newStatusDocsWorld(t *testing.T) statusDocsWorld {
	t.Helper()
	ctx := context.Background()
	store, engine, authoring, _, system := setupPhase4(t, AuthHeader)
	report, err := authoring.Import(ctx, "SDLC", system.ID, statusDocsBundle(), false)
	if err != nil {
		t.Fatalf("импорт цикла: %v", err)
	}
	if len(report.Workflows) != 1 || report.Workflows[0].Outcome != "draft_created" || !report.Validations[0].Valid {
		t.Fatalf("отчёт импорта: %+v %+v", report.Workflows, report.Validations)
	}
	id := uuid.MustParse(report.Workflows[0].ID)
	activated, err := authoring.Activate(ctx, id, system.ID)
	if err != nil {
		t.Fatalf("активация: %v", err)
	}
	return statusDocsWorld{store: store, engine: engine, authoring: authoring, system: system,
		active: id, report: report, activated: activated}
}

func (w statusDocsWorld) task(t *testing.T, title string) string {
	t.Helper()
	task, err := w.engine.CreateTask(context.Background(), "SDLC", w.system.ID, CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{Title: title}})
	if err != nil {
		t.Fatal(err)
	}
	w.move(t, task.Key, "start")
	return task.Key
}

func (w statusDocsWorld) move(t *testing.T, key, transition string) TransitionResult {
	t.Helper()
	res, err := w.engine.Execute(context.Background(), key, transition, w.system.ID, "")
	if err != nil {
		t.Fatalf("%s: %v", transition, err)
	}
	return res
}

func (w statusDocsWorld) state(t *testing.T, key string) ClaimResult {
	t.Helper()
	res, err := w.engine.State(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if res.Role == nil {
		t.Fatalf("%s: в состоянии нет роли", key)
	}
	return res
}

// AC-348.5–.8: захват и /state отдают отобранные документы в порядке
// версии, склейку из них, предупреждения по отсутствующим; без отбора —
// прежний ответ без ключа document_warnings; перепроверка — отбор своей
// строки.
func TestClaimSelectsStatusDocuments(t *testing.T) {
	ctx := context.Background()
	w := newStatusDocsWorld(t)
	key := w.task(t, "Отбор документов")

	// AC-348.5: отбор ["release.md","main.md"] — порядок версии.
	claimed, err := w.engine.Claim(ctx, key, w.system.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	want := docsByName(makerDocs, "main.md", "release.md")
	for name, res := range map[string]ClaimResult{"claim": claimed, "state": w.state(t, key)} {
		if !reflect.DeepEqual(namesOf(res.Role.Documents), []string{"main.md", "release.md"}) {
			t.Fatalf("%s: документы %v", name, namesOf(res.Role.Documents))
		}
		for i, d := range res.Role.Documents {
			if d.SHA256 != want[i].SHA256 || d.Content == nil || *d.Content != *want[i].Content {
				t.Fatalf("%s: документ %s не совпадает с версией", name, d.Name)
			}
		}
		if res.Role.Instructions != ConcatDocuments(want) ||
			!strings.Contains(res.Role.Instructions, "<!-- document: release.md -->") ||
			strings.Contains(res.Role.Instructions, "git.md") {
			t.Fatalf("%s: склейка %q", name, res.Role.Instructions)
		}
		if _, ok := roleJSON(t, res.Role)["document_warnings"]; ok {
			t.Fatalf("%s: document_warnings без отсутствующих имён", name)
		}
	}

	// AC-348.7: одно имя нет — найденные документы и строка про gone.md.
	w.move(t, key, "to_testing")
	res := w.state(t, key)
	if !reflect.DeepEqual(namesOf(res.Role.Documents), []string{"main.md"}) ||
		res.Role.Instructions != makerDocs[0].Content {
		t.Fatalf("testing: %v %q", namesOf(res.Role.Documents), res.Role.Instructions)
	}
	if len(res.Role.DocumentWarnings) != 1 || !strings.Contains(res.Role.DocumentWarnings[0], "gone.md") {
		t.Fatalf("testing: предупреждения %v", res.Role.DocumentWarnings)
	}
	if _, err := w.engine.Claim(ctx, key, w.system.ID, "test"); err != nil {
		t.Fatalf("захват с отсутствующим именем: %v", err)
	}

	// AC-348.7: ни одного — все документы и предупреждение «выданы все».
	w.move(t, key, "to_verify")
	res = w.state(t, key)
	if len(res.Role.Documents) != 3 || res.Role.Instructions != ConcatDocuments(documentsOf(makerDocs)) {
		t.Fatalf("verify: %v", namesOf(res.Role.Documents))
	}
	if len(res.Role.DocumentWarnings) != 1 || !strings.Contains(res.Role.DocumentWarnings[0], "все документы") {
		t.Fatalf("verify: предупреждения %v", res.Role.DocumentWarnings)
	}

	// AC-348.6: без отбора — все документы, склейка равна колонке, ключа нет.
	w.move(t, key, "to_review")
	res = w.state(t, key)
	rv, err := w.authoring.GetRoleVersion(ctx, "SDLC", "maker", res.Role.Version)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Role.Documents) != 3 || res.Role.Instructions != rv.Instructions {
		t.Fatalf("review без отбора: %v", namesOf(res.Role.Documents))
	}
	if _, ok := roleJSON(t, res.Role)["document_warnings"]; ok {
		t.Fatal("review без отбора: ключ document_warnings в ответе")
	}

	// AC-348.8: перепроверка — отбор строки checker, один документ байт в байт.
	run := res.StageRunID
	if _, err := w.engine.SubmitArtifact(ctx, key, w.system.ID, SubmitArtifactInput{
		Kind: "dev.note", Title: "Результат", Content: []byte("# результат"), StageRunID: &run,
	}); err != nil {
		t.Fatal(err)
	}
	if blocked := w.move(t, key, "finish"); !blocked.Blocked {
		t.Fatalf("finish обязан ждать перепроверку: %+v", blocked)
	}
	review := w.state(t, key)
	if review.Kind != domain.StageKindReview || review.Role.Key != "checker" {
		t.Fatalf("ждали прогон перепроверки роли checker: %s %s", review.Kind, review.Role.Key)
	}
	if !reflect.DeepEqual(namesOf(review.Role.Documents), []string{"review.md"}) ||
		review.Role.Instructions != checkerDocs[1].Content || review.Role.DocumentWarnings != nil {
		t.Fatalf("перепроверка: %v %q %v", namesOf(review.Role.Documents), review.Role.Instructions,
			review.Role.DocumentWarnings)
	}
}

// AC-348.2–.4: правка отбора у черновика, отказ у активной версии, отказы
// формы без изменений, снятие отбора запросом без поля, копия черновика
// переносит отбор.
func TestSetStatusRoleDocuments(t *testing.T) {
	ctx := context.Background()
	w := newStatusDocsWorld(t)

	// Копия активной версии несёт отбор у основной роли и у проверяющего.
	draft, err := w.authoring.CreateDraft(ctx, "SDLC", w.system.ID, DraftInput{From: w.active.String(), Name: statusDocsCycle})
	if err != nil {
		t.Fatal(err)
	}
	full, err := w.store.GetWorkflow(ctx, w.store.DB(), draft.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, st := range full.Statuses {
		for _, r := range st.Roles {
			if r.DocumentNames != nil {
				got[st.Key+"/"+r.RoleKey] = r.DocumentNames
			}
		}
	}
	wantCopy := map[string][]string{
		"development/maker": {"release.md", "main.md"}, "testing/maker": {"main.md", "gone.md"},
		"verify/maker": {"gone.md"}, "review/checker": {"review.md"},
	}
	if !reflect.DeepEqual(got, wantCopy) {
		t.Fatalf("копия черновика: %v", got)
	}

	// Сохранение отбора — массив в заданном порядке, ответ его несёт.
	st, err := w.authoring.SetStatusRoles(ctx, draft.ID, "review", []postgres.StatusRoleInput{
		{RoleKey: "maker", IsPrimary: true, DocumentNames: []string{"release.md", "git.md", "нет-нигде.md"}},
		{RoleKey: "checker", Position: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Roles) != 2 || !reflect.DeepEqual(st.Roles[0].DocumentNames, []string{"release.md", "git.md", "нет-нигде.md"}) ||
		st.Roles[1].DocumentNames != nil {
		t.Fatalf("ответ правки: %+v", st.Roles)
	}
	saved := func() []domain.StatusRole {
		t.Helper()
		status, err := w.store.GetStatusByKey(ctx, w.store.DB(), draft.ID, "review")
		if err != nil {
			t.Fatal(err)
		}
		roles, err := w.store.StatusRoles(ctx, w.store.DB(), status.ID)
		if err != nil {
			t.Fatal(err)
		}
		return roles
	}
	before := saved()
	if !reflect.DeepEqual(before, st.Roles) {
		t.Fatalf("в базе %+v, в ответе %+v", before, st.Roles)
	}

	many := make([]string, MaxRoleDocuments+1)
	for i := range many {
		many[i] = "d" + strconv.Itoa(i) + ".md"
	}
	for name, sel := range map[string][]string{
		"пустой список": {}, "пустое имя": {"main.md", ""}, "повтор": {"git.md", "git.md"}, "больше 50": many,
	} {
		_, err := w.authoring.SetStatusRoles(ctx, draft.ID, "review", []postgres.StatusRoleInput{
			{RoleKey: "maker", IsPrimary: true, DocumentNames: sel},
		})
		if apperr.As(err).Code != apperr.CodeInvalidInput || !strings.Contains(err.Error(), "maker") {
			t.Fatalf("%s: ждали invalid_input с ролью, получили %v", name, err)
		}
		if !reflect.DeepEqual(saved(), before) {
			t.Fatalf("%s: набор ролей изменился после отказа", name)
		}
	}

	// Запрос без отбора у роли снимает его: набор описан целиком.
	st, err = w.authoring.SetStatusRoles(ctx, draft.ID, "review", []postgres.StatusRoleInput{
		{RoleKey: "maker", IsPrimary: true}, {RoleKey: "checker", Position: 1},
	})
	if err != nil || st.Roles[0].DocumentNames != nil || saved()[0].DocumentNames != nil {
		t.Fatalf("снятие отбора: %+v %v", st.Roles, err)
	}

	// AC-348.3: активная версия не правится.
	activeStatus, err := w.store.GetStatusByKey(ctx, w.store.DB(), w.active, "development")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.authoring.SetStatusRoles(ctx, w.active, "development", []postgres.StatusRoleInput{
		{RoleKey: "maker", IsPrimary: true, DocumentNames: []string{"git.md"}},
	})
	if apperr.As(err).Code != apperr.CodeConflict {
		t.Fatalf("правка активной версии: %v", err)
	}
	roles, err := w.store.StatusRoles(ctx, w.store.DB(), activeStatus.ID)
	if err != nil || !reflect.DeepEqual(roles[0].DocumentNames, []string{"release.md", "main.md"}) {
		t.Fatalf("отбор активной версии изменился: %+v %v", roles, err)
	}
}

// AC-348.9, .10: предупреждения role_document_missing в проверке,
// активации и импорте; сверка с последней версией роли.
func TestValidateStatusDocuments(t *testing.T) {
	ctx := context.Background()
	w := newStatusDocsWorld(t)

	check := func(where string, v Validation) {
		t.Helper()
		warn := missingWarnings(v)
		if !v.Valid || len(warn) != 2 {
			t.Fatalf("%s: valid=%v, предупреждений %d: %+v", where, v.Valid, len(warn), warn)
		}
		byStatus := map[string]Problem{}
		for _, p := range warn {
			byStatus[p.Status] = p
		}
		tst, verify := byStatus["testing"], byStatus["verify"]
		if !strings.Contains(tst.Message, `"gone.md"`) || !strings.Contains(tst.Message, "maker") ||
			strings.Contains(tst.Message, "все документы") {
			t.Fatalf("%s: testing %+v", where, tst)
		}
		if !strings.Contains(verify.Message, "все документы") {
			t.Fatalf("%s: verify %+v", where, verify)
		}
	}
	check("импорт", w.report.Validations[0])
	check("активация", Validation{Valid: true, Warnings: w.activated.Warnings})
	v, err := w.authoring.Validate(ctx, w.active)
	if err != nil {
		t.Fatal(err)
	}
	check("проверка", v)

	// Повторный импорт той же связки — unchanged, предупреждения те же.
	again, err := w.authoring.Import(ctx, "SDLC", w.system.ID, Bundle{Version: BundleVersion,
		Workflows: statusDocsBundle().Workflows}, true)
	if err != nil || again.Workflows[0].Outcome != "unchanged" {
		t.Fatalf("повтор импорта: %+v %v", again.Workflows, err)
	}
	check("повтор импорта", again.Validations[0])

	// Новая версия роли с gone.md — предупреждения исчезают.
	docs := append(append([]RoleDocumentInput{}, makerDocs...), RoleDocumentInput{Name: "gone.md", Content: "Нашёлся."})
	if _, err := w.authoring.AddRoleVersion(ctx, "SDLC", "maker", w.system.ID, RoleVersionInput{Documents: docs}); err != nil {
		t.Fatal(err)
	}
	v, err = w.authoring.Validate(ctx, w.active)
	if err != nil {
		t.Fatal(err)
	}
	if warn := missingWarnings(v); len(warn) != 0 {
		t.Fatalf("после версии с gone.md: %+v", warn)
	}
}

// AC-348.11, .12: экспорт пишет отбор только там, где он есть; повторный
// импорт — unchanged, перестановка имён — тоже, смена отбора —
// draft_created; импорт в другой проект воспроизводит отбор; неверный
// отбор отбивает импорт целиком.
func TestBundleCarriesStatusDocuments(t *testing.T) {
	ctx := context.Background()
	w := newStatusDocsWorld(t)

	exported, err := w.authoring.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if exported.Version != BundleVersion || BundleVersion != 3 {
		t.Fatalf("формат связки %d", exported.Version)
	}
	var cycle *BundleWorkflow
	for i := range exported.Workflows {
		if exported.Workflows[i].Name == statusDocsCycle {
			cycle = &exported.Workflows[i]
		}
	}
	if cycle == nil {
		t.Fatal("в экспорте нет цикла")
	}
	raw, err := json.Marshal(cycle)
	if err != nil {
		t.Fatal(err)
	}
	var plain struct {
		Statuses []map[string]json.RawMessage `json:"statuses"`
	}
	if err := json.Unmarshal(raw, &plain); err != nil {
		t.Fatal(err)
	}
	for _, st := range plain.Statuses {
		var key string
		_ = json.Unmarshal(st["key"], &key)
		_, hasRole := st["role_documents"]
		_, hasReviewers := st["reviewer_documents"]
		wantRole := key == "development" || key == "testing" || key == "verify"
		if hasRole != wantRole || hasReviewers != (key == "review") {
			t.Fatalf("статус %s: role_documents=%v reviewer_documents=%v", key, hasRole, hasReviewers)
		}
	}
	if string(plain.Statuses[1]["role_documents"]) != `["release.md","main.md"]` ||
		string(plain.Statuses[4]["reviewer_documents"]) != `{"checker":["review.md"]}` {
		t.Fatalf("поля отбора: %s %s", plain.Statuses[1]["role_documents"], plain.Statuses[4]["reviewer_documents"])
	}

	importOne := func(project string, wf BundleWorkflow, roles bool) (ImportReport, error) {
		b := Bundle{Version: BundleVersion, Workflows: []BundleWorkflow{wf}}
		if roles {
			b.Roles = statusDocsBundle().Roles
		}
		return w.authoring.Import(ctx, project, w.system.ID, b, false)
	}
	if rep, err := importOne("SDLC", *cycle, false); err != nil || rep.Workflows[0].Outcome != "unchanged" {
		t.Fatalf("повтор экспорта: %+v %v", rep.Workflows, err)
	}
	permuted := *cycle
	permuted.Statuses = append([]BundleStatus{}, cycle.Statuses...)
	permuted.Statuses[1].RoleDocuments = []string{"main.md", "release.md"}
	if rep, err := importOne("SDLC", permuted, false); err != nil || rep.Workflows[0].Outcome != "unchanged" {
		t.Fatalf("перестановка имён: %+v %v", rep.Workflows, err)
	}
	changed := permuted
	changed.Statuses = append([]BundleStatus{}, permuted.Statuses...)
	changed.Statuses[1].RoleDocuments = []string{"main.md"}
	if rep, err := importOne("SDLC", changed, false); err != nil || rep.Workflows[0].Outcome != "draft_created" {
		t.Fatalf("смена отбора: %+v %v", rep.Workflows, err)
	}
	dropped := *cycle
	dropped.Statuses = append([]BundleStatus{}, cycle.Statuses...)
	dropped.Statuses[4].ReviewerDocuments = nil
	if rep, err := importOne("SDLC", dropped, false); err != nil || rep.Workflows[0].Outcome != "draft_created" {
		t.Fatalf("снятие отбора проверяющего: %+v %v", rep.Workflows, err)
	}

	// Другой проект: черновик с тем же отбором.
	project, err := w.store.GetProjectByKey(ctx, w.store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.engine.CreateProject(ctx, w.system.ID, postgres.CreateProjectInput{
		Key: "NEXT", Name: "Соседний", OwnerTeamID: *project.OwnerTeamID}); err != nil {
		t.Fatal(err)
	}
	rep, err := importOne("NEXT", *cycle, true)
	if err != nil || rep.Workflows[0].Outcome != "draft_created" {
		t.Fatalf("импорт в другой проект: %+v %v", rep.Workflows, err)
	}
	copied, err := w.store.GetWorkflow(ctx, w.store.DB(), uuid.MustParse(rep.Workflows[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if again := exportWorkflow(copied); !sameWorkflow(*again, *cycle) ||
		!reflect.DeepEqual(again.Statuses[4].ReviewerDocuments, map[string][]string{"checker": {"review.md"}}) {
		t.Fatalf("отбор в другом проекте: %+v", again.Statuses)
	}

	// AC-348.12: неверный отбор — invalid_input с ключом статуса, ничего не создано.
	countWorkflows := func() int {
		t.Helper()
		var n int
		if err := w.store.DB().QueryRow(ctx, `SELECT count(*) FROM workflows`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	many := make([]string, MaxRoleDocuments+1)
	for i := range many {
		many[i] = "d" + strconv.Itoa(i) + ".md"
	}
	bad := map[string]func(st *BundleStatus){
		"без роли":              func(st *BundleStatus) { st.Role = ""; st.Reviewers = nil },
		"пустой список":         func(st *BundleStatus) { st.RoleDocuments = []string{} },
		"пустое имя":            func(st *BundleStatus) { st.RoleDocuments = []string{"main.md", " "} },
		"повтор":                func(st *BundleStatus) { st.RoleDocuments = []string{"main.md", "main.md"} },
		"больше 50":             func(st *BundleStatus) { st.RoleDocuments = many },
		"ключ не из reviewers":  func(st *BundleStatus) { st.ReviewerDocuments = map[string][]string{"stranger": {"main.md"}} },
		"пустой у проверяющего": func(st *BundleStatus) { st.ReviewerDocuments = map[string][]string{"checker": {}} },
		"null у проверяющего":   func(st *BundleStatus) { st.ReviewerDocuments = map[string][]string{"checker": nil} },
	}
	before := countWorkflows()
	for name, mutate := range bad {
		wf := statusDocsBundle().Workflows[0]
		wf.Name = "Неверный отбор"
		st := &wf.Statuses[4] // review: maker и проверяющий checker
		st.RoleDocuments = []string{"main.md"}
		mutate(st)
		_, err := w.authoring.Import(ctx, "SDLC", w.system.ID, Bundle{Version: BundleVersion,
			Roles: statusDocsBundle().Roles, Workflows: []BundleWorkflow{wf}}, false)
		if apperr.As(err).Code != apperr.CodeInvalidInput || !strings.Contains(err.Error(), `"review"`) {
			t.Fatalf("%s: ждали invalid_input со статусом review, получили %v", name, err)
		}
		if countWorkflows() != before {
			t.Fatalf("%s: после отказа появился цикл", name)
		}
	}
}

// AC-348.1: колонка без значения по умолчанию, у существующих строк NULL,
// пустой массив запрещён схемой.
func TestStatusRoleDocumentsSchema(t *testing.T) {
	ctx := context.Background()
	store, _, _, _, _ := setupPhase4(t, AuthHeader)
	var total, selected int
	if err := store.DB().QueryRow(ctx, `
		SELECT count(*), count(document_names) FROM status_roles`).Scan(&total, &selected); err != nil {
		t.Fatal(err)
	}
	if total == 0 || selected != 0 {
		t.Fatalf("строк %d, с отбором %d: сиды обязаны остаться без отбора", total, selected)
	}
	var def *string
	if err := store.DB().QueryRow(ctx, `
		SELECT column_default FROM information_schema.columns
		WHERE table_name = 'status_roles' AND column_name = 'document_names'`).Scan(&def); err != nil {
		t.Fatal(err)
	}
	if def != nil {
		t.Fatalf("у колонки значение по умолчанию %q", *def)
	}
	_, err := store.DB().Exec(ctx, `UPDATE status_roles SET document_names = '{}'`)
	if err == nil || !strings.Contains(err.Error(), "status_roles_document_names_not_empty") {
		t.Fatalf("пустой отбор прошёл мимо CHECK: %v", err)
	}
	if _, err := store.DB().Exec(ctx, `
		UPDATE status_roles SET document_names = '{main.md}'
		WHERE id = (SELECT id FROM status_roles LIMIT 1)`); err != nil {
		t.Fatalf("непустой отбор: %v", err)
	}
}

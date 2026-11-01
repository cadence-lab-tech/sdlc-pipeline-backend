package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Библиотека инструкций (CLAB-351, решение 0069). Номера — критерии
// приёмки анализа CLAB-351.

const (
	gitV1 = "Ветки clab/<ключ>-<slug>, свежий main — merge'ем.\n"
	gitV2 = "Ветки clab/<ключ>-<slug>, свежий main — merge'ем.\nPR сдаётся MERGEABLE.\n"
)

func countN(t *testing.T, store *postgres.Store, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// roleWithGit заводит роль с документами main.md и git.md (git — обычный
// текст или ссылка на инструкцию).
func roleWithGit(t *testing.T, a *Authoring, actor domain.Principal, key string, git RoleDocumentInput) domain.RoleVersion {
	t.Helper()
	ctx := context.Background()
	docs := []RoleDocumentInput{{Name: "main.md", Content: "Ты " + key + ".\n"}, git}
	out, err := a.CreateRole(ctx, "SDLC", actor.ID, CreateRoleInput{
		Key: key, Name: key, Version: &RoleVersionInput{
			Documents: docs, InputKinds: []string{"analysis.brief"},
			OutputContract: json.RawMessage(`[{"kind":"dev.change_summary","required":true}]`),
			ModelLevel:     ptrTo("high"), RepoScope: []string{"backend"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return *out.Latest
}

func ptrTo(s string) *string { return &s }

// AC-2, AC-3: инструкция заводится с версией 1; отказы ничего не заводят;
// новая версия — N+1, «не изменилось» — отказ.
func TestInstructionCreateAndVersion(t *testing.T) {
	ctx := context.Background()
	store, _, a, _, system := setupPhase4(t, AuthHeader)

	out, err := a.CreateInstruction(ctx, "SDLC", system.ID, InstructionInput{Key: "git", Name: "Правила git", Content: gitV1})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Created || out.Version.Version != 1 || out.Version.SHA256 != shaHex(gitV1) ||
		out.Version.Kind != domain.DefaultRoleDocumentKind || out.Version.SizeBytes != int64(len(gitV1)) {
		t.Fatalf("заведение: %+v", out)
	}
	got, err := a.GetInstruction(ctx, "SDLC", "git", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version == nil || got.Version.Version != 1 || got.Version.Content == nil || *got.Version.Content != gitV1 ||
		got.Previous != nil || len(got.Versions) != 1 {
		t.Fatalf("GET: %+v", got)
	}

	before := countN(t, store, `SELECT count(*) FROM instructions`) + countN(t, store, `SELECT count(*) FROM instruction_versions`)
	for name, in := range map[string]InstructionInput{
		"ключ не по форме": {Key: "Git", Name: "x", Content: "x"},
		"ключ с точкой":    {Key: "git.md", Name: "x", Content: "x"},
		"пустой текст":     {Key: "fmt", Name: "x", Content: "  \n "},
		"NUL":              {Key: "fmt", Name: "x", Content: "a\x00b"},
		"без названия":     {Key: "fmt", Name: " ", Content: "x"},
	} {
		_, err := a.CreateInstruction(ctx, "SDLC", system.ID, in)
		wantCode(t, err, apperr.CodeInvalidInput, name)
	}
	_, err = a.CreateInstruction(ctx, "SDLC", system.ID, InstructionInput{Key: "git", Name: "x", Content: "другое"})
	wantCode(t, err, apperr.CodeConflict, "занятый ключ")
	if after := countN(t, store, `SELECT count(*) FROM instructions`) + countN(t, store, `SELECT count(*) FROM instruction_versions`); after != before {
		t.Fatalf("отказы завели строки: %d → %d", before, after)
	}

	v2, err := a.AddInstructionVersion(ctx, "SDLC", "git", system.ID, InstructionInput{Content: gitV2, Changelog: ptrTo("MERGEABLE")})
	if err != nil || v2.Version.Version != 2 {
		t.Fatalf("версия 2: %+v %v", v2, err)
	}
	_, err = a.AddInstructionVersion(ctx, "SDLC", "git", system.ID, InstructionInput{Content: gitV2})
	wantCode(t, err, apperr.CodeInvalidInput, "текст и тип те же")
	if !strings.Contains(err.Error(), "не изменились относительно версии 2") {
		t.Fatalf("текст отказа: %v", err)
	}
	// Тот же текст, другой тип — новая версия.
	if v3, err := a.AddInstructionVersion(ctx, "SDLC", "git", system.ID, InstructionInput{Content: gitV2, Kind: "reference"}); err != nil || v3.Version.Version != 3 {
		t.Fatalf("тип сменился: %+v %v", v3, err)
	}
	got, err = a.GetInstruction(ctx, "SDLC", "git", 2)
	if err != nil || got.Previous == nil || *got.Previous.Content != gitV1 || got.Instruction.LatestVersion != 3 {
		t.Fatalf("версия 2 с предыдущей: %+v %v", got, err)
	}
	if _, err := a.GetInstruction(ctx, "SDLC", "git", 9); apperr.As(err).Code != apperr.CodeNotFound {
		t.Fatalf("версия 9: %v", err)
	}
	if inst, err := a.UpdateInstruction(ctx, "SDLC", "git", ptrTo("Git и PR"), ptrTo("ветки")); err != nil ||
		inst.Name != "Git и PR" || inst.Key != "git" || inst.LatestVersion != 3 {
		t.Fatalf("PATCH: %+v %v", inst, err)
	}
}

// AC-4, AC-8: закрепление ссылкой, отказы без версии, перенос по sha,
// правка и «Отвязать» снимают закрепление.
func TestRoleDocumentPinning(t *testing.T) {
	ctx := context.Background()
	store, _, a, _, system := setupPhase4(t, AuthHeader)
	if _, err := a.CreateInstruction(ctx, "SDLC", system.ID, InstructionInput{Key: "git", Name: "git", Kind: "reference", Content: gitV1}); err != nil {
		t.Fatal(err)
	}
	rv := roleWithGit(t, a, system, "builder", RoleDocumentInput{Instruction: &InstructionRef{Key: "git", Version: 1}})
	git := rv.Documents[1]
	if git.Name != "git.md" || git.Kind != "reference" || git.SHA256 != shaHex(gitV1) ||
		git.Instruction == nil || git.Instruction.Key != "git" || git.Instruction.Version != 1 {
		t.Fatalf("закреплённый документ: %+v", git)
	}
	read, err := a.GetRoleVersion(ctx, "SDLC", "builder", 1)
	if err != nil || read.Documents[1].Instruction == nil || read.Documents[1].Instruction.Version != 1 {
		t.Fatalf("GET версии: %+v %v", read.Documents, err)
	}

	other, err := a.engine.CreateProject(ctx, system.ID, postgres.CreateProjectInput{Key: "OTHER", Name: "Другой", OwnerTeamID: libraryOwner(t, store)})
	if err != nil {
		t.Fatal(err)
	}
	_ = other
	if _, err := a.CreateInstruction(ctx, "OTHER", system.ID, InstructionInput{Key: "fmt", Name: "fmt", Content: "чужое"}); err != nil {
		t.Fatal(err)
	}
	before := countN(t, store, `SELECT count(*) FROM role_versions`)
	for name, doc := range map[string]RoleDocumentInput{
		"ссылка и содержимое": {Instruction: &InstructionRef{Key: "git", Version: 1}, Content: "x"},
		"имя не git.md":       {Name: "rules.md", Instruction: &InstructionRef{Key: "git", Version: 1}},
		"нет версии":          {Instruction: &InstructionRef{Key: "git", Version: 7}},
		"без номера":          {Instruction: &InstructionRef{Key: "git"}},
		"чужой проект":        {Instruction: &InstructionRef{Key: "fmt", Version: 1}},
		"тип не тот":          {Kind: "checklist", Instruction: &InstructionRef{Key: "git", Version: 1}},
	} {
		_, err := a.AddRoleVersion(ctx, "SDLC", "builder", system.ID, RoleVersionInput{
			Documents: []RoleDocumentInput{{Name: "main.md", Content: "x"}, doc}})
		wantCode(t, err, apperr.CodeInvalidInput, name)
	}
	_, err = a.AddRoleVersion(ctx, "SDLC", "builder", system.ID, RoleVersionInput{Documents: []RoleDocumentInput{
		{Instruction: &InstructionRef{Key: "git", Version: 1}}, {Instruction: &InstructionRef{Key: "git", Version: 1}}}})
	wantCode(t, err, apperr.CodeInvalidInput, "одна инструкция дважды")
	if after := countN(t, store, `SELECT count(*) FROM role_versions`); after != before {
		t.Fatalf("отказы завели версии роли: %d → %d", before, after)
	}

	// Перенос: клиент без библиотеки шлёт тот же текст — закрепление живёт.
	same := []RoleDocumentInput{{Name: "main.md", Content: "Правка main.\n"}, {Name: "git.md", Kind: "reference", Content: gitV1}}
	v2, err := a.AddRoleVersion(ctx, "SDLC", "builder", system.ID, RoleVersionInput{Documents: same})
	if err != nil {
		t.Fatal(err)
	}
	if p := v2.Documents[1].Instruction; p == nil || p.Key != "git" || p.Version != 1 {
		t.Fatalf("перенос закрепления при том же тексте: %+v", v2.Documents[1])
	}
	// Тот же текст, но «Отвязать» — обычный документ.
	unpinned := append([]RoleDocumentInput(nil), same...)
	unpinned[0].Content = "Ещё правка main.\n"
	unpinned[1].Unpin = true
	v3, err := a.AddRoleVersion(ctx, "SDLC", "builder", system.ID, RoleVersionInput{Documents: unpinned})
	if err != nil || v3.Documents[1].Instruction != nil {
		t.Fatalf("Отвязать: %+v %v", v3.Documents, err)
	}
	// Снова закрепить и изменить текст — закрепление не переносится.
	if _, err := a.AddRoleVersion(ctx, "SDLC", "builder", system.ID, RoleVersionInput{Documents: []RoleDocumentInput{
		{Name: "main.md", Content: "x"}, {Instruction: &InstructionRef{Key: "git", Version: 1}}}}); err != nil {
		t.Fatal(err)
	}
	edited, err := a.AddRoleVersion(ctx, "SDLC", "builder", system.ID, RoleVersionInput{Documents: []RoleDocumentInput{
		{Name: "main.md", Content: "x"}, {Name: "git.md", Kind: "reference", Content: gitV1 + "Своя строка.\n"}}})
	if err != nil || edited.Documents[1].Instruction != nil {
		t.Fatalf("правленый текст остался закреплённым: %+v %v", edited.Documents, err)
	}
}

func libraryOwner(t *testing.T, store *postgres.Store) uuid.UUID {
	t.Helper()
	p, err := store.GetProjectByKey(context.Background(), store.DB(), "SDLC")
	if err != nil || p.OwnerTeamID == nil {
		t.Fatalf("команда проекта SDLC: %v", err)
	}
	return *p.OwnerTeamID
}

// AC-5, AC-6, AC-7, AC-16: новая версия инструкции ролей не меняет, роль
// отстаёт; «обновить в ролях» заводит версию только отмеченным, повтор —
// unchanged, отказы атомарны.
func TestApplyInstruction(t *testing.T) {
	ctx := context.Background()
	store, engine, a, _, system := setupPhase4(t, AuthHeader)
	if _, err := a.CreateInstruction(ctx, "SDLC", system.ID, InstructionInput{Key: "git", Name: "git", Content: gitV1}); err != nil {
		t.Fatal(err)
	}
	dev := roleWithGit(t, a, system, "dev", RoleDocumentInput{Instruction: &InstructionRef{Key: "git", Version: 1}})
	integ := roleWithGit(t, a, system, "integ", RoleDocumentInput{Instruction: &InstructionRef{Key: "git", Version: 1}})
	plain := roleWithGit(t, a, system, "plain", RoleDocumentInput{Name: "git.md", Content: gitV2})
	if _, err := a.CreateRole(ctx, "SDLC", system.ID, CreateRoleInput{Key: "nogit", Name: "nogit",
		Version: &RoleVersionInput{Instructions: "без git"}}); err != nil {
		t.Fatal(err)
	}

	// Роль analyst захватывается этапом: закрепим ей git v1 и сравним
	// захват до и после версии 2.
	if _, err := a.AddRoleVersion(ctx, "SDLC", "analyst", system.ID, RoleVersionInput{Documents: []RoleDocumentInput{
		{Name: "main.md", Content: "Ты аналитик.\n"}, {Instruction: &InstructionRef{Key: "git", Version: 1}}}}); err != nil {
		t.Fatal(err)
	}
	claimedBefore, _ := claimAnalysis(t, engine, system, "До v2")

	versionsBefore := countN(t, store, `SELECT count(*) FROM role_versions`)
	if _, err := a.AddInstructionVersion(ctx, "SDLC", "git", system.ID, InstructionInput{Content: gitV2}); err != nil {
		t.Fatal(err)
	}
	if after := countN(t, store, `SELECT count(*) FROM role_versions`); after != versionsBefore {
		t.Fatalf("новая версия инструкции завела версии ролей: %d → %d", versionsBefore, after)
	}
	claimedAfter, _ := claimAnalysis(t, engine, system, "После v2")
	if claimedAfter.Role.Instructions != claimedBefore.Role.Instructions || len(claimedAfter.Role.Documents) != 2 ||
		claimedAfter.Role.Documents[1].SHA256 != shaHex(gitV1) || claimedAfter.Role.Documents[1].Instruction != nil {
		t.Fatalf("захват изменился: до %+v, после %+v", claimedBefore.Role.Documents, claimedAfter.Role.Documents)
	}
	raw, _ := json.Marshal(claimedAfter.Role)
	if strings.Contains(string(raw), `"instruction"`) {
		t.Fatalf("закрепление в ответе захвата: %s", raw)
	}

	roles, err := a.ListRoles(ctx, "SDLC", false)
	if err != nil {
		t.Fatal(err)
	}
	lag := map[string][]domain.InstructionLag{}
	for _, r := range roles {
		lag[r.Key] = r.InstructionLag
	}
	if l := lag["dev"]; len(l) != 1 || l[0] != (domain.InstructionLag{Key: "git", Pinned: 1, Latest: 2}) {
		t.Fatalf("отставание dev в списке: %+v", l)
	}
	if len(lag["plain"]) != 0 || len(lag["nogit"]) != 0 {
		t.Fatalf("отставание у незакреплённых: %+v", lag)
	}
	got, err := a.GetRole(ctx, "SDLC", "integ")
	if err != nil || len(got.Role.InstructionLag) != 1 {
		t.Fatalf("отставание в GET роли: %+v %v", got.Role, err)
	}
	detail, err := a.GetInstruction(ctx, "SDLC", "git", 0)
	if err != nil {
		t.Fatal(err)
	}
	usage := map[string]domain.InstructionUsage{}
	for _, u := range detail.Usage {
		usage[u.RoleKey] = u
	}
	if len(usage) != 4 || usage["dev"].Pinned == nil || *usage["dev"].Pinned != 1 || usage["dev"].RoleVersion != dev.Version ||
		usage["plain"].Pinned != nil || usage["plain"].SHA256 != shaHex(gitV2) {
		t.Fatalf("где используется: %+v", detail.Usage)
	}
	if detail.Instruction.PinnedRoles != 3 || detail.Instruction.LaggingRoles != 3 {
		t.Fatalf("сводка: %+v", detail.Instruction)
	}

	// Атомарность: роль без git.md, архивная и несуществующая — 400, ни
	// одной новой версии.
	if _, err := a.ArchiveRole(ctx, "SDLC", "integ", true); err != nil {
		t.Fatal(err)
	}
	for name, roles := range map[string][]ApplyRole{
		"без git.md": {{Key: "dev"}, {Key: "nogit"}},
		"архивная":   {{Key: "dev"}, {Key: "integ"}},
		"нет роли":   {{Key: "dev"}, {Key: "ghost"}},
	} {
		_, err := a.ApplyInstruction(ctx, "SDLC", "git", system.ID, ApplyInput{Roles: roles})
		wantCode(t, err, apperr.CodeInvalidInput, name)
	}
	_, err = a.ApplyInstruction(ctx, "SDLC", "git", system.ID, ApplyInput{Roles: []ApplyRole{{Key: "dev", FromVersion: ptrInt(dev.Version - 1)}}})
	wantCode(t, err, apperr.CodeConflict, "гонка")
	if after := countN(t, store, `SELECT count(*) FROM role_versions`); after != versionsBefore {
		t.Fatalf("отказы «обновить в ролях» завели версии: %d → %d", versionsBefore, after)
	}
	if _, err := a.ArchiveRole(ctx, "SDLC", "integ", false); err != nil {
		t.Fatal(err)
	}

	res, err := a.ApplyInstruction(ctx, "SDLC", "git", system.ID, ApplyInput{
		Roles: []ApplyRole{{Key: "dev", FromVersion: ptrInt(dev.Version)}, {Key: "plain"}}, Changelog: "общие правила"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != 2 || len(res.Roles) != 2 || res.Roles[0].Outcome != "created" || res.Roles[0].RoleVersion != dev.Version+1 ||
		res.Roles[0].From == nil || *res.Roles[0].From != 1 || res.Roles[1].From != nil || res.Roles[1].Outcome != "created" {
		t.Fatalf("итог: %+v", res)
	}
	newDev, err := a.GetRoleVersion(ctx, "SDLC", "dev", dev.Version+1)
	if err != nil {
		t.Fatal(err)
	}
	if p := newDev.Documents[1].Instruction; p == nil || p.Version != 2 || newDev.Documents[1].Position != 1 ||
		newDev.Documents[1].SHA256 != shaHex(gitV2) || newDev.Documents[0].SHA256 != dev.Documents[0].SHA256 {
		t.Fatalf("новая версия dev: %+v", newDev.Documents)
	}
	if !eqStrings(newDev.InputKinds, dev.InputKinds) || !eqJSON(newDev.OutputContract, dev.OutputContract, `[]`) ||
		deref(newDev.ModelLevel) != "high" || !eqStrings(newDev.RepoScope, dev.RepoScope) ||
		deref(newDev.Changelog) != "инструкция git: v1 → v2. общие правила" {
		t.Fatalf("поля версии не перенесены: %+v", newDev)
	}
	newPlain, err := a.GetRoleVersion(ctx, "SDLC", "plain", plain.Version+1)
	if err != nil || newPlain.Documents[1].Instruction == nil || newPlain.Documents[1].Instruction.Version != 2 ||
		deref(newPlain.Changelog) != "инструкция git: обычный документ → v2. общие правила" {
		t.Fatalf("обычный документ не закрепился: %+v %v", newPlain, err)
	}
	if r, _ := a.GetRole(ctx, "SDLC", "integ"); r.Role.LatestVersion == nil || *r.Role.LatestVersion != integ.Version {
		t.Fatalf("неотмеченная integ изменилась: %+v", r.Role)
	}
	again, err := a.ApplyInstruction(ctx, "SDLC", "git", system.ID, ApplyInput{Roles: []ApplyRole{{Key: "dev"}}})
	if err != nil || again.Roles[0].Outcome != "unchanged" || again.Roles[0].RoleVersion != dev.Version+1 {
		t.Fatalf("повтор: %+v %v", again, err)
	}
	if r, _ := a.GetRole(ctx, "SDLC", "dev"); len(r.Role.InstructionLag) != 0 {
		t.Fatalf("отставание dev не погасло: %+v", r.Role.InstructionLag)
	}
}

func ptrInt(n int) *int { return &n }

// AC-11, AC-19: сборщик мусора не уносит блоб версии инструкции без ролей;
// удаление проекта с библиотекой и закреплениями проходит штатно.
func TestInstructionBlobsAndProjectRemoval(t *testing.T) {
	ctx := context.Background()
	store, engine, a, _, system := setupPhase4(t, AuthHeader)
	lonely := "Инструкция, которую никто не закрепил.\n"
	if _, err := a.CreateInstruction(ctx, "SDLC", system.ID, InstructionInput{Key: "lonely", Name: "lonely", Content: lonely}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateInstruction(ctx, "SDLC", system.ID, InstructionInput{Key: "git", Name: "git", Content: gitV1}); err != nil {
		t.Fatal(err)
	}
	roleWithGit(t, a, system, "dev", RoleDocumentInput{Instruction: &InstructionRef{Key: "git", Version: 1}})
	if _, err := store.DB().Exec(ctx, `UPDATE blobs SET created_at = now() - interval '1 hour' WHERE sha256 = $1`, shaHex(lonely)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := store.CollectBlobs(ctx, postgres.BlobGCOptions{Limit: 10_000, MinAge: 0}, nil); err != nil {
		t.Fatal(err)
	}
	if countN(t, store, `SELECT count(*) FROM blobs WHERE sha256 = $1`, shaHex(lonely)) != 1 {
		t.Fatal("сборщик унёс блоб версии инструкции")
	}

	if _, _, err := engine.DeleteProject(ctx, "SDLC", system.ID); err != nil {
		t.Fatalf("удаление проекта с библиотекой: %v", err)
	}
	if n := countN(t, store, `SELECT count(*) FROM instructions`) + countN(t, store, `SELECT count(*) FROM instruction_versions`); n != 0 {
		t.Fatalf("после удаления проекта строк библиотеки: %d", n)
	}
}

// AC-14, AC-16: экспорт несёт закреплённые и последнюю версии, импорт в
// пустой проект закрепляет по sha, повтор — unchanged, расхождение текста —
// отказ без изменений; связка без библиотеки — как прежде.
func TestBundleInstructions(t *testing.T) {
	ctx := context.Background()
	store, _, a, _, system := setupPhase4(t, AuthHeader)
	if _, err := a.CreateInstruction(ctx, "SDLC", system.ID, InstructionInput{Key: "git", Name: "git", Content: gitV1}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddInstructionVersion(ctx, "SDLC", "git", system.ID, InstructionInput{Content: gitV2}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddInstructionVersion(ctx, "SDLC", "git", system.ID, InstructionInput{Content: gitV2 + "v3\n"}); err != nil {
		t.Fatal(err)
	}
	roleWithGit(t, a, system, "dev", RoleDocumentInput{Instruction: &InstructionRef{Key: "git", Version: 1}})

	b, err := a.Export(ctx, "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Instructions) != 1 || len(b.Instructions[0].Versions) != 2 ||
		b.Instructions[0].Versions[0].Version != 1 || b.Instructions[0].Versions[1].Version != 3 {
		t.Fatalf("экспорт библиотеки: %+v", b.Instructions)
	}
	var devRole *BundleRole
	for i := range b.Roles {
		if b.Roles[i].Key == "dev" {
			devRole = &b.Roles[i]
		}
	}
	if devRole == nil || devRole.Documents[1].Instruction != "git" || devRole.Documents[1].Content != gitV1 {
		t.Fatalf("документ роли в связке: %+v", devRole)
	}

	if _, err := a.engine.CreateProject(ctx, system.ID, postgres.CreateProjectInput{Key: "COPY", Name: "Копия", OwnerTeamID: libraryOwner(t, store)}); err != nil {
		t.Fatal(err)
	}
	small := Bundle{Version: BundleVersion, Instructions: b.Instructions, Roles: []BundleRole{*devRole}}
	dry, err := a.Import(ctx, "COPY", system.ID, small, true)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := a.Import(ctx, "COPY", system.ID, small, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Instructions[0].Outcome != "created" || rep.Roles[0].Outcome != "created" ||
		dry.Instructions[0].Outcome != "created" || dry.Roles[0].Outcome != "created" {
		t.Fatalf("импорт в пустой проект: %+v / dry %+v", rep, dry)
	}
	copied, err := a.GetRole(ctx, "COPY", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if p := copied.Latest.Documents[1].Instruction; p == nil || p.Key != "git" || p.Version != 1 {
		t.Fatalf("закрепление после импорта: %+v", copied.Latest.Documents)
	}
	again, err := a.Import(ctx, "COPY", system.ID, small, false)
	if err != nil || again.Instructions[0].Outcome != "unchanged" || again.Roles[0].Outcome != "unchanged" {
		t.Fatalf("повторный импорт: %+v %v", again, err)
	}
	dryAgain, err := a.Import(ctx, "COPY", system.ID, small, true)
	if err != nil || dryAgain.Instructions[0].Outcome != "unchanged" || dryAgain.Roles[0].Outcome != "unchanged" {
		t.Fatalf("dry-run повтора: %+v %v", dryAgain, err)
	}

	before := countN(t, store, `SELECT count(*) FROM role_versions`)
	broken := small
	broken.Roles = []BundleRole{*devRole}
	broken.Roles[0].Documents = append([]BundleRoleDocument(nil), devRole.Documents...)
	broken.Roles[0].Documents[1].Content = "правлено руками\n"
	broken.Roles[0].Documents[0].Content = "и main тоже\n"
	_, err = a.Import(ctx, "COPY", system.ID, broken, false)
	wantCode(t, err, apperr.CodeInvalidInput, "текст не совпадает")
	if !strings.Contains(err.Error(), "dev") || !strings.Contains(err.Error(), "git.md") {
		t.Fatalf("отказ без имени роли и документа: %v", err)
	}
	if after := countN(t, store, `SELECT count(*) FROM role_versions`); after != before {
		t.Fatalf("отказ импорта завёл версии: %d → %d", before, after)
	}

	// Связка без библиотеки: отчёт без instructions.
	plain := Bundle{Version: BundleVersion, Roles: []BundleRole{{Key: "solo", Name: "solo",
		Documents: []BundleRoleDocument{{Name: "main.md", Content: "один"}}}}}
	pr, err := a.Import(ctx, "COPY", system.ID, plain, false)
	if err != nil || pr.Instructions != nil {
		t.Fatalf("связка без библиотеки: %+v %v", pr, err)
	}
	raw, _ := json.Marshal(pr)
	if strings.Contains(string(raw), `"instructions"`) {
		t.Fatalf("ключ instructions в отчёте связки без библиотеки: %s", raw)
	}
}

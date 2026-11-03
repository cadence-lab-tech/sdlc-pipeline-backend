// Перенос правил проекта: экспорт и импорт связки.
//
// Связка (bundle) — это репозитории, роли с инструкциями, проектные документы
// и, при желании, цикл целиком. Она нужна двум сценариям: перенести уже
// существующие правила команды (CLAUDE.md, AGENTS.md, ADR, описания ролей)
// в проект руками агента и перенести настройку одного проекта в другой
// (решение 0022).
//
// Импорт идемпотентен и не ломает инвариантов редактирования: роль с той же
// инструкцией остаётся как есть, с другой — получает новую версию, а не
// правку старой; цикл приходит черновиком, активирует его человек.
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// BundleVersion — формат связки; растёт при несовместимых изменениях.
// Вторая версия переносит несколько циклов: у проекта их столько, сколько
// типов задач (решение 0024). Третья несёт роль набором документов
// (решение 0068, п. 5.3): экспорт пишет documents, импорт форматов 1 и 2
// со строкой instructions даёт один main.md, а сервер прежней версии
// формат 3 отбивает целиком, ничего не применив.
const BundleVersion = 3

type Bundle struct {
	Version      int                `json:"version"`
	Project      *BundleProject     `json:"project,omitempty"`
	Repositories []BundleRepository `json:"repositories,omitempty"`
	Roles        []BundleRole       `json:"roles,omitempty"`
	// Библиотека инструкций (решение 0069, п. 7): необязательное поле
	// формата 3. Сервер без библиотеки его теряет, и роли приходят обычными
	// документами — содержимое у документа роли остаётся в связке.
	Instructions []BundleInstruction `json:"instructions,omitempty"`
	Documents    []BundleDocument    `json:"documents,omitempty"`
	Workflows    []BundleWorkflow    `json:"workflows,omitempty"`
	// Связка первой версии несла ровно один цикл. Поле читается на импорте,
	// чтобы старые связки продолжали применяться; экспорт его не пишет.
	Workflow *BundleWorkflow `json:"workflow,omitempty"`
}

type BundleProject struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type BundleRepository struct {
	// owner/name или URL GitHub.
	Repository    string   `json:"repository"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	CloneURL      string   `json:"clone_url,omitempty"`
	Visibility    string   `json:"visibility,omitempty"`
	RoleHint      string   `json:"role_hint,omitempty"`
	WatchBranches []string `json:"watch_branches,omitempty"`
	ExcludeGlobs  []string `json:"exclude_globs,omitempty"`
	IndexEnabled  *bool    `json:"index_enabled,omitempty"`
}

type BundleRole struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Summary string `json:"summary,omitempty"`
	// Либо Instructions (форматы 1 и 2, один main.md), либо Documents
	// (формат 3); оба у одной роли — отказ всей связки.
	Instructions   string               `json:"instructions,omitempty"`
	Documents      []BundleRoleDocument `json:"documents,omitempty"`
	InputKinds     []string             `json:"input_kinds,omitempty"`
	OutputContract json.RawMessage      `json:"output_contract,omitempty"`
	ToolPolicy     json.RawMessage      `json:"tool_policy,omitempty"`
	// Устарел (решение 0060): строка модели для раннера прежних версий.
	ModelHint string `json:"model_hint,omitempty"`
	// Уровень модели роли по умолчанию: low | medium | high. Сервер прежней
	// версии поле не знает и теряет молча; связка без него — «уровня нет».
	ModelLevel    string          `json:"model_level,omitempty"`
	RepoScope     []string        `json:"repo_scope,omitempty"`
	ContextBudget json.RawMessage `json:"context_budget,omitempty"`
	Changelog     string          `json:"changelog,omitempty"`
}

// BundleRoleDocument — документ роли в связке, в порядке отдачи агенту.
// Пустой kind — instructions. Имя и содержимое необязательны в схеме
// только ради ссылки на инструкцию (тогда их берёт сервер); у обычного
// документа пустые — отказ проверки набора.
type BundleRoleDocument struct {
	Name    string `json:"name,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Content string `json:"content,omitempty"`
	// Instruction — ключ инструкции, за версией которой закреплён документ
	// (решение 0069): версия ищется по sha содержимого. Без содержимого —
	// ссылка номером InstructionVersion (путь sdlc_put_role).
	Instruction        string `json:"instruction,omitempty"`
	InstructionVersion int    `json:"instruction_version,omitempty"`
}

// BundleInstruction — инструкция библиотеки в связке: версии по
// возрастанию номера. Номера справочные: импорт дописывает новые тексты
// следующими номерами проекта, а узнаёт версии по sha.
type BundleInstruction struct {
	Key      string                     `json:"key"`
	Name     string                     `json:"name"`
	Summary  string                     `json:"summary,omitempty"`
	Versions []BundleInstructionVersion `json:"versions"`
}

type BundleInstructionVersion struct {
	Version   int    `json:"version"`
	Kind      string `json:"kind,omitempty"`
	Content   string `json:"content"`
	Changelog string `json:"changelog,omitempty"`
}

type BundleDocument struct {
	Kind  string `json:"kind"`
	Title string `json:"title"`
	// Ровно один источник: content, url либо repository+repo_path.
	Format     string          `json:"format,omitempty"`
	Content    string          `json:"content,omitempty"`
	URL        string          `json:"url,omitempty"`
	Repository string          `json:"repository,omitempty"`
	RepoPath   string          `json:"repo_path,omitempty"`
	Meta       json.RawMessage `json:"meta,omitempty"`
}

type BundleWorkflow struct {
	Name string `json:"name"`
	// Типы задач, которые ведёт цикл. Пусто — обычные задачи.
	TaskTypes   []string           `json:"task_types,omitempty"`
	Statuses    []BundleStatus     `json:"statuses"`
	Transitions []BundleTransition `json:"transitions"`
}

type BundleStatus struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Category    string `json:"category"`
	IsInitial   bool   `json:"is_initial,omitempty"`
	IsTerminal  bool   `json:"is_terminal,omitempty"`
	// Необязательный этап (решение 0066, п. 3). Нет поля — обязательный.
	IsOptional bool `json:"is_optional,omitempty"`
	// Нет поля — «включён»: связка прежней версии и необязательный этап,
	// включённый по умолчанию, выглядят одинаково. Указатель, потому что
	// нулевое значение bool выключило бы этап у связки без поля.
	EnabledByDefault *bool    `json:"enabled_by_default,omitempty"`
	WipLimit         *int     `json:"wip_limit,omitempty"`
	Color            string   `json:"color,omitempty"`
	Role             string   `json:"role,omitempty"`
	Reviewers        []string `json:"reviewers,omitempty"`
	// Отбор документов на статусе (решение 0068, пп. 3 и 5.6): у основной
	// роли — RoleDocuments, у проверяющих — ReviewerDocuments по ключу
	// роли. Нет поля — отбора нет; экспорт пишет поля, только когда отбор
	// есть, поэтому связка цикла без отбора остаётся байт в байт прежней.
	// Формат связки не растёт: сервер прежней версии поля теряет, и этап
	// получает все документы — безопасная сторона.
	RoleDocuments     []string            `json:"role_documents,omitempty"`
	ReviewerDocuments map[string][]string `json:"reviewer_documents,omitempty"`
}

// checkSelection — отбор документов статуса связки до первой записи: отбор у
// статуса без роли, ключ проверяющего не из reviewers и форма имён
// (checkDocumentNames) отбивают импорт целиком.
func (s BundleStatus) checkSelection() error {
	fail := func(format string, args ...any) error {
		return apperr.InvalidInput("статус %q: "+format, append([]any{s.Key}, args...)...).
			WithDetails(map[string]any{"status": s.Key})
	}
	if s.RoleDocuments != nil {
		if s.Role == "" {
			return fail("role_documents без роли (role)")
		}
		if err := checkDocumentNames(s.RoleDocuments); err != nil {
			return fail("role_documents: %s", apperr.As(err).Message)
		}
	}
	if len(s.ReviewerDocuments) == 0 {
		return nil
	}
	reviewers := make(map[string]bool, len(s.Reviewers))
	for _, r := range s.Reviewers {
		reviewers[r] = true
	}
	keys := make([]string, 0, len(s.ReviewerDocuments))
	for k := range s.ReviewerDocuments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !reviewers[k] {
			return fail("reviewer_documents: роли %q нет среди проверяющих (reviewers)", k)
		}
		names := s.ReviewerDocuments[k]
		if names == nil {
			// null в JSON — то же, что пустой список: отбора «никакого» нет.
			names = []string{}
		}
		if err := checkDocumentNames(names); err != nil {
			return fail("reviewer_documents[%q]: %s", k, apperr.As(err).Message)
		}
	}
	return nil
}

// enabledByDefault — умолчание этапа с учётом отсутствующего поля.
func (s BundleStatus) enabledByDefault() bool {
	return s.EnabledByDefault == nil || *s.EnabledByDefault
}

type BundleTransition struct {
	Key   string       `json:"key"`
	Name  string       `json:"name"`
	From  string       `json:"from,omitempty"`
	To    string       `json:"to"`
	Kind  string       `json:"kind"`
	Auto  bool         `json:"auto,omitempty"`
	Gates []BundleGate `json:"gates,omitempty"`
}

type BundleGate struct {
	Key      string          `json:"key"`
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Config   json.RawMessage `json:"config,omitempty"`
	Blocking *bool           `json:"blocking,omitempty"`
}

// ImportReport — что импорт сделал с каждой записью. Отдаётся и при
// dry_run: тогда это то, что он сделал бы.
type ImportReport struct {
	DryRun       bool          `json:"dry_run"`
	Repositories []ImportEntry `json:"repositories"`
	Roles        []ImportEntry `json:"roles"`
	// Инструкции библиотеки; ключа нет, если связка их не несла.
	Instructions []ImportEntry `json:"instructions,omitempty"`
	Documents    []ImportEntry `json:"documents"`
	Workflows    []ImportEntry `json:"workflows,omitempty"`
	// Проверка каждого пришедшего цикла как графа, в том же порядке.
	Validations []Validation `json:"workflow_validations,omitempty"`
}

type ImportEntry struct {
	Key string `json:"key"`
	// created | updated | unchanged | new_version | draft_created
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	ID      string `json:"id,omitempty"`
}

// errDryRun откатывает транзакцию импорта, не считаясь ошибкой.
var errDryRun = errors.New("dry run")

// Import применяет связку к проекту. Всё одной транзакцией: половина
// импортированной связки хуже, чем ни одной.
func (a *Authoring) Import(ctx context.Context, projectKey string, actor uuid.UUID, b Bundle, dryRun bool) (ImportReport, error) {
	report := ImportReport{
		DryRun:       dryRun,
		Repositories: []ImportEntry{},
		Roles:        []ImportEntry{},
		Documents:    []ImportEntry{},
	}
	if b.Version > BundleVersion {
		return report, apperr.InvalidInput("связка версии %d новее поддерживаемой %d", b.Version, BundleVersion)
	}
	// Форма ролей — до транзакции и до первой записи: связка с ролью, у
	// которой оба поля, отбивается целиком, в том числе в dry_run.
	for _, r := range b.Roles {
		if r.Instructions != "" && r.Documents != nil {
			return report, apperr.InvalidInput(
				"роль %q: у роли в связке либо instructions, либо documents, а не оба сразу", r.Key)
		}
	}
	// Отбор документов на статусах — тоже до транзакции: ни одного черновика
	// цикла из связки с неверным отбором.
	for _, w := range b.workflows() {
		for _, st := range w.Statuses {
			if err := st.checkSelection(); err != nil {
				e := apperr.As(err)
				return report, apperr.New(e.Code, "цикл %q: %s", w.Name, e.Message).WithDetails(e.Details)
			}
		}
	}

	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}

		for _, r := range b.Repositories {
			entry, err := a.importRepository(ctx, tx, project.ID, r)
			if err != nil {
				return fmt.Errorf("репозиторий %q: %w", r.Repository, err)
			}
			report.Repositories = append(report.Repositories, entry)
		}
		// Библиотека — до ролей: закрепление документа роли разрешается по
		// уже импортированным версиям.
		for _, in := range b.Instructions {
			entry, err := a.importInstruction(ctx, tx, project.ID, actor, in)
			if err != nil {
				return fmt.Errorf("инструкция %q: %w", in.Key, err)
			}
			report.Instructions = append(report.Instructions, entry)
		}
		for _, r := range b.Roles {
			entry, err := a.importRole(ctx, tx, project.ID, actor, r)
			if err != nil {
				return fmt.Errorf("роль %q: %w", r.Key, err)
			}
			report.Roles = append(report.Roles, entry)
		}
		for _, d := range b.Documents {
			entry, err := a.importDocument(ctx, tx, project.ID, actor, d)
			if err != nil {
				return fmt.Errorf("документ %q: %w", d.Title, err)
			}
			report.Documents = append(report.Documents, entry)
		}
		for _, w := range b.workflows() {
			entry, validation, err := a.importWorkflow(ctx, tx, project.ID, actor, w)
			if err != nil {
				return fmt.Errorf("цикл %q: %w", w.Name, err)
			}
			report.Workflows = append(report.Workflows, entry)
			report.Validations = append(report.Validations, validation)
		}

		if dryRun {
			return errDryRun
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		return report, nil
	}
	if err != nil {
		// Ошибка домена внутри обёртки должна доехать до клиента своим кодом.
		var e *apperr.Error
		if errors.As(err, &e) {
			return report, apperr.New(e.Code, "%s", err.Error()).WithDetails(e.Details)
		}
		return report, err
	}
	return report, nil
}

func (a *Authoring) importRepository(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, r BundleRepository) (ImportEntry, error) {
	owner, name, err := ParseRepository(r.Repository)
	if err != nil {
		return ImportEntry{}, err
	}
	repo, created, err := a.store.UpsertRepository(ctx, tx, postgres.RepositoryInput{
		ProjectID: projectID, Owner: owner, Name: name,
		DefaultBranch: r.DefaultBranch, CloneURL: r.CloneURL, Visibility: r.Visibility,
		RoleHint: optional(r.RoleHint), WatchBranches: r.WatchBranches,
		ExcludeGlobs: r.ExcludeGlobs, IndexEnabled: r.IndexEnabled,
	})
	if err != nil {
		return ImportEntry{}, err
	}
	outcome := "updated"
	if created {
		outcome = "created"
	}
	return ImportEntry{Key: owner + "/" + name, Outcome: outcome, ID: repo.ID.String()}, nil
}

// importRole заводит роль либо добавляет версию, если инструкция или
// контракт изменились. Версия роли иммутабельна (решение 0016): «обновить»
// здесь всегда означает «новая версия».
func (a *Authoring) importRole(ctx context.Context, tx pgx.Tx, projectID, actor uuid.UUID, r BundleRole) (ImportEntry, error) {
	if strings.TrimSpace(r.Key) == "" {
		return ImportEntry{}, apperr.InvalidInput("у роли нужен key")
	}
	if r.Documents == nil && strings.TrimSpace(r.Instructions) == "" {
		return ImportEntry{}, apperr.InvalidInput("у роли нужна инструкция")
	}
	if r.Name == "" {
		r.Name = r.Key
	}
	// Проверка до сравнения с последней версией: неверный уровень — отказ
	// всей связки, в том числе в dry_run, а не «unchanged» по совпадению.
	if err := checkModelLevel(r.ModelLevel, "роль "+r.Key+": model_level"); err != nil {
		return ImportEntry{}, err
	}
	version := RoleVersionInput{
		Instructions: r.Instructions, Documents: bundleDocumentsIn(r.Documents), InputKinds: r.InputKinds,
		OutputContract: r.OutputContract, ToolPolicy: r.ToolPolicy,
		ModelHint: optional(r.ModelHint), ModelLevel: optional(r.ModelLevel), RepoScope: r.RepoScope,
		ContextBudget: r.ContextBudget, Changelog: optional(r.Changelog),
	}

	role, err := a.store.GetRoleByKey(ctx, tx, projectID, r.Key)
	if apperr.As(err).Code == apperr.CodeNotFound {
		role, err = a.store.CreateRole(ctx, tx, postgres.CreateRoleInput{
			ProjectID: projectID, Key: r.Key, Name: r.Name,
			Summary: optional(r.Summary), CreatedBy: actor,
		})
		if err != nil {
			return ImportEntry{}, err
		}
		if _, err := a.addVersion(ctx, tx, projectID, role.ID, actor, version); err != nil {
			return ImportEntry{}, err
		}
		return ImportEntry{Key: r.Key, Outcome: "created", ID: role.ID.String()}, nil
	}
	if err != nil {
		return ImportEntry{}, err
	}

	if role.Name != r.Name || !eqOptional(role.Summary, r.Summary) {
		if err := a.store.UpdateRole(ctx, tx, role.ID, &r.Name, optional(r.Summary)); err != nil {
			return ImportEntry{}, err
		}
	}
	if role.ArchivedAt != nil {
		if err := a.store.ArchiveRole(ctx, tx, role.ID, false); err != nil {
			return ImportEntry{}, err
		}
	}

	// Набор проверяется до сравнения: неверный документ — отказ связки, а
	// не «unchanged» по совпадению sha.
	docs, err := a.prepareDocuments(ctx, tx, projectID, role.ID, version)
	if err != nil {
		return ImportEntry{}, err
	}
	latest, err := a.store.LatestRoleVersion(ctx, tx, role.ID)
	switch {
	case err == nil:
		have := []domain.RoleVersion{latest}
		if err := a.store.AttachRoleDocuments(ctx, tx, have, false); err != nil {
			return ImportEntry{}, err
		}
		if sameVersion(have[0], version, documentsOf(docs)) {
			return ImportEntry{Key: r.Key, Outcome: "unchanged", ID: role.ID.String(),
				Detail: fmt.Sprintf("версия %d совпадает", latest.Version)}, nil
		}
	case apperr.As(err).Code != apperr.CodeNotFound:
		return ImportEntry{}, err
	}
	if version.Changelog == nil {
		version.Changelog = optional("импорт связки")
	}
	rv, err := a.addVersion(ctx, tx, projectID, role.ID, actor, version)
	if err != nil {
		return ImportEntry{}, err
	}
	return ImportEntry{Key: r.Key, Outcome: "new_version", ID: role.ID.String(),
		Detail: fmt.Sprintf("версия %d", rv.Version)}, nil
}

// BundleDocuments — документы роли в форме связки и sdlc_put_role (ссылка
// на инструкцию — ключ и номер или ключ с содержимым) как вход версии или
// предложения версии роли.
func BundleDocuments(docs []BundleRoleDocument) []RoleDocumentInput { return bundleDocumentsIn(docs) }

// bundleDocumentsIn — документы роли связки во вход версии; nil остаётся
// nil, чтобы роль формата 1–2 шла путём instructions.
func bundleDocumentsIn(docs []BundleRoleDocument) []RoleDocumentInput {
	if docs == nil {
		return nil
	}
	out := make([]RoleDocumentInput, len(docs))
	for i, d := range docs {
		out[i] = RoleDocumentInput{Name: d.Name, Kind: d.Kind, Content: d.Content}
		if d.Instruction != "" {
			out[i].Instruction = &InstructionRef{Key: d.Instruction, Version: d.InstructionVersion}
			out[i].matchContent = d.Content != ""
		}
	}
	return out
}

// importInstruction заводит инструкцию или дописывает ей версии, которых
// нет по sha (решение 0069, п. 7). Та же связка второй раз — unchanged.
func (a *Authoring) importInstruction(ctx context.Context, tx pgx.Tx, projectID, actor uuid.UUID, b BundleInstruction) (ImportEntry, error) {
	if err := checkInstructionKey(b.Key); err != nil {
		return ImportEntry{}, err
	}
	if len(b.Versions) == 0 {
		return ImportEntry{}, apperr.InvalidInput("у инструкции в связке нет ни одной версии")
	}
	versions := append([]BundleInstructionVersion(nil), b.Versions...)
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].Version < versions[j].Version })
	name := strings.TrimSpace(b.Name)
	if name == "" {
		name = b.Key
	}

	inst, err := a.store.GetInstructionByKey(ctx, tx, projectID, b.Key)
	created := false
	switch {
	case apperr.As(err).Code == apperr.CodeNotFound:
		if inst, err = a.store.CreateInstruction(ctx, tx, postgres.CreateInstructionInput{
			ProjectID: projectID, Key: b.Key, Name: name, Summary: optional(b.Summary), CreatedBy: actor,
		}); err != nil {
			return ImportEntry{}, err
		}
		created = true
	case err != nil:
		return ImportEntry{}, err
	default:
		if inst.Name != name || !eqOptional(inst.Summary, b.Summary) {
			if err := a.store.UpdateInstruction(ctx, tx, inst.ID, &name, optional(b.Summary)); err != nil {
				return ImportEntry{}, err
			}
		}
	}

	var added []string
	for _, v := range versions {
		if _, err := a.store.FindInstructionVersionBySHA(ctx, tx, inst.ID, sha256Hex(v.Content)); err == nil {
			continue
		} else if apperr.As(err).Code != apperr.CodeNotFound {
			return ImportEntry{}, err
		}
		got, err := a.addInstructionVersion(ctx, tx, inst, actor, InstructionInput{
			Kind: v.Kind, Content: v.Content, Changelog: optional(v.Changelog),
		}, false)
		if err != nil {
			return ImportEntry{}, fmt.Errorf("версия %d: %w", v.Version, err)
		}
		inst.LatestVersion = got.Version
		added = append(added, fmt.Sprintf("v%d", got.Version))
	}
	entry := ImportEntry{Key: b.Key, ID: inst.ID.String()}
	switch {
	case created:
		entry.Outcome, entry.Detail = "created", "версии "+strings.Join(added, ", ")
	case len(added) > 0:
		entry.Outcome, entry.Detail = "new_version", "дописаны "+strings.Join(added, ", ")
	case inst.Name != name || !eqOptional(inst.Summary, b.Summary):
		entry.Outcome = "updated"
	default:
		entry.Outcome, entry.Detail = "unchanged", fmt.Sprintf("все версии уже есть (последняя v%d)", inst.LatestVersion)
	}
	return entry, nil
}

// sameWorkflow — совпадает ли версия цикла (в виде связки) с тем, что
// импортируется: статусы с ролями, переходы и гейты в том же порядке.
// Конфиг гейта сравнивается нормализованно, отсутствующий blocking — это true.
func sameWorkflow(have, want BundleWorkflow) bool {
	if have.Name != want.Name || !eqStringSet(have.TaskTypes, want.TaskTypes) {
		return false
	}
	if len(have.Statuses) != len(want.Statuses) || len(have.Transitions) != len(want.Transitions) {
		return false
	}
	for i := range have.Statuses {
		h, w := have.Statuses[i], want.Statuses[i]
		if h.Key != w.Key || h.Name != w.Name || h.Description != w.Description ||
			h.Category != w.Category || h.IsInitial != w.IsInitial || h.IsTerminal != w.IsTerminal ||
			h.IsOptional != w.IsOptional || h.enabledByDefault() != w.enabledByDefault() ||
			!eqIntPtr(h.WipLimit, w.WipLimit) || h.Color != w.Color ||
			h.Role != w.Role || !eqStrings(h.Reviewers, w.Reviewers) ||
			!eqSelection(h.RoleDocuments, w.RoleDocuments) ||
			!eqReviewerSelection(h.ReviewerDocuments, w.ReviewerDocuments) {
			return false
		}
	}
	for i := range have.Transitions {
		h, w := have.Transitions[i], want.Transitions[i]
		if h.Key != w.Key || h.Name != w.Name || h.From != w.From || h.To != w.To ||
			h.Kind != w.Kind || h.Auto != w.Auto || len(h.Gates) != len(w.Gates) {
			return false
		}
		for j := range h.Gates {
			hg, wg := h.Gates[j], w.Gates[j]
			if hg.Key != wg.Key || hg.Name != wg.Name || hg.Type != wg.Type ||
				!eqJSON(hg.Config, wg.Config, `{}`) || gateBlocking(hg) != gateBlocking(wg) {
				return false
			}
		}
	}
	return true
}

func gateBlocking(g BundleGate) bool { return g.Blocking == nil || *g.Blocking }

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// eqSelection — тот же отбор документов: оба без отбора либо оба с
// одинаковым множеством имён (порядок отбора ничего не значит).
func eqSelection(a, b []string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return eqStringSet(a, b)
}

// eqReviewerSelection — отборы проверяющих по ключу роли.
func eqReviewerSelection(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !eqSelection(av, bv) {
			return false
		}
	}
	return true
}

// eqStringSet — те же элементы без учёта порядка; пусто и nil равны.
func eqStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sa := append([]string(nil), a...)
	sb := append([]string(nil), b...)
	sort.Strings(sa)
	sort.Strings(sb)
	return eqStrings(sa, sb)
}

// sameVersion — совпадает ли последняя версия роли с тем, что импортируется:
// поля версии и набор документов по (name, kind, position, sha256)
// (решение 0068, п. 5.5). have — с документами (AttachRoleDocuments),
// wantDocs — набор входа. JSON сравнивается нормализованно: пробелы и
// порядок ключей не должны заводить новую версию.
func sameVersion(have domain.RoleVersion, want RoleVersionInput, wantDocs []domain.RoleDocument) bool {
	if !sameDocuments(have.Documents, wantDocs) {
		return false
	}
	if !eqStrings(have.InputKinds, want.InputKinds) || !eqStrings(have.RepoScope, want.RepoScope) {
		return false
	}
	if !eqOptional(have.ModelHint, deref(want.ModelHint)) ||
		!eqOptional(have.ModelLevel, deref(want.ModelLevel)) {
		return false
	}
	return eqJSON(have.OutputContract, want.OutputContract, `[]`) &&
		eqJSON(have.ToolPolicy, want.ToolPolicy, `{}`) &&
		eqJSON(have.ContextBudget, want.ContextBudget, `{}`)
}

func (a *Authoring) importDocument(ctx context.Context, tx pgx.Tx, projectID, actor uuid.UUID, d BundleDocument) (ImportEntry, error) {
	doc := postgres.DocumentInput{
		ProjectID: projectID, Kind: d.Kind, Title: d.Title, Meta: d.Meta, CreatedBy: actor,
	}
	switch {
	case d.Content != "":
		sha, err := a.store.PutBlob(ctx, tx, []byte(d.Content), mediaTypeFor(d.Format))
		if err != nil {
			return ImportEntry{}, err
		}
		doc.Source, doc.BlobSHA = "inline", &sha
	case d.URL != "":
		doc.Source, doc.URL = "url", optional(d.URL)
	case d.RepoPath != "":
		repo, err := a.store.FindRepository(ctx, tx, projectID, d.Repository)
		if err != nil {
			return ImportEntry{}, err
		}
		doc.Source, doc.RepositoryID, doc.RepoPath = "repo_path", &repo.ID, optional(d.RepoPath)
	default:
		return ImportEntry{}, apperr.InvalidInput("нужен источник: content, url или repository+repo_path")
	}
	out, outcome, err := a.store.UpsertDocument(ctx, tx, doc)
	if err != nil {
		return ImportEntry{}, err
	}
	return ImportEntry{Key: d.Kind + ": " + d.Title, Outcome: outcome, ID: out.ID.String()}, nil
}

// importWorkflow заводит черновик новой версии цикла. Активный цикл
// иммутабелен; активирует черновик человек, увидев результат проверки.
//
// Та же связка второй раз черновиков не плодит (CLAB-108): цикл, совпадающий
// с действующей версией или уже лежащим черновиком, отчитывается unchanged —
// как роли, у которых повтор инструкции не заводит версию.
func (a *Authoring) importWorkflow(ctx context.Context, tx pgx.Tx, projectID, actor uuid.UUID, w BundleWorkflow) (ImportEntry, Validation, error) {
	if len(w.Statuses) == 0 {
		return ImportEntry{}, Validation{}, apperr.InvalidInput("в цикле нет статусов")
	}
	if w.Name == "" {
		w.Name = "Импортированный цикл"
	}
	existing, err := a.store.ListWorkflows(ctx, tx, projectID)
	if err != nil {
		return ImportEntry{}, Validation{}, err
	}
	for _, head := range existing {
		if head.State != domain.WorkflowActive && head.State != domain.WorkflowDraft {
			continue
		}
		if head.Name != w.Name || !eqStringSet(head.TaskTypes, w.TaskTypes) {
			continue
		}
		full, err := a.store.GetWorkflow(ctx, tx, head.ID)
		if err != nil {
			return ImportEntry{}, Validation{}, err
		}
		if !sameWorkflow(*exportWorkflow(full), w) {
			continue
		}
		entry := ImportEntry{Key: w.Name, Outcome: "unchanged", ID: head.ID.String(),
			Detail: fmt.Sprintf("совпадает с версией %d (%s)", head.Version, head.State)}
		v, err := a.validate(ctx, tx, full)
		return entry, v, err
	}
	wf, err := a.store.CreateWorkflow(ctx, tx, projectID, w.Name, w.TaskTypes, actor)
	if err != nil {
		return ImportEntry{}, Validation{}, err
	}
	for i, st := range w.Statuses {
		pos := i + 1
		initial, terminal := st.IsInitial, st.IsTerminal
		isOptional, enabled := st.IsOptional, st.enabledByDefault()
		in := postgres.StatusInput{
			Key: st.Key, Name: st.Name, Description: optional(st.Description),
			Category: st.Category, Position: &pos, IsInitial: &initial, IsTerminal: &terminal,
			IsOptional: &isOptional, EnabledByDefault: &enabled,
			WipLimit: st.WipLimit, Color: optional(st.Color),
		}
		// Та же пара, что у REST авторинга: несогласованная откатывает импорт
		// целиком, а не заводит черновик со статусом, который не сохранить.
		if err := resolveOptional(false, true, &in); err != nil {
			return ImportEntry{}, Validation{}, apperr.InvalidInput(
				"статус %q: enabled_by_default: false бывает только у необязательного статуса (is_optional: true)",
				st.Key).WithDetails(map[string]any{"status": st.Key})
		}
		if _, err := a.store.CreateStatus(ctx, tx, wf.ID, in); err != nil {
			return ImportEntry{}, Validation{}, fmt.Errorf("статус %q: %w", st.Key, err)
		}
		roles := []postgres.StatusRoleInput{}
		if st.Role != "" {
			roles = append(roles, postgres.StatusRoleInput{
				RoleKey: st.Role, IsPrimary: true, DocumentNames: st.RoleDocuments})
		}
		for j, rk := range st.Reviewers {
			roles = append(roles, postgres.StatusRoleInput{
				RoleKey: rk, Position: j + 1, DocumentNames: st.ReviewerDocuments[rk]})
		}
		if len(roles) > 0 {
			status, err := a.store.GetStatusByKey(ctx, tx, wf.ID, st.Key)
			if err != nil {
				return ImportEntry{}, Validation{}, err
			}
			if err := a.store.SetStatusRoles(ctx, tx, projectID, status.ID, roles); err != nil {
				return ImportEntry{}, Validation{}, fmt.Errorf("роли статуса %q: %w", st.Key, err)
			}
		}
	}
	for i, t := range w.Transitions {
		pos := i + 1
		auto := t.Auto
		in := postgres.TransitionInput{
			Key: t.Key, Name: t.Name, ToStatus: t.To, Kind: t.Kind, Auto: &auto, Position: &pos,
		}
		if t.From != "" {
			in.FromStatus = optional(t.From)
		}
		tr, err := a.store.CreateTransition(ctx, tx, wf.ID, in)
		if err != nil {
			return ImportEntry{}, Validation{}, fmt.Errorf("переход %q: %w", t.Key, err)
		}
		for j, g := range t.Gates {
			gpos := j + 1
			if _, err := a.store.CreateGate(ctx, tx, tr.ID, postgres.GateInput{
				Key: g.Key, Name: g.Name, Type: g.Type, Config: g.Config,
				Blocking: g.Blocking, Position: &gpos,
			}); err != nil {
				return ImportEntry{}, Validation{}, fmt.Errorf("гейт %q на %q: %w", g.Key, t.Key, err)
			}
		}
	}

	full, err := a.store.GetWorkflow(ctx, tx, wf.ID)
	if err != nil {
		return ImportEntry{}, Validation{}, err
	}
	validation, err := a.validate(ctx, tx, full)
	if err != nil {
		return ImportEntry{}, Validation{}, err
	}
	entry := ImportEntry{Key: w.Name, Outcome: "draft_created", ID: wf.ID.String(),
		Detail: fmt.Sprintf("версия %d, черновик; активируйте после проверки", wf.Version)}
	return entry, validation, nil
}

// Export собирает текущее состояние проекта в связку: последние версии
// ролей, репозитории, документы с содержимым и активный цикл.
func (a *Authoring) Export(ctx context.Context, projectKey string) (Bundle, error) {
	q := a.store.DB()
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return Bundle{}, err
	}
	out := Bundle{Version: BundleVersion, Project: &BundleProject{
		Key: project.Key, Name: project.Name, Description: deref(project.Description),
	}}

	repos, err := a.store.ListRepositories(ctx, q, project.ID)
	if err != nil {
		return out, err
	}
	for _, r := range repos {
		settings, err := a.store.RepositorySettings(ctx, q, r.ID)
		if err != nil {
			return out, err
		}
		enabled := settings.IndexEnabled
		out.Repositories = append(out.Repositories, BundleRepository{
			Repository: r.Owner + "/" + r.Name, DefaultBranch: r.DefaultBranch,
			CloneURL: r.CloneURL, Visibility: r.Visibility, RoleHint: deref(r.RoleHint),
			WatchBranches: settings.WatchBranches, ExcludeGlobs: settings.ExcludeGlobs,
			IndexEnabled: &enabled,
		})
	}

	roles, err := a.store.ListRoles(ctx, q, project.ID, false)
	if err != nil {
		return out, err
	}
	// Версии инструкций, закреплённые экспортируемыми ролями: связка несёт
	// их и последнюю (решение 0069, п. 7).
	pinned := map[string][]int{}
	for _, r := range roles {
		if r.LatestVersion == nil {
			continue
		}
		v, err := a.latestVersion(ctx, q, r.ID)
		if err != nil {
			return out, err
		}
		// Формат 3 пишет только documents: склейку импорт соберёт сам.
		docs := make([]BundleRoleDocument, len(v.Documents))
		for i, d := range v.Documents {
			docs[i] = BundleRoleDocument{Name: d.Name, Kind: d.Kind, Content: content(d)}
			if d.Instruction != nil {
				docs[i].Instruction = d.Instruction.Key
				pinned[d.Instruction.Key] = append(pinned[d.Instruction.Key], d.Instruction.Version)
			}
		}
		out.Roles = append(out.Roles, BundleRole{
			Key: r.Key, Name: r.Name, Summary: deref(r.Summary),
			Documents: docs, InputKinds: v.InputKinds,
			OutputContract: v.OutputContract, ToolPolicy: v.ToolPolicy,
			ModelHint: deref(v.ModelHint), ModelLevel: deref(v.ModelLevel),
			RepoScope: v.RepoScope, ContextBudget: v.ContextBudget,
		})
	}

	if out.Instructions, err = a.exportInstructions(ctx, q, project.ID, pinned); err != nil {
		return out, err
	}

	docs, err := a.store.ListProjectDocuments(ctx, q, project.ID)
	if err != nil {
		return out, err
	}
	for _, d := range docs {
		bd := BundleDocument{Kind: d.Kind, Title: d.Title, Meta: d.Meta,
			URL: deref(d.URL), Repository: deref(d.Repository), RepoPath: deref(d.RepoPath)}
		if d.BlobSHA != nil {
			content, mediaType, err := a.store.BlobContent(ctx, q, *d.BlobSHA)
			if err != nil {
				return out, err
			}
			bd.Content = string(content)
			bd.Format = formatFor(mediaType)
		}
		out.Documents = append(out.Documents, bd)
	}

	// Циклов у проекта столько, сколько типов задач, и связка переносит их все:
	// без цикла эпиков перенесённый проект не умеет планировать.
	active, err := a.store.ListActiveWorkflows(ctx, q, project.ID)
	if err != nil {
		return out, err
	}
	for _, head := range active {
		wf, err := a.store.GetWorkflow(ctx, q, head.ID)
		if err != nil {
			return out, err
		}
		out.Workflows = append(out.Workflows, *exportWorkflow(wf))
	}
	return out, nil
}

// exportInstructions — инструкции проекта с закреплёнными и последней
// версиями по возрастанию номера. Проект без библиотеки — nil: связка
// остаётся байт в байт прежней.
func (a *Authoring) exportInstructions(ctx context.Context, q postgres.Querier, projectID uuid.UUID, pinned map[string][]int) ([]BundleInstruction, error) {
	list, err := a.store.ListInstructions(ctx, q, projectID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	out := make([]BundleInstruction, 0, len(list))
	for _, inst := range list {
		want := map[int]bool{inst.LatestVersion: true}
		for _, v := range pinned[inst.Key] {
			want[v] = true
		}
		numbers := make([]int, 0, len(want))
		for v := range want {
			numbers = append(numbers, v)
		}
		sort.Ints(numbers)
		bi := BundleInstruction{Key: inst.Key, Name: inst.Name, Summary: deref(inst.Summary)}
		for _, n := range numbers {
			v, err := a.store.GetInstructionVersion(ctx, q, inst.ID, n, true)
			if err != nil {
				return nil, err
			}
			bi.Versions = append(bi.Versions, BundleInstructionVersion{
				Version: v.Version, Kind: v.Kind, Content: deref(v.Content), Changelog: deref(v.Changelog),
			})
		}
		out = append(out, bi)
	}
	return out, nil
}

// workflows сводит оба поля связки в один список: первая версия несла цикл
// одиночным полем, вторая — списком.
func (b Bundle) workflows() []BundleWorkflow {
	out := append([]BundleWorkflow{}, b.Workflows...)
	if b.Workflow != nil {
		out = append(out, *b.Workflow)
	}
	return out
}

func exportWorkflow(wf domain.Workflow) *BundleWorkflow {
	statusKey := map[uuid.UUID]string{}
	bw := &BundleWorkflow{Name: wf.Name, TaskTypes: wf.TaskTypes,
		Statuses: []BundleStatus{}, Transitions: []BundleTransition{}}
	for _, st := range wf.Statuses {
		statusKey[st.ID] = st.Key
		bs := BundleStatus{
			Key: st.Key, Name: st.Name, Description: deref(st.Description),
			Category: st.Category, IsInitial: st.IsInitial, IsTerminal: st.IsTerminal,
			WipLimit: st.WipLimit, Color: deref(st.Color),
			IsOptional: st.IsOptional,
		}
		// Умолчание пишется, только когда этап выключен по умолчанию: связка
		// цикла без необязательных этапов остаётся байт в байт прежней.
		if st.IsOptional && !st.EnabledByDefault {
			off := false
			bs.EnabledByDefault = &off
		}
		for _, r := range st.Roles {
			if r.IsPrimary {
				bs.Role = r.RoleKey
				bs.RoleDocuments = r.DocumentNames
				continue
			}
			bs.Reviewers = append(bs.Reviewers, r.RoleKey)
			if r.DocumentNames != nil {
				if bs.ReviewerDocuments == nil {
					bs.ReviewerDocuments = map[string][]string{}
				}
				bs.ReviewerDocuments[r.RoleKey] = r.DocumentNames
			}
		}
		bw.Statuses = append(bw.Statuses, bs)
	}
	for _, t := range wf.Transitions {
		bt := BundleTransition{Key: t.Key, Name: t.Name, To: statusKey[t.ToStatusID], Kind: t.Kind, Auto: t.Auto}
		if t.FromStatusID != nil {
			bt.From = statusKey[*t.FromStatusID]
		}
		for _, g := range t.Gates {
			blocking := g.Blocking
			bt.Gates = append(bt.Gates, BundleGate{
				Key: g.Key, Name: g.Name, Type: g.Type, Config: g.Config, Blocking: &blocking,
			})
		}
		bw.Transitions = append(bw.Transitions, bt)
	}
	return bw
}

func formatFor(mediaType string) string {
	switch mediaType {
	case "application/json":
		return "json"
	case "application/yaml":
		return "yaml"
	case "text/html":
		return "html"
	case "text/plain":
		return "text"
	case "text/x-diff":
		return "diff"
	default:
		return "markdown"
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func eqOptional(have *string, want string) bool {
	return deref(have) == want
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// eqJSON сравнивает два JSON-значения нормализованно; пустое считается def.
func eqJSON(a, b json.RawMessage, def string) bool {
	if len(bytes.TrimSpace(a)) == 0 {
		a = json.RawMessage(def)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		b = json.RawMessage(def)
	}
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return bytes.Equal(a, b)
	}
	ca, _ := json.Marshal(va)
	cb, _ := json.Marshal(vb)
	return bytes.Equal(ca, cb)
}

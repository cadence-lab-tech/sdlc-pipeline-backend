package workflow

// Библиотека инструкций проекта (решение 0069): общее правило заводится
// один раз и версиями, а роли берут его закреплением документа за версией.
// Новая версия инструкции ролей не меняет: роль переходит на неё только
// новой версией роли — действием «обновить в ролях» или правкой роли. Так
// текст под идущим этапом не меняется тихо (решение 0016).

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// instructionKey — форма ключа: имя документа в ролях без «.md». Та же
// форма — CHECK в 000040.
var instructionKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,60}$`)

// InstructionInput — заведение инструкции или её новой версии.
type InstructionInput struct {
	Key     string
	Name    string
	Summary *string
	// Пустой Kind — тип последней версии, у первой — instructions.
	Kind      string
	Content   string
	Changelog *string
}

// InstructionDetail — инструкция с версиями без содержимого, запрошенной
// версией и предыдущей с содержимым (для диффа) и «где используется».
type InstructionDetail struct {
	Instruction domain.Instruction          `json:"instruction"`
	Versions    []domain.InstructionVersion `json:"versions"`
	Version     *domain.InstructionVersion  `json:"version"`
	Previous    *domain.InstructionVersion  `json:"previous,omitempty"`
	Usage       []domain.InstructionUsage   `json:"usage"`
}

// InstructionWrite — ответ заведения: инструкция и заведённая версия.
type InstructionWrite struct {
	Instruction domain.Instruction        `json:"instruction"`
	Version     domain.InstructionVersion `json:"version"`
	// Created — заведена сама инструкция (а не только версия).
	Created bool `json:"created"`
}

func (a *Authoring) ListInstructions(ctx context.Context, projectKey string) ([]domain.Instruction, error) {
	q := a.store.DB()
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	return a.store.ListInstructions(ctx, q, project.ID)
}

// GetInstruction — инструкция и версия version (0 — последняя).
func (a *Authoring) GetInstruction(ctx context.Context, projectKey, key string, version int) (InstructionDetail, error) {
	var out InstructionDetail
	q := a.store.DB()
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return out, err
	}
	inst, err := a.store.GetInstructionByKey(ctx, q, project.ID, key)
	if err != nil {
		return out, err
	}
	out.Instruction = inst
	if out.Versions, err = a.store.ListInstructionVersions(ctx, q, inst.ID); err != nil {
		return out, err
	}
	if version == 0 {
		version = inst.LatestVersion
	}
	v, err := a.store.GetInstructionVersion(ctx, q, inst.ID, version, true)
	if apperr.As(err).Code == apperr.CodeNotFound {
		return out, apperr.NotFound("версии %d инструкции %s нет", version, key)
	}
	if err != nil {
		return out, err
	}
	out.Version = &v
	if version > 1 {
		prev, err := a.store.GetInstructionVersion(ctx, q, inst.ID, version-1, true)
		if err != nil {
			return out, err
		}
		out.Previous = &prev
	}
	out.Usage, err = a.store.InstructionUsage(ctx, q, project.ID, inst.ID, key)
	return out, err
}

// GetInstructionVersion — версия инструкции с содержимым.
func (a *Authoring) GetInstructionVersion(ctx context.Context, projectKey, key string, version int) (domain.InstructionVersion, error) {
	q := a.store.DB()
	inst, err := a.instruction(ctx, q, projectKey, key)
	if err != nil {
		return domain.InstructionVersion{}, err
	}
	v, err := a.store.GetInstructionVersion(ctx, q, inst.ID, version, true)
	if apperr.As(err).Code == apperr.CodeNotFound {
		return v, apperr.NotFound("версии %d инструкции %s нет", version, key)
	}
	return v, err
}

// CreateInstruction заводит инструкцию с версией 1. Занятый ключ — 409.
func (a *Authoring) CreateInstruction(ctx context.Context, projectKey string, actor uuid.UUID, in InstructionInput) (InstructionWrite, error) {
	var out InstructionWrite
	if err := checkInstructionKey(in.Key); err != nil {
		return out, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return out, apperr.InvalidInput("у инструкции нужно название (name)")
	}
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		out, err = a.createInstruction(ctx, tx, project.ID, actor, in)
		return err
	})
	return out, err
}

func (a *Authoring) createInstruction(ctx context.Context, tx pgx.Tx, projectID, actor uuid.UUID, in InstructionInput) (InstructionWrite, error) {
	var out InstructionWrite
	inst, err := a.store.CreateInstruction(ctx, tx, postgres.CreateInstructionInput{
		ProjectID: projectID, Key: in.Key, Name: strings.TrimSpace(in.Name),
		Summary: in.Summary, CreatedBy: actor,
	})
	if err != nil {
		return out, err
	}
	v, err := a.addInstructionVersion(ctx, tx, inst, actor, in, true)
	if err != nil {
		return out, err
	}
	inst.LatestVersion = v.Version
	return InstructionWrite{Instruction: inst, Version: v, Created: true}, nil
}

// AddInstructionVersion заводит следующую версию существующей инструкции.
func (a *Authoring) AddInstructionVersion(ctx context.Context, projectKey, key string, actor uuid.UUID, in InstructionInput) (InstructionWrite, error) {
	var out InstructionWrite
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		inst, err := a.instruction(ctx, tx, projectKey, key)
		if err != nil {
			return err
		}
		v, err := a.addInstructionVersion(ctx, tx, inst, actor, in, true)
		if err != nil {
			return err
		}
		inst.LatestVersion = v.Version
		out = InstructionWrite{Instruction: inst, Version: v}
		return nil
	})
	return out, err
}

// PutInstruction — заведение или новая версия по ключу: путь MCP и
// clab instruction put. Отказ «ключ занят» есть только у REST-заведения.
func (a *Authoring) PutInstruction(ctx context.Context, projectKey string, actor uuid.UUID, in InstructionInput) (InstructionWrite, error) {
	var out InstructionWrite
	if err := checkInstructionKey(in.Key); err != nil {
		return out, err
	}
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		inst, err := a.store.GetInstructionByKey(ctx, tx, project.ID, in.Key)
		if apperr.As(err).Code == apperr.CodeNotFound {
			if strings.TrimSpace(in.Name) == "" {
				in.Name = in.Key
			}
			out, err = a.createInstruction(ctx, tx, project.ID, actor, in)
			return err
		}
		if err != nil {
			return err
		}
		if name := strings.TrimSpace(in.Name); (name != "" && name != inst.Name) ||
			(in.Summary != nil && deref(in.Summary) != deref(inst.Summary)) {
			var n *string
			if name != "" {
				n = &name
			}
			if err := a.store.UpdateInstruction(ctx, tx, inst.ID, n, in.Summary); err != nil {
				return err
			}
			if inst, err = a.store.GetInstructionByKey(ctx, tx, project.ID, in.Key); err != nil {
				return err
			}
		}
		v, err := a.addInstructionVersion(ctx, tx, inst, actor, in, true)
		if err != nil {
			return err
		}
		inst.LatestVersion = v.Version
		out = InstructionWrite{Instruction: inst, Version: v}
		return nil
	})
	return out, err
}

// addInstructionVersion — единственная точка заведения версии инструкции:
// REST, MCP, CLI и импорт связки приходят сюда (место CLAB-307 — версия
// инструкции участником agent). Проверки содержимого — те же, что у
// документа роли; strict — отказ, если текст и тип равны последней версии
// (импорт сверяет по sha сам и этой проверки не просит).
func (a *Authoring) addInstructionVersion(ctx context.Context, tx pgx.Tx, inst domain.Instruction, actor uuid.UUID, in InstructionInput, strict bool) (domain.InstructionVersion, error) {
	if err := a.store.LockInstruction(ctx, tx, inst.ID); err != nil {
		return domain.InstructionVersion{}, err
	}
	var latest *domain.InstructionVersion
	if inst.LatestVersion > 0 {
		v, err := a.store.GetInstructionVersion(ctx, tx, inst.ID, inst.LatestVersion, false)
		if err != nil {
			return v, err
		}
		latest = &v
	}
	kind := in.Kind
	if kind == "" {
		kind = domain.DefaultRoleDocumentKind
		if latest != nil {
			kind = latest.Kind
		}
	}
	if err := checkInstructionContent(kind, in.Content, a.store.BlobLimit()); err != nil {
		return domain.InstructionVersion{}, err
	}
	if strict && latest != nil && latest.Kind == kind && latest.SHA256 == sha256Hex(in.Content) {
		return domain.InstructionVersion{}, apperr.InvalidInput(
			"содержимое и тип не изменились относительно версии %d: версия не заведена", latest.Version).
			WithDetails(map[string]any{"unchanged": true, "version": latest.Version})
	}
	v, err := a.store.CreateInstructionVersion(ctx, tx, postgres.CreateInstructionVersionInput{
		InstructionID: inst.ID, Kind: kind, Content: []byte(in.Content),
		Changelog: in.Changelog, CreatedBy: actor,
	})
	if err != nil {
		return v, err
	}
	return v, nil
}

// UpdateInstruction меняет название и описание; ключ и версии не правятся.
func (a *Authoring) UpdateInstruction(ctx context.Context, projectKey, key string, name, summary *string) (domain.Instruction, error) {
	q := a.store.DB()
	inst, err := a.instruction(ctx, q, projectKey, key)
	if err != nil {
		return inst, err
	}
	if name != nil && strings.TrimSpace(*name) == "" {
		return inst, apperr.InvalidInput("название инструкции не может быть пустым")
	}
	if err := a.store.UpdateInstruction(ctx, q, inst.ID, name, summary); err != nil {
		return inst, err
	}
	return a.store.GetInstructionByKey(ctx, q, inst.ProjectID, key)
}

func (a *Authoring) instruction(ctx context.Context, q postgres.Querier, projectKey, key string) (domain.Instruction, error) {
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return domain.Instruction{}, err
	}
	return a.store.GetInstructionByKey(ctx, q, project.ID, key)
}

func checkInstructionKey(key string) error {
	if !instructionKey.MatchString(key) {
		return apperr.InvalidInput(
			"ключ инструкции %q: строчные латинские буквы, цифры, «_» и «-», начинается с буквы или цифры, "+
				"не длиннее 61 символа", key).WithDetails(map[string]any{"field": "key"})
	}
	return nil
}

// checkInstructionContent — те же отказы, что у документа роли
// (checkRoleDocuments): закреплённый документ — те же байты, и правила
// у них общие.
func checkInstructionContent(kind, content string, limit int) error {
	if !roleDocumentKind.MatchString(kind) {
		return apperr.InvalidInput("тип %q — строчные латинские буквы, цифры, «_», «-», начинается с буквы, "+
			"не длиннее 32 символов", kind).WithDetails(map[string]any{"field": "kind"})
	}
	field := map[string]any{"field": "content"}
	if strings.TrimSpace(content) == "" {
		return apperr.InvalidInput("пустое содержимое инструкции").WithDetails(field)
	}
	if strings.IndexByte(content, 0) >= 0 {
		return apperr.InvalidInput("содержимое инструкции с нулевым байтом").WithDetails(field)
	}
	if !utf8.ValidString(content) {
		return apperr.InvalidInput("содержимое инструкции — не UTF-8").WithDetails(field)
	}
	if limit > 0 && len(content) > limit {
		return apperr.InvalidInput("содержимое %d байт больше предела %d (SDLC_BLOB_MAX_BYTES)",
			len(content), limit).WithDetails(field)
	}
	return nil
}

// ── обновить в ролях ────────────────────────────────────────────────────────

// ApplyInput — «обновить в ролях» (решение 0069, п. 5).
type ApplyInput struct {
	// Версия инструкции; 0 — последняя.
	Version int
	Roles   []ApplyRole
	// Текст человека: допишется к changelog каждой новой версии роли.
	Changelog string
}

// ApplyRole — роль и номер её версии, от которой считал клиент (защита от
// гонки); nil — без проверки.
type ApplyRole struct {
	Key         string `json:"key"`
	FromVersion *int   `json:"from_version,omitempty"`
}

// ApplyResult — итог по ролям.
type ApplyResult struct {
	Instruction string            `json:"instruction"`
	Version     int               `json:"version"`
	Roles       []ApplyRoleResult `json:"roles"`
}

// ApplyRoleResult — исход роли: created — заведена версия RoleVersion,
// unchanged — роль уже на этой версии инструкции. From — закреплённая
// прежде версия, nil — документ был обычным.
type ApplyRoleResult struct {
	Role        string `json:"role"`
	Outcome     string `json:"outcome"`
	RoleVersion int    `json:"role_version"`
	From        *int   `json:"from_version"`
	To          int    `json:"to_version"`
}

// ApplyInstruction заводит отмеченным ролям новые версии с документом
// <ключ>.md, закреплённым за выбранной версией инструкции. Одна транзакция:
// отказ по любой роли — ни одной новой версии. Версии идут через addVersion,
// поэтому прочие документы сохраняют свои закрепления переносом.
func (a *Authoring) ApplyInstruction(ctx context.Context, projectKey, key string, actor uuid.UUID, in ApplyInput) (ApplyResult, error) {
	out := ApplyResult{Instruction: key, Roles: []ApplyRoleResult{}}
	if len(in.Roles) == 0 {
		return out, apperr.InvalidInput("не указана роль: отметьте хотя бы одну")
	}
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		out.Roles = out.Roles[:0]
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		inst, err := a.store.GetInstructionByKey(ctx, tx, project.ID, key)
		if err != nil {
			return err
		}
		version := in.Version
		if version == 0 {
			version = inst.LatestVersion
		}
		target, err := a.store.GetInstructionVersion(ctx, tx, inst.ID, version, false)
		if apperr.As(err).Code == apperr.CodeNotFound {
			return apperr.InvalidInput("версии %d инструкции %s нет", version, key)
		}
		if err != nil {
			return err
		}
		out.Version = target.Version
		docName := InstructionDocumentName(key)
		seen := map[string]bool{}
		for _, r := range in.Roles {
			if seen[r.Key] {
				continue
			}
			seen[r.Key] = true
			res, err := a.applyToRole(ctx, tx, project.ID, actor, inst, target, docName, r, in.Changelog)
			if err != nil {
				return err
			}
			out.Roles = append(out.Roles, res)
		}
		return nil
	})
	return out, err
}

func (a *Authoring) applyToRole(ctx context.Context, tx pgx.Tx, projectID, actor uuid.UUID, inst domain.Instruction,
	target domain.InstructionVersion, docName string, r ApplyRole, note string) (ApplyRoleResult, error) {
	res := ApplyRoleResult{Role: r.Key, To: target.Version}
	role, err := a.store.GetRoleByKey(ctx, tx, projectID, r.Key)
	if apperr.As(err).Code == apperr.CodeNotFound {
		return res, apperr.InvalidInput("роль %s не найдена в проекте; ни одной версии не заведено", r.Key).
			WithDetails(map[string]any{"role": r.Key})
	}
	if err != nil {
		return res, err
	}
	if role.ArchivedAt != nil {
		return res, apperr.InvalidInput("роль %s в архиве; ни одной версии не заведено", r.Key).
			WithDetails(map[string]any{"role": r.Key})
	}
	if err := a.store.LockRole(ctx, tx, role.ID); err != nil {
		return res, err
	}
	latest, err := a.latestVersion(ctx, tx, role.ID)
	if apperr.As(err).Code == apperr.CodeNotFound {
		return res, apperr.InvalidInput("роль %s: у роли нет версий; ни одной версии не заведено", r.Key).
			WithDetails(map[string]any{"role": r.Key})
	}
	if err != nil {
		return res, err
	}
	if r.FromVersion != nil && *r.FromVersion != latest.Version {
		return res, apperr.Conflict("роль %s уже на версии %d (запрос считан от версии %d): ничего не заведено, повторите",
			r.Key, latest.Version, *r.FromVersion).
			WithDetails(map[string]any{"role": r.Key, "latest_version": latest.Version, "from_version": *r.FromVersion})
	}
	idx := -1
	for i, d := range latest.Documents {
		if d.Name == docName {
			idx = i
		}
	}
	if idx < 0 {
		return res, apperr.InvalidInput("роль %s: у последней версии нет документа %s; ни одной версии не заведено",
			r.Key, docName).WithDetails(map[string]any{"role": r.Key})
	}
	doc := latest.Documents[idx]
	if doc.Instruction != nil && doc.Instruction.Key == inst.Key {
		v := doc.Instruction.Version
		res.From = &v
	}
	if res.From != nil && *res.From == target.Version {
		res.Outcome, res.RoleVersion = "unchanged", latest.Version
		return res, nil
	}

	docs := make([]RoleDocumentInput, len(latest.Documents))
	for i, d := range latest.Documents {
		docs[i] = RoleDocumentInput{Name: d.Name, Kind: d.Kind, Content: content(d)}
	}
	docs[idx] = RoleDocumentInput{Instruction: &InstructionRef{Key: inst.Key, Version: target.Version}}
	from := "обычный документ"
	if res.From != nil {
		from = fmt.Sprintf("v%d", *res.From)
	}
	changelog := fmt.Sprintf("инструкция %s: %s → v%d", inst.Key, from, target.Version)
	if n := strings.TrimSpace(note); n != "" {
		changelog += ". " + n
	}
	rv, err := a.addVersion(ctx, tx, projectID, role.ID, actor, RoleVersionInput{
		Documents: docs, InputKinds: latest.InputKinds, OutputContract: latest.OutputContract,
		ToolPolicy: latest.ToolPolicy, ModelHint: latest.ModelHint, ModelLevel: latest.ModelLevel,
		RepoScope: latest.RepoScope, ContextBudget: latest.ContextBudget, Changelog: &changelog,
	})
	if err != nil {
		if e := apperr.As(err); e.Code != apperr.CodeInternal {
			return res, apperr.New(e.Code, "роль %s: %s", r.Key, e.Message).WithDetails(e.Details)
		}
		return res, err
	}
	res.Outcome, res.RoleVersion = "created", rv.Version
	return res, nil
}

// attachLags проставляет ролям отставание по библиотеке.
func (a *Authoring) attachLags(ctx context.Context, q postgres.Querier, projectID uuid.UUID, roles []domain.Role) error {
	if len(roles) == 0 {
		return nil
	}
	lags, err := a.store.RoleInstructionLags(ctx, q, projectID)
	if err != nil {
		return err
	}
	for i := range roles {
		roles[i].InstructionLag = lags[roles[i].ID]
	}
	return nil
}

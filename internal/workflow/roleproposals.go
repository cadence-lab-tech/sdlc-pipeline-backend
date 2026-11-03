package workflow

// Предложения версий ролей (CLAB-381, решение 0072 п. 5). Участник уровня
// agent предлагает новую версию роли из своего набора, сопровождающий
// (maintainer, admin; человек) утверждает или отклоняет. Предложение —
// отдельная запись: role_versions не меняется ни подачей, ни отклонением,
// и последняя версия по-прежнему действующая. Утверждение заводит версию
// через addVersion — ту же точку, что у всех путей заведения версии.
//
// Право считают методы Auth в этом файле, их зовут и REST, и MCP; маршруты
// и тулы ставят только уровень-минимум. Токену задачи ни одно действие
// предложений не открыто (executorActions, решение 0072 п. 3).

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// ProposeHint — подсказка в отказе участнику уровня agent на прямую версию
// роли (POST …/versions, clab role put, sdlc_put_role): голый 403 не
// говорит, что делать дальше.
const ProposeHint = "версию роли участник уровня agent предлагает, а утверждает сопровождающий: " +
	"clab role propose <роль> (MCP — sdlc_propose_role_version)"

// ── право ───────────────────────────────────────────────────────────────────

// MayProposeRole — может ли актор предложить версию роли roleKey: уровень
// agent и выше, и роль входит в его набор (AllowedRoles: пути участия с
// уровнем agent и выше, суженные role_keys токена; nil — любая роль, [] —
// ни одной). Токен задачи — отказ при любом уровне держателя аренды.
func (a *Auth) MayProposeRole(ctx context.Context, id Identity, projectID uuid.UUID, roleKey string) error {
	if b := a.Bound(id); b != nil {
		return executorRefused(b, "предложение версии роли")
	}
	if err := a.Require(ctx, id, projectID, domain.MemberAgent); err != nil {
		return err
	}
	allowed, err := a.AllowedRoles(ctx, id, projectID)
	if err != nil {
		return err
	}
	if allowed == nil {
		return nil
	}
	for _, k := range allowed {
		if k == roleKey {
			return nil
		}
	}
	return apperr.Forbidden("предложить версию можно только своей роли: роль %q, у вас роли [%s]",
		roleKey, strings.Join(allowed, ", ")).
		WithDetails(map[string]any{"role": roleKey, "allowed_roles": allowed})
}

// MayDecideRoleProposal — утвердить или отклонить: человек (принципал вида
// user) уровня maintainer и выше. Агент, в том числе автор, — отказ; токен
// задачи — отказ при любом уровне держателя аренды. Вид принципала
// сверяется в любом режиме, как у апрува человека: это защита от
// случайного самоапрува агентом, а не от злого умысла.
func (a *Auth) MayDecideRoleProposal(ctx context.Context, id Identity, projectID uuid.UUID) error {
	if b := a.Bound(id); b != nil {
		return executorRefused(b, "решение по предложению версии роли")
	}
	if err := a.Require(ctx, id, projectID, domain.MemberMaintainer); err != nil {
		return apperr.Forbidden("%s: предложение версии роли утверждает и отклоняет сопровождающий "+
			"(уровень maintainer или admin)", apperr.As(err).Message)
	}
	if id.Principal.Kind != domain.PrincipalUser {
		return apperr.Forbidden("предложение версии роли утверждает человек, а не %s %q",
			id.Principal.Kind, id.Principal.Handle)
	}
	return nil
}

// MayWithdrawRoleProposal — отозвать: автор предложения или maintainer и
// выше.
func (a *Auth) MayWithdrawRoleProposal(ctx context.Context, id Identity, projectID uuid.UUID, p domain.RoleProposal) error {
	if b := a.Bound(id); b != nil {
		return executorRefused(b, "отзыв предложения версии роли")
	}
	if !a.Enforced() || p.CreatedBy == id.Principal.ID {
		return nil
	}
	if err := a.Require(ctx, id, projectID, domain.MemberMaintainer); err != nil {
		return apperr.Forbidden("%s: отозвать предложение может его автор или сопровождающий",
			apperr.As(err).Message)
	}
	return nil
}

// RoleVersionRefusal добавляет к отказу 403 на прямую версию роли
// подсказку про предложение — если актор участник уровня agent, то есть
// предложить мог бы. Прочие ошибки и прочие уровни — как есть.
func (a *Auth) RoleVersionRefusal(ctx context.Context, id Identity, projectID uuid.UUID, err error) error {
	e := apperr.As(err)
	if e.Code != apperr.CodeForbidden || a.Bound(id) != nil {
		return err
	}
	level, lerr := a.Level(ctx, id, projectID)
	if lerr != nil || level != domain.MemberAgent {
		return err
	}
	return apperr.Forbidden("%s; %s", e.Message, ProposeHint).WithDetails(e.Details)
}

// ── предложения ─────────────────────────────────────────────────────────────

// ProposeRoleVersion подаёт предложение новой версии роли. Проверки те же,
// что у версии (набор документов, ссылки на библиотеку, контракт, уровень
// модели), и выполняются здесь: негодное предложение в очередь не попадает,
// и текст отказа тот же, что дала бы версия. Ссылки на инструкции
// разрешаются сейчас и хранятся закреплёнными с байтами: новая версия
// инструкции после подачи предложение не меняет.
//
// База — последняя версия под замком роли: подача, пересёкшаяся с новой
// версией, получает новую базу, а не старую.
func (a *Authoring) ProposeRoleVersion(ctx context.Context, auth *Auth, id Identity, projectKey, roleKey string, in RoleVersionInput) (domain.RoleProposal, error) {
	var out domain.RoleProposal
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		role, err := a.role(ctx, tx, projectKey, roleKey)
		if err != nil {
			return err
		}
		if err := auth.MayProposeRole(ctx, id, role.ProjectID, role.Key); err != nil {
			return err
		}
		if role.ArchivedAt != nil {
			return apperr.InvalidInput("роль %q в архиве: версию ей не предлагают", roleKey)
		}
		if in.Changelog == nil || strings.TrimSpace(*in.Changelog) == "" {
			return apperr.InvalidInput("у предложения версии роли обязателен changelog: что и зачем меняется")
		}
		if err := a.store.LockRole(ctx, tx, role.ID); err != nil {
			return err
		}
		latest, err := a.store.LatestRoleVersion(ctx, tx, role.ID)
		if apperr.As(err).Code == apperr.CodeNotFound {
			return apperr.InvalidInput("у роли %q нет ни одной версии: первую заводит сопровождающий", roleKey)
		}
		if err != nil {
			return err
		}
		docs, level, err := a.checkVersion(ctx, tx, role.ProjectID, role.ID, proposalDocuments(in))
		if err != nil {
			return err
		}
		out, err = a.store.CreateRoleProposal(ctx, tx, postgres.CreateRoleProposalInput{
			RoleID:         role.ID,
			BaseVersion:    latest.Version,
			Documents:      storeDocuments(docs),
			InputKinds:     in.InputKinds,
			OutputContract: in.OutputContract,
			ToolPolicy:     in.ToolPolicy,
			ModelHint:      in.ModelHint,
			ModelLevel:     level,
			RepoScope:      in.RepoScope,
			ContextBudget:  in.ContextBudget,
			Changelog:      strings.TrimSpace(*in.Changelog),
			CreatedBy:      id.Principal.ID,
		})
		return err
	})
	return out, err
}

// proposalDocuments — у предложения ссылка на инструкцию бывает и с
// содержимым без номера версии: так её шлёт clab role propose --dir, где
// закреплённый документ лежит файлом <ключ>.md. Версия тогда ищется по sha
// содержимого, как в связке; у прямой версии роли эта форма — по-прежнему
// отказ.
func proposalDocuments(in RoleVersionInput) RoleVersionInput {
	if in.Documents == nil {
		return in
	}
	docs := make([]RoleDocumentInput, len(in.Documents))
	copy(docs, in.Documents)
	for i := range docs {
		d := &docs[i]
		if d.Instruction != nil && d.Instruction.Version == 0 && d.Content != "" {
			d.matchContent = true
		}
	}
	in.Documents = docs
	return in
}

// ListRoleProposals — предложения роли; state пусто — все.
func (a *Authoring) ListRoleProposals(ctx context.Context, projectKey, roleKey, state string) ([]domain.RoleProposal, error) {
	if state != "" && !domain.ValidProposalState(state) {
		return nil, apperr.InvalidInput("состояние предложения: pending, approved, rejected или withdrawn, а не %q", state)
	}
	q := a.store.DB()
	role, err := a.role(ctx, q, projectKey, roleKey)
	if err != nil {
		return nil, err
	}
	return a.store.ListRoleProposals(ctx, q, role.ID, state)
}

// GetRoleProposal — предложение роли с документами и их содержимым.
func (a *Authoring) GetRoleProposal(ctx context.Context, projectKey, roleKey string, proposalID uuid.UUID) (domain.RoleProposal, error) {
	q := a.store.DB()
	role, err := a.role(ctx, q, projectKey, roleKey)
	if err != nil {
		return domain.RoleProposal{}, err
	}
	return a.proposalOf(ctx, q, role, proposalID, false)
}

// proposalOf читает предложение и сверяет, что оно этой роли: чужое — 404,
// как несуществующее.
func (a *Authoring) proposalOf(ctx context.Context, q postgres.Querier, role domain.Role, id uuid.UUID, forUpdate bool) (domain.RoleProposal, error) {
	p, err := a.store.GetRoleProposal(ctx, q, id, forUpdate)
	if err != nil {
		return p, err
	}
	if p.RoleID != role.ID {
		return domain.RoleProposal{}, apperr.NotFound("предложение %s не найдено у роли %q", id, role.Key)
	}
	return p, nil
}

func notPending(p domain.RoleProposal) error {
	return apperr.Conflict("предложение уже решено: состояние %s", p.State).
		WithDetails(map[string]any{"state": p.State, "result_version": p.ResultVersion})
}

// ApproveRoleProposal утверждает предложение одной транзакцией: под замком
// роли сверяет базу с последней версией, заводит версию N+1 через
// addVersion из сохранённого содержимого (created_by — автор предложения)
// и закрывает предложение. База отстала — stale_role_base, версия не
// заводится, предложение остаётся pending.
func (a *Authoring) ApproveRoleProposal(ctx context.Context, auth *Auth, id Identity, projectKey, roleKey string, proposalID uuid.UUID) (domain.RoleProposal, error) {
	var out domain.RoleProposal
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		role, err := a.role(ctx, tx, projectKey, roleKey)
		if err != nil {
			return err
		}
		if err := auth.MayDecideRoleProposal(ctx, id, role.ProjectID); err != nil {
			return err
		}
		// Замок роли — первым: утверждения двух предложений одной роли и
		// прямая версия встают в очередь здесь, а не на строке предложения.
		if err := a.store.LockRole(ctx, tx, role.ID); err != nil {
			return err
		}
		p, err := a.proposalOf(ctx, tx, role, proposalID, true)
		if err != nil {
			return err
		}
		if p.State != domain.ProposalPending {
			return notPending(p)
		}
		latest, err := a.store.LatestRoleVersion(ctx, tx, role.ID)
		if err != nil {
			return err
		}
		if latest.Version != p.BaseVersion {
			return apperr.New(apperr.CodeStaleRoleBase,
				"предложение подано от версии %d, а последняя версия роли %q — %d: "+
					"утвердить нельзя, отклоните или отзовите его; автор подаёт заново от новой базы",
				p.BaseVersion, role.Key, latest.Version).
				WithDetails(map[string]any{"base_version": p.BaseVersion, "latest_version": latest.Version})
		}
		rv, err := a.addVersion(ctx, tx, role.ProjectID, role.ID, p.CreatedBy, versionOfProposal(p))
		if err != nil {
			return err
		}
		if err := sameProposalDocuments(p.Documents, rv.Documents); err != nil {
			return err
		}
		if err := a.store.DecideRoleProposal(ctx, tx, postgres.DecideRoleProposalInput{
			ID: p.ID, State: domain.ProposalApproved, DecidedBy: id.Principal.ID, ResultVersion: &rv.Version,
		}); err != nil {
			return err
		}
		out, err = a.store.GetRoleProposal(ctx, tx, p.ID, false)
		return err
	})
	return out, err
}

// versionOfProposal — вход addVersion из сохранённого предложения.
// Закрепление переносится готовым (версии инструкций иммутабельны, байты
// те же — их держит составной ключ схемы), а у незакреплённого документа
// стоит Unpin: при подаче перенос закрепления уже отработал, и повторный
// перенос при утверждении закрепил бы документ, который автор отвязал.
func versionOfProposal(p domain.RoleProposal) RoleVersionInput {
	docs := make([]RoleDocumentInput, len(p.Documents))
	for i, d := range p.Documents {
		docs[i] = RoleDocumentInput{Name: d.Name, Kind: d.Kind, Content: deref(d.Content)}
		if d.Instruction != nil {
			pin := *d.Instruction
			docs[i].pin = &pin
		} else {
			docs[i].Unpin = true
		}
	}
	changelog := p.Changelog
	return RoleVersionInput{
		Documents:      docs,
		InputKinds:     p.InputKinds,
		OutputContract: p.OutputContract,
		ToolPolicy:     p.ToolPolicy,
		ModelHint:      p.ModelHint,
		ModelLevel:     p.ModelLevel,
		RepoScope:      p.RepoScope,
		ContextBudget:  p.ContextBudget,
		Changelog:      &changelog,
	}
}

// sameProposalDocuments — версия равна предложению по имени, типу,
// позиции, sha и закреплению. Расхождение — ошибка движка, а не ввода, и
// откатывает утверждение целиком: утверждающий видел предложение, а не то,
// что из него вышло.
func sameProposalDocuments(proposal, version []domain.RoleDocument) error {
	if !sameDocuments(proposal, version) {
		return apperr.Internal(nil, "документы версии роли разошлись с документами предложения")
	}
	return nil
}

// RejectRoleProposal отклоняет предложение; заметка обязательна — автор
// должен узнать, что поправить.
func (a *Authoring) RejectRoleProposal(ctx context.Context, auth *Auth, id Identity, projectKey, roleKey string, proposalID uuid.UUID, note string) (domain.RoleProposal, error) {
	note = strings.TrimSpace(note)
	return a.closeProposal(ctx, projectKey, roleKey, proposalID, func(role domain.Role, p domain.RoleProposal) (postgres.DecideRoleProposalInput, error) {
		if err := auth.MayDecideRoleProposal(ctx, id, role.ProjectID); err != nil {
			return postgres.DecideRoleProposalInput{}, err
		}
		if note == "" {
			return postgres.DecideRoleProposalInput{}, apperr.InvalidInput("отклонение без заметки: напишите автору, что поправить")
		}
		return postgres.DecideRoleProposalInput{
			ID: p.ID, State: domain.ProposalRejected, DecidedBy: id.Principal.ID, Note: &note,
		}, nil
	})
}

// WithdrawRoleProposal отзывает предложение: автор или maintainer и выше.
// Заметка необязательна.
func (a *Authoring) WithdrawRoleProposal(ctx context.Context, auth *Auth, id Identity, projectKey, roleKey string, proposalID uuid.UUID, note string) (domain.RoleProposal, error) {
	note = strings.TrimSpace(note)
	return a.closeProposal(ctx, projectKey, roleKey, proposalID, func(role domain.Role, p domain.RoleProposal) (postgres.DecideRoleProposalInput, error) {
		if err := auth.MayWithdrawRoleProposal(ctx, id, role.ProjectID, p); err != nil {
			return postgres.DecideRoleProposalInput{}, err
		}
		in := postgres.DecideRoleProposalInput{ID: p.ID, State: domain.ProposalWithdrawn, DecidedBy: id.Principal.ID}
		if note != "" {
			in.Note = &note
		}
		return in, nil
	})
}

// closeProposal — отклонение и отзыв: предложение под замком, право и
// состояние, перевод. Версия не заводится, роль не блокируется.
func (a *Authoring) closeProposal(ctx context.Context, projectKey, roleKey string, proposalID uuid.UUID,
	decide func(domain.Role, domain.RoleProposal) (postgres.DecideRoleProposalInput, error)) (domain.RoleProposal, error) {
	var out domain.RoleProposal
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		role, err := a.role(ctx, tx, projectKey, roleKey)
		if err != nil {
			return err
		}
		p, err := a.proposalOf(ctx, tx, role, proposalID, true)
		if err != nil {
			return err
		}
		in, err := decide(role, p)
		if err != nil {
			return err
		}
		if p.State != domain.ProposalPending {
			return notPending(p)
		}
		if err := a.store.DecideRoleProposal(ctx, tx, in); err != nil {
			return err
		}
		out, err = a.store.GetRoleProposal(ctx, tx, p.ID, false)
		return err
	})
	return out, err
}

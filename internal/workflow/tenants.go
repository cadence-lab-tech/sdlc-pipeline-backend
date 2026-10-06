// Арендатор — команда (решение 0033).
//
// Личная команда заводится при входе и делает владельца проекта бесплатным:
// он есть у любого, кто вошёл. Организацию заводит любой вошедший человек,
// а рост её состава держит тариф. Квота живёт здесь, рядом с правами и
// бюджетом, а не в движке: движок про тарифы не знает.
//
// Всё это — самообслуживание общего инстанса. На выделенном инстансе
// компании (`SDLC_TENANCY=single`, CLAB-88) его нет: личные команды не
// заводятся, проект заводится только в команде, команды заводит
// администратор инстанса, тарифов и квот нет. Модель та же — меняется
// только то, что инстанс заводит и показывает.
package workflow

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Профили поставки. На выделенном инстансе пределы задаёт договор, и
// проверки квот отвечают «можно», не спрашивая базу.
const (
	TenancyShared = "shared"
	TenancySingle = "single"
)

// Tenancy задаёт профиль поставки. Пусто — общий инстанс.
func (a *Auth) Tenancy(profile string) *Auth {
	a.tenancy = profile
	return a
}

// QuotasOn — проверяются ли пределы тарифа.
func (a *Auth) QuotasOn() bool { return !a.single() }

// single — выделенный инстанс компании: самообслуживания общего инстанса
// (личных команд, организаций по кнопке, тарифов) на нём нет. Все ветвления
// по профилю читают этот предикат, а не строку.
func (a *Auth) single() bool { return a.tenancy == TenancySingle }

// Отказы профиля single: одна формулировка, каким путём ни пришёл.
func errNoPersonalTeams() error {
	return apperr.InvalidInput("укажите команду-владельца: личных команд на этом инстансе нет")
}

func errNoPlans() error {
	return apperr.NotFound("тарифов на этом инстансе нет")
}

// personalTeamAtLogin заводит личную команду при входе и бутстрапе — на
// общем инстансе. На выделенном человек попадает только в команды, куда его
// добавили, пригласили или синхронизировали из групп провайдера.
func (a *Auth) personalTeamAtLogin(ctx context.Context, tx pgx.Tx, p domain.Principal) error {
	if a.single() {
		return nil
	}
	_, err := a.ensurePersonalTeam(ctx, tx, p)
	return err
}

// ensurePersonalTeam находит личную команду человека или заводит её.
//
// Ключ — хэндл; занятый ключ получает суффикс от идентификатора, как и
// хэндл при входе. Право заводить проекты — по режиму инстанса: при
// открытом создании оно есть сразу, иначе его выдаёт администратор. На
// выделенном инстансе новая личная команда не заводится: оставшаяся с
// общего находится, иначе — «не найдена».
func (a *Auth) ensurePersonalTeam(ctx context.Context, tx pgx.Tx, p domain.Principal) (domain.Team, error) {
	if p.Kind != domain.PrincipalUser {
		return domain.Team{}, apperr.InvalidInput("личная команда бывает только у человека, а %s:%s — %s", p.Kind, p.Handle, p.Kind)
	}
	team, err := a.store.PersonalTeamOf(ctx, tx, p.ID)
	if err == nil {
		return team, nil
	}
	if apperr.As(err).Code != apperr.CodeNotFound {
		return domain.Team{}, err
	}
	if a.single() {
		return domain.Team{}, apperr.NotFound("личных команд на этом инстансе нет")
	}
	name := strings.TrimSpace(p.DisplayName)
	if name == "" {
		name = p.Handle
	}
	in := postgres.CreateTeamInput{
		Key: p.Handle, Name: name, Kind: domain.TeamPersonal, PersonalOf: &p.ID,
		CanCreateProjects: a.openProjects, CreatedBy: p.ID,
	}
	team, err = a.store.CreateTeam(ctx, tx, in)
	if err != nil && apperr.As(err).Code == apperr.CodeConflict {
		in.Key = p.Handle + "-" + shortSubject(p.ID.String())
		team, err = a.store.CreateTeam(ctx, tx, in)
	}
	if err != nil {
		return domain.Team{}, err
	}
	if _, err := a.store.SetTeamMember(ctx, tx, team.ID, p.ID, domain.TeamRoleLead, p.ID); err != nil {
		return domain.Team{}, err
	}
	return team, nil
}

// PersonalTeam — личная команда актора; заводится, если её ещё нет. Так
// её получает и актор из заголовка в режиме отладки, и администратор из
// бутстрапа — все, кто заводит проект без явной команды. На выделенном
// инстансе не заводится (см. ensurePersonalTeam).
func (a *Auth) PersonalTeam(ctx context.Context, id Identity) (domain.Team, error) {
	var out domain.Team
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.ensurePersonalTeam(ctx, tx, id.Principal)
		out = team
		return err
	})
	return out, err
}

// findPrincipal находит участника для добавления в проект или команду.
// Человек обязан существовать: заводить его по хэндлу нельзя (решение
// 0033) — на открытом инстансе это способ занять чужой логин. Агент
// заводится на месте: у него нет входа, которым его можно перехватить.
func (a *Auth) findPrincipal(ctx context.Context, q postgres.Querier, raw string, defaultKind domain.PrincipalKind) (domain.Principal, error) {
	kind, handle := SplitHandle(raw, defaultKind)
	if strings.TrimSpace(handle) == "" {
		return domain.Principal{}, apperr.InvalidInput("пустой хэндл")
	}
	if kind == domain.PrincipalAgent {
		return a.store.ResolveActor(ctx, q, handle, kind)
	}
	p, err := a.store.GetPrincipalByHandle(ctx, q, handle)
	if err != nil {
		if apperr.As(err).Code == apperr.CodeNotFound {
			return p, apperr.NotFound("принципал %q не найден: человек появляется после первого входа, добавить его заранее нельзя", handle)
		}
		return p, err
	}
	if p.Kind != kind {
		return p, apperr.NotFound("принципал %s:%s не найден (есть %s:%s)", kind, handle, p.Kind, p.Handle)
	}
	return p, nil
}

// memberQuota отбивает добавление, если тариф команды не вмещает ещё
// adding участников. Личная команда участников не принимает вовсе.
func (a *Auth) memberQuota(ctx context.Context, q postgres.Querier, team domain.Team, adding int) error {
	if team.Kind == domain.TeamPersonal {
		return errPersonalTeam()
	}
	if !a.QuotasOn() {
		return nil
	}
	plan, err := a.store.PlanOf(ctx, q, team.ID)
	if err != nil {
		return err
	}
	if plan.MembersMax == nil {
		return nil
	}
	n, err := a.store.CountTeamMembers(ctx, q, team.ID)
	if err != nil {
		return err
	}
	if n+adding > *plan.MembersMax {
		return apperr.Forbidden("тариф «%s»: в организации %s не больше %d участников, сейчас %d",
			plan.Name, team.Key, *plan.MembersMax, n)
	}
	return nil
}

// errPersonalTeam — отказ личной команде в новом участнике: и прямом, и по
// приглашению, чтобы человек видел одну причину, каким путём ни пришёл.
func errPersonalTeam() error {
	return apperr.Forbidden("личный аккаунт — один человек; чтобы работать с коллегами, заведите организацию или превратите в неё этот аккаунт")
}

// ConvertTeamInput — новые ключ и название организации; пусто — прежние.
type ConvertTeamInput struct {
	Key  string
	Name string
}

// ConvertTeam превращает личную команду в организацию: проекты остаются,
// меняется вид. Следующий вход заведёт человеку новую личную команду. На
// выделенном инстансе превращает только администратор: команды там заводит
// он, а оставшиеся с общего инстанса личные команды — его наследство.
func (a *Auth) ConvertTeam(ctx context.Context, id Identity, key string, in ConvertTeamInput) (domain.Team, error) {
	if a.single() && a.Enforced() && !id.Principal.IsAdmin {
		return domain.Team{}, apperr.Forbidden("команды на этом инстансе заводит и превращает в организации администратор")
	}
	var out domain.Team
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.store.GetTeamByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if _, err := a.teamRole(ctx, tx, id, team.ID, true); err != nil {
			return err
		}
		if team.Kind != domain.TeamPersonal {
			return apperr.Conflict("%s — уже организация", team.Key)
		}
		out, err = a.store.ConvertTeam(ctx, tx, team.ID, strings.TrimSpace(in.Key), strings.TrimSpace(in.Name))
		return err
	})
	return out, err
}

// ── тарифы ──────────────────────────────────────────────────────────────────

// ListPlans — тарифы с пределами. Открыт любому вошедшему: чтобы понять,
// во что упёрся, нужно видеть, что предлагается. На выделенном инстансе
// тарифов нет — «не найдено», как и у остальных ручек тарифа.
func (a *Auth) ListPlans(ctx context.Context, id Identity) ([]domain.Plan, error) {
	if a.single() {
		return nil, errNoPlans()
	}
	return a.store.ListPlans(ctx, a.store.DB())
}

// TeamPlan — действующий тариф команды.
func (a *Auth) TeamPlan(ctx context.Context, id Identity, key string) (domain.Plan, error) {
	if a.single() {
		return domain.Plan{}, errNoPlans()
	}
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, key)
	if err != nil {
		return domain.Plan{}, err
	}
	if _, err := a.teamRole(ctx, q, id, team.ID, false); err != nil {
		return domain.Plan{}, err
	}
	return a.store.PlanOf(ctx, q, team.ID)
}

// SetTeamPlan переводит команду на тариф. Право — у администратора
// инстанса: команда, назначающая себе тариф сама, — это не тариф. Приём
// денег этот шов не трогает: когда он появится, сюда придёт платёжный
// провайдер, а не ещё одна ручка.
func (a *Auth) SetTeamPlan(ctx context.Context, id Identity, key, planCode string) (domain.Plan, error) {
	// Раньше проверки права: тарифов нет ни у кого, и администратору тоже.
	if a.single() {
		return domain.Plan{}, errNoPlans()
	}
	if err := a.RequireAdmin(id); err != nil {
		return domain.Plan{}, err
	}
	planCode = strings.TrimSpace(planCode)
	var out domain.Plan
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.store.GetTeamByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		plan, err := a.store.GetPlan(ctx, tx, planCode)
		if err != nil {
			return err
		}
		if err := a.store.SetSubscription(ctx, tx, team.ID, plan.Code, id.Principal.ID); err != nil {
			return err
		}
		out = plan
		return nil
	})
	return out, err
}

// teamIDsOf — идентификаторы команд, где принципал лид: для проверки
// «может ли завести проект» без похода за каждой командой отдельно.
func leadTeams(teams []domain.TeamMember) []uuid.UUID {
	out := []uuid.UUID{}
	for _, t := range teams {
		if t.Role == domain.TeamRoleLead {
			out = append(out, t.TeamID)
		}
	}
	return out
}

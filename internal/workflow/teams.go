// Команды: группы принципалов с правами на проекты (решение 0027).
//
// Правила живут рядом с остальными правами, в Auth, а не в отдельном сервисе:
// действующий уровень в проекте — это максимум из прямого участия и участия
// через команду, и разносить половины этого правила по разным местам значило
// бы считать права в двух местах.
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

// TeamDetail — команда вместе с составом и проектами: страница администратора
// и `sdlc team show` показывают их вместе, и три запроса подряд сюда же.
type TeamDetail struct {
	Team     domain.Team          `json:"team"`
	Members  []domain.TeamMember  `json:"members"`
	Projects []domain.ProjectTeam `json:"projects"`
	// Действующий тариф: предел участников читается вместе с составом.
	Plan domain.Plan `json:"plan"`
	// Установки GitHub App, записанные на команду (решение 0034).
	Installations []domain.GitHubInstallation `json:"installations"`
}

type CreateTeamInput struct {
	Key               string
	Name              string
	Description       *string
	CanCreateProjects bool
}

// UpdateTeamInput — nil-поля не меняются.
type UpdateTeamInput struct {
	Name              *string
	Description       *string
	CanCreateProjects *bool
}

// ListTeams — все команды администратору системы, свои — остальным.
func (a *Auth) ListTeams(ctx context.Context, id Identity) ([]domain.Team, error) {
	if !a.Enforced() || id.Principal.IsAdmin {
		return a.store.ListTeams(ctx, a.store.DB(), nil)
	}
	return a.store.ListTeams(ctx, a.store.DB(), &id.Principal.ID)
}

func (a *Auth) GetTeam(ctx context.Context, id Identity, key string) (TeamDetail, error) {
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, key)
	if err != nil {
		return TeamDetail{}, err
	}
	if _, err := a.teamRole(ctx, q, id, team.ID, false); err != nil {
		return TeamDetail{}, err
	}
	members, err := a.store.ListTeamMembers(ctx, q, team.ID)
	if err != nil {
		return TeamDetail{}, err
	}
	projects, err := a.store.ListProjectTeams(ctx, q, nil, &team.ID)
	if err != nil {
		return TeamDetail{}, err
	}
	plan, err := a.store.PlanOf(ctx, q, team.ID)
	if err != nil {
		return TeamDetail{}, err
	}
	installations, err := a.store.ListInstallations(ctx, q, team.ID)
	if err != nil {
		return TeamDetail{}, err
	}
	return TeamDetail{Team: team, Members: members, Projects: projects, Plan: plan, Installations: installations}, nil
}

// CreateTeam заводит организацию и делает создателя её лидом: команда без
// лида не сможет ни принять человека, ни завести проект. Заводит любой
// вошедший человек (решение 0033); право на проекты новая команда получает
// по режиму инстанса, а явно его выдаёт только администратор.
func (a *Auth) CreateTeam(ctx context.Context, id Identity, in CreateTeamInput) (domain.Team, error) {
	if a.Enforced() && !id.Principal.IsAdmin && id.Principal.Kind != domain.PrincipalUser {
		return domain.Team{}, apperr.Forbidden("организацию заводит человек, а не %s", id.Principal.Kind)
	}
	// Явно выданное право только поднимает: в открытом режиме оно есть у всех.
	canCreate := a.openProjects || (in.CanCreateProjects && a.RequireAdmin(id) == nil)
	key := strings.TrimSpace(in.Key)
	if key == "" || strings.TrimSpace(in.Name) == "" {
		return domain.Team{}, apperr.InvalidInput("у команды должны быть ключ и название")
	}
	var out domain.Team
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.store.CreateTeam(ctx, tx, postgres.CreateTeamInput{
			Key: key, Name: in.Name, Description: in.Description, Kind: domain.TeamOrganization,
			CanCreateProjects: canCreate, CreatedBy: id.Principal.ID,
		})
		if err != nil {
			return err
		}
		if _, err := a.store.SetTeamMember(ctx, tx, team.ID, id.Principal.ID,
			domain.TeamRoleLead, id.Principal.ID); err != nil {
			return err
		}
		out = team
		return nil
	})
	return out, err
}

// UpdateTeam: название и описание правит лид, право заводить проекты —
// только администратор системы. Иначе команда выдала бы это право себе сама.
func (a *Auth) UpdateTeam(ctx context.Context, id Identity, key string, in UpdateTeamInput) (domain.Team, error) {
	var out domain.Team
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.store.GetTeamByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if _, err := a.teamRole(ctx, tx, id, team.ID, true); err != nil {
			return err
		}
		if in.CanCreateProjects != nil && *in.CanCreateProjects != team.CanCreateProjects {
			if err := a.RequireAdmin(id); err != nil {
				return apperr.Forbidden("право заводить проекты выдаёт администратор системы")
			}
		}
		out, err = a.store.UpdateTeam(ctx, tx, team.ID, postgres.UpdateTeamInput{
			Name: in.Name, Description: in.Description, CanCreateProjects: in.CanCreateProjects,
		})
		return err
	})
	return out, err
}

func (a *Auth) ArchiveTeam(ctx context.Context, id Identity, key string) error {
	if err := a.RequireAdmin(id); err != nil {
		return err
	}
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, key)
	if err != nil {
		return err
	}
	return a.store.ArchiveTeam(ctx, q, team.ID)
}

// SetTeamMember добавляет принципала в команду или меняет его роль. Человек
// обязан уже существовать, агент заводится на месте; новый участник проходит
// квоту тарифа, личная команда не принимает никого (решение 0033).
func (a *Auth) SetTeamMember(ctx context.Context, id Identity, key, principal string, role domain.TeamRole) (domain.TeamMember, error) {
	if !role.Valid() {
		return domain.TeamMember{}, apperr.InvalidInput("роль в команде: member или lead, а не %q", role)
	}
	var out domain.TeamMember
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.store.GetTeamByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if _, err := a.teamRole(ctx, tx, id, team.ID, true); err != nil {
			return err
		}
		p, err := a.findPrincipal(ctx, tx, principal, domain.PrincipalUser)
		if err != nil {
			return err
		}
		current, err := a.store.TeamRoleOf(ctx, tx, team.ID, p.ID)
		if err != nil {
			return err
		}
		if current == "" {
			if err := a.memberQuota(ctx, tx, team, 1); err != nil {
				return err
			}
		}
		if role == domain.TeamRoleMember {
			if err := a.guardLastLead(ctx, tx, team.ID, p.ID); err != nil {
				return err
			}
		}
		out, err = a.store.SetTeamMember(ctx, tx, team.ID, p.ID, role, id.Principal.ID)
		return err
	})
	return out, err
}

func (a *Auth) RemoveTeamMember(ctx context.Context, id Identity, key, principal string) error {
	return a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.store.GetTeamByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if _, err := a.teamRole(ctx, tx, id, team.ID, true); err != nil {
			return err
		}
		_, handle := SplitHandle(principal, domain.PrincipalUser)
		p, err := a.store.GetPrincipalByHandle(ctx, tx, handle)
		if err != nil {
			return err
		}
		if err := a.guardLastLead(ctx, tx, team.ID, p.ID); err != nil {
			return err
		}
		return a.store.RemoveTeamMember(ctx, tx, team.ID, p.ID)
	})
}

// ── команды в проекте ───────────────────────────────────────────────────────

func (a *Auth) ListProjectTeams(ctx context.Context, id Identity, projectKey string) ([]domain.ProjectTeam, error) {
	q := a.store.DB()
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	if err := a.Require(ctx, id, project.ID, domain.MemberViewer); err != nil {
		return nil, err
	}
	return a.store.ListProjectTeams(ctx, q, &project.ID, nil)
}

// SetProjectTeam выдаёт команде уровень и роли специалистов в проекте
// (roleKeys nil — любые, решение 0030). Право — у администратора проекта:
// раздача доступа целой группе не мельче, чем поимённая.
func (a *Auth) SetProjectTeam(ctx context.Context, id Identity, projectKey, teamKey string, level domain.MemberRole, roleKeys []string) (domain.ProjectTeam, error) {
	if !level.Valid() {
		return domain.ProjectTeam{}, apperr.InvalidInput(
			"уровень участия: viewer, agent, maintainer или admin, а не %q", level)
	}
	var out domain.ProjectTeam
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		if err := a.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
			return err
		}
		team, err := a.store.GetTeamByKey(ctx, tx, teamKey)
		if err != nil {
			return err
		}
		if err := a.checkRoleKeys(ctx, tx, project.ID, roleKeys); err != nil {
			return err
		}
		out, err = a.store.SetProjectTeam(ctx, tx, project.ID, team.ID, level, roleKeys, id.Principal.ID)
		return err
	})
	return out, err
}

func (a *Auth) RemoveProjectTeam(ctx context.Context, id Identity, projectKey, teamKey string) error {
	return a.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := a.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		if err := a.Require(ctx, id, project.ID, domain.MemberAdmin); err != nil {
			return err
		}
		team, err := a.store.GetTeamByKey(ctx, tx, teamKey)
		if err != nil {
			return err
		}
		// Команда-владелец теряет участие, но не владение: проект остаётся
		// её ответственностью, и лид сохраняет доступ.
		return a.store.RemoveProjectTeam(ctx, tx, project.ID, team.ID)
	})
}

// OwnerTeamFor проверяет право завести проект и отдаёт команду-владельца.
//
// Проект всегда принадлежит команде (решение 0033). Без явной команды он
// уходит в личную команду создателя — она заводится, если её ещё нет, — и
// право на проекты у неё есть по режиму инстанса. Явно названная команда
// проверяется как раньше: лид и `can_create_projects`. Администратор
// системы и режим без проверки прав проходят в любую.
func (a *Auth) OwnerTeamFor(ctx context.Context, id Identity, teamKey string) (uuid.UUID, error) {
	teamKey = strings.TrimSpace(teamKey)
	if teamKey == "" {
		if id.Principal.Kind != domain.PrincipalUser {
			return uuid.Nil, apperr.InvalidInput("укажите команду-владельца: личной команды у %s нет", id.Principal.Kind)
		}
		team, err := a.PersonalTeam(ctx, id)
		if err != nil {
			return uuid.Nil, err
		}
		// При открытом создании личной команде право не нужно: режим
		// инстанса выше флага, поставленного при её заведении.
		if !a.Enforced() || id.Principal.IsAdmin || team.CanCreateProjects || a.openProjects {
			return team.ID, nil
		}
		return uuid.Nil, apperr.Forbidden("проекты заводят администратор системы и лиды команд с правом на это")
	}
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, teamKey)
	if err != nil {
		return uuid.Nil, err
	}
	if !a.Enforced() || id.Principal.IsAdmin {
		return team.ID, nil
	}
	role, err := a.store.TeamRoleOf(ctx, q, team.ID, id.Principal.ID)
	if err != nil {
		return uuid.Nil, err
	}
	if role != domain.TeamRoleLead {
		return uuid.Nil, apperr.Forbidden("проект команде заводит её лид")
	}
	if !team.CanCreateProjects {
		return uuid.Nil, apperr.Forbidden("команде %q не выдано право заводить проекты", team.Key)
	}
	return team.ID, nil
}

// BootstrapTeam заводит первую команду и делает администратора её лидом:
// в пустом инстансе иначе некому завести ни команду, ни проект под ней.
// Повторный старт с тем же ключом ничего не меняет — как и бутстрап токена.
func (a *Auth) BootstrapTeam(ctx context.Context, adminHandle, key, name string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if strings.TrimSpace(name) == "" {
		name = key
	}
	return a.store.InTx(ctx, func(tx pgx.Tx) error {
		admin, err := a.store.ResolveActor(ctx, tx, adminHandle, domain.PrincipalUser)
		if err != nil {
			return err
		}
		team, err := a.store.GetTeamByKey(ctx, tx, key)
		if err != nil {
			if apperr.As(err).Code != apperr.CodeNotFound {
				return err
			}
			// Право заводить проекты у первой команды есть по определению:
			// без него инстанс остаётся пустым до ручного вмешательства.
			team, err = a.store.CreateTeam(ctx, tx, postgres.CreateTeamInput{
				Key: key, Name: name, CanCreateProjects: true, CreatedBy: admin.ID,
			})
			if err != nil {
				return err
			}
		}
		role, err := a.store.TeamRoleOf(ctx, tx, team.ID, admin.ID)
		if err != nil || role == domain.TeamRoleLead {
			return err
		}
		_, err = a.store.SetTeamMember(ctx, tx, team.ID, admin.ID, domain.TeamRoleLead, admin.ID)
		return err
	})
}

// ── квоты по стоимости ──────────────────────────────────────────────────────

// budgetAllows отбивает захват этапа, когда команда-владелец проекта
// исчерпала предел. Живёт в движке, а не в транспорте: захват идёт и через
// REST, и через MCP-тул, и правило у них одно.
func (e *Engine) budgetAllows(ctx context.Context, q postgres.Querier, projectID uuid.UUID) error {
	state, err := e.store.ExceededBudget(ctx, q, projectID)
	if err != nil {
		return err
	}
	if !state.Exceeded() {
		return nil
	}
	return apperr.Forbidden(
		"команда %s исчерпала предел на %s: потрачено %.2f из %.2f USD; начатые прогоны доходят, новые этапы не забираются",
		state.TeamKey, periodName(state.Period), state.SpentUSD, state.LimitUSD)
}

func periodName(period string) string {
	switch period {
	case "day":
		return "день"
	case "week":
		return "неделю"
	case "month":
		return "месяц"
	}
	return period
}

// TeamBudgets — пределы команды вместе с текущим расходом: без расхода предел
// ничего не говорит.
type TeamBudgets struct {
	Team    domain.Team          `json:"team"`
	Budgets []domain.BudgetState `json:"budgets"`
}

func (a *Auth) TeamBudgets(ctx context.Context, id Identity, key string) (TeamBudgets, error) {
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, key)
	if err != nil {
		return TeamBudgets{}, err
	}
	if _, err := a.teamRole(ctx, q, id, team.ID, false); err != nil {
		return TeamBudgets{}, err
	}
	limits, err := a.store.ListTeamBudgets(ctx, q, team.ID)
	if err != nil {
		return TeamBudgets{}, err
	}
	out := TeamBudgets{Team: team, Budgets: []domain.BudgetState{}}
	for _, b := range limits {
		spent, err := a.store.TeamSpend(ctx, q, team.ID, b.Period)
		if err != nil {
			return TeamBudgets{}, err
		}
		out.Budgets = append(out.Budgets, domain.BudgetState{
			TeamKey: team.Key, Period: b.Period, LimitUSD: b.LimitUSD, SpentUSD: spent,
		})
	}
	return out, nil
}

// SetTeamBudget ставит или снимает предел. Право — у администратора системы:
// команда, назначающая себе бюджет сама, — это не бюджет.
func (a *Auth) SetTeamBudget(ctx context.Context, id Identity, key, period string, limitUSD float64) (TeamBudgets, error) {
	if err := a.RequireAdmin(id); err != nil {
		return TeamBudgets{}, err
	}
	switch period {
	case "day", "week", "month":
	default:
		return TeamBudgets{}, apperr.InvalidInput("окно бюджета: day, week или month, а не %q", period)
	}
	err := a.store.InTx(ctx, func(tx pgx.Tx) error {
		team, err := a.store.GetTeamByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if limitUSD <= 0 {
			return a.store.DeleteTeamBudget(ctx, tx, team.ID, period)
		}
		_, err = a.store.SetTeamBudget(ctx, tx, team.ID, period, limitUSD, id.Principal.ID)
		return err
	})
	if err != nil {
		return TeamBudgets{}, err
	}
	return a.TeamBudgets(ctx, id, key)
}

// ── общее ───────────────────────────────────────────────────────────────────

// teamRole проверяет доступ к команде и отдаёт роль актора в ней. lead —
// нужны ли права лида; иначе достаточно состоять в команде.
func (a *Auth) teamRole(ctx context.Context, q postgres.Querier, id Identity, teamID uuid.UUID, lead bool) (domain.TeamRole, error) {
	if !a.Enforced() || id.Principal.IsAdmin {
		return domain.TeamRoleLead, nil
	}
	role, err := a.store.TeamRoleOf(ctx, q, teamID, id.Principal.ID)
	if err != nil {
		return "", err
	}
	switch {
	case role == "":
		return "", apperr.Forbidden("нет доступа к команде")
	case lead && role != domain.TeamRoleLead:
		return "", apperr.Forbidden("это делает лид команды")
	}
	return role, nil
}

// guardLastLead не даёт оставить команду без лида.
func (a *Auth) guardLastLead(ctx context.Context, q postgres.Querier, teamID, principalID uuid.UUID) error {
	role, err := a.store.TeamRoleOf(ctx, q, teamID, principalID)
	if err != nil {
		return err
	}
	if role != domain.TeamRoleLead {
		return nil
	}
	n, err := a.store.CountTeamLeads(ctx, q, teamID)
	if err != nil {
		return err
	}
	if n <= 1 {
		return apperr.Conflict("это последний лид команды; сначала назначьте другого")
	}
	return nil
}

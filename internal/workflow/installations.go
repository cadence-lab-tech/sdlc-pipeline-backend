// Установки GitHub App на арендаторе (решение 0034).
//
// Человек нажимает «Подключить GitHub», выбирает аккаунт и репозитории на
// стороне GitHub и возвращается с installation_id. Право на установку
// доказывает не сам идентификатор — они идут подряд, — а список установок,
// видимых вошедшему: за ним бэкенд ходит токеном пользователя, полученным по
// коду повторной авторизации, и тем же токеном спрашивает, какие репозитории
// установки человек видит сам. Токен живёт в запросе и нигде не сохраняется;
// читает репозитории по-прежнему установка приложения.
package workflow

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// ConnectGitHubInput — возврат от GitHub. Код авторизации обязателен: без
// него нечем доказать право на установку и нечем спросить репозитории.
// InstallationID приходит после установки приложения; без него запрос лишь
// перечитывает репозитории уже подключённых установок команды.
type ConnectGitHubInput struct {
	TeamKey string
	// Проект, из настроек которого человек пришёл: даёт право участнику
	// проекта, не состоящему в команде-владельце.
	ProjectKey     string
	Code           string
	RedirectURI    string
	InstallationID *int64
}

// GitHubRepoChoice — репозиторий, который можно привязать: он в установке
// команды, и вошедший видит его сам.
type GitHubRepoChoice struct {
	InstallationID int64  `json:"installation_id"`
	AccountLogin   string `json:"account_login"`
	github.UserRepository
}

// GitHubConnection — что показать после возврата от GitHub: установки
// команды, только что подключённая и репозитории на выбор.
type GitHubConnection struct {
	Team          domain.Team                 `json:"team"`
	Installations []domain.GitHubInstallation `json:"installations"`
	Connected     *domain.GitHubInstallation  `json:"connected,omitempty"`
	Repositories  []GitHubRepoChoice          `json:"repositories"`
}

// GitHubInstallURL — куда вести кнопку «Подключить GitHub». Пусто — слаг
// приложения не задан, и подключать нечего.
func (a *Auth) GitHubInstallURL() string { return a.githubInstallURL }

// GitHubApp задаёт слаг приложения: адрес его установки строится из него.
func (a *Auth) GitHubApp(slug string) *Auth {
	if slug != "" && a.github != nil {
		a.githubInstallURL = a.github.InstallURL(slug)
	}
	return a
}

// ConnectGitHub записывает установку на команду и отдаёт репозитории на выбор.
func (a *Auth) ConnectGitHub(ctx context.Context, id Identity, in ConnectGitHubInput) (GitHubConnection, error) {
	if !a.GitHubEnabled() {
		return GitHubConnection{}, apperr.InvalidInput("вход через GitHub не настроен")
	}
	if id.Principal.Kind != domain.PrincipalUser {
		return GitHubConnection{}, apperr.Forbidden("GitHub подключает человек, а не %s", id.Principal.Kind)
	}
	if strings.TrimSpace(in.Code) == "" {
		return GitHubConnection{}, apperr.InvalidInput("нет кода авторизации GitHub")
	}
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, in.TeamKey)
	if err != nil {
		return GitHubConnection{}, err
	}
	// Подключить установку — дело лида (или администратора проекта команды,
	// пришедшего из его настроек); выбрать репозитории — любого участника.
	need := domain.MemberMaintainer
	if in.InstallationID != nil {
		need = domain.MemberAdmin
	}
	if err := a.installationAccess(ctx, q, id, team, in.ProjectKey, need); err != nil {
		return GitHubConnection{}, err
	}

	token, err := a.github.Exchange(ctx, in.Code, in.RedirectURI)
	if err != nil {
		if errors.Is(err, github.ErrOAuthDenied) {
			return GitHubConnection{}, apperr.Unauthenticated("%s", err.Error())
		}
		return GitHubConnection{}, apperr.Internal(err, "GitHub не отвечает")
	}
	mine, err := a.github.UserInstallations(ctx, token)
	if err != nil {
		return GitHubConnection{}, apperr.Internal(err, "GitHub не отдал установки")
	}
	visible := map[int64]github.Installation{}
	for _, i := range mine {
		visible[i.ID] = i
	}

	out := GitHubConnection{Team: team, Repositories: []GitHubRepoChoice{}}
	if in.InstallationID != nil {
		src, ok := visible[*in.InstallationID]
		if !ok {
			return GitHubConnection{}, apperr.Forbidden(
				"установка %d не видна вашему аккаунту GitHub: подключить её может тот, кто её ставил",
				*in.InstallationID)
		}
		err := a.store.InTx(ctx, func(tx pgx.Tx) error {
			rec, err := a.store.UpsertInstallation(ctx, tx, postgres.InstallationInput{
				InstallationID: src.ID, AccountLogin: src.AccountLogin, AccountID: src.AccountID,
				AccountType: src.AccountType, OwnerTeamID: team.ID, ConnectedBy: id.Principal.ID,
			})
			if err != nil {
				return err
			}
			out.Connected = &rec
			return nil
		})
		if err != nil {
			return GitHubConnection{}, err
		}
	}

	out.Installations, err = a.store.ListInstallations(ctx, q, team.ID)
	if err != nil {
		return GitHubConnection{}, err
	}
	// Репозитории — пересечение: установка команды, живая, и видна
	// вошедшему. Чужие установки того же человека (его личный аккаунт при
	// работе в организации) в список не попадают: их надо подключить.
	for _, inst := range out.Installations {
		if !inst.Active() {
			continue
		}
		if _, ok := visible[inst.InstallationID]; !ok {
			continue
		}
		repos, err := a.github.InstallationRepositories(ctx, token, inst.InstallationID)
		if err != nil {
			return GitHubConnection{}, apperr.Internal(err, "GitHub не отдал репозитории установки %d", inst.InstallationID)
		}
		for _, r := range repos {
			out.Repositories = append(out.Repositories, GitHubRepoChoice{
				InstallationID: inst.InstallationID, AccountLogin: inst.AccountLogin, UserRepository: r,
			})
		}
	}
	return out, nil
}

// ListInstallations — установки команды. Видят участники команды.
func (a *Auth) ListInstallations(ctx context.Context, id Identity, teamKey string) ([]domain.GitHubInstallation, error) {
	q := a.store.DB()
	team, err := a.store.GetTeamByKey(ctx, q, teamKey)
	if err != nil {
		return nil, err
	}
	if _, err := a.teamRole(ctx, q, id, team.ID, false); err != nil {
		return nil, err
	}
	return a.store.ListInstallations(ctx, q, team.ID)
}

// installationAccess — кто распоряжается установками команды: лид команды
// всегда; участник — на чтение; участник проекта, которым команда владеет,
// с уровнем need — из настроек этого проекта.
func (a *Auth) installationAccess(ctx context.Context, q postgres.Querier, id Identity, team domain.Team, projectKey string, need domain.MemberRole) error {
	role, err := a.teamRole(ctx, q, id, team.ID, false)
	if err == nil && (role == domain.TeamRoleLead || need == domain.MemberMaintainer) {
		return nil
	}
	if projectKey == "" {
		if err != nil {
			return err
		}
		return apperr.Forbidden("это делает лид команды")
	}
	project, err := a.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return err
	}
	if project.OwnerTeamID == nil || *project.OwnerTeamID != team.ID {
		return apperr.Forbidden("проект %s не принадлежит команде %s", projectKey, team.Key)
	}
	return a.Require(ctx, id, project.ID, need)
}

// ── привязка репозиториев из установки ──────────────────────────────────────

// RepoPick — репозиторий из списка на выбор, каким его отдал GitHub.
type RepoPick struct {
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	CloneURL      string `json:"clone_url"`
	RoleHint      string `json:"role_hint"`
}

type AttachRepositoriesInput struct {
	InstallationID int64
	Repositories   []RepoPick
}

type AttachResult struct {
	Repositories []domain.Repository `json:"repositories"`
	Created      int                 `json:"created"`
}

// AttachRepositories привязывает к проекту пачку репозиториев одной
// установки. Установка должна принадлежать команде-владельцу проекта и быть
// живой; каждый репозиторий проверяется её токеном — привязать можно только
// то, что в установке есть. Прогон индексации ставит воркер сам, увидев
// новую цель.
func (e *Engine) AttachRepositories(ctx context.Context, projectKey string, in AttachRepositoriesInput) (AttachResult, error) {
	if len(in.Repositories) == 0 {
		return AttachResult{}, apperr.InvalidInput("не выбран ни один репозиторий")
	}
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return AttachResult{}, err
	}
	inst, err := e.installationOf(ctx, project, in.InstallationID)
	if err != nil {
		return AttachResult{}, err
	}
	for _, r := range in.Repositories {
		if r.Owner == "" || r.Name == "" {
			return AttachResult{}, apperr.InvalidInput("у репозитория нужны owner и name")
		}
		if err := e.probeInstallation(ctx, inst.InstallationID, r.Owner, r.Name); err != nil {
			return AttachResult{}, err
		}
	}

	out := AttachResult{Repositories: []domain.Repository{}}
	err = e.store.InTx(ctx, func(tx pgx.Tx) error {
		for _, r := range in.Repositories {
			visibility := "public"
			if r.Private {
				visibility = "private"
			}
			repo, created, err := e.store.UpsertRepository(ctx, tx, postgres.RepositoryInput{
				ProjectID: project.ID, Owner: r.Owner, Name: r.Name,
				DefaultBranch: r.DefaultBranch, CloneURL: r.CloneURL, Visibility: visibility,
				RoleHint: optional(r.RoleHint), InstallationID: &inst.InstallationID,
			})
			if err != nil {
				return err
			}
			if created {
				out.Created++
			}
			out.Repositories = append(out.Repositories, repo)
		}
		return nil
	})
	return out, err
}

// installationOf — установка, которой проект вправе пользоваться: она
// записана на его команду-владельца и жива.
func (e *Engine) installationOf(ctx context.Context, project domain.Project, installationID int64) (domain.GitHubInstallation, error) {
	inst, err := e.store.GetInstallation(ctx, e.store.DB(), installationID)
	if err != nil {
		return inst, err
	}
	if project.OwnerTeamID == nil || inst.OwnerTeamID != *project.OwnerTeamID {
		return inst, apperr.Forbidden("установка %d принадлежит команде %s, а не владельцу проекта %s",
			installationID, inst.OwnerTeamKey, project.Key)
	}
	switch {
	case inst.RemovedAt != nil:
		return inst, apperr.InvalidInput("установка %d снята на стороне GitHub", installationID)
	case inst.SuspendedAt != nil:
		return inst, apperr.InvalidInput("установка %d приостановлена на стороне GitHub", installationID)
	}
	return inst, nil
}

// probeInstallation спрашивает провайдера токеном установки. Без приложения
// у инстанса проверять нечем, и привязка проходит на слово: так ведёт себя и
// проект без учётных данных.
func (e *Engine) probeInstallation(ctx context.Context, installationID int64, owner, name string) error {
	if e.github == nil || !e.github.AppConfigured() {
		return nil
	}
	if err := e.github.CheckRepo(ctx, &installationID, "", owner, name); err != nil {
		if refusal := appRefusal(err); refusal != nil {
			return refusal
		}
		return apperr.InvalidInput("репозиторий %s/%s недоступен установке %d: %v", owner, name, installationID, err)
	}
	return nil
}

// ProjectGitHub — что показать в настройках проекта при открытии: установки
// команды-владельца, учётные данные проекта и состояние приложения из кэша.
// К провайдеру ходит не чаще раза за срок кэша и не ждёт его дольше пяти
// секунд; свежая проверка — CheckGitHubAccess по кнопке.
type ProjectGitHub struct {
	OwnerTeamKey   string                      `json:"owner_team_key"`
	Installations  []domain.GitHubInstallation `json:"installations"`
	InstallationID *int64                      `json:"installation_id,omitempty"`
	CredentialRef  *string                     `json:"credential_ref,omitempty"`
	AppConfigured  bool                        `json:"app_configured"`
	App            github.AppStatus            `json:"app"`
}

func (e *Engine) ProjectGitHub(ctx context.Context, projectKey string) (ProjectGitHub, error) {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return ProjectGitHub{}, err
	}
	out := ProjectGitHub{
		OwnerTeamKey: deref(project.OwnerTeamKey), Installations: []domain.GitHubInstallation{},
		InstallationID: project.GitHubInstallationID, CredentialRef: project.GitHubCredentialRef,
		AppConfigured: e.github != nil && e.github.AppConfigured(),
	}
	if project.OwnerTeamID != nil {
		out.Installations, err = e.store.ListInstallations(ctx, q, *project.OwnerTeamID)
		if err != nil {
			return ProjectGitHub{}, err
		}
	}
	// После базы: отказ в проекте не должен стоить похода к провайдеру.
	out.App = e.appStatus(ctx, false)
	return out, nil
}

// ProjectHealth — сводка состояния проекта для оболочки интерфейса
// (CLAB-79): отметки в меню на каждой странице проекта.
//
// Зовут её все участники на каждом переходе, поэтому она только читает: кэш
// состояния приложения и базу. Вешать на неё новую проверку у провайдера
// нельзя. Текста провайдера и расхождения часов здесь нет намеренно: сводку
// видит любой участник, а подробности — те же, кто видел их в настройках.
type ProjectHealth struct {
	App                    ProjectHealthApp `json:"app"`
	UnreadableRepositories int              `json:"unreadable_repositories"`
}

type ProjectHealthApp struct {
	State     github.AppState `json:"state"`
	CheckedAt *time.Time      `json:"checked_at,omitempty"`
}

func (e *Engine) ProjectHealth(ctx context.Context, projectKey string) (ProjectHealth, error) {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return ProjectHealth{}, err
	}
	unreadable, err := e.store.CountUnreadableRepositories(ctx, q, project.ID)
	if err != nil {
		return ProjectHealth{}, err
	}
	app := e.appStatus(ctx, false)
	return ProjectHealth{
		App:                    ProjectHealthApp{State: app.State, CheckedAt: app.CheckedAt},
		UnreadableRepositories: unreadable,
	}, nil
}

// pickInstallation подбирает репозиторию, привязываемому по координатам,
// живую установку команды-владельца, которой он доступен. Так `clab repo
// add` и агент получают ту же маршрутизацию вебхуков, что и выбор из списка.
// Ни одна не подошла — репозиторий остаётся на учётных данных проекта.
// Отказ в учётных данных приложения — не «не подошла»: он одинаков для всех
// установок, и перебор на нём останавливается с ошибкой, а не уводит
// репозиторий на учётные данные проекта мимо маршрутизации вебхуков.
func (e *Engine) pickInstallation(ctx context.Context, project domain.Project, owner, name string) (*int64, error) {
	if project.OwnerTeamID == nil || e.github == nil || !e.github.AppConfigured() {
		return nil, nil
	}
	installations, err := e.store.ListInstallations(ctx, e.store.DB(), *project.OwnerTeamID)
	if err != nil {
		return nil, err
	}
	for _, inst := range installations {
		if !inst.Active() {
			continue
		}
		err := e.github.CheckRepo(ctx, &inst.InstallationID, "", owner, name)
		if err == nil {
			id := inst.InstallationID
			return &id, nil
		}
		if refusal := appRefusal(err); refusal != nil {
			return nil, refusal
		}
	}
	return nil, nil
}

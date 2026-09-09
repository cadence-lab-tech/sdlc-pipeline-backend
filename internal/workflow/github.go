// Доступ к GitHub на уровне проекта (решение 0028).
//
// Проект ссылается на установку приложения либо на именованный секрет; в базе
// лежат ссылки, значения приезжают из окружения и живут в памяти процесса.
// Движок сам в провайдера не ходит: он зовёт GitHubAccess, который собирают в
// cmd/api поверх github.Provider. Так правило «репозиторий вне установки не
// привязывается» остаётся здесь, а транспорт до провайдера — там.
package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// GitHubAccess проверяет, достаёт ли система до репозитория учётными данными
// проекта. Реализация — в cmd/api; движок про клиента провайдера не знает.
type GitHubAccess interface {
	CheckRepo(ctx context.Context, installationID *int64, secretRef, owner, name string) error
	// AppConfigured — есть ли у инстанса приложение. Без него установку
	// проекту прописывать бессмысленно, и настройки это показывают.
	AppConfigured() bool
}

// SetGitHubInput — привязка проекта к установке приложения или к секрету.
// nil-поля не меняются.
type SetGitHubInput struct {
	InstallationID *int64
	CredentialRef  *string
}

func (e *Engine) SetProjectGitHub(ctx context.Context, projectKey string, in SetGitHubInput) (domain.Project, error) {
	var out domain.Project
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := e.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		out, err = e.store.SetProjectGitHub(ctx, tx, project.ID, in.InstallationID, in.CredentialRef)
		return err
	})
	return out, err
}

// RepoAccessResult — доступ к одному репозиторию проекта.
type RepoAccessResult struct {
	Repository string `json:"repository"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// GitHubStatus — что показывать в настройках проекта: чем система ходит в
// провайдера и достаёт ли она до каждого репозитория области.
type GitHubStatus struct {
	InstallationID *int64             `json:"installation_id,omitempty"`
	CredentialRef  *string            `json:"credential_ref,omitempty"`
	AppConfigured  bool               `json:"app_configured"`
	Repositories   []RepoAccessResult `json:"repositories"`
}

// CheckGitHubAccess опрашивает провайдера по каждому репозиторию проекта.
//
// Ходит в сеть, поэтому вызывается только из настроек проекта — по кнопке.
// Ни гейт, ни прогон индексации этим не пользуются: гейты в сеть не ходят
// (решение 0020), а прогон и так упрётся в ошибку доступа сам.
func (e *Engine) CheckGitHubAccess(ctx context.Context, projectKey string) (GitHubStatus, error) {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return GitHubStatus{}, err
	}
	repos, err := e.store.ListRepositories(ctx, q, project.ID)
	if err != nil {
		return GitHubStatus{}, err
	}
	status := GitHubStatus{
		InstallationID: project.GitHubInstallationID,
		CredentialRef:  project.GitHubCredentialRef,
		AppConfigured:  e.github != nil && e.github.AppConfigured(),
		Repositories:   []RepoAccessResult{},
	}
	if e.github == nil {
		return status, nil
	}
	for _, r := range repos {
		if r.Provider != "github" {
			continue
		}
		res := RepoAccessResult{Repository: r.Owner + "/" + r.Name, OK: true}
		if err := e.probeRepoAccess(ctx, project, r.Owner, r.Name); err != nil {
			res.OK, res.Error = false, apperr.As(err).Message
		}
		status.Repositories = append(status.Repositories, res)
	}
	return status, nil
}

// checkRepoAccess проверяет доступ там, где проект объявил свои учётные
// данные. Проект без установки и без секрета не проверяется вовсе: ходить в
// провайдера общим токеном на каждую привязку репозитория значило бы
// поставить заведение проекта в зависимость от доступности GitHub.
func (e *Engine) checkRepoAccess(ctx context.Context, project domain.Project, owner, name string) error {
	if e.github == nil || (project.GitHubInstallationID == nil && deref(project.GitHubCredentialRef) == "") {
		return nil
	}
	return e.probeRepoAccess(ctx, project, owner, name)
}

// probeRepoAccess спрашивает провайдера и переводит отказ в ошибку домена:
// для человека это неверная настройка, а не сбой.
func (e *Engine) probeRepoAccess(ctx context.Context, project domain.Project, owner, name string) error {
	err := e.github.CheckRepo(ctx, project.GitHubInstallationID, deref(project.GitHubCredentialRef), owner, name)
	if err == nil {
		return nil
	}
	if project.GitHubInstallationID != nil {
		return apperr.InvalidInput(
			"репозиторий %s/%s недоступен установке приложения у проекта %s: %v",
			owner, name, project.Key, err)
	}
	return apperr.InvalidInput("репозиторий %s/%s недоступен: %v", owner, name, err)
}

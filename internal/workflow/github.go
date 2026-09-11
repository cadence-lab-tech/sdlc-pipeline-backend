// Доступ к GitHub на уровне проекта (решения 0028, 0034).
//
// Проект и репозиторий ссылаются на установку приложения либо на именованный
// секрет; в базе лежат ссылки, значения приезжают из окружения и живут в
// памяти процесса. Какие из них брать, решает github.CredentialRefs.Resolve.
// Движок сам в провайдера не ходит: он зовёт GitHubAccess, который собирают в
// cmd/api поверх github.Provider. Так правило «репозиторий вне установки не
// привязывается» остаётся здесь, а транспорт до провайдера — там.
package workflow

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
)

// GitHubAccess проверяет, достаёт ли система до репозитория выбранными
// учётными данными. Реализация — в cmd/api; движок про клиента провайдера не
// знает.
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

// RepoAccessResult — доступ к одному репозиторию проекта и то, чем его
// проверяли. Секрет виден только по имени, значение сюда не попадает.
type RepoAccessResult struct {
	Repository     string                  `json:"repository"`
	OK             bool                    `json:"ok"`
	Error          string                  `json:"error,omitempty"`
	Source         github.CredentialSource `json:"source,omitempty"`
	InstallationID *int64                  `json:"installation_id,omitempty"`
	CredentialRef  string                  `json:"credential_ref,omitempty"`
}

// GitHubStatus — что показывать в настройках проекта: чем система ходит в
// провайдера и достаёт ли она до каждого репозитория области.
type GitHubStatus struct {
	InstallationID *int64             `json:"installation_id,omitempty"`
	CredentialRef  *string            `json:"credential_ref,omitempty"`
	AppConfigured  bool               `json:"app_configured"`
	Repositories   []RepoAccessResult `json:"repositories"`
}

// CheckGitHubAccess опрашивает провайдера по каждому репозиторию проекта теми
// учётными данными, которыми репозиторий читается (решение 0034).
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
	installations := map[int64]domain.GitHubInstallation{}
	for _, r := range repos {
		if r.Provider != "github" {
			continue
		}
		creds, source := repoCredentials(project, deref(r.CredentialRef), r.InstallationID).Resolve()
		res := RepoAccessResult{
			Repository: r.Owner + "/" + r.Name, OK: true,
			Source: source, InstallationID: creds.InstallationID, CredentialRef: creds.SecretRef,
		}
		refusal, err := e.repoRefusal(ctx, r, installations)
		if err != nil {
			return GitHubStatus{}, err
		}
		if refusal == nil {
			refusal = e.probeRepoAccess(ctx, source, creds, r.Owner, r.Name)
		}
		if refusal != nil {
			res.OK, res.Error = false, apperr.As(refusal).Message
		}
		status.Repositories = append(status.Repositories, res)
	}
	return status, nil
}

// repoCredentials — ссылки репозитория и проекта в одном месте, чтобы порядок
// между ними выбирал github.CredentialRefs.Resolve, а не вызывающий.
func repoCredentials(project domain.Project, repoSecret string, repoInstallation *int64) github.CredentialRefs {
	return github.CredentialRefs{
		RepositorySecret:       repoSecret,
		RepositoryInstallation: repoInstallation,
		ProjectInstallation:    project.GitHubInstallationID,
		ProjectSecret:          deref(project.GitHubCredentialRef),
	}
}

// repoRefusal — отказ без похода в сеть: репозиторий убран из своей установки
// или сама установка снята либо приостановлена. Учётными данными проекта это
// не подменяется, как и в целях индексации (indexTargetLive): мёртвая
// установка останавливает чтение, а не переключает его на другой доступ.
// Второе значение — сбой базы, а не ответ человеку.
func (e *Engine) repoRefusal(ctx context.Context, r domain.Repository, cache map[int64]domain.GitHubInstallation) (error, error) {
	if r.InstallationID == nil {
		if r.UnavailableAt != nil {
			return apperr.InvalidInput("репозиторий %s/%s убран из установки на стороне GitHub", r.Owner, r.Name), nil
		}
		return nil, nil
	}
	id := *r.InstallationID
	if r.UnavailableAt != nil {
		return apperr.InvalidInput("репозиторий %s/%s убран из установки %d на стороне GitHub", r.Owner, r.Name, id), nil
	}
	inst, ok := cache[id]
	if !ok {
		var err error
		if inst, err = e.store.GetInstallation(ctx, e.store.DB(), id); err != nil {
			return nil, err
		}
		cache[id] = inst
	}
	switch {
	case inst.RemovedAt != nil:
		return apperr.InvalidInput("установка %d репозитория %s/%s снята на стороне GitHub", id, r.Owner, r.Name), nil
	case inst.SuspendedAt != nil:
		return apperr.InvalidInput("установка %d репозитория %s/%s приостановлена на стороне GitHub", id, r.Owner, r.Name), nil
	}
	return nil, nil
}

// checkRepoAccess проверяет при привязке по координатам доступ тем, чем
// репозиторий будут читать: своим секретом, если он задан, иначе учётными
// данными проекта. Если не объявлено ничего, провайдера не спрашивают вовсе:
// ходить общим токеном на каждую привязку репозитория значило бы поставить
// заведение проекта в зависимость от доступности GitHub.
func (e *Engine) checkRepoAccess(ctx context.Context, project domain.Project, repoSecret, owner, name string) error {
	if e.github == nil {
		return nil
	}
	creds, source := repoCredentials(project, repoSecret, nil).Resolve()
	if source == github.SourceInstance {
		return nil
	}
	return e.probeRepoAccess(ctx, source, creds, owner, name)
}

// probeRepoAccess спрашивает провайдера выбранными учётными данными и
// переводит отказ в ошибку домена: для человека это неверная настройка, а не
// сбой. В тексте назван источник — чем проверяли, то и не достало.
func (e *Engine) probeRepoAccess(ctx context.Context, source github.CredentialSource, creds github.Credentials, owner, name string) error {
	err := e.github.CheckRepo(ctx, creds.InstallationID, creds.SecretRef, owner, name)
	if err == nil {
		return nil
	}
	return apperr.InvalidInput("репозиторий %s/%s недоступен (%s): %v",
		owner, name, describeSource(source, creds), err)
}

// describeSource — источник учётных данных словами, теми же, что в колонке
// «Чем проверяли» в настройках проекта. Секрет называется только по имени.
func describeSource(source github.CredentialSource, creds github.Credentials) string {
	var installation int64
	if creds.InstallationID != nil {
		installation = *creds.InstallationID
	}
	switch source {
	case github.SourceRepositorySecret:
		return fmt.Sprintf("секрет репозитория %s", creds.SecretRef)
	case github.SourceRepositoryInstallation:
		return fmt.Sprintf("установка репозитория %d", installation)
	case github.SourceProjectInstallation:
		return fmt.Sprintf("установка проекта %d", installation)
	case github.SourceProjectSecret:
		return fmt.Sprintf("секрет проекта %s", creds.SecretRef)
	}
	return "токен инстанса"
}

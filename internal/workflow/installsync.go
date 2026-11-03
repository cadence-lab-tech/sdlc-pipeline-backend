// Опрос установок GitHub App (CLAB-395).
//
// Вебхуки installation и installation_repositories — ускорение, а не условие
// корректности: без них отозванная установка и убранный из неё репозиторий
// оставались бы в базе «живыми», индексация ходила бы в провайдера и
// получала отказы. Воркер сверяет подключённые установки с провайдером и
// приводит базу к тому же состоянию, что дал бы вебхук, теми же методами
// хранилища: SetInstallationState и SetRepositoryAvailability.
//
// Сверка осторожна, потому что ошибка в ней останавливает индексацию
// клиента:
//   - состояние меняется только по полностью прочитанному ответу; ошибка,
//     лимит или оборванная пагинация ничего не меняют у той установки, к
//     которой относятся, и проход завершается ошибкой (метрика задания);
//   - снятие установки подтверждается запросом по её идентификатору, а
//     репозиторий, которого нет в списке, — запросом самого репозитория:
//     переименованный провайдер отдаёт по старому имени с новым full_name;
//   - снятую установку опрос не оживляет: подключают её заново, как и
//     сегодня, кнопкой (UpsertInstallation);
//   - пишется только расхождение: согласованная строка не получает UPDATE.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// InstallationSource — то, что сверке нужно от провайдера. Реализация —
// github.Provider; движок про транспорт не знает.
type InstallationSource interface {
	AppInstallations(ctx context.Context) ([]github.AppInstallation, error)
	AppInstallation(ctx context.Context, installationID int64) (github.AppInstallation, error)
	InstallationRepositories(ctx context.Context, installationID int64) ([]github.Repo, error)
	InstallationRepo(ctx context.Context, installationID int64, repo github.Repo) (github.Repository, error)
	ForgetInstallationToken(installationID int64)
}

// Изменения, которые нашла сверка. Значения — для лога воркера.
const (
	ChangeInstallationRemoved     = "installation_removed"
	ChangeInstallationSuspended   = "installation_suspended"
	ChangeInstallationUnsuspended = "installation_unsuspended"
	ChangeRepositoryUnavailable   = "repository_unavailable"
	ChangeRepositoryAvailable     = "repository_available"
)

// InstallationChange — одно применённое изменение.
type InstallationChange struct {
	InstallationID int64
	Change         string
	// Repository — owner/name для изменений репозитория.
	Repository string
}

// SyncInstallations сверяет подключённые установки с провайдером. Отдаёт
// применённые изменения; ошибка — склейка отказов по установкам, при ней
// изменения других установок уже применены.
func (e *Engine) SyncInstallations(ctx context.Context, src InstallationSource) ([]InstallationChange, error) {
	known, err := e.store.AllInstallations(ctx, e.store.DB())
	if err != nil {
		return nil, err
	}
	var watched []domain.GitHubInstallation
	for _, inst := range known {
		if inst.RemovedAt != nil {
			// Снятую опрос не оживляет и в провайдера за ней не ходит; токен
			// её в кэше не нужен никому.
			src.ForgetInstallationToken(inst.InstallationID)
			continue
		}
		watched = append(watched, inst)
	}
	if len(watched) == 0 {
		return nil, nil
	}

	listed, err := src.AppInstallations(ctx)
	if err != nil {
		return nil, fmt.Errorf("список установок приложения: %w", err)
	}
	byID := make(map[int64]github.AppInstallation, len(listed))
	for _, i := range listed {
		byID[i.ID] = i
	}

	var changes []InstallationChange
	var errs []error
	for _, inst := range watched {
		got, err := e.syncInstallation(ctx, src, inst, byID)
		changes = append(changes, got...)
		if err != nil {
			errs = append(errs, fmt.Errorf("установка %d: %w", inst.InstallationID, err))
		}
	}
	return changes, errors.Join(errs...)
}

func (e *Engine) syncInstallation(ctx context.Context, src InstallationSource, inst domain.GitHubInstallation,
	byID map[int64]github.AppInstallation) ([]InstallationChange, error) {
	id := inst.InstallationID
	remote, ok := byID[id]
	if !ok {
		// Отсутствие в полном списке — повод спросить, а не вывод: список
		// мог сдвинуться между страницами.
		var err error
		remote, err = src.AppInstallation(ctx, id)
		if errors.Is(err, github.ErrNotFound) {
			src.ForgetInstallationToken(id)
			if err := e.setInstallationState(ctx, id, postgres.InstallationRemoved); err != nil {
				return nil, err
			}
			return []InstallationChange{{InstallationID: id, Change: ChangeInstallationRemoved}}, nil
		}
		if err != nil {
			return nil, err
		}
	}

	var changes []InstallationChange
	switch {
	case remote.Suspended() && inst.SuspendedAt == nil:
		if err := e.setInstallationState(ctx, id, postgres.InstallationSuspended); err != nil {
			return nil, err
		}
		changes = append(changes, InstallationChange{InstallationID: id, Change: ChangeInstallationSuspended})
	case !remote.Suspended() && inst.SuspendedAt != nil:
		// Токен, выписанный до приостановки, мог остаться в кэше: после
		// возобновления индексация пойдёт со свежим.
		src.ForgetInstallationToken(id)
		if err := e.setInstallationState(ctx, id, postgres.InstallationUnsuspended); err != nil {
			return nil, err
		}
		changes = append(changes, InstallationChange{InstallationID: id, Change: ChangeInstallationUnsuspended})
	}
	if remote.Suspended() {
		// Репозитории приостановленной установки не читаются, и спрашивать
		// их список незачем: индексация по ним и так стоит. Токен из кэша
		// выбрасывается, чтобы возобновление (вебхуком или опросом) не
		// застало прежний.
		src.ForgetInstallationToken(id)
		return changes, nil
	}

	repoChanges, err := e.syncInstallationRepositories(ctx, src, id)
	return append(changes, repoChanges...), err
}

// syncInstallationRepositories сверяет доступность репозиториев установки
// с её списком у провайдера.
func (e *Engine) syncInstallationRepositories(ctx context.Context, src InstallationSource, id int64) ([]InstallationChange, error) {
	ours, err := e.store.RepositoriesOfInstallation(ctx, e.store.DB(), id)
	if err != nil {
		return nil, err
	}
	if len(ours) == 0 {
		// Установка подключена, но репозиториев на ней нет: список у
		// провайдера сравнивать не с чем.
		return nil, nil
	}
	repos, err := src.InstallationRepositories(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("репозитории установки: %w", err)
	}
	listed := make(map[string]bool, len(repos))
	for _, r := range repos {
		listed[strings.ToLower(r.String())] = true
	}

	// Одноимённые строки разных проектов на одной установке —
	// SetRepositoryAvailability меняет их разом, решение принимается раз на
	// координату.
	var changes []InstallationChange
	var errs []error
	handled := map[string]bool{}
	for _, r := range ours {
		key := strings.ToLower(r.Owner + "/" + r.Name)
		if handled[key] {
			continue
		}
		want := listed[key]
		if !want && r.UnavailableAt == nil {
			// Пропал из списка — перепроверить сам репозиторий: так
			// отличается убранный из установки от переименованного.
			want, err = e.stillInInstallation(ctx, src, id, github.Repo{Owner: r.Owner, Name: r.Name}, listed)
			if err != nil {
				handled[key] = true
				errs = append(errs, fmt.Errorf("репозиторий %s/%s: %w", r.Owner, r.Name, err))
				continue
			}
		}
		var change string
		switch {
		case want && r.UnavailableAt != nil:
			change = ChangeRepositoryAvailable
		case !want && r.UnavailableAt == nil:
			change = ChangeRepositoryUnavailable
		default:
			continue
		}
		if _, err := e.store.SetRepositoryAvailability(ctx, e.store.DB(), id, r.Owner, r.Name, want); err != nil {
			return changes, err
		}
		handled[key] = true
		changes = append(changes, InstallationChange{InstallationID: id, Change: change, Repository: r.Owner + "/" + r.Name})
	}
	return changes, errors.Join(errs...)
}

// stillInInstallation перепроверяет репозиторий, которого нет в списке
// установки. 404 — убран. Ответ с тем же именем — публичный репозиторий
// вне установки: убран, как и сказал бы вебхук. Ответ с другим именем —
// переименован; доступен, если под новым именем он в списке.
func (e *Engine) stillInInstallation(ctx context.Context, src InstallationSource, id int64, repo github.Repo, listed map[string]bool) (bool, error) {
	got, err := src.InstallationRepo(ctx, id, repo)
	if errors.Is(err, github.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	current := strings.ToLower(got.FullName)
	if current == "" || current == strings.ToLower(repo.String()) {
		return false, nil
	}
	return listed[current], nil
}

func (e *Engine) setInstallationState(ctx context.Context, id int64, state string) error {
	_, err := e.store.SetInstallationState(ctx, e.store.DB(), id, state)
	return err
}

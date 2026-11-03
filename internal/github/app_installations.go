package github

// Опрос установок приложения (CLAB-395): вебхуки installation и
// installation_repositories необязательны, и воркер сам сверяет с
// провайдером, жива ли установка и какие репозитории ей отданы.
//
// Сверка меняет состояние только по полному ответу: оборванный список
// выглядит как массовое снятие установок и репозиториев, ровно как
// обрезанный листинг дерева выглядит как массовое удаление файлов. Поэтому
// неполнота здесь — ошибка ErrIncomplete, а не короткий список.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// ErrIncomplete — список у провайдера длиннее, чем удалось прочитать: предел
// страниц или расхождение с total_count. По такому ответу отсутствие в
// списке ничего не значит.
var ErrIncomplete = errors.New("github: список прочитан не полностью")

// AppInstallation — установка приложения, как её видит само приложение.
type AppInstallation struct {
	ID          int64      `json:"id"`
	SuspendedAt *time.Time `json:"suspended_at"`
	Account     struct {
		Login string `json:"login"`
	} `json:"account"`
}

// Suspended — установка приостановлена на стороне GitHub.
func (i AppInstallation) Suspended() bool { return i.SuspendedAt != nil }

const (
	// appListMaxPages — предел листания установок приложения: десять тысяч.
	// У публичного приложения общего инстанса установок больше, чем
	// подключённых, а неполный список сверку не пускает дальше чтения.
	appListMaxPages = 100
	// installationReposMaxPages — предел листания репозиториев установки.
	installationReposMaxPages = 50
	listPageSize              = 100
)

// Installations — все установки приложения, постранично. Неполный список —
// ErrIncomplete: отсутствие установки в нём не доказывает, что её сняли.
func (a *App) Installations(ctx context.Context) ([]AppInstallation, error) {
	var out []AppInstallation
	for page := 1; page <= appListMaxPages; page++ {
		var batch []AppInstallation
		path := "/app/installations?per_page=" + strconv.Itoa(listPageSize) + "&page=" + strconv.Itoa(page)
		if err := a.appGet(ctx, path, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < listPageSize {
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: установок приложения больше %d", ErrIncomplete, appListMaxPages*listPageSize)
}

// Installation — одна установка по идентификатору. ErrNotFound — установки у
// приложения нет: так подтверждается снятие, когда её не оказалось в списке.
func (a *App) Installation(ctx context.Context, installationID int64) (AppInstallation, error) {
	var out AppInstallation
	err := a.appGet(ctx, "/app/installations/"+strconv.FormatInt(installationID, 10), &out)
	return out, err
}

// ForgetToken выбрасывает токен установки из кэша. Его зовёт сверка для
// снятых и приостановленных установок и после отказа чтения репозиториев:
// токен, выписанный до приостановки, иначе жил бы в кэше до своего срока, и
// после возобновления индексация час получала бы отказы.
func (a *App) ForgetToken(installationID int64) {
	a.mu.Lock()
	delete(a.tokens, installationID)
	a.mu.Unlock()
}

// appGet — чтение от имени приложения (утверждение JWT). Правило отказов то
// же, что у InstallationToken: 401 перевыписывается один раз после
// измеренной поправки часов, 401 после неё — ErrAppRejected, 404 —
// ErrNotFound. Повторов по лимиту нет: сверка просто не меняет состояние, и
// следующий тик повторит её целиком.
func (a *App) appGet(ctx context.Context, path string, out any) error {
	for attempt := 0; ; attempt++ {
		appJWT, err := a.jwt()
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		req.Header.Set("Authorization", "Bearer "+appJWT)
		resp, err := a.http.Do(req)
		if err != nil {
			return fmt.Errorf("github: запрос %s: %w", path, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("github: запрос %s: %w", path, err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("github: разбор ответа %s: %w", path, err)
			}
			return nil
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrNotFound, path)
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0 &&
			a.observeClock(resp.Header.Get("Date")):
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("%w: github %d на %s: %s%s", ErrAppRejected,
				resp.StatusCode, path, snippet(body), a.clockNote())
		default:
			return fmt.Errorf("github %d на %s: %s", resp.StatusCode, path, snippet(body))
		}
	}
}

// InstallationRepositories — все репозитории, отданные установке, по токену
// установки. Неполный список — ErrIncomplete.
func (c *Client) InstallationRepositories(ctx context.Context) ([]Repo, error) {
	var out []Repo
	for page := 1; page <= installationReposMaxPages; page++ {
		var resp struct {
			TotalCount   int `json:"total_count"`
			Repositories []struct {
				Name  string `json:"name"`
				Owner struct {
					Login string `json:"login"`
				} `json:"owner"`
			} `json:"repositories"`
		}
		path := "/installation/repositories?per_page=" + strconv.Itoa(listPageSize) + "&page=" + strconv.Itoa(page)
		if err := c.get(ctx, path, "", &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Repositories {
			out = append(out, Repo{Owner: r.Owner.Login, Name: r.Name})
		}
		if len(resp.Repositories) < listPageSize {
			// Список мог сдвинуться между страницами: короче total_count —
			// значит, часть репозиториев проехала мимо, и по такому списку
			// снимать никого нельзя.
			if len(out) < resp.TotalCount {
				return nil, fmt.Errorf("%w: прочитано %d репозиториев установки из %d",
					ErrIncomplete, len(out), resp.TotalCount)
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("%w: репозиториев установки больше %d",
		ErrIncomplete, installationReposMaxPages*listPageSize)
}

// ── сверка через Provider ───────────────────────────────────────────────────

// errNoApp — сверку позвали без приложения. Воркер так не делает: задание
// без приложения не заводится.
var errNoApp = errors.New("github: приложение инстанса не настроено")

// AppInstallations — все установки приложения инстанса.
func (p *Provider) AppInstallations(ctx context.Context) ([]AppInstallation, error) {
	if !p.HasApp() {
		return nil, errNoApp
	}
	return p.app.Installations(ctx)
}

// AppInstallation — одна установка приложения инстанса.
func (p *Provider) AppInstallation(ctx context.Context, installationID int64) (AppInstallation, error) {
	if !p.HasApp() {
		return AppInstallation{}, errNoApp
	}
	return p.app.Installation(ctx, installationID)
}

// InstallationRepositories — репозитории установки её же токеном. Отказ
// выбрасывает токен из кэша: следующий проход выпишет новый, а не будет час
// ходить с отозванным.
func (p *Provider) InstallationRepositories(ctx context.Context, installationID int64) ([]Repo, error) {
	client, err := p.installationClient(ctx, installationID)
	if err != nil {
		return nil, err
	}
	repos, err := client.InstallationRepositories(ctx)
	if err != nil && !errors.Is(err, ErrIncomplete) {
		p.app.ForgetToken(installationID)
	}
	return repos, err
}

// InstallationRepo — сведения о репозитории токеном установки. Этим
// перепроверяется репозиторий, которого не оказалось в списке установки:
// переименованный GitHub отдаёт по старому имени переадресацией, и в ответе
// уже новое full_name.
func (p *Provider) InstallationRepo(ctx context.Context, installationID int64, repo Repo) (Repository, error) {
	client, err := p.installationClient(ctx, installationID)
	if err != nil {
		return Repository{}, err
	}
	return client.Repo(ctx, repo)
}

// ForgetInstallationToken — см. App.ForgetToken.
func (p *Provider) ForgetInstallationToken(installationID int64) {
	if p.HasApp() {
		p.app.ForgetToken(installationID)
	}
}

func (p *Provider) installationClient(ctx context.Context, installationID int64) (*Client, error) {
	if !p.HasApp() {
		return nil, errNoApp
	}
	token, err := p.app.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	return p.base.WithToken(token), nil
}

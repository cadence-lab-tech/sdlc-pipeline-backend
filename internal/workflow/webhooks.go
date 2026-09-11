// Вебхуки GitHub.
//
// Обработчик только пишет в базу: push ставит коммит в очередь индексации,
// события PR и проверок помечают привязанные PR как требующие
// пересинхронизации. В сеть за состоянием по-прежнему ходит воркер
// (решение 0020) — вебхук лишь убирает задержку до следующего тика опроса.
// Всё идемпотентно: провайдер повторяет доставку при таймауте.
package workflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// VerifyGitHubSignature проверяет X-Hub-Signature-256.
func VerifyGitHubSignature(secret string, body []byte, header string) bool {
	if secret == "" {
		return false
	}
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// WebhookOutcome — что обработчик сделал; уходит в ответ провайдеру и в лог.
type WebhookOutcome struct {
	Event      string `json:"event"`
	Duplicate  bool   `json:"duplicate,omitempty"`
	Repository string `json:"repository,omitempty"`
	Queued     int    `json:"index_runs_queued,omitempty"`
	Touched    int    `json:"repo_refs_touched,omitempty"`
	Ignored    string `json:"ignored,omitempty"`
}

// ghInstallation — установка приложения, приславшая событие. Ноль означает
// вебхук, настроенный на репозитории руками, а не приложением: такая
// доставка не обслуживается (решение 0034) — на общем инстансе одноимённые
// репозитории разных арендаторов различает только установка.
type ghInstallation struct {
	ID int64 `json:"id"`
}

type ghFullName struct {
	FullName string `json:"full_name"`
}

type ghRepo struct {
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// HandleGitHubEvent разбирает событие и применяет его к базе.
func (e *Engine) HandleGitHubEvent(ctx context.Context, event, deliveryID string, body []byte) (WebhookOutcome, error) {
	out := WebhookOutcome{Event: event}
	if deliveryID == "" {
		return out, apperr.InvalidInput("нет заголовка X-GitHub-Delivery")
	}

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		fresh, err := e.store.RecordDelivery(ctx, tx, "github", deliveryID, event)
		if err != nil {
			return err
		}
		if !fresh {
			out.Duplicate = true
			return nil
		}

		switch event {
		case "ping":
			return nil
		case "push":
			return e.githubPush(ctx, tx, body, &out)
		case "installation":
			return e.githubInstallation(ctx, tx, body, &out)
		case "installation_repositories":
			return e.githubInstallationRepositories(ctx, tx, body, &out)
		case "pull_request":
			return e.githubPullRequest(ctx, tx, body, &out)
		case "check_run", "check_suite", "status":
			return e.githubChecks(ctx, tx, event, body, &out)
		default:
			out.Ignored = "событие не обрабатывается"
			return nil
		}
	})
	return out, err
}

func (e *Engine) githubPush(ctx context.Context, tx pgx.Tx, body []byte, out *WebhookOutcome) error {
	var p struct {
		Ref          string         `json:"ref"`
		After        string         `json:"after"`
		Deleted      bool           `json:"deleted"`
		Repository   ghRepo         `json:"repository"`
		Installation ghInstallation `json:"installation"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return apperr.InvalidInput("тело push не разбирается: %v", err)
	}
	out.Repository = p.Repository.Owner.Login + "/" + p.Repository.Name
	branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	if !ok || p.Deleted || strings.Trim(p.After, "0") == "" {
		out.Ignored = "не пуш в ветку"
		return nil
	}

	if p.Installation.ID == 0 {
		out.Ignored = "доставка без установки приложения не обслуживается"
		return nil
	}
	// Приложение приносит события всех своих установок, и доставка несёт
	// installation.id. Сначала установка, потом координаты (решение 0034):
	// одноимённый репозиторий другого арендатора событием не задевается, а
	// репозиторий без установки событий не получает.
	targets, err := e.store.RepositoriesByCoordinates(ctx, tx, "github",
		p.Repository.Owner.Login, p.Repository.Name, p.Installation.ID)
	if err != nil {
		return err
	}
	for _, t := range targets {
		watched := false
		for _, b := range t.Branches {
			if b == branch {
				watched = true
			}
		}
		if !watched {
			continue
		}
		_, created, err := e.store.EnqueueIndexRun(ctx, tx, postgres.EnqueueIndexRunInput{
			RepositoryID: t.RepositoryID, Branch: branch, CommitSHA: p.After, Trigger: "webhook",
		})
		if err != nil {
			return err
		}
		if created {
			out.Queued++
		}
	}
	if len(targets) == 0 {
		out.Ignored = "репозиторий не привязан этой установкой ни к одному проекту"
	}
	return nil
}

// githubInstallation — жизнь установки на стороне GitHub: снятие,
// приостановка, возобновление. Строка остаётся, индексация останавливается,
// собранный индекс живёт: отзыв по ошибке обычен, а переиндексация с нуля
// стоит денег. Установку, которую никто не подключал, событие не заводит:
// чья она, знает только тот, кто нажал кнопку.
func (e *Engine) githubInstallation(ctx context.Context, tx pgx.Tx, body []byte, out *WebhookOutcome) error {
	var p struct {
		Action       string         `json:"action"`
		Installation ghInstallation `json:"installation"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return apperr.InvalidInput("тело installation не разбирается: %v", err)
	}
	var state string
	switch p.Action {
	case "deleted":
		state = postgres.InstallationRemoved
	case "suspend":
		state = postgres.InstallationSuspended
	case "unsuspend":
		state = postgres.InstallationUnsuspended
	default:
		out.Ignored = "действие " + p.Action + " не обрабатывается"
		return nil
	}
	n, err := e.store.SetInstallationState(ctx, tx, p.Installation.ID, state)
	if err != nil {
		return err
	}
	out.Touched = n
	if n == 0 {
		out.Ignored = "установка не подключена"
	}
	return nil
}

// githubInstallationRepositories — репозиторий убрали из установки или
// вернули. Убранный помечается недоступным: индексация стоит, поиск по
// собранному индексу работает.
func (e *Engine) githubInstallationRepositories(ctx context.Context, tx pgx.Tx, body []byte, out *WebhookOutcome) error {
	var p struct {
		Action       string         `json:"action"`
		Installation ghInstallation `json:"installation"`
		Added        []ghFullName   `json:"repositories_added"`
		Removed      []ghFullName   `json:"repositories_removed"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return apperr.InvalidInput("тело installation_repositories не разбирается: %v", err)
	}
	mark := func(list []ghFullName, available bool) error {
		for _, r := range list {
			owner, name, ok := strings.Cut(r.FullName, "/")
			if !ok {
				continue
			}
			n, err := e.store.SetRepositoryAvailability(ctx, tx, p.Installation.ID, owner, name, available)
			if err != nil {
				return err
			}
			out.Touched += n
		}
		return nil
	}
	if err := mark(p.Removed, false); err != nil {
		return err
	}
	if err := mark(p.Added, true); err != nil {
		return err
	}
	if out.Touched == 0 {
		out.Ignored = "ни один из репозиториев не привязан этой установкой"
	}
	return nil
}

func (e *Engine) githubPullRequest(ctx context.Context, tx pgx.Tx, body []byte, out *WebhookOutcome) error {
	var p struct {
		Action      string `json:"action"`
		Number      int    `json:"number"`
		Repository  ghRepo `json:"repository"`
		PullRequest struct {
			State   string `json:"state"`
			Merged  bool   `json:"merged"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				SHA string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return apperr.InvalidInput("тело pull_request не разбирается: %v", err)
	}
	out.Repository = p.Repository.Owner.Login + "/" + p.Repository.Name
	state := p.PullRequest.State
	if p.PullRequest.Merged {
		state = "merged"
	}
	n, err := e.store.TouchPullRequest(ctx, tx, "github",
		p.Repository.Owner.Login, p.Repository.Name, p.Number,
		state, p.PullRequest.Head.SHA, p.PullRequest.HTMLURL)
	if err != nil {
		return err
	}
	out.Touched = n
	if n == 0 {
		out.Ignored = "PR не привязан ни к одной задаче"
	}
	return nil
}

func (e *Engine) githubChecks(ctx context.Context, tx pgx.Tx, event string, body []byte, out *WebhookOutcome) error {
	var p struct {
		Repository ghRepo `json:"repository"`
		// check_run / check_suite
		CheckRun struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_run"`
		CheckSuite struct {
			HeadSHA string `json:"head_sha"`
		} `json:"check_suite"`
		// status
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return apperr.InvalidInput("тело %s не разбирается: %v", event, err)
	}
	out.Repository = p.Repository.Owner.Login + "/" + p.Repository.Name
	head := p.CheckRun.HeadSHA
	if head == "" {
		head = p.CheckSuite.HeadSHA
	}
	if head == "" {
		head = p.SHA
	}
	n, err := e.store.InvalidateChecks(ctx, tx, "github",
		p.Repository.Owner.Login, p.Repository.Name, head)
	if err != nil {
		return err
	}
	out.Touched = n
	if n == 0 {
		out.Ignored = "коммит не является head ни одного привязанного PR"
	}
	return nil
}

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

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
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
// вебхук, настроенный на репозитории руками, а не приложением.
type ghInstallation struct {
	ID int64 `json:"id"`
}

// installationTargets оставляет репозитории тех проектов, которым событие
// адресовано. Установка ноль (вебхук настроен руками) не фильтрует ничего.
func installationTargets(targets []postgres.IndexTarget, installationID int64) []postgres.IndexTarget {
	if installationID == 0 {
		return targets
	}
	matched := make([]postgres.IndexTarget, 0, len(targets))
	for _, t := range targets {
		if t.InstallationID == nil || *t.InstallationID == installationID {
			matched = append(matched, t)
		}
	}
	return matched
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

	targets, err := e.store.RepositoriesByCoordinates(ctx, tx, "github",
		p.Repository.Owner.Login, p.Repository.Name)
	if err != nil {
		return err
	}
	// Приложение приносит события всех своих установок, и доставка несёт
	// installation.id (решение 0028). Если репозиторий с теми же координатами
	// привязан к нескольким проектам, событие принадлежит тому, чья установка
	// его прислала; проекты без установки остаются на прежнем пути.
	targets = installationTargets(targets, p.Installation.ID)
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
		out.Ignored = "репозиторий не привязан ни к одному проекту"
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

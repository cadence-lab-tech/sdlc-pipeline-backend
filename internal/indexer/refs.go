package indexer

import (
	"context"
	"encoding/json"
	"strconv"
)

// PullRequestState — состояние PR у провайдера.
type PullRequestState struct {
	State  string
	Merged bool
	Draft  bool
	URL    string
	Head   string
}

// CheckRun — одна проверка CI на коммите.
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// ChecksSource — вторая половина провайдера: состояние PR и его проверок.
// Отдельным интерфейсом, потому что индексации она не нужна, а гейту
// external_check не нужна индексация.
type ChecksSource interface {
	PullRequest(ctx context.Context, repo Repo, number int) (PullRequestState, error)
	CheckRuns(ctx context.Context, repo Repo, sha string) ([]CheckRun, error)
}

// SyncRepoRefs обновляет состояние привязанных к задачам PR.
//
// Это то, чем питается гейт external_check: он исполняется внутри транзакции
// перехода и в сеть не ходит, поэтому состояние проверок обязано приезжать
// заранее и лежать в базе.
func (ix *Indexer) SyncRepoRefs(ctx context.Context, limit int) (int, error) {
	source, ok := ix.source.(ChecksSource)
	if !ok {
		return 0, nil
	}

	refs, err := ix.store.RepoRefsToSync(ctx, ix.store.DB(), limit)
	if err != nil || len(refs) == 0 {
		return 0, err
	}

	synced := 0
	for _, ref := range refs {
		number, err := strconv.Atoi(ref.Ref)
		if err != nil {
			// Ссылка не номером PR — синхронизировать нечего.
			continue
		}
		repo := Repo{ref.Owner, ref.Name}

		pr, err := source.PullRequest(ctx, repo, number)
		if err != nil {
			ix.log.Warn("состояние PR не прочитано",
				"task", ref.TaskKey, "repository", repo.String(), "pr", number, "error", err)
			continue
		}

		state := pr.State
		if pr.Merged {
			state = "merged"
		}

		snapshot := map[string]any{"runs": []CheckRun{}}
		if pr.Head != "" {
			runs, err := source.CheckRuns(ctx, repo, pr.Head)
			if err != nil {
				ix.log.Warn("проверки PR не прочитаны",
					"task", ref.TaskKey, "pr", number, "error", err)
			} else {
				snapshot["runs"] = runs
				snapshot["conclusion"] = conclude(runs)
			}
		}
		payload, err := json.Marshal(snapshot)
		if err != nil {
			return synced, err
		}

		if err := ix.store.UpdateRepoRefChecks(ctx, ix.store.DB(),
			ref.ID, state, pr.Head, pr.URL, payload); err != nil {
			return synced, err
		}
		synced++
	}
	return synced, nil
}

// conclude сводит проверки коммита к одному заключению.
//
// Пустое — проверки ещё идут: это не отрицательный вердикт, и гейт обязан
// отличать «не прошло» от «ещё не известно».
func conclude(runs []CheckRun) string {
	if len(runs) == 0 {
		return "success"
	}
	worst := ""
	for _, r := range runs {
		if r.Status != "completed" {
			return ""
		}
		switch r.Conclusion {
		case "failure", "timed_out", "cancelled", "action_required", "stale":
			return r.Conclusion
		case "neutral", "skipped":
			if worst == "" {
				worst = "success"
			}
		case "success":
			if worst == "" {
				worst = "success"
			}
		default:
			return r.Conclusion
		}
	}
	if worst == "" {
		worst = "success"
	}
	return worst
}

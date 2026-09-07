package workflow

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

type LinkRepoRefInput struct {
	// Ссылка на репозиторий проекта: owner/name, name или role_hint.
	Repository string
	RefType    string
	Ref        string
	URL        string
	State      string
}

// LinkRepoRef привязывает к задаче PR, ветку или коммит.
//
// Сам код изменений живёт в PR, а не в артефакте (решение 0005): гейт
// repo_ref_required проверяет именно эту привязку.
func (e *Engine) LinkRepoRef(ctx context.Context, taskKey string, actor uuid.UUID, in LinkRepoRefInput) (postgres.TaskRepoRef, error) {
	var out postgres.TaskRepoRef

	if in.RefType == "" {
		in.RefType = "pull_request"
	}
	if in.Ref == "" {
		return out, apperr.InvalidInput("не указан ref: номер PR, имя ветки или коммит")
	}

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		repo, err := e.store.FindRepository(ctx, tx, task.ProjectID, in.Repository)
		if err != nil {
			return err
		}

		out, err = e.store.LinkRepoRef(ctx, tx, postgres.LinkRepoRefInput{
			Task: task, RepositoryID: repo.ID, RefType: in.RefType,
			Ref: in.Ref, URL: in.URL, State: in.State,
		})
		if err != nil {
			return err
		}

		var runID *uuid.UUID
		if run, err := e.store.CurrentStageRun(ctx, tx, task.ID, task.StatusID); err == nil {
			runID = &run.ID
		}
		return e.store.AppendEvent(ctx, tx, task.ID, runID, domain.EventRepoRefLinked, actor,
			map[string]any{
				"repository": out.Repository, "ref_type": out.RefType,
				"ref": out.Ref, "state": in.State,
			})
	})

	return out, err
}

type ResolveDriftInput struct {
	SignalID uuid.UUID
	// "resolved" — разобрано, выводы этапа в силе; "dismissed" — сигнал
	// нерелевантен. Если разбор показал, что выводы сломались, сигнал не
	// резолвят: задача уходит назад обычным reject-переходом.
	Resolution string
	Note       string
	// Разбор дрейфа сам является артефактом задачи.
	Artifact *SubmitArtifactInput
}

func (e *Engine) ResolveDrift(ctx context.Context, actor uuid.UUID, in ResolveDriftInput) (postgres.DriftSignal, error) {
	var out postgres.DriftSignal

	switch in.Resolution {
	case "resolved", "dismissed":
	default:
		return out, apperr.InvalidInput(
			"разрешение сигнала — resolved или dismissed, а не %q", in.Resolution)
	}

	signal, err := e.store.GetDriftSignal(ctx, e.store.DB(), in.SignalID)
	if err != nil {
		return out, err
	}
	task, err := e.store.GetTaskByID(ctx, e.store.DB(), signal.TaskID)
	if err != nil {
		return out, err
	}

	var artifactID *uuid.UUID
	if in.Artifact != nil {
		a, err := e.SubmitArtifact(ctx, task.Key, actor, *in.Artifact)
		if err != nil {
			return out, err
		}
		artifactID = &a.ID
	}

	err = e.store.InTx(ctx, func(tx pgx.Tx) error {
		if err := e.store.ResolveDriftSignal(ctx, tx, in.SignalID, actor,
			in.Resolution, in.Note, artifactID); err != nil {
			return err
		}
		out, err = e.store.GetDriftSignal(ctx, tx, in.SignalID)
		if err != nil {
			return err
		}
		return e.store.AppendEvent(ctx, tx, task.ID, nil, domain.EventDriftResolved, actor,
			map[string]any{
				"signal_id": in.SignalID.String(), "resolution": in.Resolution,
				"note": in.Note, "kind": signal.Kind, "severity": signal.Severity,
			})
	})

	return out, err
}

// Drift — открытые расхождения задачи вместе с её базовыми линиями.
type Drift struct {
	Baselines []postgres.TaskBaseline `json:"baselines"`
	Signals   []postgres.DriftSignal  `json:"signals"`
}

func (e *Engine) Drift(ctx context.Context, taskKey string, states []string) (Drift, error) {
	var out Drift
	q := e.store.DB()

	task, err := e.store.GetTaskByKey(ctx, q, taskKey)
	if err != nil {
		return out, err
	}
	if out.Baselines, err = e.store.ListBaselines(ctx, q, task.ID); err != nil {
		return out, err
	}
	out.Signals, err = e.store.ListDriftSignals(ctx, q, task.ID, states)
	return out, err
}

// CheckGates — вердикт гейтов конкретного перехода без побочных эффектов.
func (e *Engine) CheckGates(ctx context.Context, taskKey, transitionKey string) (TransitionPreview, error) {
	task, err := e.store.GetTaskByKey(ctx, e.store.DB(), taskKey)
	if err != nil {
		return TransitionPreview{}, err
	}
	previews, err := e.Preview(ctx, task)
	if err != nil {
		return TransitionPreview{}, err
	}
	for _, p := range previews {
		if p.Key == transitionKey {
			return p, nil
		}
	}
	return TransitionPreview{}, apperr.New(apperr.CodeTransitionNotAllowed,
		"перехода %q нет из статуса %q", transitionKey, task.StatusKey)
}

package workflow

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Перемещение задачи в бэклоге (решение 0075, CLAB-399). Ранг — место в
// группе (верхний уровень проекта или дети одного родителя); человек ставит
// задачу рядом с соседом или к краю группы, сервер считает ключ. REST
// (POST /v1/tasks/:key/rank) и MCP зовут одно и то же: ParseRankPlace,
// Auth.RequireRankCircle, Engine.RankTask.

// Края группы в теле перемещения.
const (
	RankTop    = "top"
	RankBottom = "bottom"
)

// RankPlace — куда поставить задачу: ровно одно из полей. Before: K — перед
// K (K станет соседом ниже), After: K — после K (K станет соседом выше),
// Position — к краю группы.
type RankPlace struct {
	Before   string
	After    string
	Position string
}

// ParseRankPlace проверяет тело перемещения: ровно одно место, край — top
// или bottom. Проверка идёт до прав токена задачи и до базы.
func ParseRankPlace(before, after, position string) (RankPlace, error) {
	set := 0
	for _, v := range []string{before, after, position} {
		if v != "" {
			set++
		}
	}
	if set != 1 || (position != "" && position != RankTop && position != RankBottom) {
		return RankPlace{}, apperr.InvalidInput("укажите ровно одно из before, after, position")
	}
	return RankPlace{Before: before, After: after, Position: position}, nil
}

func (p RankPlace) neighbor() string {
	if p.Before != "" {
		return p.Before
	}
	return p.After
}

// RankResult — ответ перемещения и нагрузка события task.ranked. Above и
// Below — ближайшие незакрытые соседи после перемещения: Above — выше,
// Below — ниже, nil на краю. Changed=false — задача уже стояла на этом
// месте: ранг и лента не менялись.
type RankResult struct {
	Task    domain.Task `json:"task"`
	Changed bool        `json:"changed"`
	Parent  *string     `json:"parent"`
	Above   *string     `json:"above"`
	Below   *string     `json:"below"`
}

// rankedPayload — нагрузка события task.ranked: те же соседи, что в ответе.
type rankedPayload struct {
	Parent *string `json:"parent"`
	Above  *string `json:"above"`
	Below  *string `json:"below"`
}

// RankTask ставит задачу на место в её группе бэклога.
//
// Всё — под блокировкой группы (pg_advisory_xact_lock): перемещения одной
// группы идут по очереди, каждое — относительно названного соседа в
// состоянии, которое видно после блокировки (сосед мог закрыться, пока вызов
// ждал очереди). Порядок отказов — как в решении 0075: сосед не найден (404),
// сосед — сама задача (400), сосед в другой группе (400), задача закрыта
// (409), сосед закрыт (409). Перемещение пишет только rank задачи (или
// перераздаёт группу, если ключ вышел длиннее порога) и событие task.ranked;
// updated_at и last_moved_at не меняются.
func (e *Engine) RankTask(ctx context.Context, taskKey string, place RankPlace, actor uuid.UUID) (RankResult, error) {
	var out RankResult
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.GetTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		if err := e.store.LockRankGroup(ctx, tx, task.ProjectID, task.ParentID); err != nil {
			return err
		}
		// Под блокировкой — заново: статус и группа могли измениться, пока
		// вызов ждал очереди.
		if task, err = e.store.GetTaskByKey(ctx, tx, taskKey); err != nil {
			return err
		}

		var neighbor *domain.Task
		if k := place.neighbor(); k != "" {
			n, err := e.store.GetTaskByKey(ctx, tx, k)
			// Нет такой задачи, ключ не ключ или задача чужого проекта —
			// для вызывающего одно: соседа в этом проекте нет.
			if ae := apperr.As(err); err != nil && (ae == nil ||
				(ae.Code != apperr.CodeNotFound && ae.Code != apperr.CodeInvalidInput)) {
				return err
			}
			if err != nil || n.ProjectID != task.ProjectID {
				return apperr.NotFound("задачи %s в проекте %s нет", k, task.ProjectKey)
			}
			if n.ID == task.ID {
				return apperr.InvalidInput("задачу нельзя поставить рядом с ней самой")
			}
			if !sameParent(n.ParentID, task.ParentID) {
				return apperr.InvalidInput("%s и %s в разных группах бэклога: ставить можно рядом с задачей "+
					"того же эпика или, на верхнем уровне, рядом с задачей верхнего уровня", task.Key, n.Key)
			}
			neighbor = &n
		}

		group, err := e.store.RankGroup(ctx, tx, task.ProjectID, task.ParentID)
		if err != nil {
			return err
		}
		self := slices.IndexFunc(group, func(r postgres.RankEntry) bool { return r.ID == task.ID })
		if self < 0 {
			return apperr.Internal(fmt.Errorf("задачи %s нет в своей группе", task.Key), "группа бэклога")
		}
		if group[self].Terminal {
			return apperr.Conflict("%s закрыта (%s): закрытую задачу не двигают", task.Key, group[self].StatusName)
		}
		if neighbor != nil {
			at := slices.IndexFunc(group, func(r postgres.RankEntry) bool { return r.ID == neighbor.ID })
			if at < 0 {
				return apperr.Internal(fmt.Errorf("задачи %s нет в группе %s", neighbor.Key, task.Key), "группа бэклога")
			}
			if group[at].Terminal {
				return apperr.Conflict("%s закрыта (%s): рядом с закрытой задачей не ставят",
					neighbor.Key, group[at].StatusName)
			}
		}

		out.Parent = task.ParentKey
		if samePlace(group, self, place) {
			out.Task = task
			out.Above, out.Below = openNeighbors(group, task.ID)
			return nil
		}

		rest := slices.Delete(slices.Clone(group), self, self+1)
		at := rankInsertAt(rest, place)
		moved := slices.Insert(slices.Clone(rest), at, group[self])
		if err := e.writeRank(ctx, tx, rest, at, moved, task.ID); err != nil {
			return err
		}
		out.Changed = true
		out.Above, out.Below = openNeighbors(moved, task.ID)
		if err := e.store.AppendEvent(ctx, tx, task.ID, nil, domain.EventTaskRanked, actor,
			rankedPayload{Parent: out.Parent, Above: out.Above, Below: out.Below}); err != nil {
			return err
		}
		out.Task, err = e.store.GetTaskByKey(ctx, tx, taskKey)
		return err
	})
	return out, err
}

func sameParent(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// openOrder — индексы незакрытых задач группы: «то же место» и соседи
// события считаются по ним, закрытая между двумя место не меняет.
func openOrder(group []postgres.RankEntry) []int {
	out := []int{}
	for i, r := range group {
		if !r.Terminal {
			out = append(out, i)
		}
	}
	return out
}

// samePlace — задача уже стоит там, куда её ставят: before: K — ближайшая
// незакрытая ниже уже K, after: K — выше уже K, top и bottom — первая и
// последняя среди незакрытых.
func samePlace(group []postgres.RankEntry, self int, place RankPlace) bool {
	open := openOrder(group)
	pos := slices.Index(open, self)
	switch {
	case place.Position == RankTop:
		return pos == 0
	case place.Position == RankBottom:
		return pos == len(open)-1
	case place.Before != "":
		return pos+1 < len(open) && group[open[pos+1]].Key == place.Before
	default:
		return pos > 0 && group[open[pos-1]].Key == place.After
	}
}

// rankInsertAt — индекс в группе без задачи, перед которым она встанет.
// Ключ строится по соседям в полном порядке группы (закрытые тоже): так он
// не совпадёт с чужим.
func rankInsertAt(rest []postgres.RankEntry, place RankPlace) int {
	switch {
	case place.Position == RankTop:
		return 0
	case place.Position == RankBottom:
		return len(rest)
	}
	at := slices.IndexFunc(rest, func(r postgres.RankEntry) bool { return r.Key == place.neighbor() })
	if place.After != "" {
		at++
	}
	return at
}

// writeRank пишет ключ задачи между соседями по месту at. Ключ не строится
// (у соседей равные ранги — так бывает после ручной вставки) или вышел
// длиннее порога — группа перераздаётся равномерным набором в новом
// порядке: соседям меняется только rank, событий у них нет.
func (e *Engine) writeRank(ctx context.Context, tx pgx.Tx, rest []postgres.RankEntry, at int, moved []postgres.RankEntry, taskID uuid.UUID) error {
	var lo, hi string
	if at > 0 {
		lo = rest[at-1].Rank
	}
	if at < len(rest) {
		hi = rest[at].Rank
	}
	if k, err := domain.RankBetween(lo, hi); err == nil && len(k) <= domain.RankMaxLen {
		return e.store.SetTaskRanks(ctx, tx, []uuid.UUID{taskID}, []string{k})
	}
	keys := domain.RankSpread(len(moved))
	ids := make([]uuid.UUID, len(moved))
	for i, r := range moved {
		ids[i] = r.ID
	}
	return e.store.SetTaskRanks(ctx, tx, ids, keys)
}

// openNeighbors — ближайшие незакрытые соседи задачи в порядке group.
func openNeighbors(group []postgres.RankEntry, taskID uuid.UUID) (above, below *string) {
	open := openOrder(group)
	for i, gi := range open {
		if group[gi].ID != taskID {
			continue
		}
		if i > 0 {
			k := group[open[i-1]].Key
			above = &k
		}
		if i+1 < len(open) {
			k := group[open[i+1]].Key
			below = &k
		}
	}
	return above, below
}

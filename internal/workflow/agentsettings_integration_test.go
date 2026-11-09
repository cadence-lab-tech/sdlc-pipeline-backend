package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Настройка агентов по настоящей схеме (CLAB-425): снимок версии при
// захвате и перезахвате, перепроверка, ответ захвата, правка только
// черновика, откат активации, гонки. Номера — критерии приёмки CLAB-425.

func (w levelWorld) human(t *testing.T) Actor {
	t.Helper()
	p, err := w.store.ResolveActor(context.Background(), w.store.DB(), "alice", domain.PrincipalUser)
	if err != nil {
		t.Fatal(err)
	}
	return Actor{Principal: p, Level: domain.MemberMaintainer}
}

// activate заводит черновик с документом и активирует его; отдаёт номер.
func (w levelWorld) activate(t *testing.T, who Actor, doc string) int {
	t.Helper()
	ctx := context.Background()
	v, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsDraftInput{Document: json.RawMessage(doc)})
	if err != nil {
		t.Fatalf("черновик: %v", err)
	}
	if _, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", who, &v.Version); err != nil {
		t.Fatalf("активация %d: %v", v.Version, err)
	}
	return v.Version
}

// runVersion — номер версии-снимка на строке прогона; 0 — NULL.
func (w levelWorld) runVersion(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	var v *int
	if err := w.store.DB().QueryRow(context.Background(), `
		SELECT asv.version FROM stage_runs sr
		LEFT JOIN agent_settings_versions asv ON asv.id = sr.agent_settings_version_id
		WHERE sr.id = $1`, runID).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v == nil {
		return 0
	}
	return *v
}

func execVersion(e Execution) int {
	if e.AgentSettingsVersion == nil {
		return 0
	}
	return *e.AgentSettingsVersion
}

func modelsJSON(e Execution) string {
	if e.Models == nil {
		return "-"
	}
	raw, _ := json.Marshal(*e.Models)
	return string(raw)
}

func docWith(high string) string {
	return `{"agents": {"claude": {"levels": {"high": {"model": "` + high + `", "effort": "high"}}}, "codex": {"levels": {"low": {"model": "gpt-low"}}}}}`
}

// AC-15, AC-16, AC-18: снимок пишется с парой уровня на новой аренде, не
// плывёт от подтверждения, heartbeat и активации, переписывается release,
// протуханием и захватом другим.
func TestAgentSettingsSnapshotOnClaim(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)
	who := w.human(t)

	// Без настройки: снимок NULL, версия null, моделей нет.
	bare := w.create(t, "без настройки", str("high"), "")
	w.start(t, bare.Key)
	res := w.claim(t, w.pool, bare.Key)
	if execVersion(res.Execution) != 0 || res.Execution.Models != nil || w.runVersion(t, res.StageRunID) != 0 {
		t.Fatalf("без настройки: %+v", res.Execution)
	}
	raw, _ := json.Marshal(res.Execution)
	if string(raw) != `{"model_level":"high","model_level_source":"task","agent_settings_version":null}` {
		t.Fatalf("execution без настройки: %s", raw)
	}

	v1 := w.activate(t, who, docWith("opus-1"))
	task := w.create(t, "с настройкой", str("high"), "")
	w.start(t, task.Key)
	// До захвата — то, что даст захват по текущей активной.
	state, err := w.engine.State(ctx, task.Key)
	if err != nil || execVersion(state.Execution) != v1 {
		t.Fatalf("до захвата: %+v %v", state.Execution, err)
	}
	res = w.claim(t, w.pool, task.Key)
	if execVersion(res.Execution) != v1 || w.runVersion(t, res.StageRunID) != v1 ||
		modelsJSON(res.Execution) != `{"claude":{"model":"opus-1","effort":"high"}}` {
		t.Fatalf("захват при v%d: %d %s, снимок %d", v1, execVersion(res.Execution), modelsJSON(res.Execution), w.runVersion(t, res.StageRunID))
	}

	// Активация v2 идущий прогон не трогает: подтверждение, heartbeat, state.
	v2 := w.activate(t, who, docWith("opus-2"))
	if again := w.claim(t, w.pool, task.Key); execVersion(again.Execution) != v1 || modelsJSON(again.Execution) != `{"claude":{"model":"opus-1","effort":"high"}}` {
		t.Fatalf("подтверждение аренды: %d %s", execVersion(again.Execution), modelsJSON(again.Execution))
	}
	if _, err := w.engine.Heartbeat(ctx, res.StageRunID, w.pool.ID); err != nil {
		t.Fatal(err)
	}
	if state, _ := w.engine.State(ctx, task.Key); execVersion(state.Execution) != v1 || w.runVersion(t, res.StageRunID) != v1 {
		t.Fatalf("после heartbeat: %d, снимок %d", execVersion(state.Execution), w.runVersion(t, res.StageRunID))
	}

	// release и новый захват — новая версия.
	if err := w.engine.Release(ctx, res.StageRunID, w.pool.ID, ""); err != nil {
		t.Fatal(err)
	}
	if again := w.claim(t, w.pool, task.Key); execVersion(again.Execution) != v2 || w.runVersion(t, res.StageRunID) != v2 {
		t.Fatalf("после release: %d", execVersion(again.Execution))
	}

	// Протухшая аренда, захват другим — следующая версия.
	v3 := w.activate(t, who, docWith("opus-3"))
	if _, err := w.store.DB().Exec(ctx, `UPDATE stage_runs SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, res.StageRunID); err != nil {
		t.Fatal(err)
	}
	if again := w.claim(t, w.other, task.Key); execVersion(again.Execution) != v3 || w.runVersion(t, res.StageRunID) != v3 {
		t.Fatalf("после протухания: %d", execVersion(again.Execution))
	}

	// Карточка: stage_runs[] несут номер снимка.
	runs, err := w.store.ListStageRuns(ctx, w.store.DB(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.ID == res.StageRunID && (r.AgentSettingsVersion == nil || *r.AgentSettingsVersion != v3) {
			t.Fatalf("stage_runs[]: %v", r.AgentSettingsVersion)
		}
	}

	// Закрытый прогон не переписывается прямым условием finished_at IS NULL.
	if _, err := w.engine.Execute(ctx, task.Key, "cancel", w.system.ID, ""); err != nil {
		t.Fatal(err)
	}
	active, err := w.store.ActiveAgentSettingsID(ctx, w.store.DB(), task.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.store.InTx(ctx, func(tx pgx.Tx) error {
		return w.store.SnapshotExecution(ctx, tx, res.StageRunID, str("low"), "task", nil)
	}); err != nil {
		t.Fatal(err)
	}
	if w.runVersion(t, res.StageRunID) != v3 || active == nil {
		t.Fatalf("закрытый прогон переписан: %d", w.runVersion(t, res.StageRunID))
	}

	// Уровня нет — версия есть, моделей нет; уровень без записи — {}.
	noLevel := w.create(t, "без уровня", nil, "")
	w.start(t, noLevel.Key)
	if got := w.claim(t, w.pool, noLevel.Key); execVersion(got.Execution) != v3 || got.Execution.Models != nil {
		t.Fatalf("без уровня: %d %s", execVersion(got.Execution), modelsJSON(got.Execution))
	}
	medium := w.create(t, "уровень без записи", str("medium"), "")
	w.start(t, medium.Key)
	if got := w.claim(t, w.pool, medium.Key); modelsJSON(got.Execution) != `{}` {
		t.Fatalf("уровень без записи: %s", modelsJSON(got.Execution))
	} else if raw, _ := json.Marshal(got.Execution); string(raw) != `{"model_level":"medium","model_level_source":"task","agent_settings_version":`+strconv.Itoa(v3)+`,"models":{}}` {
		t.Fatalf("execution с пустыми моделями: %s", raw)
	}
}

// AC-17: перепроверка получает собственный снимок, у родителя — прежний.
func TestAgentSettingsReviewSnapshot(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)
	who := w.human(t)
	v1 := w.activate(t, who, docWith("opus-1"))

	task := w.create(t, "перепроверка", str("high"), "")
	w.start(t, task.Key)
	parent := w.claim(t, w.pool, task.Key)
	for _, kind := range []string{"analysis.brief", "analysis.acceptance_criteria"} {
		if _, err := w.engine.SubmitArtifact(ctx, task.Key, w.pool.ID, SubmitArtifactInput{
			Kind: kind, Title: kind, Content: []byte("# " + kind),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := w.engine.Execute(ctx, task.Key, "analysis_done", w.pool.ID, ""); err != nil || !res.Blocked {
		t.Fatalf("до ревью: %v %+v", err, res)
	}
	v2 := w.activate(t, who, docWith("opus-2"))
	review := w.claim(t, w.other, task.Key)
	if review.Kind != domain.StageKindReview {
		t.Fatalf("захвачен не прогон ревью: %s", review.Kind)
	}
	if execVersion(review.Execution) != v2 || w.runVersion(t, review.StageRunID) != v2 {
		t.Fatalf("перепроверка: %d, снимок %d, ожидали %d", execVersion(review.Execution), w.runVersion(t, review.StageRunID), v2)
	}
	if w.runVersion(t, parent.StageRunID) != v1 {
		t.Fatalf("родитель переписан: %d", w.runVersion(t, parent.StageRunID))
	}
}

// AC-4: сервисная запись документа в active или retired — 409, документ не
// меняется. AC-5: активация повторяет проверку — документ, записанный в
// обход, не активируется. AC-7: сбой посреди активации откатывает и снятие.
func TestAgentSettingsWriteGuards(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)
	who := w.human(t)
	v1 := w.activate(t, who, docWith("opus-1"))
	v2 := w.activate(t, who, docWith("opus-2"))
	project, err := w.store.GetProjectByKey(ctx, w.store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{v1, v2} {
		v, err := w.store.AgentSettingsByVersion(ctx, w.store.DB(), project.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		err = w.store.InTx(ctx, func(tx pgx.Tx) error {
			_, err := w.engine.ReplaceAgentSettingsVersion(ctx, tx, v.ID, json.RawMessage(`{}`), nil)
			return err
		})
		if apperr.As(err).Code != apperr.CodeConflict {
			t.Fatalf("запись в %s: %v", v.State, err)
		}
		after, _ := w.store.AgentSettingsByVersion(ctx, w.store.DB(), project.ID, n)
		if string(after.Document) == "{}" {
			t.Fatalf("документ %s изменён", v.State)
		}
	}

	states := func() map[int]string {
		list, err := w.store.ListAgentSettings(ctx, w.store.DB(), project.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int]string{}
		for _, v := range list {
			out[v.Version] = v.State
		}
		return out
	}

	// Невалидный документ в обход сервиса: активация — 400, состояние то же.
	draft, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsDraftInput{From: AgentSettingsFromEmpty})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.store.DB().Exec(ctx, `UPDATE agent_settings_versions SET document = '{"foo": 1}' WHERE project_id = $1 AND version = $2`,
		project.ID, draft.Version); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", who, nil); apperr.As(err).Code != apperr.CodeInvalidInput {
		t.Fatalf("активация невалидного: %v", err)
	}
	if got := states(); got[v2] != domain.AgentSettingsActive || got[draft.Version] != domain.AgentSettingsDraft {
		t.Fatalf("после отказа: %v", got)
	}

	// Внедрённый сбой после снятия прежней: всё на месте.
	if _, _, err := w.engine.PutAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsPutInput{Document: json.RawMessage(docWith("opus-3"))}); err != nil {
		t.Fatal(err)
	}
	w.engine.agentSettingsFault = func() error { return errors.New("сбой посреди активации") }
	if _, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", who, nil); err == nil {
		t.Fatal("сбой не дошёл")
	}
	w.engine.agentSettingsFault = nil
	if got := states(); got[v2] != domain.AgentSettingsActive || got[draft.Version] != domain.AgentSettingsDraft || got[v1] != domain.AgentSettingsRetired {
		t.Fatalf("после сбоя: %v", got)
	}
	out, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", who, &draft.Version)
	if err != nil || out.Retired == nil || *out.Retired != v2 || out.Activated.Version != draft.Version {
		t.Fatalf("активация после сбоя: %+v %v", out, err)
	}
}

// AC-22: гонки держит схема и транзакции.
func TestAgentSettingsRaces(t *testing.T) {
	ctx := context.Background()
	w := newLevelWorld(t)
	who := w.human(t)
	project, err := w.store.GetProjectByKey(ctx, w.store.DB(), "SDLC")
	if err != nil {
		t.Fatal(err)
	}
	counts := func() (active, drafts, retired int) {
		if err := w.store.DB().QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE state = 'active'), count(*) FILTER (WHERE state = 'draft'),
			       count(*) FILTER (WHERE state = 'retired')
			FROM agent_settings_versions WHERE project_id = $1`, project.ID).Scan(&active, &drafts, &retired); err != nil {
			t.Fatal(err)
		}
		return
	}
	parallel := func(n int, fn func() error) []error {
		var wg sync.WaitGroup
		errs := make([]error, n)
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = fn()
			}()
		}
		close(start)
		wg.Wait()
		return errs
	}

	// Два заведения — один черновик, второй conflict.
	errs := parallel(2, func() error {
		_, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsDraftInput{Document: json.RawMessage(docWith("a"))})
		return err
	})
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("два заведения: %v", errs)
	}
	for _, err := range errs {
		if err != nil && apperr.As(err).Code != apperr.CodeConflict {
			t.Fatalf("второе заведение: %v", err)
		}
	}
	if _, drafts, _ := counts(); drafts != 1 {
		t.Fatalf("черновиков %d", drafts)
	}

	// Две активации одного черновика — один успех, без второй retired.
	if _, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", who, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsDraftInput{Document: json.RawMessage(docWith("b"))}); err != nil {
		t.Fatal(err)
	}
	errs = parallel(2, func() error {
		_, _, err := w.engine.ActivateAgentSettings(ctx, "SDLC", who, nil)
		return err
	})
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Fatalf("две активации: %v", errs)
	}
	if active, drafts, retired := counts(); active != 1 || drafts != 0 || retired != 1 {
		t.Fatalf("после двух активаций: active %d draft %d retired %d", active, drafts, retired)
	}

	// Активация одновременно с захватом: снимок — старая или новая версия.
	for i := range 8 {
		before, err := w.store.ActiveAgentSettingsID(ctx, w.store.DB(), project.ID)
		if err != nil || before == nil {
			t.Fatal(err)
		}
		draft, _, err := w.engine.CreateAgentSettingsDraft(ctx, "SDLC", who, AgentSettingsDraftInput{Document: json.RawMessage(docWith("c"))})
		if err != nil {
			t.Fatal(err)
		}
		task := w.create(t, "гонка "+time.Now().Format(time.RFC3339Nano), str("high"), "")
		w.start(t, task.Key)
		var runID uuid.UUID
		var claimErr, activateErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			res, err := w.engine.ClaimWith(ctx, task.Key, w.pool.ID, "race", ClaimOptions{})
			runID, claimErr = res.StageRunID, err
		}()
		go func() {
			defer wg.Done()
			_, _, activateErr = w.engine.ActivateAgentSettings(ctx, "SDLC", who, &draft.Version)
		}()
		wg.Wait()
		if claimErr != nil || activateErr != nil {
			t.Fatalf("итерация %d: %v %v", i, claimErr, activateErr)
		}
		var snap *uuid.UUID
		if err := w.store.DB().QueryRow(ctx, `SELECT agent_settings_version_id FROM stage_runs WHERE id = $1`, runID).Scan(&snap); err != nil {
			t.Fatal(err)
		}
		newID, _ := w.store.ActiveAgentSettingsID(ctx, w.store.DB(), project.ID)
		if snap == nil || (*snap != *before && *snap != *newID) {
			t.Fatalf("итерация %d: снимок %v, старая %v, новая %v", i, snap, before, newID)
		}
		if active, drafts, _ := counts(); active != 1 || drafts != 0 {
			t.Fatalf("итерация %d: active %d draft %d", i, active, drafts)
		}
	}
}

package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

type SubmitArtifactInput struct {
	Kind         string
	Title        string
	Format       string
	Content      []byte
	MediaType    string
	SupersedesID *uuid.UUID
	Meta         json.RawMessage
	Links        []postgres.ArtifactLink
	CodeRefs     []postgres.ArtifactCodeRef
	// Прогон, к которому крепится результат. Пусто — движок определит его
	// сам: прогон перепроверки, если агент держит его, иначе текущий этап.
	StageRunID *uuid.UUID
	// Файл по контракту (решение 0064): принят до транзакции, Content не
	// читается. FileName и MediaType — уже санитизированные транспортом.
	File     *postgres.SpooledBlob
	FileName string
}

// SubmitArtifact прикрепляет результат работы к попытке этапа.
//
// Привязка именно к stage_run, а не к задаче, — то, на чём стоит гейт
// artifacts_required: после отката задача заходит на статус заново, и
// артефакты прошлой попытки не должны закрывать гейт вместо новой работы.
func (e *Engine) SubmitArtifact(ctx context.Context, taskKey string, actor uuid.UUID, in SubmitArtifactInput) (domain.Artifact, error) {
	var out domain.Artifact

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		run, err := e.workRun(ctx, tx, task, actor, in.StageRunID)
		if err != nil {
			return err
		}
		if run.FinishedAt != nil {
			return apperr.Conflict("этап уже завершён, артефакт прикреплять некуда")
		}
		schema, err := e.checkContract(ctx, tx, run, in.Kind)
		if err != nil {
			return err
		}
		if in.File != nil {
			if err := fileForbiddenBySchema(in.Kind, schema); err != nil {
				return err
			}
		} else if err := checkArtifactSchema(in.Kind, schema, in.Content); err != nil {
			return err
		}
		// Повторная сдача того же kind — это новая версия, а не второй
		// «актуальный» артефакт: иначе гейты и контекст следующего этапа
		// видели бы две версии одного документа и выбирали бы случайную.
		if in.SupersedesID == nil {
			prev, err := e.store.LatestArtifactOfKind(ctx, tx, task.ID, in.Kind)
			if err != nil {
				return err
			}
			in.SupersedesID = prev
		}

		out, err = e.store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task:         task,
			StageRunID:   &run.ID,
			Kind:         in.Kind,
			Title:        in.Title,
			Format:       in.Format,
			Content:      in.Content,
			MediaType:    in.MediaType,
			Blob:         in.File,
			FileName:     in.FileName,
			SupersedesID: in.SupersedesID,
			Status:       domain.ArtifactSubmitted,
			Meta:         in.Meta,
			Links:        in.Links,
			CodeRefs:     in.CodeRefs,
			ProducedBy:   actor,
		})
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE stage_runs SET state = 'submitted'
			WHERE id = $1 AND state IN ('pending', 'claimed', 'in_progress')`, run.ID); err != nil {
			return err
		}

		if err := e.store.AppendEvent(ctx, tx, task.ID, &run.ID, domain.EventArtifactSubmitted, actor,
			map[string]any{
				"artifact_id": out.ID.String(),
				"kind":        out.Kind,
				"title":       out.Title,
				"version":     out.Version,
				"stage_kind":  run.Kind,
			}); err != nil {
			return err
		}

		if run.Kind == domain.StageKindReview {
			// Сдача вердикта и есть завершение работы ревьюера: собственного
			// перехода у него нет.
			return e.completeReview(ctx, tx, task, run, out, actor)
		}
		// Исполнитель сдал новую версию результата — прежний вердикт вынесен
		// о том, чего уже нет, и перепроверку придётся пройти заново.
		return e.resetReview(ctx, tx, run)
	})

	return out, err
}

// CheckFileSubmission — проверки сдачи файла по контракту, которые не
// требуют самого файла: прогон, контракт роли, схема kind. Транспорт зовёт её
// до чтения тела (R-1 CLAB-296): неверный kind или завершённый этап не должны
// стоить загрузки файла на десятки МиБ. Без транзакции и блокировки задачи —
// SubmitArtifact повторит те же проверки под ними.
func (e *Engine) CheckFileSubmission(ctx context.Context, taskKey string, actor uuid.UUID, kind string, stageRunID *uuid.UUID) error {
	q := e.store.DB()
	task, err := e.store.GetTaskByKey(ctx, q, taskKey)
	if err != nil {
		return err
	}
	run, err := e.workRun(ctx, q, task, actor, stageRunID)
	if err != nil {
		return err
	}
	if run.FinishedAt != nil {
		return apperr.Conflict("этап уже завершён, артефакт прикреплять некуда")
	}
	schema, err := e.checkContract(ctx, q, run, kind)
	if err != nil {
		return err
	}
	return fileForbiddenBySchema(kind, schema)
}

// fileForbiddenBySchema — файл не сдаётся под kind со схемой: проверить файл
// JSON-схемой нечем, а гейт schema_validation ждёт по нему документ.
func fileForbiddenBySchema(kind string, schema json.RawMessage) error {
	if t := bytes.TrimSpace(schema); len(t) == 0 || string(t) == "null" {
		return nil
	}
	return apperr.InvalidInput(
		"у %q в контракте роли JSON-схема — файл под таким kind не сдаётся, только документ строкой", kind).
		WithDetails(map[string]any{"kind": kind})
}

// AttachInput — вложение к задаче. Имя и тип — уже санитизированные
// транспортом; файл принят до транзакции.
type AttachInput struct {
	Title        string
	FileName     string
	MediaType    string
	SupersedesID *uuid.UUID
	File         *postgres.SpooledBlob
}

// AttachFile прикладывает файл к задаче (решение 0064, п. 2).
//
// Вложение живёт вне цикла: ни прогона, ни контракта роли, ни проверки
// статуса — задача любого типа и в любом статусе, включая закрытые. Поэтому
// здесь нет ни checkContract, ни resetReview, ни автозамены по kind, а
// stage_runs не трогаются: гейты и перепроверка вложения не видят. Новая
// версия — только явным supersedes_id; несколько вложений живут рядом.
//
// Задача берётся FOR KEY SHARE, а не FOR UPDATE: вложение не меняет её
// состояния, вложения друг друга не ждут, а параллельные замены одной
// версии сериализует замок строки заменяемого в CreateArtifact. Но взять её
// надо до замка по sha блоба — в том же порядке, что сдача этапа, иначе те
// же байты вложением и сдачей одновременно блокируют друг друга.
// Транзакция короткая — файл к ней уже принят.
func (e *Engine) AttachFile(ctx context.Context, taskKey string, actor uuid.UUID, in AttachInput) (domain.Artifact, error) {
	if in.File == nil {
		return domain.Artifact{}, apperr.InvalidInput("файл не передан")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = in.FileName
	}
	var out domain.Artifact
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.GetTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		if err := e.store.KeyShareTask(ctx, tx, task.ID); err != nil {
			return err
		}
		out, err = e.store.CreateArtifact(ctx, tx, postgres.CreateArtifactInput{
			Task:         task,
			Kind:         domain.AttachmentKind,
			Title:        title,
			Format:       "binary",
			MediaType:    in.MediaType,
			Blob:         in.File,
			FileName:     in.FileName,
			IsAttachment: true,
			SupersedesID: in.SupersedesID,
			Status:       domain.ArtifactSubmitted,
			ProducedBy:   actor,
		})
		if err != nil {
			return err
		}
		payload := map[string]any{
			"artifact_id": out.ID.String(),
			"file_name":   in.FileName,
			"media_type":  in.MediaType,
			"size_bytes":  out.SizeBytes,
			"version":     out.Version,
		}
		if out.SupersedesID != nil {
			payload["supersedes_id"] = out.SupersedesID.String()
		}
		return e.store.AppendEvent(ctx, tx, task.ID, nil, domain.EventAttachmentAdded, actor, payload)
	})
	return out, err
}

// workRun определяет, в чей прогон агент сдаёт результат.
//
// Ревьюер и исполнитель работают на одном статусе, но в разных прогонах;
// перепутать их значит закрыть гейт artifacts_required вердиктом ревью или
// наоборот.
func (e *Engine) workRun(ctx context.Context, q postgres.Querier, task domain.Task, actor uuid.UUID, explicit *uuid.UUID) (domain.StageRun, error) {
	if explicit != nil {
		run, err := e.store.GetStageRun(ctx, q, *explicit)
		if err != nil {
			return run, err
		}
		if run.TaskID != task.ID {
			return run, apperr.InvalidInput("прогон принадлежит другой задаче")
		}
		return run, nil
	}

	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return run, err
	}
	reviews, err := e.store.OpenReviewRuns(ctx, q, run.ID)
	if err != nil {
		return run, err
	}
	for _, r := range reviews {
		if r.ClaimedBy != nil && *r.ClaimedBy == actor {
			return r, nil
		}
	}
	return run, nil
}

// checkContract не пускает артефакт, которого нет в output_contract роли.
//
// Это то место, где контракт роли перестаёт быть просто текстом: гейт
// artifacts_required проверяет наличие, а здесь проверяется, что роль вообще
// имеет право сдавать такой kind.
// checkContract проверяет, что kind входит в контракт роли, и отдаёт схему
// этого вида из контракта, если она объявлена.
func (e *Engine) checkContract(ctx context.Context, q postgres.Querier, run domain.StageRun, kind string) (json.RawMessage, error) {
	if run.RoleVersionID == nil {
		return nil, nil
	}
	rv, err := e.store.GetRoleVersion(ctx, q, *run.RoleVersionID)
	if err != nil {
		return nil, err
	}

	var contract []struct {
		Kind   string          `json:"kind"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(rv.OutputContract, &contract); err != nil || len(contract) == 0 {
		// Роль без объявленного контракта ничего не ограничивает: описать
		// контракт — решение проекта, а не требование движка.
		return nil, nil
	}
	allowed := make([]string, 0, len(contract))
	for _, c := range contract {
		if c.Kind == kind {
			return c.Schema, nil
		}
		allowed = append(allowed, c.Kind)
	}
	// Описание проблемы сдаётся при откате назад и в контракт роли не входит:
	// это не результат этапа, а объяснение, почему его не будет.
	if isProblemReport(kind) {
		return nil, nil
	}

	return nil, apperr.New(apperr.CodeContractViolation,
		"артефакт %q не входит в контракт роли", kind).
		WithDetails(map[string]any{"allowed_kinds": allowed})
}

// checkArtifactSchema сверяет содержимое со схемой из контракта роли в момент
// сдачи. Гейт schema_validation проверяет то же на переходе, но агент узнал бы
// об ошибке только тогда и успел бы сдать несколько невалидных версий
// (CLAB-102): контракт — проверка, и срабатывать она должна сразу.
func checkArtifactSchema(kind string, schema json.RawMessage, content []byte) error {
	if len(bytes.TrimSpace(schema)) == 0 || string(bytes.TrimSpace(schema)) == "null" {
		return nil
	}
	var doc any
	if err := json.Unmarshal(content, &doc); err != nil {
		return apperr.New(apperr.CodeValidationFailed,
			"артефакт %q не разбирается как JSON, а контракт роли задаёт ему схему", kind).
			WithDetails(map[string]any{"kind": kind, "parse_error": err.Error()})
	}
	problems, err := validateAgainstSchema(schema, doc)
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		return nil
	}
	return apperr.New(apperr.CodeValidationFailed,
		"артефакт %q не соответствует схеме из контракта роли", kind).
		WithDetails(map[string]any{"kind": kind, "problems": problems})
}

type UpdateScopeInput struct {
	// Ссылки на репозитории проекта: owner/name, name или role_hint.
	AddRepositories []string
	// Репозитории, которые уходят из области; ссылки — те же, что у
	// AddRepositories. Сужение — решение человека: агентский тул его не
	// передаёт, а движок требует уровень не ниже сопровождающего.
	RemoveRepositories []string
	// Свойства задачи целиком; nil — не трогать.
	Properties json.RawMessage
	Note       string
	// Уровень актора в проекте. Пусто — права выключены (режим header).
	Level domain.MemberRole
}

// UpdateScope меняет область задачи по репозиториям и её свойства.
//
// Включение multi_repo и добавление второго репозитория идут одной
// транзакцией намеренно: инвариант держит отложенный триггер
// enforce_task_repo_scope(), который сработает на коммите и увидит уже
// согласованное состояние. Разбить это на два запроса нельзя — ни один из
// порядков не проходит по отдельности.
//
// Сужение идёт раньше расширения: так одним вызовом можно заменить
// единственный репозиторий однорепозиторной задачи, и пустой область
// оказывается только посередине, а не на коммите.
func (e *Engine) UpdateScope(ctx context.Context, taskKey string, actor uuid.UUID, in UpdateScopeInput) (domain.Task, error) {
	if len(in.RemoveRepositories) > 0 && in.Level != "" && !in.Level.Covers(domain.MemberMaintainer) {
		return domain.Task{}, apperr.Forbidden(
			"сузить область задачи может сопровождающий проекта: " +
				"убрать репозиторий из области — решение человека, не агента")
	}

	var out domain.Task

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}

		if in.Properties != nil {
			if err := e.store.UpdateTaskProperties(ctx, tx, task.ID, in.Properties); err != nil {
				return err
			}
			task.Properties = in.Properties
		}

		var run *uuid.UUID
		if r, err := e.store.CurrentStageRun(ctx, tx, task.ID, task.StatusID); err == nil {
			run = &r.ID
		}

		removed, err := e.narrowScope(ctx, tx, &task, in)
		if err != nil {
			return err
		}

		added := []string{}
		for _, ref := range in.AddRepositories {
			repo, err := e.store.FindRepository(ctx, tx, task.ProjectID, ref)
			if err != nil {
				return err
			}
			if err := e.store.AddTaskRepository(ctx, tx, task, repo.ID, run, actor, in.Note); err != nil {
				return err
			}
			added = append(added, repo.Owner+"/"+repo.Name)
			task.Repositories = append(task.Repositories, domain.TaskRepository{
				RepositoryID: repo.ID, Owner: repo.Owner, Name: repo.Name,
				IsPrimary: len(task.Repositories) == 0,
			})
		}

		if len(removed) > 0 && len(task.Repositories) == 0 {
			return apperr.Conflict(
				"нельзя убрать из области задачи %s последний репозиторий: "+
					"без области не проходит гейт repo_scope и дрейфу не за чем следить — "+
					"добавьте замену в том же запросе или снимите задачу", task.Key)
		}

		if len(added) > 0 || len(removed) > 0 || in.Properties != nil {
			payload := map[string]any{
				"added_repositories": added,
				"multi_repo":         task.MultiRepo(),
				"note":               in.Note,
			}
			if len(removed) > 0 {
				payload["removed_repositories"] = removed
				for _, r := range task.Repositories {
					if r.IsPrimary {
						payload["primary_repository"] = r.Owner + "/" + r.Name
					}
				}
			}
			if err := e.store.AppendEvent(ctx, tx, task.ID, run, "task.scope_updated", actor, payload); err != nil {
				return err
			}
		}

		out, err = e.store.GetTaskByKey(ctx, tx, taskKey)
		return err
	})

	return out, err
}

// narrowScope убирает репозитории из области задачи и правит task на месте,
// чтобы расширение в том же вызове видело уже суженную область.
//
// Репозиторий с привязанными PR, ветками или коммитами не убирается: в нём
// уже идёт работа задачи, гейты по PR и external_check читают эти ссылки, и
// после сужения они указывали бы за область. Отвязки ссылок нет, поэтому
// такой репозиторий остаётся в области до конца задачи — убирают только
// тот, что был справочником.
//
// Ушёл основной — основным становится первый из оставшихся по owner/name:
// правило детерминированное и не требует от человека второго решения в том
// же вызове. Если важен другой, его ставят отдельной правкой.
func (e *Engine) narrowScope(ctx context.Context, tx pgx.Tx, task *domain.Task, in UpdateScopeInput) ([]string, error) {
	removed := []string{}
	if len(in.RemoveRepositories) == 0 {
		return removed, nil
	}

	adding := map[uuid.UUID]bool{}
	for _, ref := range in.AddRepositories {
		repo, err := e.store.FindRepository(ctx, tx, task.ProjectID, ref)
		if err != nil {
			return nil, err
		}
		adding[repo.ID] = true
	}
	refs, err := e.store.ListRepoRefs(ctx, tx, task.ID)
	if err != nil {
		return nil, err
	}

	primaryGone := false
	for _, ref := range in.RemoveRepositories {
		repo, err := e.store.FindRepository(ctx, tx, task.ProjectID, ref)
		if err != nil {
			return nil, err
		}
		name := repo.Owner + "/" + repo.Name
		if adding[repo.ID] {
			return nil, apperr.InvalidInput(
				"репозиторий %s указан и в добавлении, и в удалении", name)
		}
		idx := -1
		for i, r := range task.Repositories {
			if r.RepositoryID == repo.ID {
				idx = i
			}
		}
		if idx < 0 {
			return nil, apperr.InvalidInput(
				"репозитория %s нет в области задачи %s", name, task.Key)
		}

		var linked []string
		for _, r := range refs {
			if r.RepositoryID == repo.ID {
				linked = append(linked, r.RefType+" "+r.Ref)
			}
		}
		if len(linked) > 0 {
			return nil, apperr.Conflict(
				"репозиторий %s не убрать из области задачи %s: к нему привязаны %s, "+
					"в нём уже идёт работа задачи", name, task.Key, strings.Join(linked, ", ")).
				WithDetails(map[string]any{"repository": name, "repo_refs": linked})
		}

		wasPrimary, err := e.store.RemoveTaskRepository(ctx, tx, task.ID, repo.ID)
		if err != nil {
			return nil, err
		}
		primaryGone = primaryGone || wasPrimary
		task.Repositories = append(task.Repositories[:idx], task.Repositories[idx+1:]...)
		removed = append(removed, name)
	}

	if primaryGone && len(task.Repositories) > 0 {
		next := 0
		for i, r := range task.Repositories {
			cur := task.Repositories[next]
			if r.Owner < cur.Owner || (r.Owner == cur.Owner && r.Name < cur.Name) {
				next = i
			}
		}
		if err := e.store.PromoteTaskRepository(ctx, tx, task.ID, task.Repositories[next].RepositoryID); err != nil {
			return nil, err
		}
		task.Repositories[next].IsPrimary = true
	}
	return removed, nil
}

// maxTaskTitleRunes — предел заголовка задачи. Считается рунами, а не
// байтами: русский заголовок в байтах вдвое длиннее, и байтовый предел резал
// бы его посередине слова. Предел живёт в коде, а не в схеме: менять его не
// должно стоить миграции.
const maxTaskTitleRunes = 80

// UpdateTaskInput — правка постановки задачи. nil означает «не трогать»:
// правка описания не должна затирать заголовок.
type UpdateTaskInput struct {
	Title       *string
	Description *string
	Priority    *string
	Labels      *[]string
	// Ссылка на основной трекер целиком: `{}` снимает все ссылки (CLAB-95).
	ExternalRef *json.RawMessage
	// Уровень модели задачи: пустая строка снимает его (решение 0060).
	ModelLevel *string
	// Настройки этапов: сливаются с прежними по ключам статуса и полям.
	StageSettings *json.RawMessage
	// Выбор необязательных этапов: сливается с прежним по ключам (решение
	// 0066, п. 9). nil — не трогать, {} — не правка.
	OptionalStages *json.RawMessage
	// Привязка токена задачи (Auth.Bound): выбор этапов своей задачи токену
	// задачи закрыт (п. 11). nil — человек или личный токен.
	Bound *TaskBinding
	// Уровень актора в проекте. Ниже сопровождающего действует правило
	// агента: править можно задачу с его живой арендой и её дочерние —
	// ровно та рамка, в которой он их заводит. Пусто — правило выключено:
	// в режиме header прав нет вовсе.
	Level domain.MemberRole
}

// UpdateTask меняет постановку задачи: заголовок, описание, приоритет, метки
// и ссылку на основной трекер.
//
// Тип, статус, родитель, область и исполнитель этой ручкой не меняются — у
// каждого своя операция и свои правила. Особого правила для терминальных
// статусов у постановки нет: агента и так ограничивает аренда, которой у
// закрытой задачи не бывает, а сопровождающий вправе поправить опечатку и в
// закрытой постановке. Исключение — выбор этапов (решение 0066, п. 9): путь
// закрытой задачи пройден, и менять его нечему.
func (e *Engine) UpdateTask(ctx context.Context, taskKey string, actor uuid.UUID, in UpdateTaskInput) (domain.Task, error) {
	fields := postgres.UpdateTaskFieldsInput{
		Description: in.Description, Priority: in.Priority, Labels: in.Labels,
	}
	if in.Title != nil {
		title := strings.TrimSpace(*in.Title)
		if title == "" {
			return domain.Task{}, apperr.InvalidInput("заголовок задачи не может быть пустым")
		}
		if n := utf8.RuneCountInString(title); n > maxTaskTitleRunes {
			return domain.Task{}, apperr.InvalidInput(
				"заголовок длиннее %d символов (в нём %d): уточнение места — в описании",
				maxTaskTitleRunes, n)
		}
		fields.Title = &title
	}
	if in.Priority != nil && !domain.ValidTaskPriority(*in.Priority) {
		return domain.Task{}, apperr.InvalidInput(
			"приоритет %q не поддерживается: %v", *in.Priority, domain.TaskPriorities)
	}
	if in.ExternalRef != nil {
		ref, err := normalizeExternalRef(*in.ExternalRef)
		if err != nil {
			return domain.Task{}, err
		}
		fields.ExternalRef = &ref
	}
	if in.ModelLevel != nil {
		if err := checkModelLevel(*in.ModelLevel, "model_level"); err != nil {
			return domain.Task{}, err
		}
		fields.ModelLevel = in.ModelLevel
	}
	var patch stagePatch
	if in.StageSettings != nil {
		var err error
		if patch, err = parseStagePatch(*in.StageSettings); err != nil {
			return domain.Task{}, err
		}
	}
	if !fields.Any() && in.StageSettings == nil && in.OptionalStages == nil {
		return domain.Task{}, apperr.InvalidInput(
			"не указано ни одно поле: править можно title, description, priority, labels, " +
				"external_ref, model_level, stage_settings, optional_stages")
	}

	var out domain.Task
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}
		// Отказ токену задачи — раньше разбора выбора и раньше всего
		// остального: весь запрос отбивается, а неверный ключ в нём не
		// превращает 403 в 400.
		if in.OptionalStages != nil {
			if err := ownStageChoice(in.Bound, task); err != nil {
				return err
			}
		}
		if err := e.agentMayEdit(ctx, tx, actor, task, in.Level); err != nil {
			return err
		}
		// Слияние — под блокировкой задачи: прочитать, слить и записать
		// одной транзакцией, иначе две правки разных этапов затёрли бы друг
		// друга.
		if in.StageSettings != nil {
			if err := e.checkStageKeys(ctx, tx, task.WorkflowID, patch); err != nil {
				return err
			}
			merged, err := mergeStageSettings(task.StageSettings, patch)
			if err != nil {
				return err
			}
			fields.StageSettings = &merged
		}
		// Выбор этапов — под той же блокировкой, что переход: правка ждёт
		// идущий переход и проверяется по статусу, где задача встала.
		var stagesFrom, stagesTo map[string]bool
		if in.OptionalStages != nil {
			if stagesFrom, stagesTo, err = e.mergeOptionalStages(ctx, tx, task, *in.OptionalStages); err != nil {
				return err
			}
			if !sameChoice(stagesFrom, stagesTo) {
				raw, err := choiceJSON(stagesTo)
				if err != nil {
					return err
				}
				fields.OptionalStages = &raw
			}
		}
		// Выбор не изменился и больше править нечего — не правка: 200 без
		// события, а не «не указано ни одно поле» клиенту, который шлёт
		// форму целиком.
		if !fields.Any() {
			out, err = e.store.GetTaskByKey(ctx, tx, taskKey)
			return err
		}
		if err := e.store.UpdateTaskFields(ctx, tx, task.ID, fields); err != nil {
			return err
		}

		// Прежние значения пишутся в ленту: версий у постановки нет, и лента
		// событий — единственное место, где остаётся, что было до правки.
		changed := map[string]any{}
		if fields.Title != nil {
			changed["title"] = map[string]any{"from": task.Title, "to": *fields.Title}
		}
		if fields.Description != nil {
			changed["description"] = map[string]any{
				"from": task.Description, "to": nullIfEmpty(*fields.Description),
			}
		}
		if fields.Priority != nil {
			changed["priority"] = map[string]any{"from": task.Priority, "to": *fields.Priority}
		}
		if fields.Labels != nil {
			changed["labels"] = map[string]any{"from": task.Labels, "to": *fields.Labels}
		}
		if fields.ExternalRef != nil {
			changed["external_ref"] = map[string]any{
				"from": task.ExternalRef, "to": *fields.ExternalRef,
			}
		}
		if fields.ModelLevel != nil {
			changed["model_level"] = map[string]any{
				"from": task.ModelLevel, "to": nullIfEmpty(*fields.ModelLevel),
			}
		}
		if fields.StageSettings != nil {
			changed["stage_settings"] = map[string]any{
				"from": task.StageSettings, "to": *fields.StageSettings,
			}
		}
		if fields.OptionalStages != nil {
			changed["optional_stages"] = map[string]any{"from": stagesFrom, "to": stagesTo}
		}
		fieldNames := make([]string, 0, len(changed))
		for name := range changed {
			fieldNames = append(fieldNames, name)
		}
		sort.Strings(fieldNames)

		var run *uuid.UUID
		if r, err := e.store.CurrentStageRun(ctx, tx, task.ID, task.StatusID); err == nil {
			run = &r.ID
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, run, domain.EventTaskUpdated, actor,
			map[string]any{"fields": fieldNames, "changed": changed}); err != nil {
			return err
		}

		out, err = e.store.GetTaskByKey(ctx, tx, taskKey)
		return err
	})

	return out, err
}

// mergeOptionalStages сверяет и сливает выбор этапов из запроса с выбором
// задачи (решение 0066, п. 9). Отдаёт полные наборы до и после; «до» — с
// разрешёнными умолчаниями, чтобы задача, заведённая до выбора, не считала
// правкой запись тех же значений.
//
// Порядок отказов: разбор и ключи (400), затем закрытая задача и текущий
// статус (409) — эти зависят от состояния, а не от запроса.
func (e *Engine) mergeOptionalStages(ctx context.Context, tx pgx.Tx, task domain.Task, raw json.RawMessage) (from, to map[string]bool, err error) {
	patch, err := parseOptionalStages(raw)
	if err != nil {
		return nil, nil, err
	}
	statuses, err := e.store.ListStatuses(ctx, tx, task.WorkflowID)
	if err != nil {
		return nil, nil, err
	}
	optional := optionalOf(statuses)
	if err := checkOptionalKeys(optional, patch); err != nil {
		return nil, nil, err
	}
	from = resolveChoice(optional, storedChoice(task.OptionalStages), nil)
	if len(patch) == 0 {
		return from, from, nil
	}
	if task.ClosedAt != nil {
		return nil, nil, apperr.Conflict(
			"задача %s закрыта: выбор этапов у неё не меняется", task.Key)
	}
	to = resolveChoice(optional, from, patch)
	// Задача не стоит в выключенном этапе: выключить текущий статус нельзя,
	// а запись того же значения — не правка.
	if on, ok := patch[task.StatusKey]; ok && !on && from[task.StatusKey] {
		return nil, nil, apperr.Conflict(
			"этап %s — текущий статус задачи %s: выключить его нельзя", task.StatusKey, task.Key).
			WithDetails(map[string]any{"status": task.StatusKey})
	}
	return from, to, nil
}

// nullIfEmpty отличает «описания нет» от пустой строки: в ленте у очистки
// описания и у правки на пустую строку обязан быть один и тот же вид, потому
// что в базе это одно и то же.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// agentMayEdit — рамка, в которой агент вправе править постановку.
//
// Она повторяет рамку заведения задач (agentMayCreate): аналитик уточняет
// нарезку эпика, который держит, и не трогает чужие задачи. Сопровождающий и
// выше не ограничены ничем — в том числе на закрытых задачах.
func (e *Engine) agentMayEdit(ctx context.Context, q postgres.Querier, actor uuid.UUID, task domain.Task, level domain.MemberRole) error {
	if level == "" || level.Covers(domain.MemberMaintainer) {
		return nil
	}
	held, err := e.holdsLease(ctx, q, task, actor)
	if err != nil {
		return err
	}
	if held {
		return nil
	}
	if task.ParentKey != nil {
		parent, err := e.store.GetTaskByKey(ctx, q, *task.ParentKey)
		if err != nil {
			return err
		}
		held, err := e.holdsLease(ctx, q, parent, actor)
		if err != nil {
			return err
		}
		if held {
			return nil
		}
	}
	return apperr.Forbidden(
		"задачу %s держите не вы: править постановку может тот, кто забрал её этап "+
			"или этап её родительской задачи", task.Key)
}

// holdsLease отвечает, держит ли актор живую аренду текущего этапа задачи.
// Это и есть право агента на задачу: аренда снимается явно, и протухшая
// аренда права не даёт.
func (e *Engine) holdsLease(ctx context.Context, q postgres.Querier, task domain.Task, actor uuid.UUID) (bool, error) {
	run, err := e.store.CurrentStageRun(ctx, q, task.ID, task.StatusID)
	if err != nil {
		return false, err
	}
	return run.ClaimedBy != nil && *run.ClaimedBy == actor && run.LeaseHeld(time.Now()), nil
}

// CreateTaskInput — заведение задачи движком: то же, что у хранилища, плюс
// связи, назначение и уровень актора.
type CreateTaskInput struct {
	postgres.CreateTaskInput
	// Хэндл принципала-исполнителя: оркестратор адресует нарезку конкретному
	// раннеру или человеку.
	AssigneeHandle string
	// Ключи задач, которые новая задача блокирует: порядок работ оркестратор
	// задаёт сразу при нарезке, а держит его гейт dependencies_closed.
	Blocks []string
	// Выбор необязательных этапов (решение 0066, п. 9): не названные берут
	// умолчание цикла, который задача получит по типу.
	OptionalStages json.RawMessage
	// Уровень актора в проекте. Ниже сопровождающего действует правило агента:
	// заводить можно только детей задачи, которую он держит по аренде, и только
	// внутри её области (решение 0024). Пусто — правило выключено: в режиме
	// header прав нет вовсе.
	Level domain.MemberRole
}

// CreateTask заводит задачу и ставит её на начальный статус цикла её типа.
func (e *Engine) CreateTask(ctx context.Context, projectKey string, actor uuid.UUID, in CreateTaskInput) (domain.Task, error) {
	var out domain.Task

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := e.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		in.ProjectID = project.ID
		in.CreatedBy = actor

		if in.ExternalRef, err = normalizeExternalRef(in.ExternalRef); err != nil {
			return err
		}
		patch, err := prepareTaskModelLevels(&in)
		if err != nil {
			return err
		}
		stages, err := parseOptionalStages(in.OptionalStages)
		if err != nil {
			return err
		}
		if in.AssigneeHandle != "" {
			assignee, err := e.resolveAssignee(ctx, tx, in.AssigneeHandle)
			if err != nil {
				return err
			}
			in.AssigneeID = &assignee.ID
		}
		if err := e.agentMayCreate(ctx, tx, actor, in); err != nil {
			return err
		}

		out, err = e.store.CreateTask(ctx, tx, in.CreateTaskInput)
		if err != nil {
			return err
		}
		// Ключи этапов сверяются с циклом, который хранилище выбрало по типу:
		// раньше его не знает никто. Отказ откатывает и задачу.
		if err := e.checkStageKeys(ctx, tx, out.WorkflowID, patch); err != nil {
			return err
		}
		// Выбор этапов — тоже по циклу, выбранному хранилищем, и полным
		// набором: умолчания цикла пишутся явно, правка умолчания в следующей
		// версии путь этой задачи не меняет. Отказ откатывает и задачу, и
		// счётчик ключей.
		if out.OptionalStages, err = e.initialOptionalStages(ctx, tx, out, stages); err != nil {
			return err
		}

		blocked := []string{}
		for _, key := range in.Blocks {
			target, err := e.store.GetTaskByKey(ctx, tx, key)
			if err != nil {
				return err
			}
			if target.ProjectID != project.ID {
				return apperr.InvalidInput("задача %q из другого проекта", key)
			}
			if _, err := e.store.LinkTasks(ctx, tx, out.ID, target.ID,
				domain.TaskLinkBlocks, "", actor); err != nil {
				return err
			}
			blocked = append(blocked, target.Key)
		}

		run, err := e.store.CurrentStageRun(ctx, tx, out.ID, out.StatusID)
		if err != nil {
			return err
		}
		payload := map[string]any{
			"key": out.Key, "title": out.Title, "status": out.StatusKey, "type": out.Type(),
		}
		if out.ParentKey != nil {
			payload["parent"] = *out.ParentKey
		}
		if len(blocked) > 0 {
			payload["blocks"] = blocked
		}
		if in.AssigneeHandle != "" {
			payload["assignee"] = in.AssigneeHandle
		}
		if string(in.ExternalRef) != "{}" {
			payload["external_ref"] = in.ExternalRef
		}
		if out.ModelLevel != nil {
			payload["model_level"] = *out.ModelLevel
		}
		if string(out.StageSettings) != "{}" {
			payload["stage_settings"] = out.StageSettings
		}
		if string(out.OptionalStages) != "{}" {
			payload["optional_stages"] = out.OptionalStages
		}
		if err := e.store.AppendEvent(ctx, tx, out.ID, &run.ID, domain.EventTaskCreated, actor,
			payload); err != nil {
			return err
		}
		return e.store.AppendEvent(ctx, tx, out.ID, &run.ID, domain.EventStageEntered, actor,
			map[string]any{"status": out.StatusKey, "attempt": run.Attempt})
	})

	return out, err
}

// initialOptionalStages сверяет выбор заводимой задачи с версией её цикла и
// пишет полный набор. Цикл без необязательных — {} без записи.
func (e *Engine) initialOptionalStages(ctx context.Context, tx pgx.Tx, task domain.Task, patch optionalPatch) (json.RawMessage, error) {
	statuses, err := e.store.ListStatuses(ctx, tx, task.WorkflowID)
	if err != nil {
		return nil, err
	}
	optional := optionalOf(statuses)
	if err := checkOptionalKeys(optional, patch); err != nil {
		return nil, err
	}
	if len(optional) == 0 {
		return json.RawMessage(`{}`), nil
	}
	raw, err := choiceJSON(resolveChoice(optional, nil, patch))
	if err != nil {
		return nil, err
	}
	if err := e.store.UpdateTaskFields(ctx, tx, task.ID,
		postgres.UpdateTaskFieldsInput{OptionalStages: &raw}); err != nil {
		return nil, err
	}
	return raw, nil
}

// prepareTaskModelLevels проверяет уровни модели заводимой задачи и сводит
// stage_settings к хранимому виду. Пустые значения при заведении значат «не
// задано» и в объект не попадают; ключи этапов остаются в разборе — их
// сверяют с циклом задачи после выбора цикла.
func prepareTaskModelLevels(in *CreateTaskInput) (stagePatch, error) {
	if in.ModelLevel != nil {
		if err := checkModelLevel(*in.ModelLevel, "model_level"); err != nil {
			return nil, err
		}
		if *in.ModelLevel == "" {
			in.ModelLevel = nil
		}
	}
	patch, err := parseStagePatch(in.StageSettings)
	if err != nil {
		return nil, err
	}
	if in.StageSettings, err = mergeStageSettings(nil, patch); err != nil {
		return nil, err
	}
	return patch, nil
}

// agentMayCreate — рамка, в которой агент вправе заводить задачи.
//
// Оркестратор нарезает эпик, который держит: он не может завести задачу в
// чужом эпике и не может расширить область — расширение области решает
// человек (решение 0015). Сопровождающий и выше не ограничены ничем.
func (e *Engine) agentMayCreate(ctx context.Context, q postgres.Querier, actor uuid.UUID, in CreateTaskInput) error {
	if in.Level == "" || in.Level.Covers(domain.MemberMaintainer) {
		return nil
	}
	if in.ParentKey == "" {
		return apperr.Forbidden(
			"агент заводит только дочерние задачи той, которую держит: укажите parent")
	}

	parent, err := e.store.GetTaskByKey(ctx, q, in.ParentKey)
	if err != nil {
		return err
	}
	held, err := e.holdsLease(ctx, q, parent, actor)
	if err != nil {
		return err
	}
	if !held {
		return apperr.Forbidden(
			"задачу %s держите не вы: нарезать её на дочерние может только тот, кто её забрал",
			parent.Key)
	}

	if len(in.Repositories) == 0 {
		return nil
	}
	allowed := map[string]bool{}
	for _, r := range parent.Repositories {
		allowed[r.Owner+"/"+r.Name] = true
		allowed[r.Name] = true
		if r.RoleHint != nil {
			allowed[*r.RoleHint] = true
		}
	}
	for _, ref := range in.Repositories {
		if !allowed[ref] {
			return apperr.New(apperr.CodeScopeExpansionForbidden,
				"репозиторий %q вне области задачи %s: расширить область — решение человека",
				ref, parent.Key)
		}
	}
	return nil
}

// resolveAssignee находит исполнителя по хэндлу.
//
// Хэндл принимается и с видом принципала (agent:runner-bot), и без него:
// первым его пишут люди и агенты, вторым он хранится. Нового принципала здесь
// не заводят: назначение на несуществующего исполнителя — опечатка, а не
// повод завести учётную запись.
func (e *Engine) resolveAssignee(ctx context.Context, q postgres.Querier, raw string) (domain.Principal, error) {
	_, handle := SplitHandle(raw, domain.PrincipalAgent)
	return e.store.GetPrincipalByHandle(ctx, q, handle)
}

// LinkTasks связывает две задачи. Отношение blocks задаёт порядок работ, и
// его читает гейт dependencies_closed; остальные отношения — контекст для
// агента и человека.
func (e *Engine) LinkTasks(ctx context.Context, fromKey, toKey, rel, note string, actor uuid.UUID, remove bool) ([]postgres.TaskLink, error) {
	if !domain.ValidTaskLinkRel(rel) {
		return nil, apperr.InvalidInput("отношение %q не поддерживается: %v", rel, domain.TaskLinkRels)
	}

	var out []postgres.TaskLink
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		from, err := e.store.GetTaskByKey(ctx, tx, fromKey)
		if err != nil {
			return err
		}
		to, err := e.store.GetTaskByKey(ctx, tx, toKey)
		if err != nil {
			return err
		}
		if from.ID == to.ID {
			return apperr.InvalidInput("задачу нельзя связать с самой собой")
		}
		if from.ProjectID != to.ProjectID {
			return apperr.InvalidInput("задачи из разных проектов")
		}

		if remove {
			if err := e.store.UnlinkTasks(ctx, tx, from.ID, to.ID, rel); err != nil {
				return err
			}
		} else if _, err := e.store.LinkTasks(ctx, tx, from.ID, to.ID, rel, note, actor); err != nil {
			return err
		}

		event := "task.linked"
		if remove {
			event = "task.unlinked"
		}
		if err := e.store.AppendEvent(ctx, tx, from.ID, nil, event, actor,
			map[string]any{"rel": rel, "task": to.Key, "note": note}); err != nil {
			return err
		}
		out, err = e.store.ListTaskLinks(ctx, tx, from.ID)
		return err
	})

	return out, err
}

// Assign меняет исполнителя задачи. Назначенную задачу заберёт только он:
// так оркестратор адресует нарезку конкретному раннеру (решение 0025).
func (e *Engine) Assign(ctx context.Context, taskKey, handle string, actor uuid.UUID) (domain.Task, error) {
	var out domain.Task

	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		task, err := e.store.LockTaskByKey(ctx, tx, taskKey)
		if err != nil {
			return err
		}

		var assignee *uuid.UUID
		if handle != "" {
			p, err := e.resolveAssignee(ctx, tx, handle)
			if err != nil {
				return err
			}
			assignee = &p.ID
		}
		if err := e.store.SetAssignee(ctx, tx, task.ID, assignee); err != nil {
			return err
		}
		if err := e.store.AppendEvent(ctx, tx, task.ID, nil, "task.assigned", actor,
			map[string]any{"assignee": handle}); err != nil {
			return err
		}

		out, err = e.store.GetTaskByKey(ctx, tx, taskKey)
		return err
	})

	return out, err
}

// Store открывает доступ к хранилищу для чтения из транспортного слоя.
// Читающие ручки не проходят через движок: им нечего решать.
func (e *Engine) Store() *postgres.Store { return e.store }

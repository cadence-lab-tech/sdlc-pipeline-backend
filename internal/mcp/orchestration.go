package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/trust"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Тулы оркестрации (решения 0024-0026): оркестратор нарезает эпик на задачи,
// связывает их порядком и отчитывается о стоимости. Правила — в движке:
// агент заводит только детей задачи, которую держит, и только внутри её
// области; тул лишь передаёт уровень актора.

func (s *Server) registerOrchestrationTools() {
	add(s, "sdlc_create_task",
		"Завести задачу. Оркестратор так нарезает эпик: type задаёт цикл, parent вешает "+
			"задачу на эпик, blocks задаёт порядок, assignee адресует её исполнителю, "+
			"external_ref ссылается на тикет основного трекера. "+
			"model_level — рекомендованный уровень модели задачи (low, medium, high), "+
			"stage_settings — уровни этапов: {\"development\": {\"model_level\": \"high\"}}, "+
			"ключи — статусы цикла задачи с ролью-исполнителем. "+
			"Агент вправе создавать только дочерние задачи той, которую держит, и только "+
			"внутри её области репозиториев.",
		s.createTask)
	add(s, "sdlc_update_task",
		"Поправить постановку задачи: заголовок, описание, приоритет, метки, ссылку на "+
			"основной трекер, уровень модели задачи (model_level) и этапов (stage_settings). "+
			"stage_settings сливается с прежними по ключам статуса и полям этапа: пустое "+
			"значение снимает уровень, этап без полей уходит, не названные ключи не меняются; "+
			"model_level \"\" снимает уровень задачи. Так аналитик "+
			"уточняет нарезку эпика, когда код ушёл вперёд её описания. Агент вправе править "+
			"задачу, которую держит, и её дочерние. Не переданное поле не меняется, пустое "+
			"описание очищает его, метки и external_ref заменяются целиком ({} снимает ссылки). Тип, статус, родитель, "+
			"область и исполнитель этой ручкой не меняются — у каждого своя.",
		s.updateTask)
	add(s, "sdlc_link_tasks",
		"Связать задачи: blocks задаёт порядок (его читает гейт dependencies_closed), "+
			"relates_to, duplicates, shares_surface и split_from — контекст. remove=true снимает связь.",
		s.linkTasks)
	add(s, "sdlc_report_usage",
		"Отчитаться о стоимости прогона этапа: модель, токены, деньги, время. "+
			"Отчитывается тот, кто забирал этап, — сам агент или запустивший его раннер. "+
			"Без отчёта прогон стоит «неизвестно», а не ноль. "+
			"billing — тип оплаты: api или subscription; условная стоимость на подписке "+
			"в бюджет команды не входит, без признака строка считается как api.",
		s.reportUsage)
	add(s, "sdlc_assign_task",
		"Назначить задачу принципалу: забрать её сможет только он. Пустой assignee снимает назначение.",
		s.assignTask)
}

type createTaskInput struct {
	Title string `json:"title" jsonschema:"заголовок задачи"`
	// Тип определяет цикл: task, epic — что заведено в проекте.
	Type         string          `json:"type,omitempty" jsonschema:"тип задачи: по нему выбирается цикл"`
	Parent       string          `json:"parent,omitempty" jsonschema:"ключ родительской задачи, например SDLC-42"`
	Description  string          `json:"description,omitempty"`
	Priority     string          `json:"priority,omitempty" jsonschema:"low, normal, high, urgent"`
	Assignee     string          `json:"assignee,omitempty" jsonschema:"хэндл принципала-исполнителя"`
	Labels       []string        `json:"labels,omitempty"`
	Properties   json.RawMessage `json:"properties,omitempty" jsonschema:"свойства задачи объектом"`
	Repositories []string        `json:"repositories,omitempty" jsonschema:"область задачи: owner/name, name или подсказка роли"`
	Blocks       []string        `json:"blocks,omitempty" jsonschema:"ключи задач, которые новая блокирует"`
	// any, а не json.RawMessage: схему тула выводит jsonschema-go, и
	// RawMessage для него — массив байтов, а не объект.
	ExternalRef any    `json:"external_ref,omitempty" jsonschema:"ссылка на основной трекер: {\"jira\": {\"key\": \"PROJ-12\", \"url\": \"https://jira.example.com/browse/PROJ-12\"}}"`
	ModelLevel  string `json:"model_level,omitempty" jsonschema:"рекомендованный уровень модели задачи: low, medium, high"`
	// any — по той же причине, что external_ref.
	StageSettings any `json:"stage_settings,omitempty" jsonschema:"уровни этапов: {\"development\": {\"model_level\": \"high\"}}; ключи — статусы цикла задачи с ролью-исполнителем"`
}

func (s *Server) createTask(ctx context.Context, in createTaskInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberAgent); err != nil {
		return fail(err)
	}
	// Токен задачи заводит только детей своей задачи.
	if err := s.auth.RequireOwnTaskKey(identity(ctx), in.Parent); err != nil {
		return fail(err)
	}
	key, err := projectKey(ctx)
	if err != nil {
		return fail(err)
	}
	level, err := s.level(ctx)
	if err != nil {
		return fail(err)
	}

	var description *string
	if in.Description != "" {
		description = &in.Description
	}
	externalRef, err := rawArgument(in.ExternalRef)
	if err != nil {
		return fail(err)
	}
	stageSettings, err := rawArgument(in.StageSettings)
	if err != nil {
		return fail(err)
	}
	var modelLevel *string
	if in.ModelLevel != "" {
		modelLevel = &in.ModelLevel
	}
	task, err := s.engine.CreateTask(ctx, key, actor(ctx).ID, workflow.CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			ParentKey: in.Parent, TaskType: in.Type, Title: in.Title,
			Description: description, Priority: in.Priority, Labels: in.Labels,
			Properties: in.Properties, Repositories: in.Repositories,
			ExternalRef: externalRef, ModelLevel: modelLevel, StageSettings: stageSettings,
		},
		AssigneeHandle: in.Assignee,
		Blocks:         in.Blocks,
		Level:          level,
	})
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{
		"key": task.Key, "title": task.Title, "type": task.Type(),
		"status": task.StatusKey, "parent": task.ParentKey,
		"assignee": task.AssigneeHandle, "repositories": repoNames(task),
		"external_ref": task.ExternalRef,
		"model_level":  task.ModelLevel, "stage_settings": task.StageSettings,
	})
}

// rawArgument возвращает аргумент тула открытой формы как JSON: проверяет его
// движок, одним правилом с REST. nil — аргумент не передан.
func rawArgument(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	body, err := json.Marshal(v)
	if err != nil {
		return nil, apperr.InvalidInput("аргумент не сериализуется в JSON: %v", err)
	}
	return body, nil
}

type updateTaskInput struct {
	TaskKey     string    `json:"task_key" jsonschema:"ключ задачи, например SDLC-42"`
	Title       *string   `json:"title,omitempty" jsonschema:"новый заголовок, не длиннее 80 символов"`
	Description *string   `json:"description,omitempty" jsonschema:"новое описание; пустая строка очищает его"`
	Priority    *string   `json:"priority,omitempty" jsonschema:"low, normal, high, urgent"`
	Labels      *[]string `json:"labels,omitempty" jsonschema:"метки целиком: список заменяет прежний набор"`
	// nil — не трогать: null и отсутствие поля для агента одно и то же.
	ExternalRef any     `json:"external_ref,omitempty" jsonschema:"ссылка на основной трекер целиком: {\"jira\": {\"key\": \"PROJ-12\", \"url\": \"https://…\"}}; {} снимает ссылки"`
	ModelLevel  *string `json:"model_level,omitempty" jsonschema:"уровень модели задачи: low, medium, high; пустая строка снимает его"`
	// any — по той же причине, что external_ref; nil — не трогать.
	StageSettings any `json:"stage_settings,omitempty" jsonschema:"уровни этапов, сливаются с прежними: {\"testing\": {\"model_level\": \"low\"}}; {\"testing\": {\"model_level\": \"\"}} снимает уровень этапа"`
}

func (s *Server) updateTask(ctx context.Context, in updateTaskInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberAgent); err != nil {
		return fail(err)
	}
	if err := s.inProject(ctx, in.TaskKey); err != nil {
		return fail(err)
	}
	// Токен задачи правит только свою задачу и её детей (CLAB-208).
	if err := s.auth.RequireWriteCircle(ctx, identity(ctx), in.TaskKey, "правка постановки"); err != nil {
		return fail(err)
	}
	level, err := s.level(ctx)
	if err != nil {
		return fail(err)
	}

	edit := workflow.UpdateTaskInput{
		Title: in.Title, Description: in.Description, Priority: in.Priority,
		Labels: in.Labels, ModelLevel: in.ModelLevel, Level: level,
	}
	if in.StageSettings != nil {
		settings, err := rawArgument(in.StageSettings)
		if err != nil {
			return fail(err)
		}
		edit.StageSettings = &settings
	}
	if in.ExternalRef != nil {
		ref, err := rawArgument(in.ExternalRef)
		if err != nil {
			return fail(err)
		}
		edit.ExternalRef = &ref
	}
	task, err := s.engine.UpdateTask(ctx, in.TaskKey, actor(ctx).ID, edit)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{
		"key": task.Key, "title": task.Title, "description": task.Description,
		"priority": task.Priority, "labels": task.Labels, "status": task.StatusKey,
		"external_ref": task.ExternalRef,
		"model_level":  task.ModelLevel, "stage_settings": task.StageSettings,
	})
}

type linkTasksInput struct {
	From   string `json:"from" jsonschema:"ключ задачи, от которой идёт связь"`
	To     string `json:"to" jsonschema:"ключ задачи на другом конце"`
	Rel    string `json:"rel,omitempty" jsonschema:"blocks (по умолчанию), relates_to, duplicates, shares_surface, split_from"`
	Note   string `json:"note,omitempty"`
	Remove bool   `json:"remove,omitempty" jsonschema:"снять связь вместо заведения"`
}

func (s *Server) linkTasks(ctx context.Context, in linkTasksInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberAgent); err != nil {
		return fail(err)
	}
	if err := s.inProject(ctx, in.From); err != nil {
		return fail(err)
	}
	if err := s.inProject(ctx, in.To); err != nil {
		return fail(err)
	}
	rel := in.Rel
	if rel == "" {
		rel = domain.TaskLinkBlocks
	}
	if err := s.auth.RequireLink(ctx, identity(ctx), in.From, in.To, rel); err != nil {
		return fail(err)
	}
	links, err := s.engine.LinkTasks(ctx, in.From, in.To, rel, in.Note, actor(ctx).ID, in.Remove)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"task": in.From, "links": links})
}

type reportUsageInput struct {
	TaskKey          string          `json:"task_key" jsonschema:"ключ задачи"`
	Model            string          `json:"model" jsonschema:"модель как она называется у провайдера"`
	StageRunID       string          `json:"stage_run_id,omitempty" jsonschema:"прогон; пусто — тот, который вы держали"`
	InputTokens      *int64          `json:"input_tokens,omitempty"`
	OutputTokens     *int64          `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64          `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64          `json:"cache_write_tokens,omitempty"`
	CostUSD          *float64        `json:"cost_usd,omitempty" jsonschema:"стоимость в долларах"`
	DurationMS       *int64          `json:"duration_ms,omitempty"`
	Details          json.RawMessage `json:"details,omitempty"`
	Billing          string          `json:"billing,omitempty" jsonschema:"тип оплаты: api или subscription; пусто — не указано, считается как api"`
}

func (s *Server) reportUsage(ctx context.Context, in reportUsageInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberAgent); err != nil {
		return fail(err)
	}
	if err := s.inProject(ctx, in.TaskKey); err != nil {
		return fail(err)
	}

	report := workflow.ReportUsageInput{
		Model: in.Model, InputTokens: in.InputTokens, OutputTokens: in.OutputTokens,
		CacheReadTokens: in.CacheReadTokens, CacheWriteTokens: in.CacheWriteTokens,
		CostUSD: in.CostUSD, DurationMS: in.DurationMS, Details: in.Details,
		Billing: in.Billing,
	}
	if in.StageRunID != "" {
		id, err := uuid.Parse(in.StageRunID)
		if err != nil {
			return fail(apperr.InvalidInput("stage_run_id не UUID"))
		}
		report.StageRunID = &id
	}
	// Токен задачи отчитывается только о своём прогоне.
	runID, err := s.auth.OwnRun(identity(ctx), in.TaskKey, report.StageRunID)
	if err != nil {
		return fail(err)
	}
	report.StageRunID = runID

	out, err := s.engine.ReportUsage(ctx, in.TaskKey, actor(ctx).ID, report)
	if err != nil {
		return fail(err)
	}
	usage, err := s.engine.TaskUsage(ctx, in.TaskKey)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"recorded": out, "task_usage": usage})
}

type assignTaskInput struct {
	TaskKey  string `json:"task_key" jsonschema:"ключ задачи"`
	Assignee string `json:"assignee,omitempty" jsonschema:"хэндл принципала; пусто — снять назначение"`
}

func (s *Server) assignTask(ctx context.Context, in assignTaskInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberAgent); err != nil {
		return fail(err)
	}
	if err := s.inProject(ctx, in.TaskKey); err != nil {
		return fail(err)
	}
	if err := s.auth.RequireWriteCircle(ctx, identity(ctx), in.TaskKey, "назначение"); err != nil {
		return fail(err)
	}
	task, err := s.engine.Assign(ctx, in.TaskKey, in.Assignee, actor(ctx).ID)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"key": task.Key, "assignee": task.AssigneeHandle})
}

// level — уровень актора в проекте: по нему движок решает, вправе ли агент
// заводить задачу где угодно или только внутри той, которую держит.
func (s *Server) level(ctx context.Context) (domain.MemberRole, error) {
	key, err := projectKey(ctx)
	if err != nil {
		return "", err
	}
	project, err := s.store.GetProjectByKey(ctx, s.store.DB(), key)
	if err != nil {
		return "", err
	}
	return s.auth.Level(ctx, identity(ctx), project.ID)
}

// registerWorkPrompt объявляет единую точку старта работы над задачей.
//
// Ею пользуются двое: человек, который говорит агенту «займись SDLC-42», и
// раннер, который поднимает саб-агента (решение 0025). Один промпт вместо
// двух формулировок — иначе агент, запущенный раннером, ведёт себя иначе,
// чем запущенный руками, и разница всплывает в артефактах.
func (s *Server) registerWorkPrompt() {
	prompt := &sdk.Prompt{
		Name:        "work_on_task",
		Description: "Взять задачу в работу: забрать этап, сделать работу роли, довести до перехода.",
		Arguments: []*sdk.PromptArgument{
			{Name: "task", Description: "ключ задачи, например SDLC-42", Required: true},
			{Name: "role", Description: "ключ роли, если этап адресован конкретной"},
			// Аргументы слэш-команды позиционные: usage идёт после role, и
			// раннер, который его ставит, обязан передать и роль.
			{Name: "usage", Description: "runner — стоимость сообщит раннер, который запустил агента; без него агент отчитывается сам"},
		},
	}
	s.addPrompt(prompt, func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		if err := requirePromptArguments(prompt, req); err != nil {
			return nil, err
		}
		args := req.Params.Arguments
		byRunner := strings.TrimSpace(args["usage"]) == usageByRunner
		text := workPromptText(args["task"], args["role"], byRunner)
		return &sdk.GetPromptResult{
			Description: "работа над задачей " + args["task"],
			Messages: []*sdk.PromptMessage{{
				Role:    "user",
				Content: &sdk.TextContent{Text: text},
			}},
		}, nil
	})
}

// requirePromptArguments отбивает prompts/get без обязательного аргумента
// ошибкой invalid params, как у тулов. SDK этого не делает, а промпт с пустым
// ключом задачи агент принял бы за настоящий и пошёл бы забирать «ничего»
// (CLAB-104).
func requirePromptArguments(prompt *sdk.Prompt, req *sdk.GetPromptRequest) error {
	var missing []string
	for _, arg := range prompt.Arguments {
		if !arg.Required {
			continue
		}
		if req.Params == nil || strings.TrimSpace(req.Params.Arguments[arg.Name]) == "" {
			missing = append(missing, arg.Name+" ("+arg.Description+")")
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &jsonrpc.Error{
		Code:    jsonrpc.CodeInvalidParams,
		Message: "промпту " + prompt.Name + " нужны аргументы: " + strings.Join(missing, ", "),
	}
}

// usageByRunner — значение аргумента usage промпта work_on_task: стоимость
// прогона сообщает раннер по выводу агента (CLAB-214). Любое другое значение
// даёт прежний текст: агент отчитается сам, и в худшем случае строк учёта
// будет две, а не ноль, и промпт не упадёт у раннера, который пришлёт
// значение новее сервера.
const usageByRunner = "runner"

// usageStep — шаг 7 промпта work_on_task: отчёт о стоимости или, под
// раннером, запрет на него.
func usageStep(byRunner bool) string {
	if byRunner {
		return `7. О стоимости не отчитывайся: не вызывай sdlc_report_usage и clab usage,
   даже если ответ захвата ждёт отчёта (usage_report). Тебя запустил
   раннер, он сообщит стоимость прогона по твоему выводу сам, и второй
   отчёт удвоил бы её.`
	}
	return `7. До перехода отчитайся о стоимости: sdlc_report_usage с полным id модели из
   сессии (claude-opus-5-5, а не алиас opus), токенами и деньгами. Переход
   отпускает прогон, и отчёт после него уже не примут; без отчёта прогон
   стоит «неизвестно».`
}

// workPromptText — текст промпта work_on_task. byRunner — агента поднял
// раннер и о стоимости отчитается сам: у прогона один источник учёта, и
// шаг отчёта агенту заменён запретом. Ответ захвата при этом всё равно
// приносит usage_report: expected, поэтому одного пропуска шага мало.
func workPromptText(task, role string, byRunner bool) string {
	head := "Возьми задачу " + task + " в работу."
	if role == "" {
		head = "Возьми задачу " + task + " в работу ролью, которой адресован её текущий этап."
	} else {
		head = "Возьми задачу " + task + " в работу ролью " + role + "."
	}
	return head + `

Порядок:

1. sdlc_claim_task — заберёт этап и выдаст аренду. В ответе приедут инструкция
   роли, входные артефакты предыдущих этапов, область репозиториев и
   требования гейтов впереди. Работай строго внутри области: выходить за неё
   нельзя, расширяет её человек.
2. Сверь модель с рекомендованной. Поле execution ответа захвата —
   рекомендованный уровень модели прогона: model_level (low — низкий,
   medium — средний, high — высокий) и model_level_source (stage, task, role,
   none). Уровню соответствует модель: low — haiku, medium — sonnet,
   high — opus. Твоя модель с уровнем расходится — скажи человеку одной
   строкой: «рекомендован уровень X (источник), вы работаете на Y» — и
   продолжай работу. Совпадает, уровень none или null, поля execution нет —
   ничего не говори. Твоя модель вне этой таблицы (другой провайдер) — назови
   уровень и свою модель, не оценивая совпадение. Модель не меняй, не
   останавливайся и разрешения продолжить не спрашивай: решает человек.
3. Делай то, что написано в инструкции роли, а не то, что кажется нужным.
   Незнакомое в коде и решениях ищи через sdlc_search_context.
4. Держи аренду живой: sdlc_heartbeat с интервалом из ответа захвата.
5. Если задача оказалась эпиком, нарежь её на дочерние задачи
   sdlc_create_task (parent — ключ этого эпика) и задай порядок sdlc_link_tasks.
6. Сдай результат sdlc_submit_artifact ровно тех видов, что перечислены в
   контракте роли. Код изменений живёт в PR: привяжи его sdlc_link_repo_ref.
` + usageStep(byRunner) + `
8. sdlc_check_gates покажет, что мешает переходу. Закрой мешающее и позови
   sdlc_request_transition.

Правила и данные. ` + trust.Rule + ` Разметка этих полей приезжает в
ответе захвата полем boundary.

Если гейт не проходит и починить это нельзя — не двигай задачу силой: сдай
описание проблемы артефактом вида problem.* и откати задачу назад тем
переходом, который для этого заведён в цикле. Если заблокирован и сам откат
— гейтом, который закрывает человек, — отпусти задачу sdlc_release с причиной
и остановись: решение за человеком. Так же, если переход держит предел,
который снимает человек, — попыток или перепроверок («перепроверок N из N»):
новой перепроверки не будет, отпусти задачу с причиной и остановись.`
}

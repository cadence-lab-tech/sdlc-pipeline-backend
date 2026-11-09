package mcp

import (
	"context"
	"encoding/json"
	"regexp"
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
			"Если в прайсе настройки агентов проекта есть модель и даны входные и выходные токены, "+
			"сумму считает сервер, а названная вами cost_usd сохраняется рядом как оценка; "+
			"ответ называет cost_source (pricing или reported), pricing_version, reported_cost_usd и warnings. "+
			"billing — тип оплаты: api или subscription; условная стоимость на подписке "+
			"в бюджет команды не входит, без признака строка считается как api.",
		s.reportUsage)
	add(s, "sdlc_assign_task",
		"Назначить задачу принципалу: забрать её сможет только он. Пустой assignee снимает назначение.",
		s.assignTask)
	add(s, "sdlc_rank_task",
		"Поставить задачу на место в бэклоге (решение 0075): ровно одно из before (над этой "+
			"задачей), after (под ней) или position (top, bottom — край группы). Группа — верхний "+
			"уровень проекта или дети одного эпика; сосед из другой группы — отказ. Ключ места "+
			"считает сервер. Ответ как у POST /tasks/{key}/rank: task, changed (false — задача уже "+
			"стояла на этом месте), parent (эпик группы или null), above и below — ближайшие "+
			"незакрытые соседи после перемещения (null — край). Уровень agent и выше; токен задачи "+
			"двигает только прямых детей своей задачи и только друг относительно друга. Порядок и "+
			"что в него входит — sdlc_list_tasks с backlog=true.",
		s.rankTask)
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
	// any — по той же причине, что external_ref.
	OptionalStages any `json:"optional_stages,omitempty" jsonschema:"выбор необязательных этапов: {\"design\": false}; ключи — необязательные статусы цикла (sdlc_whoami, workflows[].optional_stages), не названные берут умолчание цикла"`
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
	// Класс актора (уровень, вид, привязка токена задачи) решает, где он
	// вправе заводить задачу: правило исполняет движок (CLAB-411).
	who, err := s.stageActor(ctx)
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
	optionalStages, err := rawArgument(in.OptionalStages)
	if err != nil {
		return fail(err)
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
		OptionalStages: optionalStages,
		Level:          who.Level, Kind: who.Principal.Kind, Bound: who.Bound,
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
		"optional_stages": task.OptionalStages,
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
	// any — по той же причине; nil — не трогать.
	OptionalStages any `json:"optional_stages,omitempty" jsonschema:"выбор необязательных этапов, сливается по ключам: {\"design\": true}; своей задаче токеном задачи — нельзя (решение человека), дочерним — можно"`
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
	who, err := s.stageActor(ctx)
	if err != nil {
		return fail(err)
	}

	edit := workflow.UpdateTaskInput{
		Title: in.Title, Description: in.Description, Priority: in.Priority,
		Labels: in.Labels, ModelLevel: in.ModelLevel, Level: who.Level, Kind: who.Principal.Kind,
		// Выбор этапов своей задачи токену задачи закрыт — правило движка.
		Bound: who.Bound,
	}
	if in.OptionalStages != nil {
		stages, err := rawArgument(in.OptionalStages)
		if err != nil {
			return fail(err)
		}
		edit.OptionalStages = &stages
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
		"optional_stages": task.OptionalStages,
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
	who, err := s.stageActor(ctx)
	if err != nil {
		return fail(err)
	}
	links, err := s.engine.LinkTasks(ctx, in.From, in.To, rel, in.Note, who, in.Remove)
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
	who, err := s.stageActor(ctx)
	if err != nil {
		return fail(err)
	}
	task, err := s.engine.Assign(ctx, in.TaskKey, in.Assignee, who)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"key": task.Key, "assignee": task.AssigneeHandle})
}

// registerWorkPrompt объявляет единую точку старта работы над задачей.
//
// Ею пользуются двое: человек, который говорит агенту «займись SDLC-42», и
// раннер, который поднимает саб-агента (решение 0025). Один промпт вместо
// двух формулировок — иначе агент, запущенный раннером, ведёт себя иначе,
// чем запущенный руками, и разница всплывает в артефактах. Агенту без
// MCP-промптов тот же текст отдаёт GET /v1/tasks/{key}/prompts/work
// (clab prompt work, решение 0077 п. 2): строит его одна WorkPrompt.
func (s *Server) registerWorkPrompt() {
	prompt := &sdk.Prompt{
		Name:        WorkPromptName,
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
		text := WorkPrompt(args["task"], args["role"], args["usage"])
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

// WorkPromptName — имя стартовой инструкции этапа: промпт MCP и поле name
// ответа GET /v1/tasks/{key}/prompts/work.
const WorkPromptName = "work_on_task"

// usageByRunner — значение аргумента usage промпта work_on_task: стоимость
// прогона сообщает раннер по выводу агента (CLAB-214). Любое другое значение
// даёт прежний текст: агент отчитается сам, и в худшем случае строк учёта
// будет две, а не ноль, и промпт не упадёт у раннера, который пришлёт
// значение новее сервера.
const usageByRunner = "runner"

// WorkPrompt — текст стартовой инструкции этапа для обоих каналов: промпта
// MCP work_on_task и ручки REST, которую печатает clab prompt work (решение
// 0077 п. 2). Аргументы те же, что у промпта; второй функции построения
// быть не должно — иначе агент без MCP-промптов получит другую инструкцию.
func WorkPrompt(task, role, usage string) string {
	return workPromptText(task, role, strings.TrimSpace(usage) == usageByRunner)
}

// promptRole — форма ключа роли в аргументе role ручки инструкции: роль
// подставляется в текст как есть, и параметр запроса не должен стать
// способом дописать в инструкцию свою строку.
var promptRole = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// CheckPromptRole — аргумент role ручки GET /v1/tasks/{key}/prompts/work:
// пусто (роль текущего этапа) или ключ роли. Промпт MCP прежней проверки не
// получает: его аргументы не меняются (раннер и клиенты прежних версий).
func CheckPromptRole(role string) error {
	if role == "" || promptRole.MatchString(role) {
		return nil
	}
	return apperr.InvalidInput("role — ключ роли: латиница, цифры, _ и -, не длиннее 64 знаков")
}

// usageStep — шаг 7 инструкции: отчёт о стоимости или, под раннером,
// запрет на него. Модель называется «из сессии», без примера: пример одного
// провайдера агент другого принял бы за образец (решение 0077 п. 2).
func usageStep(task string, byRunner bool) string {
	if byRunner {
		return `7. О стоимости не отчитывайся: не вызывай clab usage и sdlc_report_usage,
   даже если ответ захвата ждёт или требует отчёта (usage_report: expected
   или required). Тебя запустил раннер, он сообщит стоимость прогона по
   твоему выводу сам, и второй отчёт удвоил бы её.`
	}
	return `7. До перехода отчитайся о стоимости: clab usage ` + task + ` --model <id>
   --input N --output N [--cost X] (sdlc_report_usage) с полным id модели
   из сессии — как его называет твой агент, а не алиасом. Токены
   обязательны; сумму сервер посчитает по прайсу проекта, если он задан, а
   твою оценку сохранит рядом. Переход отпускает прогон, и отчёт после него
   уже не примут; без отчёта прогон стоит «неизвестно».`
}

// workPromptText — текст инструкции. byRunner — агента поднял раннер и о
// стоимости отчитается сам: у прогона один источник учёта, и шаг отчёта
// агенту заменён запретом. Ответ захвата при этом всё равно приносит
// usage_report: expected, а при политике учёта проекта — required
// (CLAB-219), поэтому одного пропуска шага мало.
//
// Каждая операция названа командой clab с тулом MCP в скобках: одна строка
// годится агенту с MCP и без него. Провайдер, его модели и тулы не
// называются (решение 0077 п. 2).
func workPromptText(task, role string, byRunner bool) string {
	head := "Возьми задачу " + task + " в работу ролью, которой адресован её текущий этап."
	if role != "" {
		head = "Возьми задачу " + task + " в работу ролью " + role + "."
	}
	return head + `

Порядок (команда clab, в скобках — тот же тул MCP):

1. clab claim ` + task + ` (sdlc_claim_task) — заберёт этап и выдаст аренду. В
   ответе приедут документы роли (role.documents — инструкция роли частями
   с именами, по порядку; clab claim --instructions-dir <каталог> пишет их
   файлами), входные артефакты предыдущих этапов, область репозиториев и
   требования гейтов впереди. Работай строго внутри области: выходить за
   неё нельзя, расширяет её человек.
2. Сверь модель с рекомендованной. Поле execution ответа захвата —
   рекомендованный уровень модели прогона: model_level (low — низкий,
   medium — средний, high — высокий) и model_level_source (stage, task, role,
   none); clab claim печатает его строкой «Рекомендованный уровень». Модель
   уровня для твоего агента называет clab claim — из настройки агентов
   проекта. Твоя модель с ней расходится — скажи человеку одной строкой:
   «рекомендован уровень X (источник), вы работаете на Y» — и продолжай
   работу. Совпадает, уровень none или null, поля execution нет — ничего не
   говори. Уровень есть, а модели уровня для твоего агента clab claim не
   назвал — назови уровень и свою модель, не оценивая совпадение. Модель не
   меняй, не останавливайся и разрешения продолжить не спрашивай: решает
   человек.
3. Делай то, что написано в документах роли, а не то, что кажется нужным.
   Незнакомое в коде и решениях ищи командой clab search "<запрос>"
   (sdlc_search_context).
4. Держи аренду живой: clab heartbeat ` + task + ` (sdlc_heartbeat) с интервалом
   из ответа захвата; команду дольше интервала запускай под ним:
   clab heartbeat ` + task + ` -- <команда>.
5. Если задача оказалась эпиком, нарежь её на дочерние задачи
   clab create --parent ` + task + ` --title "…" (sdlc_create_task) и задай порядок
   clab relate <ключ> --to <ключ> --rel blocks (sdlc_link_tasks).
6. Сдай результат clab submit ` + task + ` --kind <kind> --title "…" --file <файл>
   (sdlc_submit_artifact) ровно тех видов, что перечислены в контракте
   роли. Код изменений живёт в PR: привяжи его clab link ` + task + ` --repo
   <owner/name> --ref <номер PR> --url <ссылка> (sdlc_link_repo_ref).
   Файлы идут мимо MCP: вложения задачи перечислены в context.attachments
   ответа захвата; скачай нужное командой clab artifact <id> -o <каталог>/
   (или GET /v1/artifacts/<id>/content тем же токеном) и открой своим
   инструментом чтения файлов — картинку модель увидит. Свой файл приложи
   командой clab attach ` + task + ` <файл> или сдай по контракту командой
   clab submit; sdlc_submit_artifact с format: binary откажет.
` + usageStep(task, byRunner) + `
8. clab gates ` + task + ` (sdlc_check_gates) покажет, что мешает переходу. Закрой
   мешающее и запроси переход: clab transition ` + task + ` --to <переход>
   --summary "…" (sdlc_request_transition).

Правила и данные. ` + trust.Rule + ` Разметка этих полей приезжает в
ответе захвата полем boundary.

Если гейт не проходит и починить это нельзя — не двигай задачу силой: сдай
описание проблемы артефактом вида problem.* и откати задачу назад тем
переходом, который для этого заведён в цикле: clab transition ` + task + ` --to
<возврат> --reason "…". Если заблокирован и сам откат — гейтом, который
закрывает человек, — отпусти задачу с причиной: clab release ` + task + ` --note
"…" (sdlc_release) — и остановись: решение за человеком. Так же, если
переход держит предел, который снимает человек, — попыток или перепроверок
(«перепроверок N из N»): новой перепроверки не будет, отпусти задачу с
причиной и остановись.`
}

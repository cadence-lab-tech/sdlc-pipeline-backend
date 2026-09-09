package mcp

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Тулы оркестрации (решения 0024-0026): оркестратор нарезает эпик на задачи,
// связывает их порядком и отчитывается о стоимости. Правила — в движке:
// агент заводит только детей задачи, которую держит, и только внутри её
// области; тул лишь передаёт уровень актора.

func (s *Server) registerOrchestrationTools() {
	add(s, "sdlc_create_task",
		"Завести задачу. Оркестратор так нарезает эпик: type задаёт цикл, parent вешает "+
			"задачу на эпик, blocks задаёт порядок, assignee адресует её исполнителю. "+
			"Агент вправе создавать только дочерние задачи той, которую держит, и только "+
			"внутри её области репозиториев.",
		s.createTask)
	add(s, "sdlc_link_tasks",
		"Связать задачи: blocks задаёт порядок (его читает гейт dependencies_closed), "+
			"relates_to, duplicates, shares_surface и split_from — контекст. remove=true снимает связь.",
		s.linkTasks)
	add(s, "sdlc_report_usage",
		"Отчитаться о стоимости прогона этапа: модель, токены, деньги, время. "+
			"Отчитывается тот, кто забирал этап, — сам агент или запустивший его раннер. "+
			"Без отчёта прогон стоит «неизвестно», а не ноль.",
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
}

func (s *Server) createTask(ctx context.Context, in createTaskInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberAgent); err != nil {
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
	task, err := s.engine.CreateTask(ctx, key, actor(ctx).ID, workflow.CreateTaskInput{
		CreateTaskInput: postgres.CreateTaskInput{
			ParentKey: in.Parent, TaskType: in.Type, Title: in.Title,
			Description: description, Priority: in.Priority, Labels: in.Labels,
			Properties: in.Properties, Repositories: in.Repositories,
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
	}
	if in.StageRunID != "" {
		id, err := uuid.Parse(in.StageRunID)
		if err != nil {
			return fail(apperr.InvalidInput("stage_run_id не UUID"))
		}
		report.StageRunID = &id
	}

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
	s.mcp.AddPrompt(&sdk.Prompt{
		Name:        "work_on_task",
		Description: "Взять задачу в работу: забрать этап, сделать работу роли, довести до перехода.",
		Arguments: []*sdk.PromptArgument{
			{Name: "task", Description: "ключ задачи, например SDLC-42", Required: true},
			{Name: "role", Description: "ключ роли, если этап адресован конкретной"},
		},
	}, func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		task, role := "", ""
		if req.Params != nil {
			task = req.Params.Arguments["task"]
			role = req.Params.Arguments["role"]
		}
		text := workPromptText(task, role)
		return &sdk.GetPromptResult{
			Description: "работа над задачей " + task,
			Messages: []*sdk.PromptMessage{{
				Role:    "user",
				Content: &sdk.TextContent{Text: text},
			}},
		}, nil
	})
}

func workPromptText(task, role string) string {
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
2. Делай то, что написано в инструкции роли, а не то, что кажется нужным.
   Незнакомое в коде и решениях ищи через sdlc_search_context.
3. Держи аренду живой: sdlc_heartbeat с интервалом из ответа захвата.
4. Если задача оказалась эпиком, нарежь её на дочерние задачи
   sdlc_create_task (parent — ключ этого эпика) и задай порядок sdlc_link_tasks.
5. Сдай результат sdlc_submit_artifact ровно тех видов, что перечислены в
   контракте роли. Код изменений живёт в PR: привяжи его sdlc_link_repo_ref.
6. sdlc_check_gates покажет, что мешает переходу. Закрой мешающее и позови
   sdlc_request_transition.
7. В конце отчитайся о стоимости: sdlc_report_usage с моделью, токенами и
   деньгами. Без отчёта прогон стоит «неизвестно».

Если гейт не проходит и починить это нельзя — не двигай задачу силой: сдай
описание проблемы артефактом вида problem.* и откати задачу назад тем
переходом, который для этого заведён в цикле.`
}

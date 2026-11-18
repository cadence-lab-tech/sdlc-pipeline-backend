package mcp

// Инстансовый адрес MCP (CLAB-483, решение 0079, п. 6): один адрес на все
// проекты, где человек участник. Проект выбирается на каждом вызове — сессии
// stateless, «выбрать проект один раз» нечем запомнить. Выбор кладёт проект в
// тот же контекст, что адрес проекта кладёт из пути, поэтому тулы, проверки
// уровня, права этапа и inProject не знают, каким адресом пришёл вызов.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// ProjectArg — имя аргумента выбора проекта у тулов и промпта import_rules.
const ProjectArg = "project"

// projectArgDescription — описание поля project в схеме входа каждого тула.
const projectArgDescription = "ключ проекта. Нужен на инстансовом адресе /mcp, если проект не виден " +
	"из ключа задачи в аргументах и токен не привязан к проекту; список проектов — sdlc_list_projects. " +
	"На адресе проекта /mcp/projects/<ключ> не нужен, а другой проект — invalid_input"

// missingProject — отказ вызова на инстансовом адресе, когда проект не нашёлся
// ни в аргументе, ни в ключе задачи, ни в привязке токена.
const missingProject = "укажите аргумент project (ключ проекта); список проектов — sdlc_list_projects"

// UnscopedTools — тулы вне проекта: у них нет поля project, и выбор проекта
// их не касается. Новый тул без project вне этого перечня валит тест.
var UnscopedTools = map[string]bool{listProjectsTool: true}

// UnscopedPrompts — промпты без аргумента project: work_on_task получает
// ключ задачи, проект в нём уже есть.
var UnscopedPrompts = map[string]bool{WorkPromptName: true}

// taskKeyTag — метка поля входа, в котором лежит ключ задачи под другим
// именем, чем task_key: по нему инстансовый адрес выбирает проект.
const taskKeyTag = "mcp"

// taskKeyFields — индексы полей входа с ключом задачи, по порядку
// объявления: task_key и поля с меткой mcp:"task_key". Первый непустой
// выбирает проект, остальные сверяет inProject в самом туле.
func taskKeyFields[In any]() [][]int {
	t := reflect.TypeFor[In]()
	if t.Kind() != reflect.Struct {
		return nil
	}
	var out [][]int
	for _, f := range reflect.VisibleFields(t) {
		if f.Anonymous || f.Type.Kind() != reflect.String {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "task_key" || f.Tag.Get(taskKeyTag) == "task_key" {
			out = append(out, f.Index)
		}
	}
	return out
}

// taskKeyOf — первый непустой ключ задачи во входе тула.
func taskKeyOf(in any, fields [][]int) string {
	v := reflect.ValueOf(in)
	for _, idx := range fields {
		if key := v.FieldByIndex(idx).String(); key != "" {
			return key
		}
	}
	return ""
}

// inputSchema — выведенная схема входа тула с необязательным полем project.
// Поле живёт только в схеме: тул его не читает, проект ему приносит контекст.
func inputSchema[In any]() *jsonschema.Schema {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		// Схему выводит и SDK при регистрации — та же ошибка там была бы
		// паникой на старте; здесь она та же.
		panic("схема входа тула: " + err.Error())
	}
	if schema.Properties == nil {
		schema.Properties = map[string]*jsonschema.Schema{}
	}
	if _, taken := schema.Properties[ProjectArg]; taken {
		panic("поле project во входе тула занято выбором проекта")
	}
	schema.Properties[ProjectArg] = &jsonschema.Schema{Type: "string", Description: projectArgDescription}
	if len(schema.PropertyOrder) > 0 {
		schema.PropertyOrder = append(schema.PropertyOrder, ProjectArg)
	}
	return schema
}

// projectArgOf — аргумент project вызова как он пришёл: схема уже проверила,
// что это строка.
func projectArgOf(req *sdk.CallToolRequest) string {
	if req == nil || req.Params == nil || len(req.Params.Arguments) == 0 {
		return ""
	}
	var args struct {
		Project string `json:"project"`
	}
	_ = json.Unmarshal(req.Params.Arguments, &args)
	return strings.TrimSpace(args.Project)
}

// pathProject — проект адреса проекта; пусто — вызов пришёл на инстансовый адрес.
func pathProject(ctx context.Context) string {
	key, _ := ctx.Value(keyPath).(string)
	return key
}

// withSelected кладёт проект, выбранный на вызове инстансового адреса: тулы
// читают его тем же projectKey, что проект из пути.
func withSelected(ctx context.Context, projectKey string) context.Context {
	return context.WithValue(ctx, keyProject, projectKey)
}

// selectProject — проект вызова (ADR 0079, п. 6). На адресе проекта — путь,
// а аргумент project, если есть, обязан с ним совпасть. На инстансовом
// адресе — аргумент project, иначе префикс ключа задачи из аргументов (разбор
// строки, без базы: существование задачи до проверки прав не раскрывается),
// иначе проект привязки токена (токен задачи, токен с project_id), иначе
// invalid_input с именем аргумента. Права здесь не проверяются: дальше тул
// работает так же, как на адресе проекта.
func (s *Server) selectProject(ctx context.Context, arg, taskKey string) (context.Context, error) {
	if path := pathProject(ctx); path != "" {
		if arg != "" && arg != path {
			return ctx, apperr.InvalidInput("аргумент project %q не совпадает с проектом адреса %s: "+
				"на адресе проекта аргумент не нужен", arg, path)
		}
		return ctx, nil
	}
	switch {
	case arg != "":
		return withSelected(ctx, arg), nil
	case taskKey != "":
		key, err := postgres.TaskProjectKey(taskKey)
		if err != nil {
			return ctx, err
		}
		return withSelected(ctx, key), nil
	}
	if id := identity(ctx); id.Token != nil && id.Token.ProjectID != nil && s.store != nil {
		key, err := s.store.ProjectKeyByID(ctx, s.store.DB(), *id.Token.ProjectID)
		if err != nil {
			return ctx, err
		}
		return withSelected(ctx, key), nil
	}
	return ctx, apperr.InvalidInput(missingProject)
}

// selectPromptProject — выбор проекта промпта: тот же порядок, что у тулов,
// но без ключа задачи и без ошибки на инстансовом адресе — промпт без
// проекта велит выбрать его (sdlc_list_projects) и передавать project.
func selectPromptProject(ctx context.Context, arg string) (context.Context, error) {
	if path := pathProject(ctx); path != "" {
		if arg != "" && arg != path {
			return ctx, apperr.InvalidInput("аргумент project %q не совпадает с проектом адреса %s", arg, path)
		}
		return ctx, nil
	}
	if arg != "" {
		return withSelected(ctx, arg), nil
	}
	return ctx, nil
}

// ── проекты участия ─────────────────────────────────────────────────────────

const listProjectsTool = "sdlc_list_projects"

func (s *Server) registerInstanceTools() {
	addUnscoped(s, listProjectsTool,
		"Проекты, где вызывающий участник, с его уровнем в каждом (level: viewer, agent, maintainer, "+
			"admin); глобальный администратор видит все. Тул вне проекта: аргумента project у него нет, "+
			"ответ одинаков на инстансовом адресе /mcp и на адресе проекта. Токен задачи видит только "+
			"проект своей задачи, токен с привязкой к проекту — только этот проект. Ключ из ответа — "+
			"аргумент project остальных тулов на инстансовом адресе. Заведения проектов через MCP нет.",
		s.listProjects)
}

type listProjectsInput struct{}

// listProjects — проекты участия, как GET /v1/projects (Auth.VisibleProjects),
// плюс уровень в каждом. Токен с привязкой к проекту сужает список до него —
// любой, не только токен задачи: вне своего проекта он не действует
// (Auth.Require), и показывать там нечего.
func (s *Server) listProjects(ctx context.Context, _ listProjectsInput) (*sdk.CallToolResult, any, error) {
	id := identity(ctx)
	projects, err := s.auth.VisibleProjects(ctx, id)
	if err != nil {
		return fail(err)
	}
	items := make([]map[string]any, 0, len(projects))
	for _, p := range projects {
		if id.Token != nil && id.Token.ProjectID != nil && *id.Token.ProjectID != p.ID {
			continue
		}
		level, err := s.auth.Level(ctx, id, p.ID)
		if err != nil {
			return fail(err)
		}
		if level == "" {
			continue
		}
		items = append(items, map[string]any{"key": p.Key, "name": p.Name, "level": level})
	}
	me := id.Principal
	return ok(map[string]any{
		"actor":    map[string]any{"handle": me.Handle, "kind": me.Kind},
		"projects": items,
	})
}

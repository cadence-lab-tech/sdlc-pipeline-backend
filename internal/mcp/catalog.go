package mcp

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/importguide"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Тулы переноса правил (решение 0022): агент читает правила команды из
// локального репозитория — CLAUDE.md, AGENTS.md, ADR, описания ролей — и
// кладёт их в проект как роли, документы и репозитории. Те же операции даёт
// REST и CLI; правила сравнения и версионирования живут в workflow.

func (s *Server) registerCatalogTools() {
	add(s, "sdlc_import_guide",
		"Как перенести существующие правила команды в проект: где их искать в репозитории, "+
			"во что они превращаются (роли, документы, репозитории, цикл) и как собрать связку "+
			"для sdlc_import_bundle. Позвать первым, если человек попросил «перенеси наши правила».",
		s.importGuide)
	add(s, "sdlc_export_bundle",
		"Выгрузить проект связкой: репозитории, роли с документами (roles[].documents — "+
			"части инструкции по порядку), документы проекта, активные циклы. "+
			"Связка годится для sdlc_import_bundle в другой проект.",
		s.exportBundle)
	add(s, "sdlc_import_bundle",
		"Применить связку к проекту. Роли — с документами (roles[].documents: name, kind, content); "+
			"форма связки — sdlc_import_guide. Идемпотентно: роль получает новую версию при изменении "+
			"любого документа, их состава, порядка или полей роли, тот же набор — unchanged; "+
			"документ проекта с тем же kind и title обновляется, цикл приходит черновиком. "+
			"dry_run=true отдаёт отчёт, ничего не меняя.",
		s.importBundle)
	add(s, "sdlc_add_repository",
		"Привязать к проекту репозиторий GitHub (owner/name или URL) с подсказкой роли: "+
			"backend, frontend, docs.",
		s.addRepository)
	add(s, "sdlc_put_role",
		"Завести роль специалиста или добавить версию инструкции существующей. "+
			"Версия — упорядоченный набор документов (documents: [{name, kind, content}], имя вида git.md); "+
			"instructions строкой — один документ main.md; оба поля сразу — ошибка. "+
			"Версия иммутабельна: правка любого документа — новая версия со всем набором, "+
			"тот же набор и поля — unchanged. Документ из библиотеки инструкций — ссылкой: "+
			"{instruction: \"git\", instruction_version: 2} без content и name (имя станет git.md, тип и текст — "+
			"из версии); документ содержимым с тем же текстом, что у закреплённого в прошлой версии, "+
			"закрепление сохраняет. model_level — уровень модели "+
			"роли по умолчанию (low, medium, high); уровень задачи и этапа его перекрывают. "+
			"model_hint устарел: строка модели для раннера прежних версий, вместо него — model_level.",
		s.putRole)
	add(s, "sdlc_get_role",
		"Роль с последней версией: документы инструкции по порядку (latest_version.documents) "+
			"и контракт вывода. У документа, закреплённого за библиотекой, — instruction {key, version}; "+
			"у роли, отстающей от библиотеки, — role.instruction_lag.",
		s.getRole)
	s.registerInstructionTools()
	add(s, "sdlc_put_document",
		"Положить проектный документ: содержимым, ссылкой или путём в репозитории. "+
			"Тот же kind и title — обновление.",
		s.putDocument)
	add(s, "sdlc_list_documents",
		"Проектные документы: API-registry, глоссарий, соглашения, правила.",
		s.listDocuments)
	add(s, "sdlc_get_document",
		"Содержимое проектного документа.",
		s.getDocument)
}

// registerPrompts объявляет промпт переноса правил: в Claude Code он
// доступен как /mcp__sdlc__import_rules и запускает сценарий целиком.
func (s *Server) registerPrompts() {
	s.addPrompt(&sdk.Prompt{
		Name: "import_rules",
		Description: "Перенести правила команды из локального репозитория в проект SDLC: " +
			"роли, документы, репозитории.",
		Arguments: []*sdk.PromptArgument{
			{Name: "path", Description: "корень репозитория с правилами; по умолчанию текущий каталог"},
			{Name: "repository", Description: "owner/name этого репозитория на GitHub, если известен"},
		},
	}, func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		path := "."
		repo := ""
		if req.Params != nil {
			if v := req.Params.Arguments["path"]; v != "" {
				path = v
			}
			repo = req.Params.Arguments["repository"]
		}
		text := "Перенеси правила команды из репозитория " + path + " в проект SDLC.\n\n" +
			ImportGuide
		if repo != "" {
			text += "\n\nРепозиторий на GitHub: " + repo + " — добавь его в связку первым."
		}
		return &sdk.GetPromptResult{
			Description: "перенос правил в проект",
			Messages: []*sdk.PromptMessage{{
				Role:    "user",
				Content: &sdk.TextContent{Text: text},
			}},
		}, nil
	})
}

type importGuideInput struct{}

func (s *Server) importGuide(ctx context.Context, _ importGuideInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: ImportGuide}},
	}, nil, nil
}

type exportInput struct{}

func (s *Server) exportBundle(ctx context.Context, _ exportInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	bundle, err := s.authoring.Export(ctx, key)
	if err != nil {
		return fail(err)
	}
	return ok(bundle)
}

type importInput struct {
	Bundle workflow.Bundle `json:"bundle" jsonschema:"связка: repositories, roles, documents, workflow"`
	DryRun bool            `json:"dry_run,omitempty" jsonschema:"только отчёт, без изменений"`
}

func (s *Server) importBundle(ctx context.Context, in importInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberMaintainer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	report, err := s.authoring.Import(ctx, key, actor(ctx).ID, in.Bundle, in.DryRun)
	if err != nil {
		return fail(err)
	}
	return ok(report)
}

type addRepositoryInput struct {
	Repository    string   `json:"repository" jsonschema:"owner/name или URL GitHub"`
	DefaultBranch string   `json:"default_branch,omitempty"`
	RoleHint      string   `json:"role_hint,omitempty" jsonschema:"backend | frontend | docs | infra — за что отвечает репозиторий"`
	WatchBranches []string `json:"watch_branches,omitempty" jsonschema:"ветки под индексацией; по умолчанию default_branch"`
	ExcludeGlobs  []string `json:"exclude_globs,omitempty"`
	IndexEnabled  *bool    `json:"index_enabled,omitempty"`
}

func (s *Server) addRepository(ctx context.Context, in addRepositoryInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberMaintainer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	repo, created, err := s.engine.AddRepository(ctx, key, workflow.AddRepositoryInput{
		Repository: in.Repository, DefaultBranch: in.DefaultBranch, RoleHint: in.RoleHint,
		WatchBranches: in.WatchBranches, ExcludeGlobs: in.ExcludeGlobs, IndexEnabled: in.IndexEnabled,
	})
	if err != nil {
		return fail(err)
	}
	outcome := "updated"
	if created {
		outcome = "created"
	}
	return ok(map[string]any{"repository": repo, "outcome": outcome})
}

type putRoleInput struct {
	Key            string                        `json:"key" jsonschema:"ключ роли: analyst, developer, reviewer"`
	Name           string                        `json:"name,omitempty"`
	Summary        string                        `json:"summary,omitempty"`
	Instructions   string                        `json:"instructions,omitempty" jsonschema:"инструкция одной строкой (markdown) — станет документом main.md; либо documents"`
	Documents      []workflow.BundleRoleDocument `json:"documents,omitempty" jsonschema:"документы версии по порядку отдачи: name (строчные, .md, до 64 символов), kind (instructions по умолчанию, checklist, reference…), content (markdown); из библиотеки — instruction (ключ) и instruction_version без content; либо instructions"`
	InputKinds     []string                      `json:"input_kinds,omitempty" jsonschema:"артефакты предыдущих этапов на вход"`
	OutputContract json.RawMessage               `json:"output_contract,omitempty" jsonschema:"[{kind,title,format,required,schema}]"`
	ToolPolicy     json.RawMessage               `json:"tool_policy,omitempty"`
	ModelLevel     string                        `json:"model_level,omitempty" jsonschema:"уровень модели роли по умолчанию: low, medium, high; пусто — не задан"`
	ModelHint      string                        `json:"model_hint,omitempty" jsonschema:"устарел (решение 0060): строка модели для раннера прежних версий; используйте model_level"`
	RepoScope      []string                      `json:"repo_scope,omitempty"`
	ContextBudget  json.RawMessage               `json:"context_budget,omitempty"`
	Changelog      string                        `json:"changelog,omitempty" jsonschema:"что изменилось относительно прошлой версии"`
}

func (s *Server) putRole(ctx context.Context, in putRoleInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberMaintainer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	report, err := s.authoring.Import(ctx, key, actor(ctx).ID, workflow.Bundle{
		Version: workflow.BundleVersion,
		Roles: []workflow.BundleRole{{
			Key: in.Key, Name: in.Name, Summary: in.Summary, Instructions: in.Instructions,
			Documents:  in.Documents,
			InputKinds: in.InputKinds, OutputContract: in.OutputContract, ToolPolicy: in.ToolPolicy,
			ModelHint: in.ModelHint, ModelLevel: in.ModelLevel, RepoScope: in.RepoScope,
			ContextBudget: in.ContextBudget,
			Changelog:     in.Changelog,
		}},
	}, false)
	if err != nil {
		return fail(err)
	}
	role, err := s.authoring.GetRole(ctx, key, in.Key)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"outcome": report.Roles[0], "role": role.Role, "latest_version": versionForModel(role.Latest)})
}

// versionForModel — версия роли для ответа тула: документы с содержимым и
// без склейки instructions (решение 0068, п. 4.3). Ответ тула читает модель,
// а не клиент со схемой, и склейка рядом с документами удвоила бы контекст.
func versionForModel(rv *domain.RoleVersion) any {
	if rv == nil {
		return nil
	}
	raw, err := json.Marshal(rv)
	if err != nil {
		return rv
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return rv
	}
	delete(out, "instructions")
	return out
}

type roleKeyInput struct {
	Key string `json:"key"`
}

func (s *Server) getRole(ctx context.Context, in roleKeyInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	role, err := s.authoring.GetRole(ctx, key, in.Key)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"role": role.Role, "latest_version": versionForModel(role.Latest), "used_on": role.UsedOn})
}

type putDocumentInput struct {
	Kind       string          `json:"kind" jsonschema:"guideline | adr | api_registry | glossary | runbook | …"`
	Title      string          `json:"title"`
	Format     string          `json:"format,omitempty" jsonschema:"markdown | json | yaml | text | html"`
	Content    string          `json:"content,omitempty" jsonschema:"содержимое; либо url; либо repository+repo_path"`
	URL        string          `json:"url,omitempty"`
	Repository string          `json:"repository,omitempty"`
	RepoPath   string          `json:"repo_path,omitempty"`
	Meta       json.RawMessage `json:"meta,omitempty"`
}

func (s *Server) putDocument(ctx context.Context, in putDocumentInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberMaintainer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	doc, outcome, err := s.engine.PutDocument(ctx, key, actor(ctx).ID, workflow.PutDocumentInput{
		Kind: in.Kind, Title: in.Title, Format: in.Format, Content: []byte(in.Content),
		URL: in.URL, Repository: in.Repository, RepoPath: in.RepoPath, Meta: in.Meta,
	})
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"document": doc, "outcome": outcome})
}

type listDocumentsInput struct {
	Kind string `json:"kind,omitempty" jsonschema:"оставить только этот kind"`
}

func (s *Server) listDocuments(ctx context.Context, in listDocumentsInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	key, _ := projectKey(ctx)
	docs, err := s.engine.ListDocuments(ctx, key)
	if err != nil {
		return fail(err)
	}
	if in.Kind != "" {
		filtered := docs[:0]
		for _, d := range docs {
			if d.Kind == in.Kind {
				filtered = append(filtered, d)
			}
		}
		docs = filtered
	}
	return ok(map[string]any{"documents": docs})
}

type documentInput struct {
	DocumentID string `json:"document_id"`
}

func (s *Server) getDocument(ctx context.Context, in documentInput) (*sdk.CallToolResult, any, error) {
	if err := s.allow(ctx, domain.MemberViewer); err != nil {
		return fail(err)
	}
	id, err := uuid.Parse(strings.TrimSpace(in.DocumentID))
	if err != nil {
		return fail(apperr.InvalidInput("document_id не UUID"))
	}
	key, _ := projectKey(ctx)
	doc, content, mediaType, err := s.engine.DocumentContent(ctx, key, id)
	if err != nil {
		return fail(err)
	}
	return ok(map[string]any{"document": doc, "media_type": mediaType, "content": string(content)})
}

// ImportGuide — инструкция агенту по переносу правил. Текст живёт в коде, а
// не в документации, потому что это часть контракта: агент получает его
// тулом или промптом и действует по нему. Источник один —
// internal/importguide: тот же текст печатает clab import-guide.
const ImportGuide = importguide.Text

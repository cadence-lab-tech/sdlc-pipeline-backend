// Package mcp — MCP-сервер для локальных агентов.
//
// Транспорт — streamable HTTP, привязанный к проекту:
// /mcp/projects/{PROJECT}. Тулы не содержат логики: они разбирают аргументы,
// зовут те же сервисы, что и REST, и сериализуют ответ (решение 0011).
//
// Сессии сознательно stateless: каждый POST несёт своего актора заголовком, и
// длинная сессия хранила бы актора первого запроса для всех последующих.
package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/apperr"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/domain"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/workflow"
)

const (
	// Version — версия сервера, которую видит клиент при initialize.
	Version = "0.4.0"
	// ServerName показывается в клиенте рядом с тулами.
	ServerName = "sdlc-pipeline"
)

type ctxKey int

const (
	keyProject ctxKey = iota
	keyIdentity
)

// WithProject кладёт в контекст проект, к которому привязано подключение.
// Проект приезжает из пути, а не из аргументов тула: агент подключается к
// конкретному проекту и не должен иметь возможности спутать их.
func WithProject(ctx context.Context, projectKey string) context.Context {
	return context.WithValue(ctx, keyProject, projectKey)
}

// WithIdentity кладёт в контекст того, кто пришёл: принципала и токен.
func WithIdentity(ctx context.Context, id workflow.Identity) context.Context {
	return context.WithValue(ctx, keyIdentity, id)
}

func projectKey(ctx context.Context) (string, error) {
	key, _ := ctx.Value(keyProject).(string)
	if key == "" {
		return "", apperr.InvalidInput("подключение не привязано к проекту")
	}
	return key, nil
}

func identity(ctx context.Context) workflow.Identity {
	id, _ := ctx.Value(keyIdentity).(workflow.Identity)
	return id
}

func actor(ctx context.Context) domain.Principal {
	return identity(ctx).Principal
}

type Server struct {
	log       *slog.Logger
	store     *postgres.Store
	engine    *workflow.Engine
	authoring *workflow.Authoring
	auth      *workflow.Auth
	mcp       *sdk.Server
}

func NewServer(log *slog.Logger, store *postgres.Store, engine *workflow.Engine, authoring *workflow.Authoring, auth *workflow.Auth) *Server {
	s := &Server{log: log, store: store, engine: engine, authoring: authoring, auth: auth}
	s.mcp = sdk.NewServer(&sdk.Implementation{
		Name:    ServerName,
		Title:   "SDLC Pipeline",
		Version: Version,
	}, nil)
	s.registerTools()
	s.registerCatalogTools()
	s.registerOrchestrationTools()
	s.registerPrompts()
	s.registerWorkPrompt()
	return s
}

// allow проверяет уровень прав актора в проекте подключения. Правило одно
// с REST (решение 0011): тул и ручка — одно действие с одними правами.
func (s *Server) allow(ctx context.Context, need domain.MemberRole) error {
	key, err := projectKey(ctx)
	if err != nil {
		return err
	}
	if !s.auth.Enforced() {
		return nil
	}
	project, err := s.store.GetProjectByKey(ctx, s.store.DB(), key)
	if err != nil {
		return err
	}
	return s.auth.Require(ctx, identity(ctx), project.ID, need)
}

// allowedRoles — роли специалистов, доступные актору в проекте подключения:
// рамки участия, суженные токеном (решение 0030).
func (s *Server) allowedRoles(ctx context.Context) ([]string, error) {
	key, err := projectKey(ctx)
	if err != nil {
		return nil, err
	}
	project, err := s.store.GetProjectByKey(ctx, s.store.DB(), key)
	if err != nil {
		return nil, err
	}
	return s.auth.AllowedRoles(ctx, identity(ctx), project.ID)
}

// Handler отдаёт HTTP-обработчик MCP. Проект и актор кладёт в контекст
// вызывающий транспорт: сервер один на процесс, а привязка — на запрос.
func (s *Server) Handler() http.Handler {
	return sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return s.mcp },
		&sdk.StreamableHTTPOptions{Stateless: true, Logger: s.log},
	)
}

// ok заворачивает ответ тула в текстовый JSON.
//
// Структурированный вывод потребовал бы объявить схему на каждый тул, а
// половина ответов — это документы задачи с открытой формой; агент читает их
// как JSON и без схемы.
func ok(v any) (*sdk.CallToolResult, any, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: string(body)}},
	}, nil, nil
}

// fail отдаёт ошибку домена тем же кодом, что и REST: агент разбирает ответ
// одинаково, каким бы транспортом ни пришёл (таблица кодов — в MCP_CONTRACT).
//
// Это не протокольная ошибка MCP, а результат вызова с IsError: иначе модель
// не увидит, что именно не так, и не сможет доделать работу.
func fail(err error) (*sdk.CallToolResult, any, error) {
	e := apperr.As(err)
	body := map[string]any{
		"error": map[string]any{
			"code":    string(e.Code),
			"message": e.Message,
		},
	}
	if e.Details != nil {
		body["error"].(map[string]any)["details"] = e.Details
	}
	text, _ := json.MarshalIndent(body, "", "  ")
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: string(text)}},
	}, nil, nil
}

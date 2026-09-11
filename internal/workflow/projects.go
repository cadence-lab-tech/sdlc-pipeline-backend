// Проект и его каталог: репозитории и проектные документы.
//
// До фазы 4 всё это заводилось сидом. Перенос правил из существующих
// репозиториев (решение 0022) требует писать каталог через API, и правила
// того, что считается тем же самым репозиторием или документом, живут здесь,
// а не в транспорте.
package workflow

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// CreateProject заводит проект и делает создателя его администратором:
// иначе в режиме токенов проект родился бы без единого участника.
func (e *Engine) CreateProject(ctx context.Context, actor uuid.UUID, in postgres.CreateProjectInput) (domain.Project, error) {
	var out domain.Project
	in.CreatedBy = actor
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		p, err := e.store.CreateProject(ctx, tx, in)
		if err != nil {
			return err
		}
		if _, err := e.store.SetMember(ctx, tx, p.ID, actor, domain.MemberAdmin, actor); err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}

// AddRepositoryInput — привязка репозитория. Repository — "owner/name" или
// полный URL GitHub: агент чаще знает адрес, чем координаты.
type AddRepositoryInput struct {
	Repository    string
	DefaultBranch string
	CloneURL      string
	Visibility    string
	RoleHint      string
	CredentialRef string
	WatchBranches []string
	ExcludeGlobs  []string
	IndexEnabled  *bool
	// Установка команды-владельца, которой читать репозиторий (решение
	// 0034). nil — подбирается среди живых установок команды та, которой
	// репозиторий доступен; ни одной — учётные данные проекта.
	InstallationID *int64
}

// ParseRepository приводит "owner/name", "github.com/owner/name" и
// "https://github.com/owner/name.git" к координатам.
func ParseRepository(raw string) (owner, name string, err error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "git@")
	s = strings.TrimPrefix(s, "github.com:")
	s = strings.TrimPrefix(s, "github.com/")
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", apperr.InvalidInput("репозиторий задаётся как owner/name, а не %q", raw)
	}
	return parts[0], parts[1], nil
}

func (e *Engine) AddRepository(ctx context.Context, projectKey string, in AddRepositoryInput) (domain.Repository, bool, error) {
	owner, name, err := ParseRepository(in.Repository)
	if err != nil {
		return domain.Repository{}, false, err
	}
	// Доступ проверяется до транзакции: это поход в сеть, и держать на нём
	// открытой транзакцию незачем. Репозиторий, до которого проект не
	// достаёт, привязывать нельзя — иначе прогон индексации будет падать
	// молча, а человек узнает об этом из счётчиков (решение 0028).
	project, err := e.store.GetProjectByKey(ctx, e.store.DB(), projectKey)
	if err != nil {
		return domain.Repository{}, false, err
	}
	installation := in.InstallationID
	switch {
	case installation != nil:
		inst, err := e.installationOf(ctx, project, *installation)
		if err != nil {
			return domain.Repository{}, false, err
		}
		if err := e.probeInstallation(ctx, inst.InstallationID, owner, name); err != nil {
			return domain.Repository{}, false, err
		}
	default:
		if installation, err = e.pickInstallation(ctx, project, owner, name); err != nil {
			return domain.Repository{}, false, err
		}
		if installation == nil {
			if err := e.checkRepoAccess(ctx, project, owner, name); err != nil {
				return domain.Repository{}, false, err
			}
		}
	}

	var out domain.Repository
	var created bool
	err = e.store.InTx(ctx, func(tx pgx.Tx) error {
		out, created, err = e.store.UpsertRepository(ctx, tx, postgres.RepositoryInput{
			ProjectID: project.ID, Owner: owner, Name: name,
			DefaultBranch: in.DefaultBranch, CloneURL: in.CloneURL, Visibility: in.Visibility,
			RoleHint: optional(in.RoleHint), CredentialRef: optional(in.CredentialRef),
			WatchBranches: in.WatchBranches, ExcludeGlobs: in.ExcludeGlobs,
			IndexEnabled: in.IndexEnabled, InstallationID: installation,
		})
		return err
	})
	return out, created, err
}

// PutDocumentInput — проектный документ. Ровно один из Content, URL и
// (Repository, RepoPath) задаёт источник.
type PutDocumentInput struct {
	Kind       string
	Title      string
	Format     string
	Content    []byte
	URL        string
	Repository string
	RepoPath   string
	Meta       []byte
}

// PutDocument заводит документ или обновляет содержимое существующего с тем
// же kind и title. Обновлённый документ воркер переразбивает на куски сам.
func (e *Engine) PutDocument(ctx context.Context, projectKey string, actor uuid.UUID, in PutDocumentInput) (domain.ProjectDocument, string, error) {
	var out domain.ProjectDocument
	var outcome string
	err := e.store.InTx(ctx, func(tx pgx.Tx) error {
		project, err := e.store.GetProjectByKey(ctx, tx, projectKey)
		if err != nil {
			return err
		}
		doc := postgres.DocumentInput{
			ProjectID: project.ID, Kind: strings.TrimSpace(in.Kind),
			Title: strings.TrimSpace(in.Title), Meta: in.Meta, CreatedBy: actor,
		}
		switch {
		case len(in.Content) > 0:
			sha, err := e.store.PutBlob(ctx, tx, in.Content, mediaTypeFor(in.Format))
			if err != nil {
				return err
			}
			doc.Source, doc.BlobSHA = "inline", &sha
		case in.URL != "":
			doc.Source, doc.URL = "url", optional(in.URL)
		case in.RepoPath != "":
			repo, err := e.store.FindRepository(ctx, tx, project.ID, in.Repository)
			if err != nil {
				return err
			}
			doc.Source, doc.RepositoryID, doc.RepoPath = "repo_path", &repo.ID, optional(in.RepoPath)
		default:
			return apperr.InvalidInput("у документа нужен источник: content, url или repository+repo_path")
		}
		out, outcome, err = e.store.UpsertDocument(ctx, tx, doc)
		return err
	})
	return out, outcome, err
}

func (e *Engine) ListDocuments(ctx context.Context, projectKey string) ([]domain.ProjectDocument, error) {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return nil, err
	}
	return e.store.ListProjectDocuments(ctx, q, project.ID)
}

// DocumentContent отдаёт документ вместе с содержимым, если оно лежит у нас.
// Документ по ссылке или по пути в репозитории содержимого здесь не имеет.
func (e *Engine) DocumentContent(ctx context.Context, projectKey string, id uuid.UUID) (domain.ProjectDocument, []byte, string, error) {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return domain.ProjectDocument{}, nil, "", err
	}
	doc, err := e.store.GetDocument(ctx, q, id)
	if err != nil {
		return doc, nil, "", err
	}
	if doc.ProjectID != project.ID {
		return doc, nil, "", apperr.NotFound("документ не принадлежит проекту %s", projectKey)
	}
	if doc.BlobSHA == nil {
		return doc, nil, "", nil
	}
	content, mediaType, err := e.store.BlobContent(ctx, q, *doc.BlobSHA)
	return doc, content, mediaType, err
}

func (e *Engine) DeleteDocument(ctx context.Context, projectKey string, id uuid.UUID) error {
	q := e.store.DB()
	project, err := e.store.GetProjectByKey(ctx, q, projectKey)
	if err != nil {
		return err
	}
	doc, err := e.store.GetDocument(ctx, q, id)
	if err != nil {
		return err
	}
	if doc.ProjectID != project.ID {
		return apperr.NotFound("документ не принадлежит проекту %s", projectKey)
	}
	return e.store.DeleteDocument(ctx, q, id)
}

func optional(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// mediaTypeFor — формат артефакта или документа в media type блоба.
func mediaTypeFor(format string) string {
	switch format {
	case "json":
		return "application/json"
	case "yaml":
		return "application/yaml"
	case "html":
		return "text/html"
	case "text":
		return "text/plain"
	case "diff":
		return "text/x-diff"
	default:
		return "text/markdown"
	}
}

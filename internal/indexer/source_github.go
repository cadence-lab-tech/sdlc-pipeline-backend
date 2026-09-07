package indexer

import (
	"context"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/github"
)

// gitHubSource — единственная сегодня реализация Source.
//
// Провайдер спрятан за интерфейсом не ради абстракции впрок, а потому что
// индексатору нужны ровно четыре операции: голова ветки, листинг дерева,
// содержимое файла и учётные данные репозитория. Всё остальное в API
// провайдера к индексации отношения не имеет.
type gitHubSource struct {
	client *github.Client
	// provider выбирает учётные данные: установку приложения у проекта или
	// именованный секрет. В базе лежат ссылки, значения — в окружении и в
	// памяти процесса (решение 0028).
	provider *github.Provider
}

func NewGitHubSource(provider *github.Provider) Source {
	client, err := provider.Client(context.Background(), github.Credentials{})
	if err != nil {
		// Общий клиент без учётных данных провайдер отдаёт всегда: ошибка
		// здесь означала бы сломанный конструктор, а не конфигурацию.
		panic(err)
	}
	return gitHubSource{client: client, provider: provider}
}

func (s gitHubSource) ForRepository(ctx context.Context, creds Credentials) (Source, error) {
	ref := creds.RepositoryRef
	if ref == "" {
		ref = creds.ProjectRef
	}
	client, err := s.provider.Client(ctx, github.Credentials{
		InstallationID: creds.InstallationID, SecretRef: ref,
	})
	if err != nil {
		return nil, err
	}
	return gitHubSource{client: client, provider: s.provider}, nil
}

func (s gitHubSource) Head(ctx context.Context, repo Repo, branch string) (string, error) {
	return s.client.Head(ctx, github.Repo{Owner: repo.Owner, Name: repo.Name}, branch)
}

func (s gitHubSource) Tree(ctx context.Context, repo Repo, commit string) ([]Entry, bool, error) {
	entries, truncated, err := s.client.Tree(ctx, github.Repo{Owner: repo.Owner, Name: repo.Name}, commit)
	if err != nil {
		return nil, false, err
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, Entry{Path: e.Path, SHA: e.SHA, Size: e.Size})
	}
	return out, truncated, nil
}

func (s gitHubSource) Blob(ctx context.Context, repo Repo, sha string) ([]byte, error) {
	return s.client.Blob(ctx, github.Repo{Owner: repo.Owner, Name: repo.Name}, sha)
}

// gitHubSource умеет и вторую половину: состояние PR и его проверок.
func (s gitHubSource) PullRequest(ctx context.Context, repo Repo, number int) (PullRequestState, error) {
	pr, err := s.client.PullRequest(ctx, github.Repo{Owner: repo.Owner, Name: repo.Name}, number)
	if err != nil {
		return PullRequestState{}, err
	}
	return PullRequestState{
		State: pr.State, Merged: pr.Merged, Draft: pr.Draft,
		URL: pr.HTMLURL, Head: pr.Head.SHA,
	}, nil
}

func (s gitHubSource) CheckRuns(ctx context.Context, repo Repo, sha string) ([]CheckRun, error) {
	runs, err := s.client.CheckRuns(ctx, github.Repo{Owner: repo.Owner, Name: repo.Name}, sha)
	if err != nil {
		return nil, err
	}
	out := make([]CheckRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, CheckRun{Name: r.Name, Status: r.Status, Conclusion: r.Conclusion})
	}
	return out, nil
}

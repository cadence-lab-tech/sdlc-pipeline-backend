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
	// secret переводит repositories.credential_ref в токен: в базе лежит имя
	// секрета, а не сам токен.
	secret func(ref string) string
}

func NewGitHubSource(client *github.Client, secret func(ref string) string) Source {
	if secret == nil {
		secret = func(string) string { return "" }
	}
	return gitHubSource{client: client, secret: secret}
}

func (s gitHubSource) ForRepository(credentialRef string) Source {
	if credentialRef == "" {
		return s
	}
	token := s.secret(credentialRef)
	if token == "" {
		return s
	}
	return gitHubSource{client: s.client.WithToken(token), secret: s.secret}
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

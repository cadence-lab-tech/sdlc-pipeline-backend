package indexer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
)

// Индексатор читает репозиторий теми же учётными данными, которыми его
// проверяет кнопка «Проверить доступ»: порядок решения 0034 задаёт
// github.CredentialRefs.Resolve, и своего у индексатора нет. Тест без базы:
// настоящий github.Provider, приложение и API провайдера — на httptest,
// секреты — подставная функция. Каким токеном ушёл запрос за головой
// ветки, видно по заголовку Authorization.

const missingInstallation = 404

type credentialProbe struct {
	srv *httptest.Server

	mu       sync.Mutex
	issued   []int64  // установки, на которые выписывался токен
	branches []string // Authorization запросов за головой ветки
}

func (p *credentialProbe) requests() (issued []int64, branches []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int64(nil), p.issued...), append([]string(nil), p.branches...)
}

func newCredentialProbe(t *testing.T) (*credentialProbe, Source) {
	t.Helper()
	p := &credentialProbe{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id int64
		if _, err := fmt.Sscanf(r.URL.Path, "/app/installations/%d/access_tokens", &id); err == nil && r.Method == http.MethodPost {
			p.mu.Lock()
			p.issued = append(p.issued, id)
			p.mu.Unlock()
			if id == missingInstallation {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": fmt.Sprintf("ghs_installation_%d", id), "expires_at": time.Now().Add(time.Hour),
			})
			return
		}
		if r.URL.Path == "/repos/acme/widget/branches/main" {
			p.mu.Lock()
			p.branches = append(p.branches, r.Header.Get("Authorization"))
			p.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"sha": "c0ffee"}})
			return
		}
		t.Errorf("неожиданный запрос %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(p.srv.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := github.NewApp(p.srv.URL, "12345", pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string]string{"repo-secret": "token-repo", "project-secret": "token-project"}
	provider := github.NewProvider(github.New(p.srv.URL, "token-instance"), app,
		func(ref string) string { return secrets[ref] })
	return p, NewGitHubSource(provider)
}

func TestSourceChoosesCredentialsInDecision0034Order(t *testing.T) {
	repoInstallation, projectInstallation := int64(11), int64(22)

	// Сценарии требований CLAB-31: что объявлено — каким токеном читается.
	cases := []struct {
		name  string
		creds Credentials
		want  string
	}{
		{"1 секрет репозитория побеждает всё", Credentials{
			RepositorySecret: "repo-secret", RepositoryInstallation: &repoInstallation,
			ProjectInstallation: &projectInstallation, ProjectSecret: "project-secret",
		}, "Bearer token-repo"},
		{"2 установка репозитория раньше секрета проекта", Credentials{
			RepositoryInstallation: &repoInstallation, ProjectSecret: "project-secret",
		}, "Bearer ghs_installation_11"},
		{"3 установка проекта раньше секрета проекта", Credentials{
			ProjectInstallation: &projectInstallation, ProjectSecret: "project-secret",
		}, "Bearer ghs_installation_22"},
		{"4 установка репозитория раньше установки проекта", Credentials{
			RepositoryInstallation: &repoInstallation, ProjectInstallation: &projectInstallation,
		}, "Bearer ghs_installation_11"},
		{"5 только секрет проекта", Credentials{ProjectSecret: "project-secret"}, "Bearer token-project"},
		{"6 ничего — токен инстанса", Credentials{}, "Bearer token-instance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, base := newCredentialProbe(t)
			src, err := base.ForRepository(context.Background(), tc.creds)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := src.Head(context.Background(), Repo{Owner: "acme", Name: "widget"}, "main"); err != nil {
				t.Fatal(err)
			}
			_, branches := probe.requests()
			if len(branches) != 1 || branches[0] != tc.want {
				t.Fatalf("голова ветки прочитана с %q, ждали %q", branches, tc.want)
			}
		})
	}
}

func TestSourceDoesNotFallBackWhenChosenCredentialsFail(t *testing.T) {
	ctx := context.Background()

	t.Run("секрет без значения", func(t *testing.T) {
		probe, base := newCredentialProbe(t)
		repoInstallation := int64(11)
		_, err := base.ForRepository(ctx, Credentials{
			RepositorySecret: "nowhere", RepositoryInstallation: &repoInstallation,
			ProjectSecret: "project-secret",
		})
		if err == nil || !strings.Contains(err.Error(), `секрет "nowhere" не найден`) {
			t.Fatalf("ждали «секрет не найден», получили %v", err)
		}
		// Ни установкой, ни секретом проекта, ни общим токеном не пробовали.
		if issued, branches := probe.requests(); len(issued) != 0 || len(branches) != 0 {
			t.Fatalf("при ненайденном секрете ушли запросы: токены %v, ветки %v", issued, branches)
		}
	})

	t.Run("установка без токена", func(t *testing.T) {
		probe, base := newCredentialProbe(t)
		missing := int64(missingInstallation)
		_, err := base.ForRepository(ctx, Credentials{
			RepositoryInstallation: &missing, ProjectSecret: "project-secret",
		})
		if !errors.Is(err, github.ErrNotFound) || !strings.Contains(err.Error(), "установка 404") {
			t.Fatalf("ждали ошибку установки 404, получили %v", err)
		}
		issued, branches := probe.requests()
		if len(issued) != 1 || issued[0] != missingInstallation {
			t.Fatalf("токен выписывался для %v, ждали одну установку %d", issued, missingInstallation)
		}
		// Секретом проекта репозиторий прочитать не пробовали.
		if len(branches) != 0 {
			t.Fatalf("после отказа установки репозиторий читали с %q", branches)
		}
	})
}

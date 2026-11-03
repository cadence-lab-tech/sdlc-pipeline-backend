package github_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github/githubtest"
)

// Чтение установок для опроса (CLAB-395): полный список читается
// постранично, неполный — ErrIncomplete, отказы приложения узнаются по
// errors.Is, а не по тексту.

func TestAppInstallationsPagesThroughList(t *testing.T) {
	srv := githubtest.New(t)
	for id := int64(1); id <= 250; id++ {
		srv.Installations[id] = &githubtest.Installation{Suspended: id == 7}
	}
	p := srv.Provider(t)

	got, err := p.AppInstallations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 250 {
		t.Fatalf("установок %d, ждали 250", len(got))
	}
	// 250 установок — три страницы по сто, а не запрос на установку.
	if n := srv.Requests(githubtest.KindList); n != 3 {
		t.Fatalf("страниц списка %d, ждали 3", n)
	}
	for _, i := range got {
		if i.Suspended() != (i.ID == 7) {
			t.Fatalf("установка %d: suspended=%v", i.ID, i.Suspended())
		}
	}
}

func TestAppInstallationsIncompleteIsError(t *testing.T) {
	srv := githubtest.New(t)
	// Ровно на предел страниц: последняя страница полная, и чем кончается
	// список, не видно.
	for id := int64(1); id <= 10_000; id++ {
		srv.Installations[id] = &githubtest.Installation{}
	}
	_, err := srv.Provider(t).AppInstallations(context.Background())
	if !errors.Is(err, github.ErrIncomplete) {
		t.Fatalf("список на пределе страниц: %v, ждали ErrIncomplete", err)
	}
}

func TestAppInstallationRefusals(t *testing.T) {
	ctx := context.Background()
	srv := githubtest.New(t)
	srv.Installations[1] = &githubtest.Installation{}
	p := srv.Provider(t)

	if _, err := p.AppInstallation(ctx, 2); !errors.Is(err, github.ErrNotFound) {
		t.Fatalf("неизвестная установка: %v, ждали ErrNotFound", err)
	}
	if got, err := p.AppInstallation(ctx, 1); err != nil || got.ID != 1 {
		t.Fatalf("установка 1: %+v, %v", got, err)
	}

	srv.Fail[githubtest.KindList] = http.StatusUnauthorized
	if _, err := p.AppInstallations(ctx); !errors.Is(err, github.ErrAppRejected) {
		t.Fatalf("401 на список: %v, ждали ErrAppRejected", err)
	}
	for _, code := range []int{http.StatusBadGateway, http.StatusTooManyRequests, http.StatusForbidden, githubtest.Drop} {
		srv.Fail[githubtest.KindList] = code
		_, err := p.AppInstallations(ctx)
		if err == nil || errors.Is(err, github.ErrAppRejected) || errors.Is(err, github.ErrNotFound) {
			t.Fatalf("отказ %d на список: %v", code, err)
		}
	}
}

func TestInstallationRepositoriesCompleteness(t *testing.T) {
	ctx := context.Background()
	srv := githubtest.New(t)
	inst := &githubtest.Installation{}
	for i := range 230 {
		inst.Repos = append(inst.Repos, "acme/r"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	srv.Installations[5] = inst
	p := srv.Provider(t)

	repos, err := p.InstallationRepositories(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 230 || srv.Requests(githubtest.KindRepos) != 3 {
		t.Fatalf("репозиториев %d за %d запросов", len(repos), srv.Requests(githubtest.KindRepos))
	}

	// Список короче total_count — сдвинулся между страницами: неполный.
	srv.UnderreportTotal = true
	if _, err := p.InstallationRepositories(ctx, 5); !errors.Is(err, github.ErrIncomplete) {
		t.Fatalf("total_count больше списка: %v, ждали ErrIncomplete", err)
	}
	srv.UnderreportTotal = false

	// Токен установки выписан один раз и переиспользован.
	if n := srv.Requests(githubtest.KindToken); n != 1 {
		t.Fatalf("токен выписан %d раз", n)
	}
	// Отказ чтения выбрасывает токен из кэша: следующий проход выпишет
	// свежий, а не будет до срока ходить с отозванным.
	srv.Fail[githubtest.KindRepos] = http.StatusUnauthorized
	if _, err := p.InstallationRepositories(ctx, 5); err == nil {
		t.Fatal("401 на репозитории установки принят")
	}
	delete(srv.Fail, githubtest.KindRepos)
	if _, err := p.InstallationRepositories(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if n := srv.Requests(githubtest.KindToken); n != 2 {
		t.Fatalf("после отказа токен выписан %d раз, ждали перевыписку", n)
	}
}

func TestProviderWithoutAppRefusesSync(t *testing.T) {
	p := github.NewProvider(github.New("", ""), nil, nil)
	if _, err := p.AppInstallations(context.Background()); err == nil {
		t.Fatal("опрос без приложения не отказал")
	}
	p.ForgetInstallationToken(1) // без приложения — ничего, без паники
}

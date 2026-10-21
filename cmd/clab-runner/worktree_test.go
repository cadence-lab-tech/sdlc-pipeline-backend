package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Рабочие копии проверяются на настоящем git: голый origin и клон в каталоге
// с именем, отличным от имени репозитория, как backend/ в рабочей
// директории Cadence Lab. Клон называет репозиторий адресом GitHub, а ходит
// в локальный origin через url.insteadOf.

const testURL = "https://github.com/acme/api.git"

var apiRepo = []claimRepo{{Owner: "acme", Name: "api", DefaultBranch: "main"}}

type testRepo struct {
	base   string
	origin string // голый репозиторий
	repos  string // SDLC_RUNNER_REPOS
	clone  string // клон acme/api в repos/backend
	root   string // SDLC_RUNNER_WORKTREES
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	base := t.TempDir()
	// Глобальный конфиг человека (подпись коммитов, хуки) тестам не нужен.
	gitconfig := filepath.Join(base, "gitconfig")
	if err := os.WriteFile(gitconfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	r := &testRepo{
		base:   base,
		origin: filepath.Join(base, "origin.git"),
		repos:  filepath.Join(base, "repos"),
	}
	r.clone = filepath.Join(r.repos, "backend")
	r.root = filepath.Join(r.repos, ".worktrees")

	mustGit(t, base, "init", "-q", "--bare", "-b", "main", r.origin)
	seed := filepath.Join(base, "seed")
	mustGit(t, base, "clone", "-q", r.origin, seed)
	commitFile(t, seed, "README.md", "init")
	mustGit(t, seed, "push", "-q", "origin", "HEAD:main")

	if err := os.MkdirAll(r.repos, 0o755); err != nil {
		t.Fatal(err)
	}
	r.addClone(t, "backend", testURL)
	return r
}

// addClone кладёт в каталог клонов ещё один клон того же origin под другим
// адресом — так один голый репозиторий изображает несколько.
func (r *testRepo) addClone(t *testing.T, dir, url string) string {
	t.Helper()
	path := filepath.Join(r.repos, dir)
	mustGit(t, r.base, "clone", "-q", r.origin, path)
	mustGit(t, path, "remote", "set-url", "origin", url)
	mustGit(t, path, "config", "url."+r.origin+".insteadOf", url)
	return path
}

// other — клон «другой машины»: пушит в origin в обход клона раннера.
func (r *testRepo) other(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "other")
	mustGit(t, r.base, "clone", "-q", r.origin, path)
	return path
}

func (r *testRepo) spaces() *workspaces { return newWorkspaces(r.repos, r.root, quietLog()) }

func (r *testRepo) copyPath(task string) string { return filepath.Join(r.root, task, "backend") }

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitOK(dir string, args ...string) bool {
	return exec.Command("git", append([]string{"-C", dir}, args...)...).Run() == nil
}

func commitFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", name)
	mustGit(t, dir, "commit", "-q", "-m", content)
	return mustGit(t, dir, "rev-parse", "HEAD")
}

// pushBranch заводит ветку задачи в клоне раннера и публикует её на origin.
func (r *testRepo) pushBranch(t *testing.T, branch string) {
	t.Helper()
	mustGit(t, r.clone, "branch", branch, "origin/main")
	mustGit(t, r.clone, "push", "-q", "origin", branch)
}

// commitOnBranch коммитит в ветку клона, не оставляя клон на ней.
func (r *testRepo) commitOnBranch(t *testing.T, branch, content string) string {
	t.Helper()
	mustGit(t, r.clone, "checkout", "-q", branch)
	sha := commitFile(t, r.clone, content+".txt", content)
	mustGit(t, r.clone, "checkout", "-q", "main")
	return sha
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestScanClonesByOrigin(t *testing.T) {
	r := newTestRepo(t)
	web := r.addClone(t, "frontend", "git@github.com:acme/web.git")
	if err := os.MkdirAll(filepath.Join(r.repos, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}

	found, err := scanClones(context.Background(), r.repos)
	if err != nil {
		t.Fatal(err)
	}
	if found["acme/api"] != r.clone || found["acme/web"] != web || len(found) != 2 {
		t.Fatalf("клоны: %v", found)
	}
	// Имя репозитория из API сравнивается без учёта регистра.
	if dir, err := r.spaces().clone(context.Background(), "Acme/API"); err != nil || dir != r.clone {
		t.Fatalf("клон по имени в другом регистре: %q, %v", dir, err)
	}
}

func TestPrepareFreshCopyIsDetachedOnDefaultBranch(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	space, err := r.spaces().prepare(ctx, "CLAB-1", apiRepo)
	if err != nil {
		t.Fatal(err)
	}
	if space.Dir != r.copyPath("CLAB-1") {
		t.Fatalf("агент одной области стартует в копии, а не в %s", space.Dir)
	}
	if got, want := mustGit(t, space.Dir, "rev-parse", "HEAD"), mustGit(t, r.clone, "rev-parse", "origin/main"); got != want {
		t.Fatalf("копия не на свежем origin/main: %s != %s", got, want)
	}
	if gitOK(space.Dir, "symbolic-ref", "-q", "HEAD") {
		t.Fatal("раннер не придумывает ветку: копия задачи без ветки стоит в detached HEAD")
	}
	if cur := mustGit(t, r.clone, "branch", "--show-current"); cur != "main" {
		t.Fatalf("основной клон переключён на %q", cur)
	}
}

// Возврат на доработку (UC-3): копия прошлого этапа убрана, ветка задачи
// есть только на origin — новый захват встаёт на неё.
func TestReturnGetsBranchFromOrigin(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	w := r.spaces()

	space, err := w.prepare(ctx, "CLAB-1", apiRepo)
	if err != nil {
		t.Fatal(err)
	}
	mustGit(t, space.Dir, "checkout", "-q", "-b", "clab/CLAB-1-invites")
	sha := commitFile(t, space.Dir, "api.go", "v1")
	mustGit(t, space.Dir, "push", "-q", "-u", "origin", "clab/CLAB-1-invites")
	w.cleanup(ctx, space)
	if exists(space.Dir) {
		t.Fatal("чистая запушенная копия должна быть убрана")
	}

	// Этап забирает другая машина: локальной ветки у неё нет.
	mustGit(t, r.clone, "branch", "-q", "-D", "clab/CLAB-1-invites")
	space, err = w.prepare(ctx, "CLAB-1", apiRepo)
	if err != nil {
		t.Fatal(err)
	}
	if head := mustGit(t, space.Dir, "rev-parse", "HEAD"); head != sha {
		t.Fatalf("копия возврата не на ветке задачи: %s != %s", head, sha)
	}
	if cur := mustGit(t, space.Dir, "branch", "--show-current"); cur != "clab/CLAB-1-invites" {
		t.Fatalf("копия возврата на ветке %q", cur)
	}
}

func TestLocalBranchBehindOriginIsFastForwarded(t *testing.T) {
	r := newTestRepo(t)
	r.pushBranch(t, "clab/CLAB-1-x")
	other := r.other(t)
	mustGit(t, other, "checkout", "-q", "clab/CLAB-1-x")
	sha := commitFile(t, other, "fix.txt", "правка в PR")
	mustGit(t, other, "push", "-q", "origin", "clab/CLAB-1-x")

	space, err := r.spaces().prepare(context.Background(), "CLAB-1", apiRepo)
	if err != nil {
		t.Fatal(err)
	}
	if head := mustGit(t, space.Dir, "rev-parse", "HEAD"); head != sha {
		t.Fatalf("локальная ветка не перемотана до origin: %s != %s", head, sha)
	}
	if cur := mustGit(t, r.clone, "branch", "--show-current"); cur != "main" {
		t.Fatalf("основной клон переключён на %q", cur)
	}
}

func TestLocalBranchAheadOfOriginIsKept(t *testing.T) {
	r := newTestRepo(t)
	r.pushBranch(t, "clab/CLAB-1-x")
	sha := r.commitOnBranch(t, "clab/CLAB-1-x", "незапушенное")

	space, err := r.spaces().prepare(context.Background(), "CLAB-1", apiRepo)
	if err != nil {
		t.Fatal(err)
	}
	if head := mustGit(t, space.Dir, "rev-parse", "HEAD"); head != sha {
		t.Fatalf("незапушенная работа потеряна: %s != %s", head, sha)
	}
}

func TestLocalOnlyBranchIsUsed(t *testing.T) {
	r := newTestRepo(t)
	mustGit(t, r.clone, "branch", "clab/CLAB-1-x", "origin/main")
	sha := r.commitOnBranch(t, "clab/CLAB-1-x", "только локально")

	space, err := r.spaces().prepare(context.Background(), "CLAB-1", apiRepo)
	if err != nil {
		t.Fatal(err)
	}
	if head := mustGit(t, space.Dir, "rev-parse", "HEAD"); head != sha {
		t.Fatalf("копия не на локальной ветке: %s != %s", head, sha)
	}
}

func TestDivergedBranchIsRefused(t *testing.T) {
	r := newTestRepo(t)
	r.pushBranch(t, "clab/CLAB-1-x")
	r.commitOnBranch(t, "clab/CLAB-1-x", "локально")
	other := r.other(t)
	mustGit(t, other, "checkout", "-q", "clab/CLAB-1-x")
	commitFile(t, other, "remote.txt", "на origin")
	mustGit(t, other, "push", "-q", "origin", "clab/CLAB-1-x")

	_, err := r.spaces().prepare(context.Background(), "CLAB-1", apiRepo)
	if err == nil || !strings.Contains(err.Error(), "clab/CLAB-1-x") || !strings.Contains(err.Error(), "разошлась") {
		t.Fatalf("расхождение веток обязано остановить запуск: %v", err)
	}
	if exists(r.copyPath("CLAB-1")) {
		t.Fatal("при расхождении копия не заводится")
	}
}

func TestTaskBranchMatchesByKeyAndDash(t *testing.T) {
	r := newTestRepo(t)
	mustGit(t, r.clone, "branch", "clab/CLAB-1-a", "origin/main")
	// Ветка CLAB-10 не принадлежит задаче CLAB-1.
	mustGit(t, r.clone, "branch", "clab/CLAB-10-z", "origin/main")
	if branch, err := taskBranch(context.Background(), r.clone, "CLAB-1"); err != nil || branch != "clab/CLAB-1-a" {
		t.Fatalf("ветка задачи: %q, %v", branch, err)
	}

	mustGit(t, r.clone, "branch", "clab/CLAB-1-b", "origin/main")
	_, err := r.spaces().prepare(context.Background(), "CLAB-1", apiRepo)
	if err == nil || !strings.Contains(err.Error(), "clab/CLAB-1-a") || !strings.Contains(err.Error(), "clab/CLAB-1-b") {
		t.Fatalf("две ветки задачи — выбирает человек: %v", err)
	}
}

// Уборка не теряет коммитов: убирается только копия, где нечего терять.
func TestCleanupKeepsWhatCanBeLost(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	w := r.spaces()
	prepare := func(task string) *workspace {
		space, err := w.prepare(ctx, task, apiRepo)
		if err != nil {
			t.Fatal(err)
		}
		return space
	}

	clean := prepare("CLAB-1")

	dirty := prepare("CLAB-2")
	if err := os.WriteFile(filepath.Join(dirty.Dir, "draft.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}

	unpushed := prepare("CLAB-3")
	mustGit(t, unpushed.Dir, "checkout", "-q", "-b", "clab/CLAB-3-x")
	commitFile(t, unpushed.Dir, "a.txt", "не запушено")

	detached := prepare("CLAB-4")
	lost := commitFile(t, detached.Dir, "b.txt", "вне ветки")

	for _, space := range []*workspace{clean, dirty, unpushed, detached} {
		w.cleanup(ctx, space)
	}

	if exists(clean.Dir) || exists(filepath.Join(r.root, "CLAB-1")) {
		t.Fatal("чистая копия без новых коммитов должна быть убрана вместе с каталогом задачи")
	}
	for _, space := range []*workspace{dirty, unpushed, detached} {
		if !exists(space.Dir) {
			t.Fatalf("копия %s убрана, а в ней было что терять", space.Task)
		}
	}
	if head := mustGit(t, detached.Dir, "rev-parse", "HEAD"); head != lost {
		t.Fatalf("коммит вне ветки потерян: %s != %s", head, lost)
	}
}

func TestLeftoverCopyIsReusedAsIs(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	w := r.spaces()

	space, err := w.prepare(ctx, "CLAB-1", apiRepo)
	if err != nil {
		t.Fatal(err)
	}
	sha := commitFile(t, space.Dir, "work.txt", "вне ветки")
	if err := os.WriteFile(filepath.Join(space.Dir, "draft.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.cleanup(ctx, space) // агент упал: копия остаётся

	again, err := w.prepare(ctx, "CLAB-1", apiRepo)
	if err != nil {
		t.Fatal(err)
	}
	if again.Dir != space.Dir {
		t.Fatalf("уцелевшая копия не переиспользована: %s != %s", again.Dir, space.Dir)
	}
	if head := mustGit(t, again.Dir, "rev-parse", "HEAD"); head != sha {
		t.Fatalf("уцелевшую копию сдвинули: %s != %s", head, sha)
	}
	if !exists(filepath.Join(again.Dir, "draft.go")) {
		t.Fatal("незакоммиченная работа в уцелевшей копии потеряна")
	}
}

func TestForeignDirectoryIsRefused(t *testing.T) {
	r := newTestRepo(t)
	if err := os.MkdirAll(r.copyPath("CLAB-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := r.spaces().prepare(context.Background(), "CLAB-1", apiRepo)
	if err == nil || !strings.Contains(err.Error(), "не рабочая копия") {
		t.Fatalf("чужой каталог на месте копии: %v", err)
	}
}

func TestMissingCloneIsRefusedAndCopiesRemoved(t *testing.T) {
	r := newTestRepo(t)
	repos := append([]claimRepo{}, apiRepo...)
	repos = append(repos, claimRepo{Owner: "acme", Name: "tap", DefaultBranch: "main"})

	_, err := r.spaces().prepare(context.Background(), "CLAB-1", repos)
	if err == nil || !strings.Contains(err.Error(), "acme/tap") {
		t.Fatalf("отказ обязан назвать репозиторий без клона: %v", err)
	}
	if exists(r.copyPath("CLAB-1")) {
		t.Fatal("копия, заведённая до отказа, должна быть убрана")
	}
}

func TestMultiRepoTaskStartsInTaskDir(t *testing.T) {
	r := newTestRepo(t)
	r.addClone(t, "frontend", "git@github.com:acme/web.git")
	repos := append([]claimRepo{}, apiRepo...)
	repos = append(repos, claimRepo{Owner: "acme", Name: "web", DefaultBranch: "main"})

	space, err := r.spaces().prepare(context.Background(), "CLAB-1", repos)
	if err != nil {
		t.Fatal(err)
	}
	if space.Dir != filepath.Join(r.root, "CLAB-1") {
		t.Fatalf("агент нескольких репозиториев стартует в каталоге задачи, а не в %s", space.Dir)
	}
	for _, dir := range []string{"backend", "frontend"} {
		if !exists(filepath.Join(space.Dir, dir, "README.md")) {
			t.Fatalf("нет копии %s", dir)
		}
	}
}

// Два слота в одном клоне не спотыкаются о блокировки git.
func TestParallelPrepareInOneClone(t *testing.T) {
	r := newTestRepo(t)
	w := r.spaces()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = w.prepare(context.Background(), "CLAB-"+string(rune('1'+i)), apiRepo)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("слот %d: %v", i, err)
		}
	}
}

func TestConfigWorkspaces(t *testing.T) {
	t.Setenv("SDLC_API_URL", "http://example.invalid/v1")
	t.Setenv("SDLC_PROJECT", "SDLC")
	t.Setenv("SDLC_RUNNER_WORKTREES", "")

	t.Setenv("SDLC_RUNNER_REPOS", "")
	t.Setenv("SDLC_RUNNER_SLOTS", "2")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "SDLC_RUNNER_REPOS") {
		t.Fatalf("несколько слотов без клонов обязаны остановить старт: %v", err)
	}

	t.Setenv("SDLC_RUNNER_SLOTS", "1")
	if cfg, err := loadConfig(); err != nil || cfg.ReposDir != "" {
		t.Fatalf("один слот без клонов — прежнее поведение: %+v, %v", cfg, err)
	}

	dir := t.TempDir()
	t.Setenv("SDLC_RUNNER_REPOS", dir)
	t.Setenv("SDLC_RUNNER_SLOTS", "3")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorktreesDir != filepath.Join(dir, ".worktrees") {
		t.Fatalf("каталог копий по умолчанию: %s", cfg.WorktreesDir)
	}

	t.Setenv("SDLC_RUNNER_REPOS", filepath.Join(dir, "missing"))
	if _, err := loadConfig(); err == nil {
		t.Fatal("несуществующий каталог клонов принят")
	}
}

// Сквозь work(): агент стартует в копии задачи, копия после него убрана;
// без клона агент не запускается, а задача отпускается с причиной.
func TestWorkRunsAgentInCopy(t *testing.T) {
	r := newTestRepo(t)

	var mu sync.Mutex
	var released []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/release") {
			body, _ := io.ReadAll(req.Body)
			mu.Lock()
			released = append(released, string(body))
			mu.Unlock()
		}
		if strings.HasSuffix(req.URL.Path, "/task-token") {
			_, _ = w.Write([]byte(`{"token":"clab_task_x","record":{"id":"t-1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	marker := filepath.Join(t.TempDir(), "agent.out")
	script := filepath.Join(t.TempDir(), "agent.sh")
	body := "#!/bin/sh\npwd -P > " + marker + "\necho \"$1\" >> " + marker + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	run := newRunner(Config{
		APIURL: srv.URL, Project: "SDLC", Actor: "agent:test",
		Command: script + " {workdir}", Timeout: time.Minute,
		ReposDir: r.repos, WorktreesDir: r.root,
	}, quietLog())

	item := queueItem{}
	item.Task.Key = "CLAB-1"
	run.work(context.Background(), item, claimResult{Context: claimContext{Repositories: apiRepo}})

	out, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("агент не запущен: %v", err)
	}
	lines := strings.Fields(string(out))
	wantDir := filepath.Join(realPath(r.repos), ".worktrees", "CLAB-1", "backend")
	if len(lines) != 2 || lines[0] != wantDir || lines[1] != r.copyPath("CLAB-1") {
		t.Fatalf("агент стартовал не в копии: %q, ждали %s", lines, wantDir)
	}
	if exists(r.copyPath("CLAB-1")) {
		t.Fatal("чистая копия после агента должна быть убрана")
	}
	if len(released) != 0 {
		t.Fatalf("успешный прогон не отпускает задачу: %v", released)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	item.Task.Key = "CLAB-2"
	run.work(context.Background(), item, claimResult{Context: claimContext{
		Repositories: []claimRepo{{Owner: "acme", Name: "tap", DefaultBranch: "main"}},
	}})
	if exists(marker) {
		t.Fatal("без клона агент не должен запускаться")
	}
	if len(released) != 1 || !strings.Contains(released[0], "acme/tap") {
		t.Fatalf("задача обязана вернуться в очередь с причиной: %v", released)
	}
}

// Без адреса раннер не стартует: подставлять dev-стек нельзя (CLAB-187).
func TestConfigRequiresAPIURL(t *testing.T) {
	t.Setenv("SDLC_PROJECT", "SDLC")
	t.Setenv("SDLC_RUNNER_REPOS", "")
	t.Setenv("SDLC_RUNNER_SLOTS", "")
	for _, v := range []string{"", "   "} {
		t.Setenv("SDLC_API_URL", v)
		_, err := loadConfig()
		if err == nil || !strings.Contains(err.Error(), "SDLC_API_URL") || !strings.Contains(err.Error(), "/v1") {
			t.Fatalf("SDLC_API_URL=%q: нужен отказ с именем переменной и форматом адреса, а не %v", v, err)
		}
		if strings.Contains(err.Error(), "SDLC_PROJECT") {
			t.Fatalf("SDLC_API_URL=%q: проверка адреса не та: %v", v, err)
		}
	}

	t.Setenv("SDLC_API_URL", "https://cadence.example.com/v1/")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIURL != "https://cadence.example.com/v1" {
		t.Fatalf("адрес взят не из окружения: %q", cfg.APIURL)
	}
}

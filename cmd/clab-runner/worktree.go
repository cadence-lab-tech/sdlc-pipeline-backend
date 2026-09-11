package main

// Рабочая копия на задачу. Без неё агент работал в каталоге раннера, и при
// нескольких слотах агенты разных задач переключали ветки друг у друга в
// одном checkout. Теперь на каждый захват заводится git worktree по каждому
// репозиторию области задачи, агент запускается в нём, а после работы копия
// убирается.
//
// Копия живёт один прогон, работу между этапами несёт ветка задачи на
// origin. Поэтому первый захват, возврат на доработку и повтор после
// падения агента идут одним путём: раннер не знает, почему этап захвачен
// снова, и не различает роли.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/gitremote"
)

type workspaces struct {
	repos string // каталог с клонами
	root  string // каталог рабочих копий
	log   *slog.Logger

	mu     sync.Mutex
	clones map[string]string      // owner/name в нижнем регистре → каталог клона
	locks  map[string]*sync.Mutex // каталог клона → замок на операции git
}

func newWorkspaces(repos, root string, log *slog.Logger) *workspaces {
	return &workspaces{
		repos: repos, root: root, log: log,
		clones: map[string]string{}, locks: map[string]*sync.Mutex{},
	}
}

// workspace — рабочие копии одной задачи и каталог, где стартует агент.
type workspace struct {
	Task   string
	Dir    string
	copies []repoCopy
}

type repoCopy struct {
	repo  string // owner/name
	clone string
	path  string
}

// dir — каталог запуска агента; пусто, если раннер работает без копий.
func (s *workspace) dir() string {
	if s == nil {
		return ""
	}
	return s.Dir
}

// Ключ задачи становится именем каталога и частью шаблона ветки, поэтому
// разделители путей и символы шаблонов в нём недопустимы.
var taskKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// prepare заводит копии задачи по каждому репозиторию её области.
func (w *workspaces) prepare(ctx context.Context, task string, repos []claimRepo) (*workspace, error) {
	if !taskKeyRe.MatchString(task) {
		return nil, fmt.Errorf("ключ задачи %q не годится для имени каталога и ветки", task)
	}
	taskDir := filepath.Join(w.root, task)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, fmt.Errorf("каталог задачи: %w", err)
	}

	space := &workspace{Task: task, Dir: taskDir}
	names := map[string]string{}
	for _, repo := range repos {
		c, err := w.prepareRepo(ctx, task, taskDir, repo, names)
		if err != nil {
			// Агент не запустится, поэтому уже заведённые копии убираются.
			// cleanup сам оставит ту, в которой есть что терять.
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			w.cleanup(cctx, space)
			cancel()
			return nil, err
		}
		space.copies = append(space.copies, c)
	}
	if len(space.copies) == 1 {
		// Агент одной области стартует в самой копии: так он подхватывает
		// CLAUDE.md репозитория, которого в каталоге задачи нет.
		space.Dir = space.copies[0].path
	}
	return space, nil
}

func (w *workspaces) prepareRepo(ctx context.Context, task, taskDir string, repo claimRepo, names map[string]string) (repoCopy, error) {
	full := repo.fullName()
	clone, err := w.clone(ctx, full)
	if err != nil {
		return repoCopy{}, err
	}
	name := filepath.Base(clone)
	if other, dup := names[name]; dup {
		return repoCopy{}, fmt.Errorf("клоны %s и %s лежат в каталогах с одним именем %q: копии задачи столкнулись бы",
			other, full, name)
	}
	names[name] = full
	c := repoCopy{repo: full, clone: clone, path: filepath.Join(taskDir, name)}

	unlock := w.lock(clone)
	defer unlock()

	if _, err := os.Stat(c.path); err == nil {
		ok, err := isWorktreeOf(ctx, clone, c.path)
		if err != nil {
			return repoCopy{}, fmt.Errorf("%s: %w", full, err)
		}
		if !ok {
			return repoCopy{}, fmt.Errorf("каталог %s уже есть, но это не рабочая копия клона %s", c.path, clone)
		}
		// Копия пережила прошлый прогон, потому что в ней было что терять
		// (см. cleanupCopy). Раннер её не трогает — ни fetch, ни перемотки:
		// агент продолжит с того состояния, в котором её оставили.
		w.log.Warn("уцелевшая рабочая копия переиспользуется как есть", "task", task, "path", c.path)
		return c, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return repoCopy{}, err
	}

	if _, err := git(ctx, clone, "fetch", "--prune", "origin"); err != nil {
		return repoCopy{}, fmt.Errorf("%s: %w", full, err)
	}
	if err := w.addCopy(ctx, task, clone, c.path, repo.DefaultBranch); err != nil {
		return repoCopy{}, fmt.Errorf("%s: %w", full, err)
	}
	return c, nil
}

// addCopy ставит копию на ветку задачи, если она есть, иначе — на свежий
// default branch без ветки. Имя ветки раннер только находит: slug из
// заголовка он не придумывает, ветку заводит агент по инструкции роли.
func (w *workspaces) addCopy(ctx context.Context, task, clone, path, defaultBranch string) error {
	branch, err := taskBranch(ctx, clone, task)
	if err != nil {
		return err
	}
	if branch == "" {
		base := "origin/HEAD"
		if defaultBranch != "" {
			base = "origin/" + defaultBranch
		}
		_, err := git(ctx, clone, "worktree", "add", "--detach", path, base)
		return err
	}

	local := revParse(ctx, clone, "refs/heads/"+branch)
	remote := revParse(ctx, clone, "refs/remotes/origin/"+branch)
	switch {
	case local == "":
		// Ветка только на origin: копию прошлого этапа убрали, или этап
		// перешёл с другой машины.
		_, err := git(ctx, clone, "worktree", "add", "--track", "-b", branch, path, "origin/"+branch)
		return err
	case remote == "":
		w.log.Warn("ветки задачи нет на origin, копия встаёт на локальную", "branch", branch, "clone", clone)
	case local == remote:
	case isAncestor(ctx, clone, local, remote):
		// Ветку дописали с другой стороны: другая машина или правка в PR.
		if _, err := git(ctx, clone, "branch", "-f", branch, remote); err != nil {
			return err
		}
	case isAncestor(ctx, clone, remote, local):
		w.log.Warn("локальная ветка задачи впереди origin, копия встаёт на неё", "branch", branch, "clone", clone)
	default:
		return fmt.Errorf("ветка %s разошлась с origin: локально %s, origin %s — свести их решает человек",
			branch, short(local), short(remote))
	}
	_, err = git(ctx, clone, "worktree", "add", path, branch)
	return err
}

// taskBranch находит ветку задачи `clab/<ключ>-…` локально и на origin.
// Шаблон с дефисом не даёт задаче CLAB-4 забрать ветку CLAB-48.
func taskBranch(ctx context.Context, clone, task string) (string, error) {
	out, err := git(ctx, clone, "for-each-ref", "--format=%(refname)",
		"refs/heads/clab/"+task, "refs/heads/clab/"+task+"-*",
		"refs/remotes/origin/clab/"+task, "refs/remotes/origin/clab/"+task+"-*")
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, ref := range strings.Fields(out) {
		name := strings.TrimPrefix(ref, "refs/heads/")
		name = strings.TrimPrefix(name, "refs/remotes/origin/")
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return "", nil
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("у задачи несколько веток (%s): какая из них её, решает человек",
			strings.Join(names, ", "))
	}
}

// cleanup убирает копии задачи после агента. Пустой каталог задачи уходит
// вместе с ними; непустой — значит, какая-то копия осталась.
func (w *workspaces) cleanup(ctx context.Context, space *workspace) {
	for _, c := range space.copies {
		w.cleanupCopy(ctx, space.Task, c)
	}
	_ = os.Remove(filepath.Join(w.root, space.Task))
}

// cleanupCopy убирает копию, только если в ней нечего терять: дерево
// чистое и HEAD достижим с origin. Чистая копия с коммитом в detached HEAD
// — не мусор: агент мог закоммитить, не заведя ветку, и после удаления
// копии коммит стал бы недостижим.
func (w *workspaces) cleanupCopy(ctx context.Context, task string, c repoCopy) {
	unlock := w.lock(c.clone)
	defer unlock()
	keep := func(reason string) {
		w.log.Warn("рабочая копия оставлена", "task", task, "path", c.path, "reason", reason)
	}

	if _, err := git(ctx, c.clone, "fetch", "--prune", "origin"); err != nil {
		keep("не проверить, есть ли коммиты на origin: " + err.Error())
		return
	}
	status, err := git(ctx, c.path, "status", "--porcelain")
	if err != nil {
		keep(err.Error())
		return
	}
	if status != "" {
		keep("незакоммиченные изменения")
		return
	}
	remotes, err := git(ctx, c.path, "branch", "-r", "--contains", "HEAD")
	if err != nil {
		keep(err.Error())
		return
	}
	if remotes == "" {
		if _, err := git(ctx, c.path, "symbolic-ref", "-q", "HEAD"); err == nil {
			keep("коммиты не запушены")
		} else {
			keep("коммиты вне ветки")
		}
		return
	}
	// Без --force: если git всё же найдёт, что терять, копия останется.
	if _, err := git(ctx, c.clone, "worktree", "remove", c.path); err != nil {
		keep(err.Error())
	}
}

// clone находит клон репозитория. Промах перечитывает каталог: человек мог
// положить клон, пока раннер работал.
func (w *workspaces) clone(ctx context.Context, repo string) (string, error) {
	key := strings.ToLower(repo)
	w.mu.Lock()
	dir, ok := w.clones[key]
	w.mu.Unlock()
	if ok {
		if _, err := os.Stat(dir); err == nil {
			return dir, nil
		}
	}

	found, err := scanClones(ctx, w.repos)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	w.clones = found
	w.mu.Unlock()
	if dir, ok := found[key]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("нет локального клона %s в %s", repo, w.repos)
}

// scanClones находит клоны по origin, а не по имени каталога: в рабочей
// директории Cadence Lab клоны лежат как backend/ и frontend/, а не по
// имени репозитория. Скрытые каталоги пропускаются, среди них каталог копий.
func scanClones(ctx context.Context, dir string) (map[string]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("каталог клонов: %w", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
			continue
		}
		// Адрес — как он записан в клоне: подмена url.insteadOf относится к
		// транспорту, а не к тому, какой это репозиторий.
		url, err := git(ctx, path, "config", "--get", "remote.origin.url")
		if err != nil {
			continue
		}
		owner, name, ok := gitremote.Parse(url)
		if !ok {
			continue
		}
		key := strings.ToLower(owner + "/" + name)
		if _, dup := out[key]; !dup {
			out[key] = path
		}
	}
	return out, nil
}

// lock сериализует операции git над одним клоном: fetch и worktree add из
// двух слотов одновременно спотыкаются о блокировки в .git.
func (w *workspaces) lock(clone string) func() {
	w.mu.Lock()
	l, ok := w.locks[clone]
	if !ok {
		l = &sync.Mutex{}
		w.locks[clone] = l
	}
	w.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func isWorktreeOf(ctx context.Context, clone, path string) (bool, error) {
	out, err := git(ctx, clone, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	want := realPath(path)
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok && realPath(p) == want {
			return true, nil
		}
	}
	return false, nil
}

// realPath разворачивает ссылки: git пишет пути развёрнутыми (/private/var
// на macOS), а настройки раннера могут их не разворачивать.
func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func revParse(ctx context.Context, dir, ref string) string {
	sha, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return sha
}

func isAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := git(ctx, dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// git зовёт git внешней командой: сборка раннера статическая, и библиотека
// ради десятка команд в зависимости не идёт. Запрос пароля выключен —
// раннер работает без человека у терминала и повис бы на нём.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(string(out)), nil
}

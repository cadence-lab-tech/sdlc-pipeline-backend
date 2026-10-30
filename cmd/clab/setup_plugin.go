package main

// clab setup --plugin — поставить мод Claude clab-plugin (CLAB-329, решение
// 0067). Мод вшит в clab (пакет clabplugin), его версия — версия clab.
// Команда раскладывает его в каталог-маркетплейс в данных clab пользователя,
// подключает маркетплейс к Claude (claude plugin marketplace add) и ставит
// плагин в области user.
//
// Это отдельный режим «только плагин»: плагин машинный и к проекту не
// привязан, поэтому ни входа, ни хоста, ни проекта, ни сети команде не нужно
// — она не делает ни одного запроса к серверу. Повтор безвреден: та же
// версия — ничего не меняется, другая — обновление. Токенов в каталоге нет:
// мод берёт личный токен у clab auth token сам, в памяти своей сессии.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/clabplugin"
)

// pluginMarketplace — имя каталога-маркетплейса в Claude. Не clab-mods: так
// звалась проба, и её маркетплейс человек снимет сам.
const pluginMarketplace = "cadence-lab"

// pluginID — плагин в Claude: <имя>@<маркетплейс>.
var pluginID = clabplugin.Name + "@" + pluginMarketplace

// Что сделано с маркетплейсом и плагином; те же слова — в --json. check —
// только в --dry-run, skipped — claude нет.
const (
	pluginAdded     = "added"
	pluginReplaced  = "replaced"
	pluginInstalled = "installed"
	pluginUpdated   = "updated"
	pluginUnchanged = "unchanged"
	pluginDisabled  = "disabled"
	pluginCheck     = "check"
	pluginSkipped   = "skipped"
)

type pluginReport struct {
	Plugin      string       `json:"plugin"`
	Version     string       `json:"version"`
	Dir         string       `json:"dir"`
	DryRun      bool         `json:"dry_run"`
	Files       []fileAction `json:"files"`
	Marketplace string       `json:"marketplace"`
	Install     string       `json:"install"`
	Previous    string       `json:"previous_version,omitempty"`
	Restart     bool         `json:"restart"`
	// Commands — команды claude: что будет сделано (--dry-run) или что
	// выполнить вручную (claude нет в PATH).
	Commands []string `json:"commands,omitempty"`
}

// errPluginFlags — --plugin не сочетается с флагами MCP и набора агента.
var errPluginFlags = errors.New("--plugin — отдельный режим «только плагин»: он не сочетается с --project, --scope, --agent, --name и --force. Плагин и MCP ставятся двумя запусками: clab setup --plugin и clab setup")

// clabDataDir — каталог данных clab пользователя: CLAB_DATA_DIR, иначе
// $XDG_DATA_HOME/clab, на Windows %LOCALAPPDATA%\clab, иначе
// ~/.local/share/clab.
func clabDataDir() (string, error) {
	if d := os.Getenv("CLAB_DATA_DIR"); d != "" {
		return filepath.Abs(d)
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "clab"), nil
	}
	if runtime.GOOS == "windows" {
		if l := os.Getenv("LOCALAPPDATA"); l != "" {
			return filepath.Join(l, "clab"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "clab"), nil
}

// pluginFiles — каталог-маркетплейс целиком: его манифест и мод в подкаталоге
// clab-plugin/. Пути — через /, относительно каталога.
func pluginFiles(v string) ([]clabplugin.File, error) {
	mod, err := clabplugin.Files(v)
	if err != nil {
		return nil, err
	}
	market, err := json.MarshalIndent(map[string]any{
		"name":  pluginMarketplace,
		"owner": map[string]string{"name": "Cadence Lab"},
		"plugins": []map[string]string{{
			"name":        clabplugin.Name,
			"source":      "./" + clabplugin.Name,
			"description": "Cadence Lab в Claude: задачи чата и их статус; ставит clab setup --plugin",
		}},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	files := []clabplugin.File{{Path: ".claude-plugin/marketplace.json", Content: append(market, '\n')}}
	for _, f := range mod {
		files = append(files, clabplugin.File{Path: clabplugin.Name + "/" + f.Path, Content: f.Content})
	}
	return files, nil
}

// planPlugin сверяет каталог с вшитым модом. Каталог — не файл человека: он
// приводится к вшитому целиком, правка руками не сохраняется, лишние файлы
// удаляются.
func planPlugin(dir string, files []clabplugin.File) ([]fileAction, error) {
	plan := []fileAction{}
	want := map[string]bool{}
	for _, f := range files {
		want[f.Path] = true
		sum := sha256.Sum256(f.Content)
		disk, exists, err := fileSum(filepath.Join(dir, filepath.FromSlash(f.Path)))
		if err != nil {
			return nil, err
		}
		a := fileAction{Path: f.Path, content: f.Content}
		switch {
		case !exists:
			a.Action = fileCreate
		case disk == hex.EncodeToString(sum[:]):
			a.Action = fileUnchanged
		default:
			a.Action = fileUpdate
		}
		plan = append(plan, a)
	}
	var extra []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == dir {
			return fs.SkipAll
		}
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !want[rel] {
			extra = append(extra, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(extra)
	for _, p := range extra {
		plan = append(plan, fileAction{Path: p, Action: fileDelete})
	}
	return plan, nil
}

func pluginChanged(plan []fileAction) bool {
	for _, a := range plan {
		if a.Action != fileUnchanged {
			return true
		}
	}
	return false
}

// manualPluginCommands — установка руками: те же два шага, что делает clab.
func manualPluginCommands(dir string) []string {
	return []string{
		"claude plugin marketplace add " + shellArg(dir),
		"claude plugin install " + pluginID + " --scope user",
	}
}

// shellArg — аргумент для вставки в оболочку: путь с пробелами и кавычками
// остаётся одним словом.
func shellArg(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r == '/' || r == '.' || r == '-' || r == '_' || r == '~' || r == ':' || r == '\\' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func cmdSetupPlugin(ctx context.Context, dryRun bool) error {
	data, err := clabDataDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(data, "plugin-marketplace")
	v := clabplugin.Version(version)
	files, err := pluginFiles(version)
	if err != nil {
		return err
	}
	plan, err := planPlugin(dir, files)
	if err != nil {
		return err
	}
	rep := &pluginReport{Plugin: pluginID, Version: v, Dir: dir, DryRun: dryRun, Files: plan}

	if dryRun {
		rep.Marketplace, rep.Install = pluginCheck, pluginCheck
		rep.Commands = manualPluginCommands(dir)
		return emit(rep, func() error { printPlugin(rep); return nil })
	}

	if pluginChanged(plan) {
		if err := applyPlugin(dir, plan); err != nil {
			return err
		}
	}

	claude, lookErr := exec.LookPath("claude")
	if lookErr != nil {
		rep.Marketplace, rep.Install = pluginSkipped, pluginSkipped
		rep.Commands = manualPluginCommands(dir)
		if err := emit(rep, func() error { printPlugin(rep); return nil }); err != nil {
			return err
		}
		return fmt.Errorf("claude не найден в PATH: плагин записан в %s, но не установлен — выполните команды выше", dir)
	}

	if err := installPlugin(ctx, claude, rep, pluginChanged(plan)); err != nil {
		return err
	}
	return emit(rep, func() error { printPlugin(rep); return nil })
}

// applyPlugin пишет каталог: каждый файл атомарно, лишние удаляет вместе с
// опустевшими каталогами.
func applyPlugin(dir string, plan []fileAction) error {
	for _, a := range plan {
		target := filepath.Join(dir, filepath.FromSlash(a.Path))
		switch a.Action {
		case fileCreate, fileUpdate:
			if err := writeAtomic(target, a.content); err != nil {
				return err
			}
		case fileDelete:
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			for d := filepath.Dir(target); d != dir && strings.HasPrefix(d, dir); d = filepath.Dir(d) {
				if os.Remove(d) != nil {
					break
				}
			}
		}
	}
	return nil
}

type claudeMarketplace struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type claudePlugin struct {
	ID      string `json:"id"`
	Version string `json:"version"`
	Scope   string `json:"scope"`
	Enabled bool   `json:"enabled"`
}

// installPlugin подключает маркетплейс и ставит или обновляет плагин по тому,
// что Claude отвечает о себе сам (claude plugin … list --json).
func installPlugin(ctx context.Context, claude string, rep *pluginReport, changed bool) error {
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, claude, args...)
		cmd.Stdin = nil
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = strings.TrimSpace(string(out))
			}
			return out, fmt.Errorf("claude %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return out, nil
	}

	var markets []claudeMarketplace
	out, err := run("plugin", "marketplace", "list", "--json")
	if err != nil {
		return err
	}
	if err := json.Unmarshal(out, &markets); err != nil {
		return fmt.Errorf("claude plugin marketplace list --json: %w", err)
	}
	rep.Marketplace = pluginAdded
	for _, m := range markets {
		if m.Name != pluginMarketplace {
			continue
		}
		if samePath(m.Path, rep.Dir) {
			rep.Marketplace = pluginUnchanged
			break
		}
		// Маркетплейс с этим именем смотрит в другой каталог (сменился
		// CLAB_DATA_DIR): его место занимает этот.
		if _, err := run("plugin", "marketplace", "remove", pluginMarketplace); err != nil {
			return err
		}
		rep.Marketplace = pluginReplaced
		break
	}
	if rep.Marketplace != pluginUnchanged {
		if _, err := run("plugin", "marketplace", "add", rep.Dir); err != nil {
			return err
		}
	}

	var plugins []claudePlugin
	if out, err = run("plugin", "list", "--json"); err != nil {
		return err
	}
	if err := json.Unmarshal(out, &plugins); err != nil {
		return fmt.Errorf("claude plugin list --json: %w", err)
	}
	var cur *claudePlugin
	for i := range plugins {
		if plugins[i].ID == pluginID && plugins[i].Scope == "user" {
			cur = &plugins[i]
		}
	}
	switch {
	case cur == nil:
		if _, err := run("plugin", "install", pluginID, "--scope", "user"); err != nil {
			return err
		}
		rep.Install, rep.Restart = pluginInstalled, true
	case cur.Version != rep.Version:
		if rep.Marketplace == pluginUnchanged || changed {
			if _, err := run("plugin", "marketplace", "update", pluginMarketplace); err != nil {
				return err
			}
		}
		if _, err := run("plugin", "update", pluginID, "--scope", "user"); err != nil {
			return err
		}
		rep.Install, rep.Previous, rep.Restart = pluginUpdated, cur.Version, true
	case !cur.Enabled:
		// Выключил человек — включать за него не будем, только подскажем.
		rep.Install = pluginDisabled
	default:
		rep.Install = pluginUnchanged
		rep.Restart = changed
	}
	return nil
}

func samePath(a, b string) bool {
	clean := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		return filepath.Clean(p)
	}
	return clean(a) == clean(b)
}

func printPlugin(rep *pluginReport) {
	if rep.DryRun {
		fmt.Println("--dry-run: ничего не записано, claude не вызывался.")
	}
	fmt.Printf("Плагин:  %s %s\n", rep.Plugin, rep.Version)
	fmt.Printf("Каталог: %s\n", rep.Dir)
	labels := fileLabels[rep.DryRun]
	changed := false
	for _, f := range rep.Files {
		if f.Action == fileUnchanged {
			continue
		}
		changed = true
		fmt.Printf("  %-10s %s\n", labels[f.Action], f.Path)
	}
	if !changed {
		fmt.Println("  файлы плагина актуальны")
	}
	switch rep.Install {
	case pluginCheck:
		fmt.Println("Команды claude (будут выполнены, если плагина ещё нет или он другой версии):")
		for _, c := range rep.Commands {
			fmt.Println("  " + c)
		}
	case pluginSkipped:
		fmt.Println("Claude: не найден в PATH — плагин не установлен. Установить вручную:")
		for _, c := range rep.Commands {
			fmt.Println("  " + c)
		}
	case pluginInstalled:
		fmt.Printf("Claude:  установлен в области user (маркетплейс %s). Откройте новую сессию Claude или /reload-plugins; панель — /clab-plugin.\n", pluginMarketplace)
	case pluginUpdated:
		fmt.Printf("Claude:  обновлён с %s до %s. Перезапустите Claude, чтобы применить; панель — /clab-plugin.\n", rep.Previous, rep.Version)
	case pluginDisabled:
		fmt.Printf("Claude:  установлен, но выключен — включить: claude plugin enable %s\n", pluginID)
	case pluginUnchanged:
		if rep.Restart {
			fmt.Println("Claude:  установлен; файлы плагина восстановлены — откройте новую сессию Claude или /reload-plugins.")
		} else {
			fmt.Println("Без изменений: плагин этой версии уже установлен.")
		}
	}
	if !rep.DryRun && rep.Install != pluginSkipped {
		fmt.Println("Вход: clab auth login — плагин ходит в Cadence Lab вашим личным токеном.")
	}
}

// pluginOnly — какие флаги, кроме --plugin и --dry-run, заданы явно.
func pluginOnly(fs *flag.FlagSet) error {
	bad := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name != "plugin" && f.Name != "dry-run" {
			bad = true
		}
	})
	if bad {
		return errPluginFlags
	}
	return nil
}

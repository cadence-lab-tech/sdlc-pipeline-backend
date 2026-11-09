package main

// Блок привязки, который пишет clab setup (решение 0077, п. 6). Функция не
// знает про агента: clab setup любого агента зовёт её одной точкой и пишет
// один и тот же блок.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Действия с файлами блока; те же слова — в --json (marker[].action).
const (
	markerCreate    = "create"
	markerUpdate    = "update"
	markerUnchanged = "unchanged"
	markerSkip      = "skip"
)

type markerAction struct {
	Path   string `json:"path"`
	Action string `json:"action"`
	// Target — файл, в который пишется блок, если Path — ссылка на него.
	Target string `json:"target,omitempty"`
	// Reason — почему файл пропущен.
	Reason string `json:"reason,omitempty"`
	// Diff — что меняется, построчно (+ и -); только у create и update.
	Diff string `json:"diff,omitempty"`

	content []byte
	mode    fs.FileMode
	write   string
}

// markerBlockLines — блок из четырёх строк. Только ключ проекта и адрес
// инстанса: ни токена, ни модели, ни путей конфига в нём быть не может.
func markerBlockLines(project, origin string) []string {
	return []string{
		markerBegin,
		"<!-- cadence-lab: project=" + project + " host=" + origin + " -->",
		"Cadence Lab: this directory works with project " + project + " at " + origin +
			"; the `clab` CLI takes the project from this block.",
		markerEnd,
	}
}

// planSetupMarker — что сделать с блоком в каталоге dir. names — файлы из
// --marker-file (пусто — каждый существующий из AGENTS.md и CLAUDE.md, а
// нет ни одного — AGENTS.md). Неверное имя — ошибка до любой записи.
func planSetupMarker(dir string, names []string, project, origin string) ([]markerAction, error) {
	for _, n := range names {
		if n != "AGENTS.md" && n != "CLAUDE.md" {
			return nil, fmt.Errorf("--marker-file: AGENTS.md или CLAUDE.md, а не %q", n)
		}
	}
	if len(names) == 0 {
		for _, n := range markerFiles {
			if _, err := os.Lstat(filepath.Join(dir, n)); err == nil {
				names = append(names, n)
			}
		}
		if len(names) == 0 {
			names = []string{"AGENTS.md"}
		}
	}
	block := markerBlockLines(project, origin)
	plan := []markerAction{}
	targets := map[string]bool{}
	for _, n := range names {
		path := filepath.Join(dir, n)
		a := markerAction{Path: path, write: path, mode: 0o644}
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			a.Action, a.content = markerCreate, []byte(strings.Join(block, "\n")+"\n")
			a.Diff = diffLines(nil, block)
		case err != nil:
			return nil, fmt.Errorf("%s: %w", path, err)
		default:
			if info.Mode()&os.ModeSymlink != 0 {
				target, err := filepath.EvalSymlinks(path)
				if err != nil {
					a.Action, a.Reason = markerSkip, "ссылка никуда не ведёт"
					plan = append(plan, a)
					continue
				}
				a.Target, a.write = target, target
				if info, err = os.Stat(target); err != nil {
					return nil, fmt.Errorf("%s: %w", path, err)
				}
			}
			if !info.Mode().IsRegular() {
				a.Action, a.Reason = markerSkip, "не обычный файл"
				plan = append(plan, a)
				continue
			}
			key := a.write
			if real, err := filepath.EvalSymlinks(a.write); err == nil {
				key = real
			}
			if targets[key] {
				// AGENTS.md — ссылка на CLAUDE.md (или наоборот): один файл,
				// блок в нём один.
				continue
			}
			targets[key] = true
			raw, err := os.ReadFile(a.write)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			a.mode = info.Mode().Perm()
			next, diff, reason := mergeMarkerBlock(raw, block)
			switch {
			case reason != "":
				a.Action, a.Reason = markerSkip, reason
			case string(next) == string(raw):
				a.Action = markerUnchanged
			default:
				a.Action, a.content, a.Diff = markerUpdate, next, diff
			}
		}
		if a.Action == markerCreate {
			targets[path] = true
		}
		plan = append(plan, a)
	}
	return plan, nil
}

// mergeMarkerBlock — содержимое файла с блоком. Блок есть — меняются только
// строки между begin и end включительно; нет — дописывается в конец после
// пустой строки. Маркер вне блока (записан руками) и повреждённый блок —
// файл не трогается: reason говорит почему. Переводы строк файла (CRLF)
// сохраняются.
func mergeMarkerBlock(raw []byte, block []string) (next []byte, diff, reason string) {
	text := string(raw)
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	var markers []int
	for _, l := range scanMarkerLines(lines) {
		switch l.kind {
		case markerLineBegin:
			if begin >= 0 {
				return nil, "", "два блока cadence-lab:begin — поправьте файл руками"
			}
			begin = l.index
		case markerLineEnd:
			if end >= 0 || begin < 0 {
				return nil, "", "блок повреждён: cadence-lab:end без begin — поправьте файл руками"
			}
			end = l.index
		case markerLineMarker:
			markers = append(markers, l.index)
		}
	}
	if begin >= 0 && end < 0 {
		return nil, "", "блок повреждён: cadence-lab:begin без end — поправьте файл руками"
	}
	for _, m := range markers {
		if begin < 0 || m < begin || m > end {
			return nil, "", "маркер вне блока cadence-lab:begin…end (записан руками): блок не дописан"
		}
	}
	crlf := strings.Contains(text, "\r\n")
	nl := "\n"
	if crlf {
		nl = "\r\n"
	}
	if begin < 0 {
		var b strings.Builder
		b.WriteString(text)
		switch {
		case text == "":
		case strings.HasSuffix(text, nl+nl) || text == nl:
		case strings.HasSuffix(text, "\n"):
			b.WriteString(nl)
		default:
			b.WriteString(nl + nl)
		}
		b.WriteString(strings.Join(block, nl) + nl)
		return []byte(b.String()), diffLines(nil, block), ""
	}
	out := make([]string, 0, len(lines)-(end-begin+1)+len(block))
	out = append(out, lines[:begin]...)
	for i, l := range block {
		if crlf && (i < len(block)-1 || strings.HasSuffix(lines[end], "\r")) {
			l += "\r"
		}
		out = append(out, l)
	}
	out = append(out, lines[end+1:]...)
	old := make([]string, 0, end-begin+1)
	for _, l := range lines[begin : end+1] {
		old = append(old, strings.TrimSuffix(l, "\r"))
	}
	return []byte(strings.Join(out, "\n")), diffLines(old, block), ""
}

// diffLines — разница блока для --dry-run: что уходит и что приходит.
func diffLines(old, next []string) string {
	var b strings.Builder
	for _, l := range old {
		b.WriteString("- " + l + "\n")
	}
	for _, l := range next {
		b.WriteString("+ " + l + "\n")
	}
	return b.String()
}

// applySetupMarker пишет план. Запись атомарна — временный файл рядом и
// переименование, права файла прежние, ссылка остаётся ссылкой: пишется
// файл, на который она ведёт.
func applySetupMarker(plan []markerAction) error {
	for _, a := range plan {
		if a.Action != markerCreate && a.Action != markerUpdate {
			continue
		}
		if err := writeAtomicMode(a.write, a.content, a.mode); err != nil {
			return fmt.Errorf("%s: %w", a.Path, err)
		}
	}
	return nil
}

// writeAtomicMode — writeAtomic с правами файла.
func writeAtomicMode(target string, content []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".clab-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

var markerLabels = map[bool]map[string]string{
	false: {markerCreate: "создан", markerUpdate: "обновлён", markerUnchanged: "без изменений", markerSkip: "пропущен"},
	true:  {markerCreate: "создать", markerUpdate: "обновить", markerUnchanged: "без изменений", markerSkip: "пропустить"},
}

// printSetupMarker — строки блока в выводе clab setup; с --dry-run — и
// разница.
func printSetupMarker(plan []markerAction, dryRun bool) {
	if len(plan) == 0 {
		return
	}
	fmt.Println("Привязка: блок Cadence Lab в файлах каталога")
	labels := markerLabels[dryRun]
	for _, a := range plan {
		line := fmt.Sprintf("  %-13s %s", labels[a.Action], a.Path)
		if a.Target != "" {
			line += " → " + a.Target
		}
		if a.Reason != "" {
			line += " — " + a.Reason
		}
		fmt.Println(line)
		if dryRun && a.Diff != "" {
			for _, d := range strings.Split(strings.TrimSuffix(a.Diff, "\n"), "\n") {
				fmt.Println("      " + d)
			}
		}
	}
}

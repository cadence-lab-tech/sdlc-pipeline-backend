package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Библиотека инструкций в каталоге связки (решение 0069, п. 7):
//
//	D/instructions/<ключ>/instruction.json  key, name, summary и versions
//	                                         [{version, kind, changelog}]
//	D/instructions/<ключ>/v<N>.md           содержимое версии N, байт в байт
//
// Строгость та же, что у каталога роли: .md без строки в instruction.json и
// строка без файла — ошибка с именем файла, неизвестное поле — ошибка,
// подкаталог и символьная ссылка не читаются; на сервер ничего не уходит.

const (
	bundleDirInstructions  = "instructions"
	instructionDirManifest = "instruction.json"
)

// instructionDirKey — форма ключа инструкции, та же, что у сервера (CHECK
// в 000040): она же не даёт ключу выйти за каталог.
var instructionDirKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,60}$`)

// instructionVersionFile — имя файла версии.
var instructionVersionFile = regexp.MustCompile(`^v([1-9][0-9]*)\.md$`)

type instructionDirVersion struct {
	Version   int    `json:"version"`
	Kind      string `json:"kind,omitempty"`
	Changelog string `json:"changelog,omitempty"`
}

type instructionManifest struct {
	Key      string                  `json:"key"`
	Name     string                  `json:"name"`
	Summary  string                  `json:"summary,omitempty"`
	Versions []instructionDirVersion `json:"versions"`
}

// planInstructions раскладывает instructions ответа экспорта по файлам.
func planInstructions(raw any) ([]bundleDirEntry, int, error) {
	if raw == nil {
		return nil, 0, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, 0, fmt.Errorf("ответ экспорта: instructions — не список")
	}
	var files []bundleDirEntry
	seen := map[string]bool{}
	for i, item := range list {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, 0, fmt.Errorf("ответ экспорта: instructions[%d] — не объект", i)
		}
		for k := range fields {
			switch k {
			case "key", "name", "summary", "versions":
			default:
				return nil, 0, fmt.Errorf("инструкция %v: сервер отдал поле %s, которого этот clab не знает — каталог его потерял бы; "+
					"обновите clab или выгрузите JSON (export без --dir)", fields["key"], k)
			}
		}
		key, _ := fields["key"].(string)
		if !instructionDirKey.MatchString(key) {
			return nil, 0, fmt.Errorf("инструкция %q: ключ не годится в имя каталога", key)
		}
		if seen[key] {
			return nil, 0, fmt.Errorf("инструкция %s повторяется", key)
		}
		seen[key] = true
		m := instructionManifest{Key: key}
		m.Name, _ = fields["name"].(string)
		m.Summary, _ = fields["summary"].(string)
		versions, _ := fields["versions"].([]any)
		if len(versions) == 0 {
			return nil, 0, fmt.Errorf("инструкция %s: в экспорте нет ни одной версии", key)
		}
		dir := bundleDirInstructions + "/" + key + "/"
		var docs []bundleDirEntry
		for j, rv := range versions {
			v, ok := rv.(map[string]any)
			if !ok {
				return nil, 0, fmt.Errorf("инструкция %s: versions[%d] — не объект", key, j)
			}
			for k := range v {
				switch k {
				case "version", "kind", "content", "changelog":
				default:
					return nil, 0, fmt.Errorf("инструкция %s: сервер отдал у версии поле %s, которого этот clab не знает — "+
						"обновите clab или выгрузите JSON (export без --dir)", key, k)
				}
			}
			n, err := bundleVersion(v["version"])
			if err != nil || n < 1 {
				return nil, 0, fmt.Errorf("инструкция %s: versions[%d] без номера версии", key, j)
			}
			content, ok := v["content"].(string)
			if !ok {
				return nil, 0, fmt.Errorf("инструкция %s: у версии %d нет содержимого", key, n)
			}
			kind, _ := v["kind"].(string)
			changelog, _ := v["changelog"].(string)
			m.Versions = append(m.Versions, instructionDirVersion{Version: n, Kind: kind, Changelog: changelog})
			docs = append(docs, bundleDirEntry{path: dir + "v" + strconv.Itoa(n) + ".md", body: []byte(content)})
		}
		body, err := encodeIndented(m)
		if err != nil {
			return nil, 0, fmt.Errorf("инструкция %s: %w", key, err)
		}
		files = append(files, bundleDirEntry{path: dir + instructionDirManifest, body: body})
		files = append(files, docs...)
	}
	return files, len(seen), nil
}

// readInstructionsDir читает instructions/ каталога связки: нет каталога —
// библиотеки в связке нет.
func readInstructionsDir(path string) ([]any, error) {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("каталог связки: %w", err)
	case !info.IsDir():
		return nil, fmt.Errorf("%s — не каталог (символьная ссылка не читается): инструкции лежат в %s/<ключ>/",
			path, bundleDirInstructions)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("каталог связки: %w", err)
	}
	var out []any
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		sub := filepath.Join(path, name)
		if !e.IsDir() {
			return nil, fmt.Errorf("%s: в %s/ лежат только каталоги инструкций (символьная ссылка не читается)",
				sub, bundleDirInstructions)
		}
		inst, err := readInstructionDir(sub, name)
		if err != nil {
			return nil, err
		}
		out = append(out, inst)
	}
	return out, nil
}

func readInstructionDir(dir, key string) (map[string]any, error) {
	if !instructionDirKey.MatchString(key) {
		return nil, fmt.Errorf("%s: имя каталога — ключ инструкции: строчные латинские буквы, цифры, «_» и «-»", dir)
	}
	manifestPath := filepath.Join(dir, instructionDirManifest)
	raw, err := readRegularFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("каталог инструкции %s: %w", dir, err)
	}
	var m instructionManifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: поля key, name, summary и versions [{\"version\": 1, \"kind\": \"instructions\", "+
			"\"changelog\": \"…\"}]: %w", manifestPath, err)
	}
	if m.Key != "" && m.Key != key {
		return nil, fmt.Errorf("%s: key %q не равен имени каталога %s/%s — каталог и есть ключ инструкции",
			manifestPath, m.Key, bundleDirInstructions, key)
	}
	if len(m.Versions) == 0 {
		return nil, fmt.Errorf("%s: в versions нет ни одной версии", manifestPath)
	}
	listed := map[int]bool{}
	for _, v := range m.Versions {
		if v.Version < 1 {
			return nil, fmt.Errorf("%s: номер версии — целое от 1", manifestPath)
		}
		if listed[v.Version] {
			return nil, fmt.Errorf("%s: версия %d указана дважды", manifestPath, v.Version)
		}
		listed[v.Version] = true
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("каталог инструкции %s: %w", dir, err)
	}
	present := map[int]bool{}
	var extra []string
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if e.IsDir() {
			return nil, fmt.Errorf("подкаталог %s в каталоге инструкции: вложенных каталогов не бывает", path)
		}
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("%s — символьная ссылка или не обычный файл: такие файлы не читаются", path)
		}
		match := instructionVersionFile.FindStringSubmatch(e.Name())
		if match == nil {
			extra = append(extra, path)
			continue
		}
		n, _ := strconv.Atoi(match[1])
		if !listed[n] {
			extra = append(extra, path)
			continue
		}
		present[n] = true
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("файл без строки в %s: %s — допишите версию в versions или уберите файл из каталога",
			manifestPath, strings.Join(extra, ", "))
	}

	versions := make([]map[string]any, 0, len(m.Versions))
	for _, v := range m.Versions {
		path := filepath.Join(dir, "v"+strconv.Itoa(v.Version)+".md")
		if !present[v.Version] {
			return nil, fmt.Errorf("%s: версия %d указана, а файла %s нет", manifestPath, v.Version, path)
		}
		content, err := readRegularFile(path)
		if err != nil {
			return nil, fmt.Errorf("версия %s: %w", path, err)
		}
		if !utf8.Valid(content) {
			return nil, fmt.Errorf("версия %s: содержимое — не UTF-8", path)
		}
		item := map[string]any{"version": v.Version, "content": string(content)}
		if v.Kind != "" {
			item["kind"] = v.Kind
		}
		if v.Changelog != "" {
			item["changelog"] = v.Changelog
		}
		versions = append(versions, item)
	}
	out := map[string]any{"key": key, "name": m.Name, "versions": versions}
	if m.Summary != "" {
		out["summary"] = m.Summary
	}
	return out, nil
}

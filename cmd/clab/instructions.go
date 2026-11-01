package main

// Документы роли в ответе захвата (решение 0068, п. 4): печать с шапками,
// раскладка по файлам (--instructions-dir) и склейка одним файлом
// (--instructions-file). Сервер прежней версии документов не отдаёт —
// тогда роль читается одним main.md из instructions.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// roleDocView — документ роли из ответа захвата и /state.
type roleDocView struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Position  int     `json:"position"`
	SHA256    string  `json:"sha256"`
	SizeBytes int64   `json:"size_bytes"`
	Content   *string `json:"content"`
}

// mainDocument — имя единственного документа роли, пришедшей одной строкой.
const mainDocument = "main.md"

// docFileName — форма имени, под которой документ ложится файлом. Имя
// пришло от сервера, то есть это данные: форма сама исключает «/», «\»,
// ведущую точку и выход из каталога. Длину держит сервер.
var docFileName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*\.md$`)

// documentHeader — шапка документа в секции инструкции clab claim. Формат
// читают скилы и тестировщик, поэтому он задан одним местом.
func documentHeader(i, n int, d roleDocView) string {
	sha := d.SHA256
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return fmt.Sprintf("--- документ %d из %d: %s (%s, %d байт, sha256 %s) ---", i, n, d.Name, dash(d.Kind), d.SizeBytes, dash(sha))
}

// printDocuments — документы роли по порядку, каждый со шапкой и
// содержимым как есть; перевод строки после содержимого без него
// добавляется только в вывод.
func printDocuments(docs []roleDocView) {
	for i, d := range docs {
		fmt.Println(documentHeader(i+1, len(docs), d))
		text := ptr(d.Content)
		fmt.Print(text)
		if !strings.HasSuffix(text, "\n") {
			fmt.Println()
		}
	}
}

// stageDocuments — документы для записи в каталог: ответ без документов
// (сервер прежней версии) или с пустым списком — один main.md из
// instructions.
func stageDocuments(s stateView) []roleDocView {
	if len(s.Role.Documents) > 0 {
		return s.Role.Documents
	}
	text := s.Role.Instructions
	sum := sha256.Sum256([]byte(text))
	return []roleDocView{{Name: mainDocument, Kind: "instructions", SHA256: hex.EncodeToString(sum[:]),
		SizeBytes: int64(len(text)), Content: &text}}
}

// writeDocuments раскладывает документы роли файлами <dir>/<имя> байт в байт
// и отвечает путями записанного. Все проверки — до первой записи: имя не той
// формы, повтор имени, нет содержимого или sha256 не сходится — ни один файл
// не пишется. Каталог создаётся; файлы того же имени перезаписываются, чужие
// не трогаются.
func writeDocuments(s stateView, dir string) ([]string, error) {
	if s.Role == nil {
		return nil, fmt.Errorf("в ответе захвата нет роли: документы в %s писать нечего", dir)
	}
	docs := stageDocuments(s)
	seen := map[string]int{}
	for i, d := range docs {
		n := i + 1
		if !docFileName.MatchString(d.Name) {
			return nil, fmt.Errorf("документ %d (%q): имя не годится для файла (ждали %s), в %s ничего не записано",
				n, d.Name, docFileName, dir)
		}
		if first, dup := seen[d.Name]; dup {
			return nil, fmt.Errorf("документ %d (%q): имя уже у документа %d, в %s ничего не записано", n, d.Name, first, dir)
		}
		seen[d.Name] = n
		if d.Content == nil {
			return nil, fmt.Errorf("документ %d (%q): в ответе нет содержимого, в %s ничего не записано", n, d.Name, dir)
		}
		sum := sha256.Sum256([]byte(*d.Content))
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, d.SHA256) {
			return nil, fmt.Errorf("документ %d (%q): sha256 содержимого %s не совпадает с ответом %s, в %s ничего не записано",
				n, d.Name, got, dash(d.SHA256), dir)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("каталог документов роли %s не создан: %w", dir, err)
	}
	paths := make([]string, 0, len(docs))
	for i, d := range docs {
		path := filepath.Join(dir, d.Name)
		if err := os.WriteFile(path, []byte(*d.Content), 0o644); err != nil {
			return paths, fmt.Errorf("документ %d (%q) не записан в %s: %w", i+1, d.Name, path, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// writeInstructions пишет склейку role.instructions в файл байт в байт и
// отвечает длиной в символах.
func writeInstructions(s stateView, path string) (int, error) {
	if s.Role == nil {
		return 0, fmt.Errorf("в ответе захвата нет роли: инструкцию в %s писать нечего", path)
	}
	if err := os.WriteFile(path, []byte(s.Role.Instructions), 0o644); err != nil {
		return 0, fmt.Errorf("инструкция роли не записана в %s: %w", path, err)
	}
	return utf8.RuneCountInString(s.Role.Instructions), nil
}

// documentsLine — состав роли для строки «Роль:» в clab show: имя, тип и
// размер, без содержимого.
func documentsLine(docs []roleDocView) string {
	items := make([]string, len(docs))
	for i, d := range docs {
		items[i] = fmt.Sprintf("%s (%s, %d байт)", d.Name, dash(d.Kind), d.SizeBytes)
	}
	return strings.Join(items, ", ")
}

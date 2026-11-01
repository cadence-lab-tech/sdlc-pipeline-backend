package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// CLAB-344: документы роли в clab claim и clab show (решение 0068, п. 4).
// Номера — критерии приёмки анализа CLAB-344.

type testDoc struct{ name, kind, content string }

var claimDocs = []testDoc{
	{"main.md", "instructions", longInstructions},
	{"git.md", "reference", "Ветки clab/<ключ>-<slug>.\nИсторию не переписывать.\n"},
	{"selfhosted.md", "checklist", "- пакет учтён — ёж 🦔"},
}

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// concat — склейка по правилу 0068 п. 4.1, как её отдаёт сервер.
func concat(docs []testDoc) string {
	if len(docs) == 1 {
		return docs[0].content
	}
	var b strings.Builder
	for i, d := range docs {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("<!-- document: " + d.name + " -->\n" + d.content)
		if !strings.HasSuffix(d.content, "\n") {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func docsJSON(docs []testDoc) []map[string]any {
	out := make([]map[string]any, len(docs))
	for i, d := range docs {
		out[i] = map[string]any{"name": d.name, "kind": d.kind, "position": i,
			"sha256": hexSHA(d.content), "size_bytes": len(d.content), "content": d.content}
	}
	return out
}

// withDocs — ответ захвата нового сервера: documents и склейка instructions.
func withDocs(t *testing.T, docs []testDoc, mutate func(role map[string]any)) string {
	t.Helper()
	return claimReply(t, func(r map[string]any) {
		role := r["role"].(map[string]any)
		role["documents"] = docsJSON(docs)
		role["instructions"] = concat(docs)
		if mutate != nil {
			mutate(role)
		}
	})
}

func headerOf(i, n int, d testDoc) string {
	return documentHeader(i, n, roleDocView{Name: d.name, Kind: d.kind, SHA256: hexSHA(d.content), SizeBytes: int64(len(d.content))})
}

// AC-4: секция инструкции — заголовок, затем документы со шапками и
// содержимым без усечения; секция последняя, заголовки секций прежние.
func TestClaimPrintsRoleDocuments(t *testing.T) {
	withJSON(t, false)
	out, _, err, _ := runClaim(t, withDocs(t, claimDocs, nil))
	if err != nil {
		t.Fatal(err)
	}
	want := "Инструкция роли developer v13:\n"
	for i, d := range claimDocs {
		want += headerOf(i+1, 3, d) + "\n" + d.content
		if !strings.HasSuffix(d.content, "\n") {
			want += "\n"
		}
	}
	if !strings.HasSuffix(out, instructionsHeading+"\n"+want) {
		t.Fatalf("секция инструкции не та или не последняя:\n%s", tail(out))
	}
	h := headerOf(2, 3, claimDocs[1])
	if h != "--- документ 2 из 3: git.md (reference, "+itoa(len(claimDocs[1].content))+" байт, sha256 "+hexSHA(claimDocs[1].content)[:7]+") ---" {
		t.Fatalf("шапка: %q", h)
	}
	r, d, i := strings.Index(out, rulesHeading), strings.Index(out, dataHeading), strings.Index(out, instructionsHeading)
	if r < 0 || d < r || i < d {
		t.Fatalf("порядок секций: %d %d %d", r, d, i)
	}
	if strings.Contains(out, "<!-- document:") {
		t.Error("в выводе склейка вместо документов")
	}

	one := []testDoc{{"main.md", "instructions", "Одна инструкция без перевода строки"}}
	out, _, err, _ = runClaim(t, withDocs(t, one, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(out, "Инструкция роли developer v13:\n"+headerOf(1, 1, one[0])+"\n"+one[0].content+"\n") {
		t.Fatalf("роль из одного документа:\n%s", tail(out))
	}
}

func readDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// AC-5: каталог создаётся, файлы байт в байт и по sha256, в выводе пути без
// текста; повтор перезаписывает своё и не трогает чужое; с --json и вместе
// с --instructions-file.
func TestClaimInstructionsDir(t *testing.T) {
	withJSON(t, false)
	dir := filepath.Join(t.TempDir(), "нет", "role")
	reply := withDocs(t, claimDocs, nil)
	out, _, err, _ := runClaim(t, reply, "--instructions-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	files := readDir(t, dir)
	if len(files) != 3 {
		t.Fatalf("в каталоге %v", files)
	}
	for _, d := range claimDocs {
		if files[d.name] != d.content || hexSHA(files[d.name]) != hexSHA(d.content) {
			t.Fatalf("%s не совпадает с документом", d.name)
		}
		if !strings.Contains(out, "\n"+filepath.Join(dir, d.name)+"\n") {
			t.Errorf("в выводе нет пути %s", d.name)
		}
	}
	if !strings.Contains(out, "Инструкция роли developer v13: документов 3 в "+dir+"\n") {
		t.Errorf("нет заголовка:\n%s", tail(out))
	}
	if strings.Contains(out, "Последняя строка инструкции.") || strings.Contains(out, "Историю не переписывать") {
		t.Error("с каталогом текст документов всё равно напечатан")
	}
	if !strings.Contains(out, "Сдать:") || !strings.Contains(out, "Входные артефакты:") {
		t.Error("с каталогом пропали остальные секции")
	}

	if err := os.WriteFile(filepath.Join(dir, "git.md"), []byte("правка"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.txt"), []byte("чужое"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err, _ := runClaim(t, reply, "--instructions-dir", dir); err != nil {
		t.Fatal(err)
	}
	files = readDir(t, dir)
	if files["git.md"] != claimDocs[1].content || files["other.txt"] != "чужое" {
		t.Fatalf("повтор: %v", files)
	}

	// --json: ответ как есть, файлы записаны; вместе с --instructions-file.
	withJSON(t, true)
	var want any
	_ = json.Unmarshal([]byte(reply), &want)
	for _, withFile := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "d")
		file := filepath.Join(t.TempDir(), "role.md")
		args := []string{"--instructions-dir", dir}
		if withFile {
			args = append(args, "--instructions-file", file)
		}
		out, _, err, _ := runClaim(t, reply, args...)
		if err != nil {
			t.Fatal(err)
		}
		var got any
		if err := json.Unmarshal([]byte(out), &got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("--json изменил ответ: %v", err)
		}
		if files := readDir(t, dir); len(files) != 3 || files["selfhosted.md"] != claimDocs[2].content {
			t.Fatalf("--json: файлы %v", files)
		}
		if withFile {
			if b, err := os.ReadFile(file); err != nil || string(b) != concat(claimDocs) {
				t.Fatalf("--json с файлом: %v", err)
			}
		}
	}

	withJSON(t, false)
	dir = filepath.Join(t.TempDir(), "d")
	file := filepath.Join(t.TempDir(), "role.md")
	out, _, err, _ = runClaim(t, reply, "--instructions-dir", dir, "--instructions-file", file)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != concat(claimDocs) || len(readDir(t, dir)) != 3 {
		t.Fatalf("каталог и файл вместе: %v", err)
	}
	if !strings.Contains(out, file+" ("+itoa(utf8.RuneCountInString(concat(claimDocs)))+" симв.)") {
		t.Errorf("нет строки файла:\n%s", tail(out))
	}
}

// AC-6: имя не той формы, повтор имени, sha256 не сходится — ошибка с
// именем или номером документа, ни одного файла, сводка уже напечатана.
func TestClaimInstructionsDirRejects(t *testing.T) {
	bad := map[string]func(role map[string]any){}
	for _, name := range []string{"../x.md", "a/b.md", `a\b.md`, ".x.md", "X.md", "x.txt", "", "/tmp/x.md"} {
		bad["имя "+name] = func(role map[string]any) {
			role["documents"].([]map[string]any)[1]["name"] = name
		}
	}
	bad["повтор имени"] = func(role map[string]any) {
		role["documents"].([]map[string]any)[2]["name"] = "git.md"
	}
	bad["sha256"] = func(role map[string]any) {
		role["documents"].([]map[string]any)[2]["sha256"] = hexSHA("другое")
	}
	bad["нет содержимого"] = func(role map[string]any) {
		delete(role["documents"].([]map[string]any)[2], "content")
	}
	names := make([]string, 0, len(bad))
	for n := range bad {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, asJSON := range []bool{false, true} {
			withJSON(t, asJSON)
			parent := t.TempDir()
			dir := filepath.Join(parent, "role")
			out, _, err, _ := runClaim(t, withDocs(t, claimDocs, bad[name]), "--instructions-dir", dir)
			if err == nil {
				t.Fatalf("%s json=%v: ошибки нет", name, asJSON)
			}
			if !strings.Contains(err.Error(), "документ 2") && !strings.Contains(err.Error(), "документ 3") {
				t.Errorf("%s: ошибка без номера документа: %v", name, err)
			}
			if entries, _ := os.ReadDir(parent); len(entries) != 0 {
				t.Errorf("%s json=%v: записано %v", name, asJSON, entries)
			}
			if !strings.Contains(out, "5ca900b3") {
				t.Errorf("%s json=%v: до ошибки не напечатан прогон", name, asJSON)
			}
		}
	}
}

// AC-7: --instructions-file — склейка role.instructions байт в байт и
// прежняя строка с числом символов.
func TestClaimInstructionsFileWithDocuments(t *testing.T) {
	withJSON(t, false)
	for _, docs := range [][]testDoc{claimDocs, claimDocs[1:2]} {
		path := filepath.Join(t.TempDir(), "role.md")
		out, _, err, _ := runClaim(t, withDocs(t, docs, nil), "--instructions-file", path)
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(path)
		if err != nil || string(b) != concat(docs) {
			t.Fatalf("файл не равен склейке: %v", err)
		}
		want := "Инструкция роли developer v13: " + path + " (" + itoa(utf8.RuneCountInString(concat(docs))) + " симв.)\n"
		if !strings.HasSuffix(out, want) {
			t.Errorf("нет строки %q:\n%s", want, tail(out))
		}
	}
	_, _, err, _ := runClaim(t, claimReply(t, func(r map[string]any) { delete(r, "role") }),
		"--instructions-file", filepath.Join(t.TempDir(), "role.md"))
	if err == nil || !strings.Contains(err.Error(), "писать нечего") {
		t.Fatalf("ответ без роли: %v", err)
	}
}

func runShow(t *testing.T, reply string) string {
	t.Helper()
	c, _ := newInviteClient(t, http.StatusOK, reply)
	out, _, err := capture(t, func() error { return cmdShow(context.Background(), c, []string{"CLAB-146"}) })
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// AC-8: show перечисляет документы в строке «Роль:» без текста; --json —
// ответ сервера; без роли строки нет.
func TestShowListsRoleDocuments(t *testing.T) {
	withJSON(t, false)
	reply := withDocs(t, claimDocs, nil)
	out := runShow(t, reply)
	line := lineWith(out, "Роль:")
	want := "developer (Разработчик) v13; документы: main.md (instructions, " + itoa(len(longInstructions)) +
		" байт), git.md (reference, " + itoa(len(claimDocs[1].content)) + " байт), selfhosted.md (checklist, " +
		itoa(len(claimDocs[2].content)) + " байт)"
	if !strings.HasSuffix(line, want) {
		t.Fatalf("строка роли: %q", line)
	}
	if strings.Contains(out, "Историю не переписывать") || strings.Contains(out, "Последняя строка инструкции.") {
		t.Error("show напечатал текст документа")
	}

	withJSON(t, true)
	var got, wantJSON any
	_ = json.Unmarshal([]byte(reply), &wantJSON)
	if err := json.Unmarshal([]byte(runShow(t, reply)), &got); err != nil || !reflect.DeepEqual(got, wantJSON) {
		t.Fatalf("show --json изменил ответ: %v", err)
	}

	withJSON(t, false)
	if out := runShow(t, claimReply(t, func(r map[string]any) { delete(r, "role") })); strings.Contains(out, "Роль:") {
		t.Errorf("без роли строка напечатана:\n%s", out)
	}
}

// AC-9: ответ прежнего сервера (без documents или с пустым списком) —
// claim печатает instructions, как раньше; --instructions-dir пишет один
// main.md; show — прежняя строка.
func TestInstructionsAgainstOldServer(t *testing.T) {
	for name, reply := range map[string]string{
		"без documents":  claimReply(t, nil),
		"пустой список":  claimReply(t, func(r map[string]any) { r["role"].(map[string]any)["documents"] = []any{} }),
		"documents null": claimReply(t, func(r map[string]any) { r["role"].(map[string]any)["documents"] = nil }),
	} {
		withJSON(t, false)
		out, _, err, _ := runClaim(t, reply)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(out, instructionsHeading+"\nИнструкция роли developer v13:\n"+longInstructions+"\n") ||
			strings.Contains(out, "--- документ") {
			t.Fatalf("%s: вывод не прежний:\n%s", name, tail(out))
		}
		dir := filepath.Join(t.TempDir(), "d")
		out, _, err, _ = runClaim(t, reply, "--instructions-dir", dir)
		if err != nil {
			t.Fatal(err)
		}
		files := readDir(t, dir)
		if len(files) != 1 || files["main.md"] != longInstructions {
			t.Fatalf("%s: в каталоге %v", name, sortedNames(files))
		}
		if !strings.HasSuffix(out, "документов 1 в "+dir+"\n"+filepath.Join(dir, "main.md")+"\n") {
			t.Errorf("%s: вывод каталога:\n%s", name, tail(out))
		}
		if line := lineWith(runShow(t, reply), "Роль:"); line != "Роль:         developer (Разработчик) v13" {
			t.Errorf("%s: show: %q", name, line)
		}
	}
}

// Флаг виден в --help и в usage команды.
func TestClaimHelpShowsInstructionsDir(t *testing.T) {
	withJSON(t, false)
	_, errOut, _ := capture(t, func() error {
		return cmdClaim(context.Background(), &Client{}, []string{"--help"})
	})
	if !strings.Contains(errOut, "-instructions-dir") {
		t.Fatalf("--help без флага:\n%s", errOut)
	}
	for _, c := range commands() {
		if c.name == "claim" && !strings.Contains(c.usage, "--instructions-dir") {
			t.Fatalf("usage claim: %q", c.usage)
		}
	}
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

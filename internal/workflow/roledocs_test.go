package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

func doc(name, content string) domain.RoleDocument {
	return domain.RoleDocument{Name: name, Content: &content}
}

// AC-6: склейка — один документ байт в байт, несколько — маркеры, перевод
// строки на конце каждого, пустая строка между; детерминирована.
func TestConcatDocuments(t *testing.T) {
	cases := []struct {
		name string
		docs []domain.RoleDocument
		want string
	}{
		{"один без перевода строки", []domain.RoleDocument{doc("main.md", "Ты разработчик.")}, "Ты разработчик."},
		{"один с хвостовыми пробелами", []domain.RoleDocument{doc("main.md", "Правило  \n\n  ")}, "Правило  \n\n  "},
		{"один не main", []domain.RoleDocument{doc("rules.md", "x\n")}, "x\n"},
		{"два, у первого нет перевода строки",
			[]domain.RoleDocument{doc("main.md", "Главное"), doc("git.md", "Ветки\n")},
			"<!-- document: main.md -->\nГлавное\n\n<!-- document: git.md -->\nВетки\n"},
		{"два, у последнего нет перевода строки",
			[]domain.RoleDocument{doc("main.md", "Главное\n"), doc("git.md", "Ветки")},
			"<!-- document: main.md -->\nГлавное\n\n<!-- document: git.md -->\nВетки\n"},
		{"три, многобайтные",
			[]domain.RoleDocument{doc("a.md", "α"), doc("b.md", "β\n\n"), doc("c.md", "γ")},
			"<!-- document: a.md -->\nα\n\n<!-- document: b.md -->\nβ\n\n\n<!-- document: c.md -->\nγ\n"},
	}
	for _, tc := range cases {
		got := ConcatDocuments(tc.docs)
		if got != tc.want {
			t.Errorf("%s:\nполучили %q\nждали    %q", tc.name, got, tc.want)
		}
		if again := ConcatDocuments(tc.docs); again != got {
			t.Errorf("%s: склейка не детерминирована", tc.name)
		}
	}
}

// AC-5 (модульно): каждый отказ — invalid_input с номером и именем документа;
// 50 документов и имя в 64 символа проходят.
func TestRoleDocumentsRejects(t *testing.T) {
	name64 := strings.Repeat("a", 61) + ".md"
	name65 := strings.Repeat("a", 62) + ".md"
	many := func(n int) []RoleDocumentInput {
		out := make([]RoleDocumentInput, n)
		for i := range out {
			out[i] = RoleDocumentInput{Name: fmt.Sprintf("d%02d.md", i), Content: "x"}
		}
		return out
	}
	bad := []struct {
		name string
		in   RoleVersionInput
		want string
	}{
		{"пустой набор", RoleVersionInput{Documents: []RoleDocumentInput{}}, "нет ни одного документа"},
		{"ни одного поля", RoleVersionInput{}, errEmptyInstructions},
		{"пустая строка", RoleVersionInput{Instructions: "  \n"}, errEmptyInstructions},
		{"оба поля", RoleVersionInput{Instructions: "x", Documents: []RoleDocumentInput{{Name: "a.md", Content: "x"}}}, "не оба сразу"},
		{"пустое имя", RoleVersionInput{Documents: []RoleDocumentInput{{Content: "x"}}}, "документ 1: пустое имя"},
		{"заглавная", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "Git.md", Content: "x"}}}, `документ 1 ("Git.md")`},
		{"слэш", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a/b.md", Content: "x"}}}, `"a/b.md"`},
		{"ведущая точка", RoleVersionInput{Documents: []RoleDocumentInput{{Name: ".a.md", Content: "x"}}}, `".a.md"`},
		{"без .md", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "main.txt", Content: "x"}}}, `"main.txt"`},
		{"65 символов", RoleVersionInput{Documents: []RoleDocumentInput{{Name: name65, Content: "x"}}}, "не длиннее 64"},
		{"дубль", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Content: "x"}, {Name: "a.md", Content: "y"}}}, `документ 2 ("a.md"): имя уже занято документом 1`},
		{"тип", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Kind: "Check List", Content: "x"}}}, `тип "Check List"`},
		{"пустое содержимое", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Content: ""}}}, "пустое содержимое"},
		{"пробелы", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Content: "  \n"}}}, "пустое содержимое"},
		{"NUL", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Content: "a\x00b"}}}, "нулевым байтом"},
		{"NUL строкой", RoleVersionInput{Instructions: "a\x00b"}, `документ 1 ("main.md"): содержимое с нулевым байтом`},
		{"не UTF-8", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Content: "a\xffb"}}}, "не UTF-8"},
		{"51 документ", RoleVersionInput{Documents: many(51)}, "51 документов, предел 50"},
		{"предел содержимого", RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Content: "x"}, {Name: "b.md", Content: "12345678901"}}}, `документ 2 ("b.md"): содержимое 11 байт больше предела 10`},
	}
	for _, tc := range bad {
		_, err := roleDocuments(tc.in, 10)
		if err == nil {
			t.Errorf("%s: отказа нет", tc.name)
			continue
		}
		e := apperr.As(err)
		if e.Code != apperr.CodeInvalidInput || !strings.Contains(e.Message, tc.want) {
			t.Errorf("%s: %s %q, ждали %q", tc.name, e.Code, e.Message, tc.want)
		}
	}

	ok := []RoleVersionInput{
		{Documents: many(50)},
		{Documents: []RoleDocumentInput{{Name: name64, Content: "x"}}},
		{Documents: []RoleDocumentInput{{Name: "0-a_b.c.md", Kind: "check_list-2", Content: "x"}}},
		{Instructions: "строка без перевода  "},
	}
	for i, in := range ok {
		if _, err := roleDocuments(in, 0); err != nil {
			t.Errorf("допустимый набор %d: %v", i, err)
		}
	}
}

// Строка instructions — один main.md типа instructions с тем же содержимым;
// пустой kind документа — instructions.
func TestRoleDocumentsDefaults(t *testing.T) {
	docs, err := roleDocuments(RoleVersionInput{Instructions: "Пиши  "}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Name != "main.md" || docs[0].Kind != "instructions" || docs[0].Content != "Пиши  " {
		t.Fatalf("main.md из строки: %+v", docs)
	}
	in := RoleVersionInput{Documents: []RoleDocumentInput{{Name: "a.md", Content: "x"}}}
	docs, err = roleDocuments(in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if docs[0].Kind != "instructions" || in.Documents[0].Kind != "" {
		t.Fatalf("умолчание типа: %+v, вход изменён: %+v", docs, in.Documents)
	}
	d := documentsOf(docs)[0]
	sum := sha256.Sum256([]byte("x"))
	if d.SHA256 != hex.EncodeToString(sum[:]) || d.SizeBytes != 1 || d.Position != 0 {
		t.Fatalf("sha и размер документа: %+v", d)
	}
}

// AC-14 (модульно): в журнале действий у документов вместо содержимого —
// размер и sha256, даже у короткого текста; имя и тип остаются.
func TestAuditRoleDocumentsDigested(t *testing.T) {
	raw := SanitizeAuditParams(map[string]any{
		"documents": []any{
			map[string]any{"name": "main.md", "kind": "instructions", "content": "секретное правило"},
			map[string]any{"name": "git.md", "content": "ветки"},
		},
	})
	if strings.Contains(string(raw), "секретное правило") || strings.Contains(string(raw), "ветки") {
		t.Fatalf("текст документа в журнале: %s", raw)
	}
	var got struct {
		Documents []struct {
			Name    string         `json:"name"`
			Kind    string         `json:"kind"`
			Content map[string]any `json:"content"`
		} `json:"documents"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("секретное правило"))
	d := got.Documents[0]
	if d.Name != "main.md" || d.Kind != "instructions" || d.Content["sha256"] != hex.EncodeToString(sum[:]) ||
		d.Content["size"] == nil {
		t.Fatalf("документ в журнале: %s", raw)
	}
}

package workflow

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Отбор документов на статусе (CLAB-348) без базы: выбор набора, форма
// отбора и тексты предупреждений. Номера — критерии приёмки анализа
// CLAB-348.

func namesOf(docs []domain.RoleDocument) []string {
	out := make([]string, len(docs))
	for i, d := range docs {
		out[i] = d.Name
	}
	return out
}

// AC-348.5, .7: порядок — версии, а не отбора; отсутствующие — в порядке
// отбора; ни одного — все документы.
func TestSelectDocuments(t *testing.T) {
	docs := []domain.RoleDocument{{Name: "main.md"}, {Name: "git.md", Position: 1}, {Name: "release.md", Position: 2}}
	cases := []struct {
		name        string
		sel         []string
		wantDocs    []string
		wantMissing []string
	}{
		{"без отбора", nil, []string{"main.md", "git.md", "release.md"}, nil},
		{"порядок версии", []string{"release.md", "main.md"}, []string{"main.md", "release.md"}, nil},
		{"один", []string{"git.md"}, []string{"git.md"}, nil},
		{"часть нет", []string{"main.md", "gone.md"}, []string{"main.md"}, []string{"gone.md"}},
		{"ни одного", []string{"gone.md", "old.md"}, []string{"main.md", "git.md", "release.md"}, []string{"gone.md", "old.md"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, missing := selectDocuments(docs, c.sel)
			if !reflect.DeepEqual(namesOf(got), c.wantDocs) || !reflect.DeepEqual(missing, c.wantMissing) {
				t.Fatalf("документы %v, нет %v; ждали %v, %v", namesOf(got), missing, c.wantDocs, c.wantMissing)
			}
		})
	}
	// Версия без документов (не бывает, но отбор на ней не падает).
	if got, missing := selectDocuments(nil, []string{"main.md"}); len(got) != 0 || len(missing) != 1 {
		t.Fatalf("пустая версия: %v %v", got, missing)
	}
}

// AC-348.4: пустой список, пустое имя, повтор и больше 50 имён — отказ;
// nil и любые непустые уникальные имена — нет.
func TestCheckDocumentNames(t *testing.T) {
	many := make([]string, MaxRoleDocuments+1)
	for i := range many {
		many[i] = "d" + strconv.Itoa(i) + ".md"
	}
	bad := map[string][]string{
		"пустой список": {},
		"пустое имя":    {"main.md", ""},
		"пробелы":       {"  "},
		"повтор":        {"main.md", "git.md", "main.md"},
		"больше 50":     many,
	}
	for name, sel := range bad {
		if err := checkDocumentNames(sel); err == nil {
			t.Errorf("%s: отбор %v принят", name, sel)
		}
	}
	for _, sel := range [][]string{nil, {"main.md"}, {"нет-такого.md", "Любое Имя"}, many[:MaxRoleDocuments]} {
		if err := checkDocumentNames(sel); err != nil {
			t.Errorf("отбор %v отклонён: %v", sel, err)
		}
	}
}

// AC-348.7, .9: тексты предупреждений называют имя, роль и версию; «ни
// одного» говорит, что выданы все документы.
func TestStatusDocumentWarnings(t *testing.T) {
	if w := claimDocumentWarnings("maker", 3, []string{"main.md"}, nil); w != nil {
		t.Fatalf("без отсутствующих предупреждений нет: %v", w)
	}
	w := claimDocumentWarnings("maker", 3, []string{"main.md", "gone.md"}, []string{"gone.md"})
	if len(w) != 1 || !strings.Contains(w[0], "gone.md") || !strings.Contains(w[0], "maker v3") ||
		strings.Contains(w[0], "все документы") {
		t.Fatalf("часть имён: %v", w)
	}
	w = claimDocumentWarnings("maker", 3, []string{"gone.md", "old.md"}, []string{"gone.md", "old.md"})
	if len(w) != 1 || !strings.Contains(w[0], "gone.md, old.md") || !strings.Contains(w[0], "все документы") {
		t.Fatalf("ни одного: %v", w)
	}

	p := selectionWarnings("release", "maker", 4, []string{"main.md", "gone.md"}, []string{"gone.md"})
	if len(p) != 1 || p[0].Code != codeRoleDocumentMissing || p[0].Status != "release" ||
		!strings.Contains(p[0].Message, `"gone.md"`) || !strings.Contains(p[0].Message, "v4") ||
		strings.Contains(p[0].Message, "все документы") {
		t.Fatalf("проверка, часть имён: %+v", p)
	}
	p = selectionWarnings("release", "maker", 4, []string{"a.md", "b.md"}, []string{"a.md", "b.md"})
	if len(p) != 2 || !strings.Contains(p[0].Message, "все документы") || !strings.Contains(p[1].Message, `"b.md"`) {
		t.Fatalf("проверка, ни одного — запись на имя и «все документы»: %+v", p)
	}
	if p := selectionWarnings("release", "maker", 4, []string{"a.md"}, nil); len(p) != 0 {
		t.Fatalf("все имена на месте: %+v", p)
	}
}

package workflow

// Отбор документов роли на статусе цикла (решение 0068, п. 3, CLAB-348).
// Отбор — множество имён на строке status_roles: этап получает только
// названные документы версии роли прогона, в порядке версии. Имя, которого
// в версии нет, — предупреждение, а не отказ: роль меняется версиями
// независимо от цикла. Не нашлось ни одного — этап получает все документы:
// пустая инструкция хуже лишних правил.

import (
	"fmt"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// checkDocumentNames — форма отбора одной роли. nil — отбора нет, это не
// ошибка; пустой список — ошибка: «все» выражается отсутствием поля, и
// второй способ сказать то же самое разошёлся бы в связке и сравнении.
// Предел — тот же, что у числа документов версии: отбор больше версии
// осмысленным быть не может. Существование имён в версии роли не
// проверяется — это предупреждение проверки черновика.
func checkDocumentNames(names []string) error {
	if names == nil {
		return nil
	}
	if len(names) == 0 {
		return apperr.InvalidInput(
			"document_names: пустой отбор; чтобы этап получал все документы роли, уберите поле")
	}
	if len(names) > MaxRoleDocuments {
		return apperr.InvalidInput("document_names: %d имён — больше предела %d", len(names), MaxRoleDocuments)
	}
	seen := make(map[string]bool, len(names))
	for i, n := range names {
		if strings.TrimSpace(n) == "" {
			return apperr.InvalidInput("document_names: имя %d пустое", i+1)
		}
		if seen[n] {
			return apperr.InvalidInput("document_names: имя %q повторяется", n)
		}
		seen[n] = true
	}
	return nil
}

// selectDocuments — документы этапа по отбору. names == nil — все документы
// как есть. Иначе — документы, чьи имена названы, в порядке версии (порядок
// отбора ничего не значит), и имена, которых в версии нет, в порядке
// отбора. Если не нашлось ни одного имени — все документы версии: этот
// случай вызывающий узнаёт по len(missing) == len(names).
func selectDocuments(docs []domain.RoleDocument, names []string) (picked []domain.RoleDocument, missing []string) {
	if names == nil {
		return docs, nil
	}
	have := make(map[string]bool, len(docs))
	for _, d := range docs {
		have[d.Name] = true
	}
	for _, n := range names {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) == len(names) {
		return docs, missing
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	picked = make([]domain.RoleDocument, 0, len(names)-len(missing))
	for _, d := range docs {
		if want[d.Name] {
			picked = append(picked, d)
		}
	}
	return picked, missing
}

// claimDocumentWarnings — строки role.document_warnings ответа захвата.
// Ни одного не нашлось — одна строка про все имена: этапу важно, что он
// получил весь набор, а не какой документ из отбора пропал.
func claimDocumentWarnings(roleKey string, version int, names, missing []string) []string {
	if len(missing) == 0 {
		return nil
	}
	if len(missing) == len(names) {
		return []string{fmt.Sprintf(
			"ни один документ из отбора на статусе (%s) не найден в версии роли %s v%d — выданы все документы версии",
			strings.Join(missing, ", "), roleKey, version)}
	}
	out := make([]string, len(missing))
	for i, n := range missing {
		out[i] = fmt.Sprintf(
			"документ %s из отбора на статусе не найден в версии роли %s v%d — пропущен", n, roleKey, version)
	}
	return out
}

// codeRoleDocumentMissing — предупреждение проверки черновика: имя из
// отбора, которого нет в последней версии роли.
const codeRoleDocumentMissing = "role_document_missing"

// selectionWarnings — предупреждения проверки по одной роли статуса: своя
// запись на каждое отсутствующее имя; если не нашлось ни одного, текст
// говорит, что этап получит все документы.
func selectionWarnings(statusKey, roleKey string, version int, names, missing []string) []Problem {
	out := make([]Problem, 0, len(missing))
	all := len(missing) > 0 && len(missing) == len(names)
	for _, n := range missing {
		msg := fmt.Sprintf("документ %q из отбора роли %s на статусе %q не найден в последней версии роли (v%d)",
			n, roleKey, statusKey, version)
		if all {
			msg += "; ни один документ отбора не найден — этап получит все документы версии"
		}
		out = append(out, Problem{Code: codeRoleDocumentMissing, Message: msg, Status: statusKey})
	}
	return out
}

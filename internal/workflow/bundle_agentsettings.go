// Настройка агентов проекта в связке (CLAB-428, решение 0077, п. 4.10).
//
// Поле agent_settings необязательно и формата связки не меняет (как
// instructions, решение 0069, п. 7): сервер прежней версии теряет его
// молча, а CLI по отсутствию строки agent_settings в отчёте импорта
// понимает, что настройка не применена. Импорт ведёт настройку как цикл:
// отличается — черновик (активирует человек), совпадает — unchanged.
package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// Ключ строки настройки агентов в отчёте импорта и поля связки.
const importAgentSettingsKey = "agent_settings"

// Описание версии черновика, когда связка своего не принесла (как у ролей).
const importAgentSettingsChangelog = "импорт связки"

// BundleAgentSettings — настройка агентов проекта в связке: документ версии
// (решение 0077, п. 4.1) и её описание. Document — any, а не
// json.RawMessage: схему аргументов MCP-тула выводит jsonschema-go, и
// RawMessage для него массив байтов (CLAB-98, как external_ref).
type BundleAgentSettings struct {
	Document  any    `json:"document" jsonschema:"документ настройки агентов целиком: {agents, pricing}"`
	Changelog string `json:"changelog,omitempty" jsonschema:"что и зачем меняется, до 2000 символов; пусто — «импорт связки»"`
}

// agentSettingsDocument — документ связки как JSON для проверки и записи.
func (s *BundleAgentSettings) agentSettingsDocument() (json.RawMessage, error) {
	if s.Document == nil {
		return nil, agentSettingsInvalid("document", "документ обязателен: {\"agents\": …, \"pricing\": …}")
	}
	raw, err := json.Marshal(s.Document)
	if err != nil {
		return nil, agentSettingsInvalid("document", "документ не сериализуется в JSON")
	}
	return raw, nil
}

// checkBundleAgentSettings — проверка до транзакции и до первой записи
// (как checkSelection): ValidateAgentSettings и описание версии. Отказ
// получает префикс agent_settings и details.bundle_field; details.path
// остаётся путём внутри документа, как у ручной правки.
func checkBundleAgentSettings(s *BundleAgentSettings) (json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	wrap := func(err error) error {
		e := apperr.As(err)
		details := map[string]any{}
		if m, ok := e.Details.(map[string]any); ok {
			for k, v := range m {
				details[k] = v
			}
		}
		details["bundle_field"] = importAgentSettingsKey
		return apperr.New(e.Code, "%s: %s", importAgentSettingsKey, e.Message).WithDetails(details)
	}
	raw, err := s.agentSettingsDocument()
	if err != nil {
		return nil, wrap(err)
	}
	if _, err := ValidateAgentSettings(raw); err != nil {
		return nil, wrap(err)
	}
	if err := checkChangelog(&s.Changelog); err != nil {
		return nil, wrap(err)
	}
	return raw, nil
}

// importAgentSettings применяет настройку связки внутри транзакции импорта.
// Записывает движок CLAB-425 (createDraft/replaceDraft) — единственный путь
// записи документа. Черновик блокируется до сравнения: два одновременных
// импорта не заведут два черновика (индекс схемы остаётся последним рубежом).
func (a *Authoring) importAgentSettings(ctx context.Context, tx pgx.Tx, project domain.Project, who Actor, in *BundleAgentSettings, doc json.RawMessage) (ImportEntry, *AgentSettingsChange, error) {
	entry := ImportEntry{Key: importAgentSettingsKey}
	sum, _, err := AgentSettingsSHA256(doc)
	if err != nil {
		return entry, nil, err
	}

	var draft, active *domain.AgentSettingsVersion
	switch d, err := a.store.LockAgentSettingsDraft(ctx, tx, project.ID); {
	case err == nil:
		draft = &d
	case apperr.As(err).Code != apperr.CodeNotFound:
		return entry, nil, err
	}
	switch v, err := a.store.ActiveAgentSettings(ctx, tx, project.ID); {
	case err == nil:
		active = &v
	case apperr.As(err).Code != apperr.CodeNotFound:
		return entry, nil, err
	}

	same := func(v *domain.AgentSettingsVersion) (bool, error) {
		if v == nil {
			return false, nil
		}
		have, _, err := AgentSettingsSHA256(v.Document)
		return have == sum, err
	}
	if ok, err := same(draft); err != nil {
		return entry, nil, err
	} else if ok {
		entry.Outcome, entry.ID = "unchanged", draft.ID.String()
		entry.Detail = fmt.Sprintf("совпадает с черновиком, версия %d", draft.Version)
		return entry, nil, nil
	}
	if ok, err := same(active); err != nil {
		return entry, nil, err
	} else if ok {
		entry.Outcome, entry.ID = "unchanged", active.ID.String()
		entry.Detail = fmt.Sprintf("совпадает с активной версией %d", active.Version)
		if draft != nil {
			entry.Detail += fmt.Sprintf("; черновик версии %d не тронут", draft.Version)
		}
		return entry, nil, nil
	}

	changelog := in.Changelog
	if changelog == "" {
		changelog = importAgentSettingsChangelog
	}
	var view AgentSettingsView
	var change AgentSettingsChange
	if draft == nil {
		view, change, err = a.engine.createDraft(ctx, tx, project, who,
			AgentSettingsDraftInput{Document: doc, Changelog: &changelog})
		entry.Outcome = "draft_created"
		entry.Detail = fmt.Sprintf("версия %d, черновик; активируйте после проверки", view.Version)
	} else {
		view, change, err = a.engine.replaceDraft(ctx, tx, project, *draft,
			AgentSettingsPutInput{Document: doc, Changelog: &changelog})
		entry.Outcome = "updated"
		entry.Detail = fmt.Sprintf("черновик версии %d заменён; активируйте после проверки", view.Version)
	}
	if err != nil {
		return entry, nil, err
	}
	for _, w := range view.Warnings {
		entry.Detail += "; " + w
	}
	id, err := a.agentSettingsID(ctx, tx, project.ID, view.Version)
	if err != nil {
		return entry, nil, err
	}
	entry.ID = id.String()
	return entry, &change, nil
}

func (a *Authoring) agentSettingsID(ctx context.Context, tx pgx.Tx, projectID uuid.UUID, version int) (uuid.UUID, error) {
	v, err := a.store.AgentSettingsByVersion(ctx, tx, projectID, version)
	return v.ID, err
}

// exportAgentSettings — активная версия настройки проекта; активной нет
// (черновик не в счёт) — nil, и связка проекта остаётся байт в байт прежней.
func (a *Authoring) exportAgentSettings(ctx context.Context, projectID uuid.UUID) (*BundleAgentSettings, error) {
	v, err := a.store.ActiveAgentSettings(ctx, a.store.DB(), projectID)
	if err != nil {
		if apperr.As(err).Code == apperr.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &BundleAgentSettings{Document: v.Document, Changelog: deref(v.Changelog)}, nil
}

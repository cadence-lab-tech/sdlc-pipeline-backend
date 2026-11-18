package mcp

import (
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Аннотации тулов (CLAB-484, решение человека Q3): хостовый клиент по
// readOnlyHint и destructiveHint решает, какие вызовы подтверждать у человека
// и какие считать «чтением». Источник истины — таблица ниже; тул без записи в
// ней не регистрируется (паника при старте, как у UnscopedTools), поэтому
// новый тул не может выйти без title и без явных подсказок.

// toolKind — класс действия тула для клиента.
type toolKind int

const (
	// kindRead — только читает: readOnlyHint, destructiveHint=false.
	kindRead toolKind = iota
	// kindWrite — добавляет или меняет данные, ничего не стирая: версия, запись,
	// связь, аренда. destructiveHint=false.
	kindWrite
	// kindWriteIdem — запись, повтор которой с теми же аргументами ничего не
	// добавляет (продление аренды).
	kindWriteIdem
	// kindDestructive — меняет ход задачи или правила необратимо либо
	// заменяет и удаляет прежнее: решение гейта, переход, перевод, активация.
	// destructiveHint=true.
	kindDestructive
)

type toolMeta struct {
	title string
	kind  toolKind
}

// toolMetas — title и класс каждого тула. Title — человекочитаемое имя для
// списка и окна подтверждения; без имён проекта, ролей, провайдеров
// (TestMCPTextsAreNeutral).
var toolMetas = map[string]toolMeta{
	// чтение
	"sdlc_whoami":                       {"Кто я и что настроено в проекте", kindRead},
	"sdlc_list_projects":                {"Проекты участия", kindRead},
	"sdlc_list_tasks":                   {"Список задач", kindRead},
	"sdlc_get_task":                     {"Состояние задачи", kindRead},
	"sdlc_get_artifact":                 {"Артефакт задачи", kindRead},
	"sdlc_search_context":               {"Поиск по коду, артефактам и документам", kindRead},
	"sdlc_check_gates":                  {"Что мешает переходу", kindRead},
	"sdlc_get_drift":                    {"Дрейф задачи", kindRead},
	"sdlc_export_bundle":                {"Выгрузить связку правил", kindRead},
	"sdlc_import_guide":                 {"Руководство по переносу правил", kindRead},
	"sdlc_get_role":                     {"Роль и её документы", kindRead},
	"sdlc_list_documents":               {"Проектные документы", kindRead},
	"sdlc_get_document":                 {"Проектный документ", kindRead},
	"sdlc_list_instructions":            {"Библиотека инструкций", kindRead},
	"sdlc_get_instruction":              {"Инструкция библиотеки", kindRead},
	"sdlc_get_agent_settings":           {"Настройка агентов", kindRead},
	"sdlc_list_agent_settings_versions": {"Версии настройки агентов", kindRead},
	"sdlc_list_role_proposals":          {"Предложения версий ролей", kindRead},

	// запись без разрушения
	"sdlc_claim_task":             {"Забрать этап задачи (аренда)", kindWrite},
	"sdlc_submit_artifact":        {"Сдать артефакт", kindWrite},
	"sdlc_link_repo_ref":          {"Привязать PR, ветку или коммит", kindWrite},
	"sdlc_resolve_drift":          {"Закрыть сигнал дрейфа", kindWrite},
	"sdlc_create_task":            {"Завести задачу", kindWrite},
	"sdlc_update_task":            {"Поправить постановку задачи", kindWrite},
	"sdlc_link_tasks":             {"Связать задачи порядком", kindWrite},
	"sdlc_assign_task":            {"Назначить исполнителя", kindWrite},
	"sdlc_rank_task":              {"Поставить задачу на место в бэклоге", kindWrite},
	"sdlc_report_usage":           {"Отчитаться о стоимости прогона", kindWrite},
	"sdlc_add_repository":         {"Привязать репозиторий к проекту", kindWrite},
	"sdlc_put_role":               {"Завести роль или её версию", kindWrite},
	"sdlc_put_instruction":        {"Завести инструкцию или её версию", kindWrite},
	"sdlc_propose_role_version":   {"Предложить версию роли", kindWrite},
	"sdlc_reject_role_proposal":   {"Отклонить предложение версии роли", kindWrite},
	"sdlc_withdraw_role_proposal": {"Отозвать предложение версии роли", kindWrite},
	"sdlc_heartbeat":              {"Продлить аренду этапа", kindWriteIdem},

	// решения и необратимые изменения
	"sdlc_decide_gate":                  {"Решить гейт перехода", kindDestructive},
	"sdlc_request_transition":           {"Запросить переход задачи", kindDestructive},
	"sdlc_move_task":                    {"Перевести задачу в статус", kindDestructive},
	"sdlc_release":                      {"Отпустить задачу", kindDestructive},
	"sdlc_update_task_scope":            {"Расширить область задачи", kindDestructive},
	"sdlc_import_bundle":                {"Применить связку правил", kindDestructive},
	"sdlc_apply_instruction":            {"Обновить инструкцию в ролях", kindDestructive},
	"sdlc_activate_agent_settings":      {"Активировать настройку агентов", kindDestructive},
	"sdlc_approve_role_proposal":        {"Принять предложение версии роли", kindDestructive},
	"sdlc_set_task_parent":              {"Переподчинить задачу", kindDestructive},
	"sdlc_discard_agent_settings_draft": {"Удалить черновик настройки агентов", kindDestructive},
	"sdlc_put_agent_settings_draft":     {"Заменить черновик настройки агентов", kindDestructive},
	"sdlc_put_document":                 {"Положить или обновить проектный документ", kindDestructive},
}

// annotationsFor — аннотации тула по таблице; тула нет в таблице — паника.
func annotationsFor(name string) *sdk.ToolAnnotations {
	meta, ok := toolMetas[name]
	if !ok {
		panic("тул " + name + " не внесён в toolMetas (annotations.go): нужны title и класс действия")
	}
	f, tr := false, true
	a := &sdk.ToolAnnotations{Title: meta.title, DestructiveHint: &f, OpenWorldHint: &f}
	switch meta.kind {
	case kindRead:
		a.ReadOnlyHint, a.IdempotentHint = true, true
	case kindWriteIdem:
		a.IdempotentHint = true
	case kindDestructive:
		a.DestructiveHint = &tr
	}
	return a
}

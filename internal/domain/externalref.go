package domain

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Ссылка на основной трекер клиента (CLAB-95): tasks.external_ref — объект
// «система → {key, url}», одна ссылка на систему. Список систем — данные, а не
// код: jira, youtrack, linear и любые другие под тем же форматом имени.

// ExternalSystemPattern — формат имени системы трекера.
var ExternalSystemPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ExternalLink — ссылка задачи на тикет одной системы.
type ExternalLink struct {
	Key string `json:"key"`
	URL string `json:"url"`
}

// ParseExternalQuery разбирает фильтр списка задач по ссылке: «КЛЮЧ» ищет
// ключ в любой системе, «система:КЛЮЧ» — только в ней. Префикс до двоеточия
// считается системой, лишь если он похож на имя системы и за ним есть ключ:
// иначе двоеточие — часть самого ключа.
func ParseExternalQuery(q string) (system, key string) {
	q = strings.TrimSpace(q)
	if i := strings.Index(q, ":"); i > 0 {
		sys, key := strings.ToLower(q[:i]), strings.TrimSpace(q[i+1:])
		// «jira:» без ключа — не «все задачи»: пустой ключ выключил бы фильтр
		// целиком, и человек получил бы весь проект вместо пустого ответа.
		if ExternalSystemPattern.MatchString(sys) && key != "" {
			return sys, key
		}
	}
	return "", q
}

// ExternalRefMatches отвечает, есть ли в ссылках задачи тикет key (без учёта
// регистра) в системе system или, если она пуста, в любой. Записи чужой формы
// — legacy_epic_key из миграции 000011 — не совпадают ни с чем и не мешают.
// То же правило в SQL — ListTasks в хранилище.
func ExternalRefMatches(raw json.RawMessage, system, key string) bool {
	if key == "" {
		return true
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return false
	}
	for sys, value := range entries {
		if system != "" && sys != system {
			continue
		}
		var link ExternalLink
		if json.Unmarshal(value, &link) != nil {
			continue
		}
		if strings.EqualFold(link.Key, key) {
			return true
		}
	}
	return false
}

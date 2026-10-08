package workflow

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// maxExternalKeyRunes — предел ключа тикета. Ключи трекеров короткие
// (PROJ-12, #4521), предел ловит вставленный по ошибке текст, а не их.
const maxExternalKeyRunes = 128

// externalRefForm — форма ссылки в тексте ошибки: клиенту, отбитому 400,
// пример нужнее перечня нарушенных правил.
const externalRefForm = `{"jira": {"key": "PROJ-12", "url": "https://jira.example.com/browse/PROJ-12"}}`

// normalizeExternalRef проверяет ссылку на основной трекер и приводит её к
// одному виду: ключи и URL без пробелов по краям, в записи только key и url.
//
// Проверка одна на REST и MCP и живёт в движке, а не в транспорте: иначе
// правила двух ручек разошлись бы с первой же правкой (CLAB-95). Пустое тело
// и null — это «ссылок нет», а не ошибка: на заведении задачи это поле не
// обязательно.
func normalizeExternalRef(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	if trimmed[0] != '{' {
		return nil, apperr.InvalidInput("external_ref должен быть объектом вида %s", externalRefForm)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &entries); err != nil {
		return nil, apperr.InvalidInput("external_ref должен быть объектом вида %s", externalRefForm)
	}

	out := make(map[string]domain.ExternalLink, len(entries))
	for system, value := range entries {
		if !domain.ExternalSystemPattern.MatchString(system) {
			return nil, apperr.InvalidInput(
				"система трекера %q не по формату: латиница в нижнем регистре, цифры, - и _, "+
					"до 32 символов, начинается с буквы (jira, youtrack, linear)", system)
		}
		value = bytes.TrimSpace(value)
		var link struct {
			Key *string `json:"key"`
			URL *string `json:"url"`
		}
		if len(value) == 0 || value[0] != '{' || json.Unmarshal(value, &link) != nil {
			return nil, apperr.InvalidInput(
				"ссылка %s должна быть объектом {\"key\": \"…\", \"url\": \"…\"}", system)
		}

		key := ""
		if link.Key != nil {
			key = strings.TrimSpace(*link.Key)
		}
		if key == "" {
			return nil, apperr.InvalidInput("у ссылки %s пустой key: нужен ключ тикета, например PROJ-12", system)
		}
		if n := utf8.RuneCountInString(key); n > maxExternalKeyRunes {
			return nil, apperr.InvalidInput(
				"ключ тикета %s длиннее %d символов (в нём %d)", system, maxExternalKeyRunes, n)
		}

		address := ""
		if link.URL != nil {
			address = strings.TrimSpace(*link.URL)
		}
		if err := checkExternalURL(address); err != nil {
			return nil, apperr.InvalidInput("URL ссылки %s %s %q: нужен абсолютный адрес http или https с хостом",
				system, key, address)
		}
		out[system] = domain.ExternalLink{Key: key, URL: address}
	}

	// encoding/json пишет ключи карты по порядку: одинаковые ссылки дают
	// одинаковый документ, и в ленте правка без изменений видна как таковая.
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// checkExternalURL пропускает только http(s) с хостом: ссылку показывают
// кликабельной, и javascript: или относительный путь в ней — это дыра, а не
// опечатка.
func checkExternalURL(address string) error {
	if address == "" {
		return apperr.InvalidInput("пустой URL")
	}
	u, err := url.Parse(address)
	if err != nil {
		return err
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return apperr.InvalidInput("не http(s)")
	}
	return nil
}

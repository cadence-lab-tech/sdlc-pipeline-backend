package postgres

import "testing"

// Ключ задачи уникален внутри проекта, а в URL приезжает один. Проект берётся
// из префикса ключа — на этом стоит поиск задачи по SDLC-42.
func TestSplitTaskKey(t *testing.T) {
	ok := map[string]string{
		"SDLC-1":    "SDLC",
		"SDLC-42":   "SDLC",
		"MY_PROJ-7": "MY_PROJ",
		"A-B-12":    "A-B",
	}
	for key, want := range ok {
		got, err := splitTaskKey(key)
		if err != nil {
			t.Fatalf("%q: неожиданная ошибка %v", key, err)
		}
		if got != want {
			t.Fatalf("%q: получили %q, ожидали %q", key, got, want)
		}
	}

	for _, key := range []string{"SDLC", "-1", "SDLC-", "", "42"} {
		if _, err := splitTaskKey(key); err == nil {
			t.Fatalf("%q должен отбиваться как некорректный ключ", key)
		}
	}
}

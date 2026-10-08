package domain

import (
	"encoding/json"
	"testing"
)

func TestParseExternalQuery(t *testing.T) {
	cases := []struct{ in, system, key string }{
		{"PROJ-12", "", "PROJ-12"},
		{"  PROJ-12 ", "", "PROJ-12"},
		{"jira:PROJ-12", "jira", "PROJ-12"},
		{"Jira:PROJ-12", "jira", "PROJ-12"},
		{"jira: PROJ-12", "jira", "PROJ-12"},
		// Префикс не похож на систему — двоеточие часть ключа.
		{"A B:1", "", "A B:1"},
		{":PROJ-12", "", ":PROJ-12"},
		{"jira:", "", "jira:"},
		{"", "", ""},
	}
	for _, tc := range cases {
		system, key := ParseExternalQuery(tc.in)
		if system != tc.system || key != tc.key {
			t.Fatalf("%q: %q %q, ждали %q %q", tc.in, system, key, tc.system, tc.key)
		}
	}
}

// То же правило, что у SQL в ListTasks: им фильтрует sdlc_list_tasks по
// открытым прогонам.
func TestExternalRefMatches(t *testing.T) {
	ref := json.RawMessage(`{"jira":{"key":"PROJ-12","url":"https://jira.example.com/browse/PROJ-12"},"legacy_epic_key":"PROJ-12"}`)
	cases := []struct {
		system, key string
		want        bool
	}{
		{"", "PROJ-12", true},
		{"", "proj-12", true},
		{"jira", "PROJ-12", true},
		{"youtrack", "PROJ-12", false},
		{"", "PROJ-1", false},
		{"legacy_epic_key", "PROJ-12", false},
		{"", "", true},
	}
	for _, tc := range cases {
		if got := ExternalRefMatches(ref, tc.system, tc.key); got != tc.want {
			t.Fatalf("%q %q: %v", tc.system, tc.key, got)
		}
	}
	if ExternalRefMatches(json.RawMessage(`"строка"`), "", "PROJ-12") {
		t.Fatal("не объект совпал")
	}
}

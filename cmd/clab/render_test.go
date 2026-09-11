package main

import "testing"

// Подписи источника совпадают с колонкой «Чем проверяли» в настройках
// проекта; незнакомое значение от сервера новее CLI показывается как есть.
func TestCredentialSourceLabels(t *testing.T) {
	installation := int64(42)
	cases := []struct {
		source       string
		installation *int64
		ref          string
		want         string
	}{
		{"repository_secret", nil, "repo-x", "секрет репозитория repo-x"},
		{"repository_installation", &installation, "", "установка репозитория 42"},
		{"project_installation", &installation, "", "установка проекта 42"},
		{"project_secret", nil, "proj", "секрет проекта proj"},
		{"instance", nil, "", "токен инстанса"},
		{"", nil, "", "—"},
		{"something_new", nil, "", "something_new"},
	}
	for _, tc := range cases {
		if got := credentialSource(tc.source, tc.installation, tc.ref); got != tc.want {
			t.Fatalf("%q: %q вместо %q", tc.source, got, tc.want)
		}
	}
}

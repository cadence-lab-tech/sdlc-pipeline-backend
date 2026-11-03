package postgres

import (
	"slices"
	"testing"
)

// Слова-имена запроса (CLAB-362, R5): по ним кусок с объявлением идёт выше
// использований, а обычное слово фразы ничего не поднимает.
func TestNameWords(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		{"useResource", []string{"useresource"}},
		{"run", []string{"run"}},
		{"revoke task tokens RevokeRunTokens", []string{"revokeruntokens"}},
		{"e.store.RevokeRunTokens", []string{"revokeruntokens"}},
		{"internal/workflow.Engine.Advance", []string{"advance"}},
		{"obj?.x", []string{"x"}},
		{"как устроен user_id", []string{"user_id"}},
		{"Foo(ctx)", []string{"foo"}},
		{"ротация токенов", []string{}},
		{"where is v2 used", []string{"v2"}},
		{"tokens. RevokeRunTokens RevokeRunTokens", []string{"revokeruntokens"}},
		{"\"Button\" -Card", []string{"button"}},
		{"   ", []string{}},
	}
	for _, c := range cases {
		if got := nameWords(c.query); !slices.Equal(got, c.want) {
			t.Errorf("nameWords(%q) = %q, ждали %q", c.query, got, c.want)
		}
	}
}

// Слова-имена в регистре запроса (CLAB-369, R8): тот же отбор, что у
// nameWords, без приведения к нижнему регистру.
func TestExactNameWords(t *testing.T) {
	cases := []struct {
		query string
		want  []string
	}{
		{"listAttachments", []string{"listAttachments"}},
		{"ListAttachments", []string{"ListAttachments"}},
		{"listattachments", []string{"listattachments"}},
		{"revoke task tokens RevokeRunTokens", []string{"RevokeRunTokens"}},
		{"e.store.RevokeRunTokens", []string{"RevokeRunTokens"}},
		{"Foo foo FOO", []string{"Foo", "FOO"}},
		{"ротация токенов", []string{}},
	}
	for _, c := range cases {
		if got := exactNameWords(c.query); !slices.Equal(got, c.want) {
			t.Errorf("exactNameWords(%q) = %q, ждали %q", c.query, got, c.want)
		}
	}
}

// AC-11: признак тестового файла — закрытый перечень R10.
func TestIsTestPath(t *testing.T) {
	for _, p := range TestPaths {
		if !isTestPath(p) {
			t.Errorf("%s — тестовый файл", p)
		}
	}
	for _, p := range NonTestPaths {
		if isTestPath(p) {
			t.Errorf("%s — не тестовый файл", p)
		}
	}
}

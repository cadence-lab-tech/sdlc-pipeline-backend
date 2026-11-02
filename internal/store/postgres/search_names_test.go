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

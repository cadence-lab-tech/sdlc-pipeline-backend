package gitremote

import "testing"

func TestParse(t *testing.T) {
	cases := map[string][2]string{
		"git@github.com:acme/api.git":            {"acme", "api"},
		"https://github.com/acme/api":            {"acme", "api"},
		"https://github.com/acme/api.git":        {"acme", "api"},
		"ssh://git@github.com/acme/api.git":      {"acme", "api"},
		"https://ghe.example.com/team/repo.git/": {"team", "repo"},
	}
	for raw, want := range cases {
		owner, name, ok := Parse(raw)
		if !ok || owner != want[0] || name != want[1] {
			t.Errorf("%s: %s/%s (%v)", raw, owner, name, ok)
		}
	}
	for _, raw := range []string{"", "https://github.com/", "nonsense", "git@github.com:"} {
		if _, _, ok := Parse(raw); ok {
			t.Errorf("%q разобрался, а не должен", raw)
		}
	}
}

package domain

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

// К-14 (CLAB-399): генератор ранга бэклога.

func mustBetween(t *testing.T, a, b string) string {
	t.Helper()
	k, err := RankBetween(a, b)
	if err != nil {
		t.Fatalf("между %q и %q: %v", a, b, err)
	}
	if !ValidRank(k) {
		t.Fatalf("между %q и %q: %q не ключ", a, b, k)
	}
	if (a != "" && k <= a) || (b != "" && k >= b) {
		t.Fatalf("между %q и %q получилось %q", a, b, k)
	}
	return k
}

func TestRankAlphabetASCIIOrdered(t *testing.T) {
	for i := 1; i < len(RankDigits); i++ {
		if RankDigits[i-1] >= RankDigits[i] {
			t.Fatalf("алфавит не по возрастанию на %d", i)
		}
	}
	if len(RankDigits) != 62 {
		t.Fatalf("алфавит %d знаков", len(RankDigits))
	}
}

func TestRankBetweenEdges(t *testing.T) {
	first := mustBetween(t, "", "")
	if first != "V" {
		t.Fatalf("первый ключ пустой группы %q", first)
	}
	for _, c := range []struct{ a, b string }{
		{"", "V"}, {"V", ""}, {"", "1"}, {"", "01"}, {"", "11"}, {"z", ""}, {"zz", ""},
		{"V", "W"}, {"V", "V1"}, {"V1", "V2"}, {"0V", "1"}, {"a", "b"}, {"Vz", "W"},
		{"y", "z"}, {"1", "2"}, {"01", "02"}, {"V0V", "V1"},
	} {
		mustBetween(t, c.a, c.b)
	}
	for _, bad := range []struct{ a, b string }{
		{"V", "V"}, {"W", "V"}, {"V0", ""}, {"", "a0"}, {"V-", ""}, {"Я", ""},
	} {
		if _, err := RankBetween(bad.a, bad.b); err == nil {
			t.Errorf("между %q и %q — ждали ошибку", bad.a, bad.b)
		}
	}
}

func TestValidRank(t *testing.T) {
	for _, k := range []string{"V", "1", "00V", "zzz", "a0b"} {
		if !ValidRank(k) {
			t.Errorf("%q — ключ", k)
		}
	}
	for _, k := range []string{"", "0", "V0", "a b", "ё"} {
		if ValidRank(k) {
			t.Errorf("%q — не ключ", k)
		}
	}
}

// insert — n вставок в один и тот же случай; список держится упорядоченным.
func TestRankThousandInsertsInOnePlace(t *testing.T) {
	cases := map[string]func(keys []string) int{
		// Индекс, перед которым встаёт новый ключ.
		"в начало":     func([]string) int { return 0 },
		"в конец":      func(keys []string) int { return len(keys) },
		"в середину":   func(keys []string) int { return 1 },
		"поочерёдно":   func(keys []string) int { return len(keys) / 2 },
		"перед концом": func(keys []string) int { return len(keys) - 1 },
	}
	for name, at := range cases {
		keys := []string{mustBetween(t, "", "")}
		keys = append(keys, mustBetween(t, keys[0], ""))
		maxLen := 0
		for i := 0; i < 1000; i++ {
			pos := at(keys)
			var a, b string
			if pos > 0 {
				a = keys[pos-1]
			}
			if pos < len(keys) {
				b = keys[pos]
			}
			k := mustBetween(t, a, b)
			keys = slices.Insert(keys, pos, k)
			maxLen = max(maxLen, len(k))
		}
		if !sort.StringsAreSorted(keys) {
			t.Fatalf("%s: порядок сломан", name)
		}
		for i := 1; i < len(keys); i++ {
			if keys[i-1] == keys[i] {
				t.Fatalf("%s: повтор %q", name, keys[i])
			}
		}
		t.Logf("%s: самый длинный ключ %d", name, maxLen)
	}
}

// Вставка в одно место (между двумя соседями) растит ключ и рано или поздно
// переходит порог — повод перераздать группу; перераздача сохраняет порядок.
func TestRankThresholdTriggersSpread(t *testing.T) {
	keys := []string{mustBetween(t, "", "")}
	keys = append(keys, mustBetween(t, keys[0], ""))
	crossed := -1
	for i := 0; i < 1000 && crossed < 0; i++ {
		k := mustBetween(t, keys[0], keys[1])
		keys = slices.Insert(keys, 1, k)
		if len(k) > RankMaxLen {
			crossed = i
		}
	}
	if crossed < 0 {
		t.Fatalf("1000 вставок в одно место не перешли порог %d", RankMaxLen)
	}
	spread := RankSpread(len(keys))
	if !sort.StringsAreSorted(spread) || len(spread) != len(keys) {
		t.Fatal("перераздача не упорядочена")
	}
	for _, k := range spread {
		if !ValidRank(k) || len(k) > RankMaxLen {
			t.Fatalf("перераздача дала %q", k)
		}
	}
	// После перераздачи вставка между соседями не растит длину.
	if k := mustBetween(t, spread[0], spread[1]); len(k) > len(spread[0]) {
		t.Fatalf("вставка после перераздачи %q из %q и %q", k, spread[0], spread[1])
	}
}

func TestRankSpread(t *testing.T) {
	if got := RankSpread(0); len(got) != 0 {
		t.Fatalf("пусто: %v", got)
	}
	for _, n := range []int{1, 2, 3, 61, 62, 1000, 5000} {
		keys := RankSpread(n)
		if len(keys) != n {
			t.Fatalf("n=%d: %d ключей", n, len(keys))
		}
		for i, k := range keys {
			if !ValidRank(k) {
				t.Fatalf("n=%d: %q не ключ", n, k)
			}
			if i > 0 && keys[i-1] >= k {
				t.Fatalf("n=%d: %q не больше %q", n, k, keys[i-1])
			}
			if len(k) != len(keys[0]) {
				t.Fatalf("n=%d: длины разные", n)
			}
		}
	}
	// Значения, на которые опирается заполнение миграции 000045.
	if got := strings.Join(RankSpread(3), ","); got != "FV,V1,kV" {
		t.Fatalf("RankSpread(3) = %s", got)
	}
}

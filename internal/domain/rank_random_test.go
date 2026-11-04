package domain

import (
	"math/rand"
	"slices"
	"sort"
	"testing"
)

// Случайные вставки в список ключей (CLAB-399, К-14): ключ между любыми
// двумя соседями и у краёв допустим и строго между ними — и при случайных
// вставках, и при вставках в одно место, и после равномерного набора.
func TestRankRandomInsertions(t *testing.T) {
	rng := rand.New(rand.NewSource(399))
	for round := 0; round < 40; round++ {
		keys := []string{}
		if round%2 == 1 {
			keys = RankSpread(1 + rng.Intn(70)) // старт — равномерный набор
		}
		for step := 0; step < 600; step++ {
			at := rng.Intn(len(keys) + 1)
			if round%4 >= 2 && len(keys) > 0 {
				at = len(keys) / 2 // вставки в одно место
			}
			lo, hi := "", ""
			if at > 0 {
				lo = keys[at-1]
			}
			if at < len(keys) {
				hi = keys[at]
			}
			k, err := RankBetween(lo, hi)
			if err != nil {
				t.Fatalf("round %d step %d: RankBetween(%q, %q): %v", round, step, lo, hi, err)
			}
			if !ValidRank(k) || (lo != "" && k <= lo) || (hi != "" && k >= hi) {
				t.Fatalf("round %d step %d: ключ %q не между %q и %q", round, step, k, lo, hi)
			}
			keys = slices.Insert(keys, at, k)
		}
		if !sort.StringsAreSorted(keys) || len(slices.Compact(slices.Clone(keys))) != len(keys) {
			t.Fatalf("round %d: ключи не по возрастанию или повторяются", round)
		}
	}
}

package domain

import (
	"fmt"
	"strings"
)

// Ранг бэклога (решение 0075, эпик CLAB-368): ручной порядок задачи внутри
// своей группы — верхнего уровня проекта или детей одного родителя. Ключ —
// дробный лексикографический (fractional indexing): между любыми двумя
// ключами есть третий, поэтому перемещение пишет ранг одной задачи, а не
// перенумеровывает соседей. Сравнение — побайтное: колонка tasks.rank
// объявлена COLLATE "C", и порядок в базе совпадает с порядком строк Go.

// RankDigits — алфавит ключа по возрастанию кодов ASCII.
const RankDigits = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// RankMaxLen — порог длины ключа. Вставка в одно и то же место растит ключ
// примерно на знак за шесть вставок; ключ длиннее порога — повод
// перераздать ключи всей группы равномерным набором (RankSpread), порядок
// группы при этом прежний.
const RankMaxLen = 50

const rankBase = len(RankDigits)

func rankDigit(c byte) int { return strings.IndexByte(RankDigits, c) }

// ValidRank — ключ из алфавита, непустой и не оканчивается младшей цифрой:
// иначе между «a» и «a0» не вставить ничего.
func ValidRank(k string) bool {
	if k == "" || k[len(k)-1] == RankDigits[0] {
		return false
	}
	for i := 0; i < len(k); i++ {
		if rankDigit(k[i]) < 0 {
			return false
		}
	}
	return true
}

// RankBetween — ключ строго между a и b; пустой край открыт: ("", b) — перед
// b, (a, "") — после a, ("", "") — первый ключ пустой группы. Ошибка — край
// не ключ или a не меньше b (у двух задач группы один ранг): тогда вызывающий
// перераздаёт группу.
func RankBetween(a, b string) (string, error) {
	if a != "" && !ValidRank(a) {
		return "", fmt.Errorf("ранг %q не ключ", a)
	}
	if b != "" && !ValidRank(b) {
		return "", fmt.Errorf("ранг %q не ключ", b)
	}
	switch {
	case a == "" && b == "":
		return string(RankDigits[rankBase/2]), nil
	case b == "":
		return rankAfter(a), nil
	case a == "":
		return rankBefore(b), nil
	case a >= b:
		return "", fmt.Errorf("ранг %q не меньше %q", a, b)
	}
	return rankMidpoint(a, b, false), nil
}

// rankAfter — кратчайший ключ больше a: первая цифра, которую можно
// увеличить, увеличивается, хвост отбрасывается. Так новая задача в конце
// группы растит ключ на знак лишь раз в несколько десятков заведений, а не
// раз в шесть, как середина до открытого края.
func rankAfter(a string) string {
	for i := 0; i < len(a); i++ {
		if d := rankDigit(a[i]); d < rankBase-1 {
			return a[:i] + string(RankDigits[d+1])
		}
	}
	return a + string(RankDigits[rankBase/2])
}

// rankBefore — кратчайший ключ меньше b тем же способом; цифру 1 до 0 не
// уменьшить (ключ кончался бы младшей цифрой) — тогда середина до пустого
// края.
func rankBefore(b string) string {
	for i := 0; i < len(b); i++ {
		if d := rankDigit(b[i]); d >= 2 {
			return b[:i] + string(RankDigits[d-1])
		}
	}
	return rankMidpoint("", b, false)
}

// rankMidpoint — середина между дробями 0.a и 0.b (open — b открыт, то есть
// 1.0). a < b, ни один не кончается младшей цифрой.
func rankMidpoint(a, b string, open bool) string {
	zero := RankDigits[0]
	at := func(s string, i int) byte {
		if i < len(s) {
			return s[i]
		}
		return zero
	}
	if !open {
		n := 0
		for n < len(b) && at(a, n) == b[n] {
			n++
		}
		if n > 0 {
			return b[:n] + rankMidpoint(rankTail(a, n), b[n:], false)
		}
	}
	da, db := 0, rankBase
	if a != "" {
		da = rankDigit(a[0])
	}
	if !open {
		db = rankDigit(b[0])
	}
	if db-da > 1 {
		return string(RankDigits[(da+db+1)/2])
	}
	if !open && len(b) > 1 {
		return b[:1]
	}
	return string(RankDigits[da]) + rankMidpoint(rankTail(a, 1), "", true)
}

func rankTail(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

// RankSpread — n ключей по возрастанию, равномерно по пространству одной
// длины с запасом: шаг не меньше основания, так что между соседями
// поместится вставка без роста длины. Ключ, кончающийся младшей цифрой,
// сдвигается на единицу — шаг больше единицы, порядок не ломается. Тот же
// набор (длина, шаг, сдвиг) считает миграция 000045 при заполнении.
func RankSpread(n int) []string {
	if n <= 0 {
		return []string{}
	}
	length, space := 1, rankBase
	for space < rankBase*(n+1) {
		length++
		space *= rankBase
	}
	step := space / (n + 1)
	out := make([]string, n)
	buf := make([]byte, length)
	for i := range out {
		v := step * (i + 1)
		if v%rankBase == 0 {
			v++
		}
		for j := length - 1; j >= 0; j-- {
			buf[j] = RankDigits[v%rankBase]
			v /= rankBase
		}
		out[i] = string(buf)
	}
	return out
}

// Package embed — векторизация текста для семантического поиска.
//
// Провайдер выбирается настройкой, потому что у индексации три разных режима
// жизни: в продакшене считает модель, в локальном прогоне ключа обычно нет, а
// в тестах вектор вообще не нужен. Разница спрятана за интерфейсом, чтобы
// индексатор не знал, кто именно считает координаты.
package embed

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Dim — размерность вектора. Зафиксирована на уровне колонки vector(1024) в
// схеме: смена модели на другую размерность — это миграция с новой колонкой и
// полной переиндексацией, а не настройка (решение 0008).
const Dim = 1024

// Kind различает, что векторизуется. У Voyage от этого зависит вход модели:
// документ и запрос кладутся в разные концы пространства намеренно.
type Kind string

const (
	KindDocument Kind = "document"
	KindQuery    Kind = "query"
)

// Embedder считает векторы пачкой: и у модели, и у сети накладные расходы на
// вызов сильно больше, чем на лишний текст в нём.
type Embedder interface {
	// Model — имя модели, попадающее в embedding_model и в ключ кэша.
	// Вектор от другой модели несравним с этими, поэтому имя хранится рядом.
	Model() string
	Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error)
}

// Hash — детерминированная заглушка для локального прогона без ключа.
//
// Это мешок слов, спроецированный в Dim координат, а не семантика: близкими
// окажутся тексты с общими словами, и только. Нужна затем, чтобы проводка
// (кэш, запись вектора, гибридный поиск) проверялась целиком и без сети.
type Hash struct{}

func (Hash) Model() string { return "hash-dev" }

func (Hash) Embed(_ context.Context, texts []string, _ Kind) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = hashVector(t)
	}
	return out, nil
}

func hashVector(text string) []float32 {
	v := make([]float32, Dim)
	for _, tok := range tokenize(text) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum64()
		// Два касания на токен: одно измерение слишком легко забивается
		// частотными словами.
		for k := range 2 {
			idx := int((sum >> (uint(k) * 20)) % Dim)
			sign := float32(1)
			if (sum>>(uint(k)*20+19))&1 == 1 {
				sign = -1
			}
			v[idx] += sign
		}
	}
	return normalize(v)
}

func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
}

// normalize приводит вектор к единичной длине: поиск идёт по косинусу, а
// нулевой вектор в hnsw ведёт себя как равноудалённый от всего.
func normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		v[0] = 1
		return v
	}
	norm := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= norm
	}
	return v
}

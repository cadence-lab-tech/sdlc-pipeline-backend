package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"
)

// Chunk — кусок файла, который уедет в эмбеддинг и в выдачу агенту.
type Chunk struct {
	Ord       int
	StartLine int
	EndLine   int
	// Ближайшее объемлющее объявление: без него выдача — это «строки 340-360
	// какого-то файла», и агенту приходится открывать файл целиком.
	SymbolFQN string
	Text      string
	TextSHA   string
	Tokens    int
}

const (
	defaultChunkChars = 1600
	// Кусок короче этого не режется от следующего символа: сотня кусков по
	// три строки стоит столько же вызовов модели, сколько десяток осмысленных.
	minChunkChars = 400
)

// Chunks режет файл по границам объявлений, а не окном фиксированной длины.
//
// Перекрытия между кусками нет намеренно: граница проходит там, где кончается
// смысловая единица, и перекрытие только раздувало бы счёт эмбеддингов —
// а он в этой системе оплачивается на каждый коммит.
func Chunks(filePath string, content []byte, maxChars int) []Chunk {
	if maxChars <= 0 {
		maxChars = defaultChunkChars
	}
	text := string(content)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(text, "\n")

	// Строка → объемлющий символ. Символы идут по возрастанию StartLine,
	// вложенных среди них нет: извлекаются только объявления первой колонки.
	symbols := Symbols(filePath, content)
	owner := make([]string, len(lines)+1)
	starts := map[int]bool{}
	for _, s := range symbols {
		starts[s.StartLine] = true
		for ln := s.StartLine; ln <= s.EndLine && ln <= len(lines); ln++ {
			owner[ln] = s.FQN
		}
	}

	var out []Chunk
	start := 1
	size := 0
	flush := func(end int) {
		body := strings.Join(lines[start-1:end], "\n")
		if strings.TrimSpace(body) == "" {
			return
		}
		sum := sha256.Sum256([]byte(body))
		out = append(out, Chunk{
			Ord:       len(out),
			StartLine: start,
			EndLine:   end,
			SymbolFQN: owner[start],
			Text:      body,
			TextSHA:   hex.EncodeToString(sum[:]),
			Tokens:    estimateTokens(body),
		})
	}

	for ln := 1; ln <= len(lines); ln++ {
		lineLen := len(lines[ln-1]) + 1
		// Новое объявление начинает новый кусок, если предыдущий уже набрал
		// осмысленный объём; иначе мелочь слипается в один.
		if ln > start && starts[ln] && size >= minChunkChars {
			flush(ln - 1)
			start, size = ln, 0
		}
		if ln > start && size+lineLen > maxChars {
			flush(ln - 1)
			start, size = ln, 0
		}
		size += lineLen
	}
	flush(len(lines))
	return out
}

// estimateTokens — грубая оценка: у кода и русского текста разная плотность,
// а точный счёт потребовал бы токенизатора модели. Число нужно только для
// метрик и предохранителей, не для биллинга.
func estimateTokens(s string) int {
	return utf8.RuneCountInString(s)/4 + 1
}

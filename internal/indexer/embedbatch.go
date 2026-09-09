package indexer

import (
	"context"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// embedBatch копит куски нескольких файлов и считает их одним вызовом модели.
//
// Пачкой, а не по файлу: у вызова накладные расходы больше, чем у лишнего
// текста в нём, а типичный файл даёт три-четыре куска. Перед вызовом пачка
// сходит в кэш и спросит модель только про то, чего там нет.
type embedBatch struct {
	ix        *Indexer
	projectID uuid.UUID
	pending   []pendingFile
	count     int
	embedded  int
}

type pendingFile struct {
	fileID uuid.UUID
	chunks []Chunk
}

func newEmbedBatch(ix *Indexer, projectID uuid.UUID) *embedBatch {
	return &embedBatch{ix: ix, projectID: projectID}
}

func (b *embedBatch) add(ctx context.Context, fileID uuid.UUID, chunks []Chunk) error {
	if len(chunks) == 0 {
		return nil
	}
	b.pending = append(b.pending, pendingFile{fileID: fileID, chunks: chunks})
	b.count += len(chunks)
	if b.count >= b.ix.opts.EmbedBatch {
		return b.flush(ctx)
	}
	return nil
}

func (b *embedBatch) flush(ctx context.Context) error {
	if len(b.pending) == 0 {
		return nil
	}
	pending := b.pending
	b.pending, b.count = nil, 0

	texts := map[string]string{}
	for _, f := range pending {
		for _, c := range f.chunks {
			texts[c.TextSHA] = c.Text
		}
	}

	vectors, model, sent, err := embedTexts(ctx, b.ix, texts)
	if err != nil {
		// Индекс без векторов остаётся полезным: полнотекстовый поиск и
		// детектор дрейфа работают и так. Векторы досчитает backfill —
		// валить из-за чужого API прогон, от которого зависит дрейф, нельзя.
		b.ix.log.Warn("эмбеддинги не посчитаны, куски записаны без векторов", "error", err)
		vectors, model, sent = nil, "", 0
	}
	b.embedded += sent

	for _, f := range pending {
		rows := make([]postgres.CodeChunkInput, 0, len(f.chunks))
		for _, c := range f.chunks {
			row := postgres.CodeChunkInput{
				Ord: c.Ord, StartLine: c.StartLine, EndLine: c.EndLine,
				SymbolFQN: c.SymbolFQN, Text: c.Text, TextSHA: c.TextSHA, Tokens: c.Tokens,
			}
			if vec, ok := vectors[c.TextSHA]; ok {
				row.Embedding, row.EmbedModel = vec, model
			}
			rows = append(rows, row)
		}
		if err := b.ix.store.InsertCodeChunks(ctx, b.ix.store.DB(), f.fileID, b.projectID, rows); err != nil {
			return err
		}
	}
	return nil
}

// embedTexts возвращает векторы по sha текстов, имя модели и число текстов,
// реально ушедших в модель (остальное закрыл кэш).
func embedTexts(ctx context.Context, ix *Indexer, texts map[string]string) (map[string][]float32, string, int, error) {
	if ix.embedder == nil || len(texts) == 0 {
		return nil, "", 0, nil
	}
	model := ix.embedder.Model()

	shas := make([]string, 0, len(texts))
	for sha := range texts {
		shas = append(shas, sha)
	}
	cached, err := ix.store.CachedEmbeddings(ctx, ix.store.DB(), model, shas)
	if err != nil {
		return nil, "", 0, err
	}

	missingSHA := make([]string, 0, len(texts))
	missingText := make([]string, 0, len(texts))
	for sha, text := range texts {
		if _, ok := cached[sha]; ok {
			continue
		}
		missingSHA = append(missingSHA, sha)
		missingText = append(missingText, text)
	}
	if len(missingText) == 0 {
		return cached, model, 0, nil
	}

	vectors, err := ix.embedder.Embed(ctx, missingText, embed.KindDocument)
	if err != nil {
		return nil, "", 0, err
	}
	if len(vectors) != len(missingText) {
		return nil, "", 0, errCount(len(vectors), len(missingText))
	}

	fresh := make(map[string][]float32, len(vectors))
	for i, sha := range missingSHA {
		fresh[sha] = vectors[i]
		cached[sha] = vectors[i]
	}
	if err := ix.store.CacheEmbeddings(ctx, ix.store.DB(), model, fresh); err != nil {
		return nil, "", 0, err
	}
	return cached, model, len(missingText), nil
}

func errCount(got, want int) error {
	return permanent("модель вернула %d векторов на %d текстов", got, want)
}

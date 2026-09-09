package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// IndexKnowledge раскладывает на куски то, что накопила сама система:
// артефакты задач и проектные документы.
//
// Отдельным проходом, а не на сдаче артефакта: агент не должен ждать чужой API,
// чтобы закрыть этап, и падение провайдера эмбеддингов не должно превращаться
// в невозможность сдать работу.
func (ix *Indexer) IndexKnowledge(ctx context.Context, limit int) (artifacts, documents int, err error) {
	pendingArtifacts, err := ix.store.PendingArtifacts(ctx, ix.store.DB(), limit)
	if err != nil {
		return 0, 0, err
	}
	for _, t := range pendingArtifacts {
		chunks, err := ix.textChunks(ctx, t)
		if err != nil {
			return artifacts, documents, err
		}
		if t.TaskID == nil {
			continue
		}
		if err := ix.store.ReplaceArtifactChunks(ctx, ix.store.DB(),
			t.ID, t.ProjectID, *t.TaskID, chunks); err != nil {
			return artifacts, documents, err
		}
		artifacts++
	}

	pendingDocs, err := ix.store.PendingDocuments(ctx, ix.store.DB(), limit)
	if err != nil {
		return artifacts, documents, err
	}
	for _, t := range pendingDocs {
		chunks, err := ix.textChunks(ctx, t)
		if err != nil {
			return artifacts, documents, err
		}
		if err := ix.store.ReplaceDocumentChunks(ctx, ix.store.DB(),
			t.ID, t.ProjectID, chunks); err != nil {
			return artifacts, documents, err
		}
		documents++
	}
	return artifacts, documents, nil
}

// textChunks режет содержимое и считает векторы через тот же кэш, что и код:
// один и тот же разбор, приложенный к двум задачам, эмбеддится один раз.
func (ix *Indexer) textChunks(ctx context.Context, t postgres.PendingChunkTarget) ([]postgres.TextChunkInput, error) {
	content, _, err := ix.store.BlobContent(ctx, ix.store.DB(), t.BlobSHA)
	if err != nil {
		return nil, err
	}

	// Имя с расширением .md — чтобы куски резались по заголовкам разбора, а
	// не по счётчику символов: артефакты пишутся структурированным текстом.
	chunks := Chunks("artifact.md", content, ix.opts.ChunkChars)
	texts := map[string]string{}
	for _, c := range chunks {
		texts[c.TextSHA] = c.Text
	}
	vectors, model, _, err := embedTexts(ctx, ix, texts)
	if err != nil {
		ix.log.Warn("эмбеддинги для текста не посчитаны, кладём без векторов",
			"kind", t.Kind, "title", t.Title, "error", err)
		vectors, model = nil, ""
	}

	out := make([]postgres.TextChunkInput, 0, len(chunks))
	for _, c := range chunks {
		row := postgres.TextChunkInput{
			Ord: c.Ord, Text: c.Text, TextSHA: c.TextSHA, Tokens: c.Tokens,
		}
		if vec, ok := vectors[c.TextSHA]; ok {
			row.Embedding, row.EmbedModel = vec, model
		}
		out = append(out, row)
	}
	return out, nil
}

// BackfillEmbeddings досчитывает векторы кускам, записанным без них.
//
// Нужен ровно потому, что прогон не валится из-за недоступной модели: индекс
// строится, поиск живёт на полнотекстовом, а векторы приезжают, когда
// провайдер снова отвечает — или когда его вообще включили после того, как
// система уже поработала без него.
func (ix *Indexer) BackfillEmbeddings(ctx context.Context, limit int) (int, error) {
	if ix.embedder == nil {
		return 0, nil
	}
	missing, err := ix.store.ChunksMissingEmbeddings(ctx, ix.store.DB(), limit)
	if err != nil || len(missing) == 0 {
		return 0, err
	}

	texts := map[string]string{}
	shaOf := make([]string, len(missing))
	for i, m := range missing {
		sha := m.TextSHA
		if sha == "" {
			sum := sha256.Sum256([]byte(m.Text))
			sha = hex.EncodeToString(sum[:])
		}
		shaOf[i] = sha
		texts[sha] = m.Text
	}

	vectors, model, _, err := embedTexts(ctx, ix, texts)
	if err != nil {
		return 0, err
	}

	done := 0
	for i, m := range missing {
		vec, ok := vectors[shaOf[i]]
		if !ok {
			continue
		}
		if err := ix.store.SetChunkEmbedding(ctx, ix.store.DB(), m.Source, m.ID, vec, model); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

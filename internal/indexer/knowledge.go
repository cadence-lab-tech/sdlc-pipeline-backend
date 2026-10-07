package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

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
	res, err := embedTexts(ctx, ix, texts)
	if err != nil {
		ix.log.Warn("эмбеддинги для текста не посчитаны, кладём без векторов",
			"kind", t.Kind, "title", t.Title, "error", err)
	}
	vectors, model := res.vectors, res.model

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

// RefreshStats — итог прохода досчёта: сколько кусков получили вектор текущей
// модели и у скольких кусков неактуальных прогонов вектор прежней модели снят.
type RefreshStats struct {
	Embedded int
	Cleared  int
}

// RefreshEmbeddings переводит индекс на текущую модель: снимает векторы
// прежних моделей с неактуальных прогонов и досчитывает пачками всё, что без
// вектора текущей модели, пока кандидаты не кончатся или не выйдет бюджет.
//
// Одной пачки за проход мало: после смены модели пересчитывать приходится
// весь актуальный индекс. Ошибка модели (429, сеть) прерывает проход с тем,
// что уже успели, — остальное досчитает следующий.
//
// Без провайдера ничего не делает: векторы, посчитанные раньше, остаются в
// базе, а поиск без вектора запроса их всё равно не использует.
func (ix *Indexer) RefreshEmbeddings(ctx context.Context, batch int, budget time.Duration) (RefreshStats, error) {
	var stats RefreshStats
	if ix.embedder == nil {
		return stats, nil
	}
	cleared, err := ix.store.ClearStaleRunEmbeddings(ctx, ix.store.DB(), ix.embedder.Model())
	if err != nil {
		return stats, err
	}
	stats.Cleared = cleared

	deadline := time.Now().Add(budget)
	for {
		n, err := ix.BackfillEmbeddings(ctx, batch)
		stats.Embedded += n
		if err != nil || n == 0 || !time.Now().Before(deadline) {
			return stats, err
		}
	}
}

// BackfillEmbeddings досчитывает одну пачку векторов кускам, записанным без
// них или посчитанным другой моделью.
//
// Нужен ровно потому, что прогон не валится из-за недоступной модели: индекс
// строится, поиск живёт на полнотекстовом, а векторы приезжают, когда
// провайдер снова отвечает — или когда его вообще включили после того, как
// система уже поработала без него. Смена модели — тот же случай: векторы
// прежней несравнимы с новыми, поиск их не видит, и их надо посчитать заново.
func (ix *Indexer) BackfillEmbeddings(ctx context.Context, limit int) (int, error) {
	if ix.embedder == nil {
		return 0, nil
	}
	missing, err := ix.store.ChunksMissingEmbeddings(ctx, ix.store.DB(), ix.embedder.Model(), limit)
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

	res, err := embedTexts(ctx, ix, texts)
	if err != nil {
		return 0, err
	}
	vectors, model := res.vectors, res.model

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

package workflow

import (
	"context"
	"strings"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// SearchInput — запрос агента к знанию проекта.
type SearchInput struct {
	Query string
	// code, artifacts, documents. Пусто — все три.
	Scopes []string
	// owner/name; сужает поиск по коду. На артефакты и документы не влияет.
	Repositories []string
	TopK         int
}

type SearchResult struct {
	Query string `json:"query"`
	// hybrid — вектор плюс полнотекст, text — только полнотекст: провайдер
	// эмбеддингов выключен или недоступен. Агент должен видеть разницу:
	// во втором режиме запрос «как тут устроено X» найдёт заметно меньше.
	Mode   string               `json:"mode"`
	Scopes []string             `json:"scopes"`
	Hits   []postgres.SearchHit `json:"results"`
}

// snippetLimit — сколько текста куска уезжает агенту. Кусок режется по
// границе объявления и обычно короче; предел нужен на случай сгенерированного
// файла в тысячу строк без единого объявления.
const snippetLimit = 4000

// SearchContext — семантический поиск по коду, артефактам задач и проектным
// документам.
//
// Гибридный: вектор находит смысл, полнотекст — точное имя функции или ключ
// конфига, по которым вектор промахивается. Ходит по актуальному индексу
// каждой ветки, а не по всей истории прогонов.
func (e *Engine) SearchContext(ctx context.Context, projectKey string, in SearchInput) (SearchResult, error) {
	out := SearchResult{Query: in.Query, Mode: "text"}
	if strings.TrimSpace(in.Query) == "" {
		return out, apperr.InvalidInput("пустой запрос")
	}

	project, err := e.store.GetProjectByKey(ctx, e.store.DB(), projectKey)
	if err != nil {
		return out, err
	}

	q := postgres.SearchInput{
		ProjectID:    project.ID,
		Query:        in.Query,
		Scopes:       in.Scopes,
		Repositories: in.Repositories,
		TopK:         in.TopK,
	}

	if e.embedder != nil {
		vectors, err := e.embedder.Embed(ctx, []string{in.Query}, embed.KindQuery)
		if err != nil {
			// Поиск не отказывает целиком: полнотекстовая половина работает
			// и без модели, и агенту лучше получить меньше, чем ничего.
			e.log.Warn("вектор запроса не посчитан, ищем полнотекстом", "error", err)
		} else if len(vectors) == 1 {
			// Вектор ищет только среди кусков той же модели: после смены
			// модели прежние векторы несравнимы и ждут досчёта.
			q.Vector, q.Model = vectors[0], e.embedder.Model()
			out.Mode = "hybrid"
		}
	}

	hits, err := e.store.SearchContext(ctx, e.store.DB(), q)
	if err != nil {
		return out, err
	}
	for i := range hits {
		if len(hits[i].Text) > snippetLimit {
			hits[i].Text = hits[i].Text[:snippetLimit] + "…"
		}
	}

	out.Hits = hits
	out.Scopes = q.Scopes
	if len(out.Scopes) == 0 {
		out.Scopes = []string{postgres.ScopeCode, postgres.ScopeArtifacts, postgres.ScopeDocuments}
	}
	return out, nil
}

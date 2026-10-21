package postgres

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// Scope* — источники, по которым ищет агент.
const (
	ScopeCode      = "code"
	ScopeArtifacts = "artifacts"
	ScopeDocuments = "documents"
)

// SearchHit — один кусок выдачи вместе с тем, откуда он взят: артефакт какой
// задачи и этапа либо файл и диапазон строк на конкретном коммите.
type SearchHit struct {
	Scope      string     `json:"scope"`
	Score      float64    `json:"score"`
	Match      string     `json:"match"`
	Repository string     `json:"repository,omitempty"`
	Path       string     `json:"path,omitempty"`
	Lang       string     `json:"lang,omitempty"`
	Commit     string     `json:"commit,omitempty"`
	StartLine  int        `json:"start_line,omitempty"`
	EndLine    int        `json:"end_line,omitempty"`
	SymbolFQN  string     `json:"symbol_fqn,omitempty"`
	ArtifactID *uuid.UUID `json:"artifact_id,omitempty"`
	DocumentID *uuid.UUID `json:"document_id,omitempty"`
	TaskKey    string     `json:"task,omitempty"`
	Stage      string     `json:"stage,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Title      string     `json:"title,omitempty"`
	Text       string     `json:"text"`

	chunkID uuid.UUID
}

type SearchInput struct {
	ProjectID uuid.UUID
	Query     string
	// Вектор запроса. Пустой — поиск остаётся полнотекстовым: без провайдера
	// эмбеддингов система обязана продолжать работать, просто хуже.
	Vector []float32
	// Модель, посчитавшая Vector. Векторная половина ищет только среди
	// кусков этой модели: векторы разных моделей несравнимы, и после смены
	// модели старые дали бы шум с уверенным видом. Куски прежней модели
	// находит полнотекстовая половина, пока их не пересчитал досчёт.
	Model        string
	Scopes       []string
	Repositories []string
	TopK         int
	// Repositories — исчерпывающий список, а не фильтр по желанию: пустой
	// означает «кода нет», а не «весь проект» (токен задачи без области).
	RestrictRepositories bool
	// Артефакты только этих задач (круг чтения токена задачи).
	TaskIDs       []uuid.UUID
	RestrictTasks bool
}

// rrfK — константа Reciprocal Rank Fusion. Гибрид нужен потому, что половина
// запросов агента — это точное имя функции или ключ конфига, по которым
// вектор промахивается, а вторая половина — «как тут устроено X», по которым
// промахивается полнотекст. Складывать сырые оценки нельзя: они в разных
// шкалах, а ранги сравнимы.
const rrfK = 60.0

func (s *Store) SearchContext(ctx context.Context, q Querier, in SearchInput) ([]SearchHit, error) {
	if in.TopK <= 0 || in.TopK > 100 {
		in.TopK = 12
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		scopes = []string{ScopeCode, ScopeArtifacts, ScopeDocuments}
	}
	if in.Repositories == nil {
		in.Repositories = []string{}
	}
	// Кандидатов берём с запасом: слияние двух списков по рангу имеет смысл
	// только если каждый из них длиннее итоговой выдачи.
	candidates := in.TopK * 4

	merged := map[uuid.UUID]*SearchHit{}
	add := func(hits []SearchHit, source string) {
		for rank, h := range hits {
			existing, ok := merged[h.chunkID]
			if !ok {
				copyHit := h
				copyHit.Match = source
				copyHit.Score = 1 / (rrfK + float64(rank+1))
				merged[h.chunkID] = &copyHit
				continue
			}
			existing.Score += 1 / (rrfK + float64(rank+1))
			if !strings.Contains(existing.Match, source) {
				existing.Match += "+" + source
			}
		}
	}

	for _, scope := range scopes {
		if scope == ScopeCode && in.RestrictRepositories && len(in.Repositories) == 0 {
			continue
		}
		if scope == ScopeArtifacts && in.RestrictTasks && len(in.TaskIDs) == 0 {
			continue
		}
		text, vector, err := s.searchScope(ctx, q, scope, in, candidates)
		if err != nil {
			return nil, err
		}
		add(vector, "vector")
		add(text, "text")
	}

	out := make([]SearchHit, 0, len(merged))
	for _, h := range merged {
		out = append(out, *h)
	}
	// Сортировка своя, а не в SQL: списки пришли из разных запросов.
	sortHits(out)
	if len(out) > in.TopK {
		out = out[:in.TopK]
	}
	return out, nil
}

func sortHits(hits []SearchHit) {
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].Score > hits[j-1].Score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
}

func (s *Store) searchScope(ctx context.Context, q Querier, scope string, in SearchInput, limit int) (text, vector []SearchHit, err error) {
	var textSQL, vectorSQL string
	var scan func(rows interface{ Scan(...any) error }) (SearchHit, error)

	switch scope {
	case ScopeCode:
		textSQL, vectorSQL, scan = codeSearchSQL()
	case ScopeArtifacts:
		textSQL, vectorSQL, scan = artifactSearchSQL(taskFilter(in))
	case ScopeDocuments:
		textSQL, vectorSQL, scan = documentSearchSQL()
	default:
		return nil, nil, apperr.InvalidInput(
			"область поиска %q: бывают code, artifacts и documents", scope)
	}

	if strings.TrimSpace(in.Query) != "" {
		text, err = s.searchQuery(ctx, q, textSQL, scan, in.ProjectID, in.Repositories, in.Query, limit)
		if err != nil {
			return nil, nil, err
		}
	}
	if len(in.Vector) > 0 {
		vector, err = s.searchQuery(ctx, q, vectorSQL, scan, in.ProjectID, in.Repositories, vectorLiteral(in.Vector), limit, in.Model)
		if err != nil {
			return nil, nil, err
		}
	}
	return text, vector, nil
}

func (s *Store) searchQuery(ctx context.Context, q Querier, sql string,
	scan func(rows interface{ Scan(...any) error }) (SearchHit, error),
	projectID uuid.UUID, repos []string, arg string, limit int, extra ...any) ([]SearchHit, error) {

	rows, err := q.Query(ctx, sql, append([]any{projectID, repos, arg, limit}, extra...)...)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()

	var out []SearchHit
	for rows.Next() {
		h, err := scan(rows)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, h)
	}
	return out, translate(rows.Err())
}

// currentRuns — актуальный индекс каждой ветки. Поиск по коду обязан ходить
// именно по нему: прогоны предыдущих коммитов остаются в базе, и без этого
// фильтра агент получал бы несколько версий одного файла.
const currentRuns = `
	WITH runs AS (
	    SELECT DISTINCT ON (ir.repository_id, ir.branch) ir.id, ir.commit_sha
	    FROM repo_index_runs ir
	    JOIN repositories r ON r.id = ir.repository_id
	    WHERE r.project_id = $1 AND ir.state = 'completed'
	      AND (cardinality($2::text[]) = 0 OR r.owner || '/' || r.name = ANY($2))
	    ORDER BY ir.repository_id, ir.branch, ir.finished_at DESC
	)`

func codeSearchSQL() (string, string, func(interface{ Scan(...any) error }) (SearchHit, error)) {
	const columns = `
		SELECT cc.id, r.owner || '/' || r.name, cf.path, coalesce(cf.lang, ''),
		       runs.commit_sha, cc.start_line, cc.end_line,
		       coalesce(cc.symbol_fqn, ''), cc.text
		FROM code_chunks cc
		JOIN code_files cf   ON cf.id = cc.code_file_id
		JOIN runs            ON runs.id = cf.index_run_id
		JOIN repositories r  ON r.id = cf.repository_id
		WHERE cc.project_id = $1`

	text := currentRuns + columns + `
		  AND cc.tsv @@ sdlc_tsquery('simple', $3)
		ORDER BY ts_rank_cd(cc.tsv, sdlc_tsquery('simple', $3)) DESC
		LIMIT $4`

	vector := currentRuns + columns + `
		  AND cc.embedding IS NOT NULL AND cc.embedding_model = $5
		ORDER BY cc.embedding <=> $3::vector
		LIMIT $4`

	scan := func(row interface{ Scan(...any) error }) (SearchHit, error) {
		h := SearchHit{Scope: ScopeCode}
		err := row.Scan(&h.chunkID, &h.Repository, &h.Path, &h.Lang, &h.Commit,
			&h.StartLine, &h.EndLine, &h.SymbolFQN, &h.Text)
		return h, err
	}
	return text, vector, scan
}

// taskFilter — условие на задачи артефактов. Идентификаторы вставляются
// литералами: это uuid.UUID, а не ввод клиента, и позиционные параметры
// текстового и векторного запросов от этого не разъезжаются.
func taskFilter(in SearchInput) string {
	if !in.RestrictTasks {
		return ""
	}
	if len(in.TaskIDs) == 0 {
		return " AND false"
	}
	ids := make([]string, 0, len(in.TaskIDs))
	for _, id := range in.TaskIDs {
		ids = append(ids, "'"+id.String()+"'")
	}
	return " AND ac.task_id IN (" + strings.Join(ids, ",") + ")"
}

func artifactSearchSQL(filter string) (string, string, func(interface{ Scan(...any) error }) (SearchHit, error)) {
	// Перекрытые версии из выдачи исключены: агенту нужен актуальный разбор,
	// а не тот, который уже переписали.
	columns := `
		SELECT ac.id, a.id, t.key, coalesce(ws.key, ''), a.kind, a.title, ac.text
		FROM artifact_chunks ac
		JOIN artifacts a ON a.id = ac.artifact_id
		JOIN tasks t     ON t.id = ac.task_id
		LEFT JOIN stage_runs sr        ON sr.id = a.stage_run_id
		LEFT JOIN workflow_statuses ws ON ws.id = sr.status_id
		WHERE ac.project_id = $1 AND cardinality($2::text[]) >= 0
		  AND a.status <> 'superseded'` + filter

	text := columns + `
		  AND ac.tsv @@ sdlc_tsquery('russian', $3)
		ORDER BY ts_rank_cd(ac.tsv, sdlc_tsquery('russian', $3)) DESC
		LIMIT $4`

	vector := columns + `
		  AND ac.embedding IS NOT NULL AND ac.embedding_model = $5
		ORDER BY ac.embedding <=> $3::vector
		LIMIT $4`

	scan := func(row interface{ Scan(...any) error }) (SearchHit, error) {
		h := SearchHit{Scope: ScopeArtifacts}
		var artifactID uuid.UUID
		err := row.Scan(&h.chunkID, &artifactID, &h.TaskKey, &h.Stage, &h.Kind, &h.Title, &h.Text)
		h.ArtifactID = &artifactID
		return h, err
	}
	return text, vector, scan
}

func documentSearchSQL() (string, string, func(interface{ Scan(...any) error }) (SearchHit, error)) {
	const columns = `
		SELECT dc.id, d.id, d.kind, d.title, dc.text
		FROM document_chunks dc
		JOIN project_documents d ON d.id = dc.document_id
		WHERE dc.project_id = $1 AND cardinality($2::text[]) >= 0`

	text := columns + `
		  AND dc.tsv @@ sdlc_tsquery('russian', $3)
		ORDER BY ts_rank_cd(dc.tsv, sdlc_tsquery('russian', $3)) DESC
		LIMIT $4`

	vector := columns + `
		  AND dc.embedding IS NOT NULL AND dc.embedding_model = $5
		ORDER BY dc.embedding <=> $3::vector
		LIMIT $4`

	scan := func(row interface{ Scan(...any) error }) (SearchHit, error) {
		h := SearchHit{Scope: ScopeDocuments}
		var documentID uuid.UUID
		err := row.Scan(&h.chunkID, &documentID, &h.Kind, &h.Title, &h.Text)
		h.DocumentID = &documentID
		return h, err
	}
	return text, vector, scan
}

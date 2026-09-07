// Package indexer — серверная индексация репозиториев проекта.
//
// Главное свойство: прогон инкрементальный. Кодовая база под задачами
// двигается непрерывно и куда чаще, чем задачи по циклу, поэтому полная
// переиндексация на каждый коммит означала бы выкачивать репозиторий целиком
// и заново платить за эмбеддинги всех его файлов ради десятка изменившихся
// строк. Прогон сравнивает листинг дерева с предыдущим, наследует у него всё,
// что не изменилось, вместе с уже посчитанными векторами, и трогает сетью и
// моделью только дельту.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/embed"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/github"
	"github.com/stepaKaziatko/sdlc-pipeline-backend/internal/store/postgres"
)

// Repo — координаты репозитория у провайдера.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Entry — файл в дереве коммита.
type Entry struct {
	Path string
	// SHA объекта у провайдера: по нему и только по нему решается, изменился
	// ли файл. Сравнение содержимого потребовало бы его выкачать.
	SHA  string
	Size int64
}

// Source — то, что умеет читать репозиторий. Интерфейс объявлен здесь, а не
// в провайдере: индексатору нужны четыре операции, и второй провайдер (или
// фейк в тесте) обязан реализовать ровно их.
type Source interface {
	Head(ctx context.Context, repo Repo, branch string) (string, error)
	Tree(ctx context.Context, repo Repo, commit string) (entries []Entry, truncated bool, err error)
	Blob(ctx context.Context, repo Repo, sha string) ([]byte, error)
	// ForRepository отдаёт источник с учётными данными репозитория:
	// credential_ref — имя секрета, а не сам токен.
	ForRepository(credentialRef string) Source
}

// permanentError — сбой, который не исправится повтором: репозиторий не
// влезает в листинг, ветка удалена, токен не даёт доступа. Повторять такой
// прогон значит биться в ту же стену каждую минуту.
type permanentError struct{ error }

func permanent(format string, args ...any) error {
	return permanentError{fmt.Errorf(format, args...)}
}

type Options struct {
	MaxFileBytes int
	MaxFiles     int
	ChunkChars   int
	// Сколько кусков накапливать перед обращением к модели: у вызова
	// накладные расходы больше, чем у лишнего текста в нём.
	EmbedBatch   int
	MaxAttempts  int
	RetryBackoff time.Duration
	StuckAfter   time.Duration
}

const (
	defaultMaxFileBytes = 512 << 10
	defaultMaxFiles     = 20000
	defaultEmbedBatch   = 96
	defaultMaxAttempts  = 3
	defaultBackoff      = 5 * time.Minute
	defaultStuckAfter   = 30 * time.Minute
)

type Indexer struct {
	store    *postgres.Store
	source   Source
	embedder embed.Embedder
	log      *slog.Logger
	opts     Options
	// Последняя ошибка чтения головы по ветке. Воркер ходит за головами
	// каждый тик, и репозиторий без доступа кричал бы в лог круглые сутки:
	// повтор той же ошибки уходит в debug, новая — снова в warn.
	headErrors map[string]string
}

func New(store *postgres.Store, source Source, embedder embed.Embedder, log *slog.Logger, opts Options) *Indexer {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = defaultMaxFileBytes
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = defaultMaxFiles
	}
	if opts.ChunkChars <= 0 {
		opts.ChunkChars = defaultChunkChars
	}
	if opts.EmbedBatch <= 0 {
		opts.EmbedBatch = defaultEmbedBatch
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultMaxAttempts
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = defaultBackoff
	}
	if opts.StuckAfter <= 0 {
		opts.StuckAfter = defaultStuckAfter
	}
	return &Indexer{
		store: store, source: source, embedder: embedder, log: log, opts: opts,
		headErrors: map[string]string{},
	}
}

// Enqueue сверяет головы отслеживаемых веток с тем, что уже проиндексировано,
// и ставит в очередь недостающие коммиты.
//
// Ходит в сеть по одному запросу на ветку, поэтому вызывается по таймеру, а не
// на каждый чих: смысл в том, чтобы заметить движение main, а не в том, чтобы
// заметить его в ту же секунду. Вебхук, когда он появится, встанет рядом и
// будет ставить прогон тем же вызовом с trigger='webhook'.
func (ix *Indexer) Enqueue(ctx context.Context) (int, error) {
	targets, err := ix.store.IndexTargets(ctx, ix.store.DB())
	if err != nil {
		return 0, err
	}

	queued := 0
	for _, t := range targets {
		src := ix.source.ForRepository(deref(t.CredentialRef))
		for _, branch := range t.Branches {
			head, err := src.Head(ctx, Repo{t.Owner, t.Name}, branch)
			if err != nil {
				// Недоступный репозиторий не должен останавливать остальные:
				// у одного проекта их несколько, и токен мог протухнуть на
				// одном из них.
				ix.logHeadError(t.Repository(), branch, err)
				continue
			}
			delete(ix.headErrors, t.Repository()+"@"+branch)
			run, created, err := ix.store.EnqueueIndexRun(ctx, ix.store.DB(), postgres.EnqueueIndexRunInput{
				RepositoryID: t.RepositoryID,
				Branch:       branch,
				CommitSHA:    head,
				Trigger:      "schedule",
			})
			if err != nil {
				return queued, err
			}
			if created {
				queued++
				ix.log.Info("коммит поставлен в очередь на индексацию",
					"repository", t.Repository(), "branch", branch,
					"commit", short(head), "run", run.ID)
			}
		}
	}
	return queued, nil
}

// logHeadError не повторяет одну и ту же жалобу каждый тик: приватный
// репозиторий без токена — это состояние настройки, а не событие, и в логе
// оно должно появиться один раз, а не раз в минуту навсегда.
func (ix *Indexer) logHeadError(repository, branch string, err error) {
	key := repository + "@" + branch
	message := err.Error()
	hint := ""
	if errors.Is(err, github.ErrNotFound) {
		// Приватный репозиторий без прав выглядит у провайдера ровно как
		// несуществующий; сказать про это стоит прямо.
		hint = "нет ветки либо у токена нет доступа к репозиторию"
	}

	if ix.headErrors[key] == message {
		ix.log.Debug("голова ветки по-прежнему не читается",
			"repository", repository, "branch", branch, "error", err)
		return
	}
	ix.headErrors[key] = message
	ix.log.Warn("голова ветки не прочитана",
		"repository", repository, "branch", branch, "error", err, "hint", hint)
}

// RunNext забирает из очереди один прогон и исполняет его.
//
// Возвращает завершённый прогон: детектор дрейфа запускается сразу после и
// именно по нему — расхождение, замеченное через час, задача уже проехала.
func (ix *Indexer) RunNext(ctx context.Context) (*postgres.IndexRun, error) {
	if _, err := ix.store.RequeueStuckRuns(ctx, ix.store.DB(), ix.opts.StuckAfter); err != nil {
		return nil, err
	}

	run, found, err := ix.store.ClaimIndexRun(ctx)
	if err != nil || !found {
		return nil, err
	}

	started := time.Now()
	counters, err := ix.process(ctx, run)
	if err != nil {
		attempts := ix.opts.MaxAttempts
		if errors.As(err, &permanentError{}) {
			attempts = 0
		}
		ix.log.Error("прогон индексации не удался",
			"repository", run.Repository, "commit", short(run.CommitSHA),
			"attempt", run.Attempt, "error", err)
		if failErr := ix.store.FailIndexRun(ctx, ix.store.DB(), run.ID,
			err.Error(), ix.opts.RetryBackoff, attempts); failErr != nil {
			return nil, failErr
		}
		return nil, nil
	}

	if err := ix.store.FinishIndexRun(ctx, ix.store.DB(), run.ID, counters); err != nil {
		return nil, err
	}
	done, err := ix.store.GetIndexRun(ctx, ix.store.DB(), run.ID)
	if err != nil {
		return nil, err
	}

	ix.log.Info("репозиторий проиндексирован",
		"repository", run.Repository, "branch", run.Branch, "commit", short(run.CommitSHA),
		"kind", counters.Kind,
		"files_indexed", counters.FilesIndexed, "files_reused", counters.FilesReused,
		"chunks_indexed", counters.ChunksIndexed, "chunks_reused", counters.ChunksReused,
		"chunks_embedded", counters.ChunksEmbedded,
		"took", time.Since(started).Round(time.Millisecond))
	return &done, nil
}

func (ix *Indexer) process(ctx context.Context, run postgres.IndexRun) (postgres.IndexCounters, error) {
	var counters postgres.IndexCounters
	repo := Repo{run.Owner, run.Name}
	src := ix.source.ForRepository(deref(run.CredentialRef))

	entries, truncated, err := src.Tree(ctx, repo, run.CommitSHA)
	if err != nil {
		return counters, err
	}
	if truncated {
		// Неполный листинг выглядит как массовое удаление файлов и завёл бы
		// ложные сигналы дрейфа по всем задачам репозитория.
		return counters, permanent("листинг дерева %s обрезан провайдером: репозиторий слишком велик для индексации целиком", repo)
	}

	files := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if skip, _ := SkipPath(e.Path, e.Size, ix.opts.MaxFileBytes, run.ExcludeGlobs); skip {
			continue
		}
		files = append(files, e)
	}
	if len(files) > ix.opts.MaxFiles {
		return counters, permanent("в %s %d файлов при пределе %d: сузьте exclude_globs",
			repo, len(files), ix.opts.MaxFiles)
	}

	parent, hasParent, err := ix.store.LatestCompletedRun(ctx, ix.store.DB(), run.RepositoryID, run.Branch)
	if err != nil {
		return counters, err
	}
	parentFiles := map[string]postgres.ParentFile{}
	if hasParent {
		if parentFiles, err = ix.store.ParentFiles(ctx, ix.store.DB(), parent.ID); err != nil {
			return counters, err
		}
		counters.ParentID = &parent.ID
		counters.Kind = "incremental"
	} else {
		counters.Kind = "full"
	}

	// Повторная попытка того же прогона начинает с чистого листа: часть
	// файлов от прошлой попытки уже записана, а (index_run_id, path) уникален.
	if err := ix.clearRun(ctx, run.ID); err != nil {
		return counters, err
	}

	batch := newEmbedBatch(ix, run.ProjectID)
	var changes []postgres.FileChangeInput
	seen := make(map[string]bool, len(files))

	for _, e := range files {
		seen[e.Path] = true
		prev, existed := parentFiles[e.Path]

		if existed && prev.SourceSHA != "" && prev.SourceSHA == e.SHA {
			// Файл не изменился: ни сети, ни модели он не стоит.
			chunks, err := ix.store.CopyFileFromParent(ctx, ix.store.DB(), run.ID, prev.ID)
			if err != nil {
				return counters, err
			}
			counters.FilesReused++
			counters.ChunksReused += chunks
			continue
		}

		content, err := src.Blob(ctx, repo, e.SHA)
		if err != nil {
			return counters, fmt.Errorf("файл %s: %w", e.Path, err)
		}
		if SkipContent(content) {
			continue
		}

		fileID, chunks, err := ix.writeFile(ctx, run, e, content)
		if err != nil {
			return counters, err
		}
		counters.FilesIndexed++
		counters.ChunksIndexed += len(chunks)
		if err := batch.add(ctx, fileID, chunks); err != nil {
			return counters, err
		}

		if !hasParent {
			// У первого прогона ветки дельты нет: относительно чего.
			continue
		}
		change := postgres.FileChangeInput{
			RunID: run.ID, RepositoryID: run.RepositoryID, Path: e.Path,
			ChangeType: "added", CommitSHA: run.CommitSHA,
		}
		if existed {
			change.ChangeType = "modified"
			before, err := ix.previousContent(ctx, prev)
			if err != nil {
				return counters, err
			}
			change.ChangedSymbols = ChangedSymbols(e.Path, before, content)
			change.Additions, change.Deletions = lineDelta(before, content)
		} else {
			change.Additions = countLines(content)
			change.ChangedSymbols = ChangedSymbols(e.Path, nil, content)
		}
		changes = append(changes, change)
	}

	if err := batch.flush(ctx); err != nil {
		return counters, err
	}
	counters.ChunksEmbedded = batch.embedded

	if hasParent {
		for path, prev := range parentFiles {
			if seen[path] {
				continue
			}
			before, err := ix.previousContent(ctx, prev)
			if err != nil {
				return counters, err
			}
			changes = append(changes, postgres.FileChangeInput{
				RunID: run.ID, RepositoryID: run.RepositoryID, Path: path,
				ChangeType: "deleted", CommitSHA: run.CommitSHA,
				ChangedSymbols: ChangedSymbols(path, before, nil),
				Deletions:      countLines(before),
			})
		}
	}

	if err := ix.store.InsertFileChanges(ctx, ix.store.DB(), changes); err != nil {
		return counters, err
	}
	return counters, nil
}

// clearRun сносит следы предыдущей попытки этого же прогона.
func (ix *Indexer) clearRun(ctx context.Context, runID uuid.UUID) error {
	return ix.store.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM code_files WHERE index_run_id = $1`, runID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM code_file_changes WHERE index_run_id = $1`, runID)
		return err
	})
}

// writeFile кладёт содержимое в контент-адресуемое хранилище и заводит файл
// вместе с его символами. Куски уходят в пачку эмбеддингов и записываются ею.
func (ix *Indexer) writeFile(ctx context.Context, run postgres.IndexRun, e Entry, content []byte) (uuid.UUID, []Chunk, error) {
	var fileID uuid.UUID
	chunks := Chunks(e.Path, content, ix.opts.ChunkChars)

	err := ix.store.InTx(ctx, func(tx pgx.Tx) error {
		sha, err := ix.store.PutBlob(ctx, tx, content, "text/plain")
		if err != nil {
			return err
		}
		fileID, err = ix.store.InsertCodeFile(ctx, tx, postgres.CodeFileInput{
			RunID: run.ID, RepositoryID: run.RepositoryID, Path: e.Path,
			Lang: Lang(e.Path), SizeBytes: int64(len(content)),
			BlobSHA: sha, SourceSHA: e.SHA,
		})
		if err != nil {
			return err
		}

		symbols := Symbols(e.Path, content)
		rows := make([]postgres.CodeSymbolInput, 0, len(symbols))
		for _, s := range symbols {
			rows = append(rows, postgres.CodeSymbolInput{
				Kind: s.Kind, Name: s.Name, FQN: s.FQN,
				StartLine: s.StartLine, EndLine: s.EndLine, Exported: s.Exported,
			})
		}
		return ix.store.InsertCodeSymbols(ctx, tx, fileID, run.RepositoryID, rows)
	})
	return fileID, chunks, err
}

// previousContent достаёт содержимое файла из предыдущего прогона. Оно уже
// лежит в blobs, поэтому сравнение версий не стоит ни одного запроса в сеть.
func (ix *Indexer) previousContent(ctx context.Context, prev postgres.ParentFile) ([]byte, error) {
	if prev.BlobSHA == nil {
		return nil, nil
	}
	content, _, err := ix.store.BlobContent(ctx, ix.store.DB(), *prev.BlobSHA)
	if err != nil {
		// Содержимое могли вычистить вместе со старым прогоном: дельта тогда
		// деградирует до «файл изменился целиком», но прогон не валится.
		return nil, nil
	}
	return content, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	return strings.Count(string(content), "\n") + 1
}

// lineDelta считает, сколько строк прибавилось и убыло.
//
// Это разность мультимножеств строк, а не результат LCS: настоящий diff здесь
// нужен только ради двух чисел в ленте, а перестановка строк для детектора
// дрейфа всё равно значит «файл изменился».
func lineDelta(before, after []byte) (additions, deletions int) {
	counts := map[string]int{}
	for _, line := range strings.Split(string(before), "\n") {
		counts[line]++
	}
	for _, line := range strings.Split(string(after), "\n") {
		if counts[line] > 0 {
			counts[line]--
			continue
		}
		additions++
	}
	for _, n := range counts {
		deletions += n
	}
	return additions, deletions
}

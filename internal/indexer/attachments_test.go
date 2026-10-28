package indexer

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/workflow"
)

// Индексация вложений (CLAB-296, AC-14, AC-36): бинарное отсеивается в
// выборке, текст ищется с именем файла, а враждебный текст не держит проход
// и не выбирается вечно.

type artifactSeed struct {
	t     *testing.T
	store *postgres.Store
	pool  *pgxpool.Pool
	task  uuid.UUID
	proj  uuid.UUID
}

func newArtifactSeed(t *testing.T) *artifactSeed {
	store, pool := testStore(t)
	s := &artifactSeed{t: t, store: store, pool: pool}
	if err := pool.QueryRow(context.Background(),
		`SELECT id, project_id FROM tasks WHERE key = 'SDLC-1'`).Scan(&s.task, &s.proj); err != nil {
		t.Fatal(err)
	}
	return s
}

// add кладёт артефакт: attachment — вложение с именем и типом, иначе
// артефакт этапа формата format.
func (s *artifactSeed) add(attachment bool, format, mediaType, name string, content []byte) uuid.UUID {
	s.t.Helper()
	ctx := context.Background()
	sha, err := s.store.PutBlob(ctx, s.store.DB(), content, mediaType)
	if err != nil {
		s.t.Fatal(err)
	}
	var file, mt *string
	kind := "analysis.brief"
	if attachment {
		file, mt, kind = &name, &mediaType, "attachment"
	}
	var id uuid.UUID
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO artifacts (task_id, project_id, kind, title, format, blob_sha, status,
		                       is_attachment, file_name, media_type)
		VALUES ($1, $2, $3, $4, $5, $6, 'submitted', $7, $8, $9) RETURNING id`,
		s.task, s.proj, kind, name, format, sha, attachment, file, mt).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

func (s *artifactSeed) chunks(id uuid.UUID) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM artifact_chunks WHERE artifact_id = $1`, id).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

func TestBinaryAttachmentsDoNotStallIndexing(t *testing.T) {
	ctx := context.Background()
	s := newArtifactSeed(t)
	ix := New(s.store, &fakeSource{}, embed.Hash{}, testdb.Logger(), Options{ChunkChars: 400, EmbedBatch: 8})

	var binaries []uuid.UUID
	for i := range 60 {
		png := append([]byte("\x89PNG\r\n\x1a\n"), []byte(strconv.Itoa(i))...)
		binaries = append(binaries, s.add(true, "binary", "image/png", "shot"+strconv.Itoa(i)+".png", png))
	}
	contract := s.add(false, "binary", "text/markdown", "файл по контракту", []byte("# ротация по контракту"))
	text := s.add(true, "binary", "text/markdown", "runbook.md", []byte("# Ротация ключей\n\nКлючи меняются раз в квартал.\n"))

	// Предел прохода меньше числа картинок: они отсеяны в выборке и очередь
	// не держат.
	if _, _, err := ix.IndexKnowledge(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if s.chunks(text) == 0 {
		t.Fatal("текстовое вложение за 60 картинками не проиндексировано")
	}
	for _, id := range append(binaries, contract) {
		if s.chunks(id) != 0 {
			t.Fatalf("бинарное %s нарезано", id)
		}
	}

	engine := workflow.NewEngine(s.store, testdb.Logger(), workflow.Options{Embedder: embed.Hash{}})
	res, err := engine.SearchContext(ctx, "SDLC", workflow.SearchInput{
		Query: "ротация ключей", Scopes: []string{"artifacts"}, TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range res.Hits {
		if h.ArtifactID != nil && *h.ArtifactID == text {
			found = h.FileName == "runbook.md" && h.Attachment
		}
	}
	if !found {
		t.Fatalf("вложение не найдено с именем и признаком: %+v", res.Hits)
	}
}

func TestHostileTextAttachmentsSkippedOnce(t *testing.T) {
	ctx := context.Background()
	s := newArtifactSeed(t)
	ix := New(s.store, &fakeSource{}, embed.Hash{}, testdb.Logger(),
		Options{ChunkChars: 400, EmbedBatch: 8, MaxFileBytes: 1024})

	var hostile []uuid.UUID
	for i := range 10 {
		n := strconv.Itoa(i)
		hostile = append(hostile,
			s.add(true, "binary", "text/plain", "nul"+n+".txt", []byte("до\x00после "+n)),
			s.add(true, "binary", "text/plain", "bad"+n+".txt", []byte("\xff\xfe битый "+n)),
			s.add(true, "binary", "text/plain", "blank"+n+".txt", bytes.Repeat([]byte(" \n"), 5+i)))
	}
	big := s.add(true, "binary", "text/plain", "big.txt", []byte(strings.Repeat("большой ", 400)))
	normal := s.add(false, "markdown", "text/markdown", "обычный", []byte("# Обычный разбор\n\nКарантин очереди.\n"))

	if _, _, err := ix.IndexKnowledge(ctx, 100); err != nil {
		t.Fatalf("проход встал на враждебном вложении: %v", err)
	}
	if s.chunks(normal) == 0 {
		t.Fatal("обычный артефакт не проиндексирован")
	}
	for _, id := range append(hostile, big) {
		if s.chunks(id) != 0 {
			t.Fatalf("враждебное %s нарезано", id)
		}
	}
	// Следующие проходы их не выбирают: счётчик выбранных не растёт.
	for range 2 {
		pending, err := s.store.PendingArtifacts(ctx, s.store.DB(), 100, 1024)
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) != 0 {
			t.Fatalf("повторно выбрано %d", len(pending))
		}
		if n, _, err := ix.IndexKnowledge(ctx, 100); err != nil || n != 0 {
			t.Fatalf("повторный проход: %d %v", n, err)
		}
	}
	engine := workflow.NewEngine(s.store, testdb.Logger(), workflow.Options{Embedder: embed.Hash{}})
	res, err := engine.SearchContext(ctx, "SDLC", workflow.SearchInput{
		Query: "карантин очереди", Scopes: []string{"artifacts"}, TopK: 5})
	if err != nil || len(res.Hits) == 0 || res.Hits[0].ArtifactID == nil || *res.Hits[0].ArtifactID != normal {
		t.Fatalf("обычный не найден: %+v %v", res.Hits, err)
	}
}

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Миграция 000038_role_version_documents и хранилище документов версий
// ролей (CLAB-343, решение 0068). Номера — критерии приёмки анализа CLAB-343.

const roleDocsMigration = "000038_role_version_documents"

func execMigrationFile(t *testing.T, pool *pgxpool.Pool, suffix string) {
	t.Helper()
	body, err := migrations.Schema.ReadFile(roleDocsMigration + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("%s%s: %v", roleDocsMigration, suffix, err)
	}
}

func roleOf(t *testing.T, pool *pgxpool.Pool, key string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(), `
		SELECT r.id FROM roles r JOIN projects p ON p.id = r.project_id
		WHERE p.key = 'SDLC' AND r.key = $1`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

type versionsDigest struct {
	count int
	md5   string
}

func digestVersions(t *testing.T, pool *pgxpool.Pool) versionsDigest {
	t.Helper()
	var d versionsDigest
	if err := pool.QueryRow(context.Background(), `
		SELECT count(*), coalesce(md5(string_agg(id::text || md5(instructions), ',' ORDER BY id)), '')
		FROM role_versions`).Scan(&d.count, &d.md5); err != nil {
		t.Fatal(err)
	}
	return d
}

// checkMigrated — AC-1 после up: у каждой версии ровно один main.md, блоб
// равен колонке байт в байт, sha совпадает с посчитанным в Go.
func checkMigrated(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `
		SELECT v.id, v.instructions,
		       (SELECT count(*) FROM role_version_documents d WHERE d.role_version_id = v.id),
		       d.name, d.kind, d.position, d.blob_sha, b.content, b.media_type, b.storage, b.size_bytes
		FROM role_versions v
		LEFT JOIN role_version_documents d ON d.role_version_id = v.id
		LEFT JOIN blobs b ON b.sha256 = d.blob_sha`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id uuid.UUID
		var instructions, name, kind, sha, mediaType, storage string
		var docs, position int
		var size int64
		var content []byte
		if err := rows.Scan(&id, &instructions, &docs, &name, &kind, &position, &sha, &content,
			&mediaType, &storage, &size); err != nil {
			t.Fatalf("версия без документа после миграции: %v", err)
		}
		sum := sha256.Sum256([]byte(instructions))
		if docs != 1 || name != "main.md" || kind != "instructions" || position != 0 ||
			string(content) != instructions || sha != hex.EncodeToString(sum[:]) ||
			mediaType != "text/markdown" || storage != "inline" || size != int64(len(instructions)) {
			t.Fatalf("версия %s: документов %d, %s/%s/%d, sha %s, %s %s %d", id, docs, name, kind, position,
				sha, mediaType, storage, size)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("нет ни одной версии роли")
	}
}

// AC-1, AC-3: перенос на данных, в том числе крупная инструкция, многобайтная,
// без перевода строки, с хвостовыми пробелами, две одинаковые и пустая из
// прямой вставки; up → down → up.
func TestRoleDocumentsMigrationWithData(t *testing.T) {
	ctx := context.Background()
	_, pool := testdb.Store(t)

	// База с актуальной схемой: снимаем 000038, кладём данные, как их
	// застанет апгрейд, и катим up заново.
	execMigrationFile(t, pool, ".down.sql")
	dev := roleOf(t, pool, "developer")
	sys := uuid.Nil
	if err := pool.QueryRow(ctx, `SELECT created_by FROM role_versions LIMIT 1`).Scan(&sys); err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("Большая инструкция роли. ", 50_000) // > 1 МБ
	texts := []string{
		big,
		"многобайтный текст: αβγ — ёж 🦔",
		"без перевода строки в конце",
		"хвостовые пробелы   \n\t ",
		"одинаковый текст двух версий",
		"одинаковый текст двух версий",
		"",
	}
	for i, text := range texts {
		if _, err := pool.Exec(ctx, `
			INSERT INTO role_versions (role_id, version, instructions, created_by)
			VALUES ($1, $2, $3, $4)`, dev, 100+i, text, sys); err != nil {
			t.Fatal(err)
		}
	}
	before := digestVersions(t, pool)

	execMigrationFile(t, pool, ".up.sql")
	checkMigrated(t, pool)
	if after := digestVersions(t, pool); after != before {
		t.Fatalf("миграция изменила role_versions: %+v → %+v", before, after)
	}
	var shas int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT d.blob_sha) FROM role_version_documents d
		JOIN role_versions v ON v.id = d.role_version_id
		WHERE v.instructions = 'одинаковый текст двух версий'`).Scan(&shas); err != nil || shas != 1 {
		t.Fatalf("одинаковый текст — %d блобов: %v", shas, err)
	}

	// down снимает только таблицу документов; повторный up даёт то же.
	execMigrationFile(t, pool, ".down.sql")
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.role_version_documents') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("down не снял таблицу: %v %v", exists, err)
	}
	if after := digestVersions(t, pool); after != before {
		t.Fatalf("down тронул role_versions: %+v → %+v", before, after)
	}
	execMigrationFile(t, pool, ".up.sql")
	checkMigrated(t, pool)
}

// AC-2: схема сама отбивает прямую вставку, каскад и индекс.
func TestRoleDocumentsSchemaRejects(t *testing.T) {
	ctx := context.Background()
	_, pool := testdb.Store(t)

	var version uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM role_versions LIMIT 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 64)
	if _, err := pool.Exec(ctx, `
		INSERT INTO blobs (sha256, size_bytes, storage, content) VALUES ($1, 1, 'inline', 'x')`, sha); err != nil {
		t.Fatal(err)
	}
	insert := func(pos int, name, blob string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO role_version_documents (role_version_id, position, name, kind, blob_sha)
			VALUES ($1, $2, $3, 'instructions', $4)`, version, pos, name, blob)
		return err
	}
	if err := insert(0, "main.md", sha); err != nil {
		t.Fatal(err)
	}
	if err := insert(1, strings.Repeat("b", 61)+".md", sha); err != nil {
		t.Fatalf("имя в 64 символа: %v", err)
	}
	for name, try := range map[string]func() error{
		"заглавная":      func() error { return insert(5, "Git.md", sha) },
		"слэш":           func() error { return insert(5, "a/b.md", sha) },
		"ведущая точка":  func() error { return insert(5, ".git.md", sha) },
		"без .md":        func() error { return insert(5, "git.txt", sha) },
		"65 символов":    func() error { return insert(5, strings.Repeat("c", 62)+".md", sha) },
		"дубль имени":    func() error { return insert(5, "main.md", sha) },
		"дубль позиции":  func() error { return insert(0, "other.md", sha) },
		"отрицательная":  func() error { return insert(-1, "neg.md", sha) },
		"блоб вне blobs": func() error { return insert(6, "ghost.md", strings.Repeat("f", 64)) },
	} {
		if err := try(); err == nil {
			t.Errorf("%s: вставка прошла", name)
		}
	}

	var idx int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE tablename = 'role_version_documents' AND indexdef LIKE '%(blob_sha)%'`).Scan(&idx); err != nil || idx != 1 {
		t.Fatalf("индекс по blob_sha: %d %v", idx, err)
	}

	// Каскад: удаление версии уносит её документы. Версия сида без прогонов.
	if _, err := pool.Exec(ctx, `DELETE FROM role_versions WHERE id = $1`, version); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM role_version_documents WHERE role_version_id = $1`, version).Scan(&left); err != nil || left != 0 {
		t.Fatalf("каскад: осталось %d (%v)", left, err)
	}
}

// AC-12: блоб, на который ссылается только документ версии роли, переживает
// проход сборщика; после каскадного удаления версии — уходит.
func TestRoleDocumentBlobSurvivesGC(t *testing.T) {
	ctx := context.Background()
	store, pool := testdb.Store(t)
	dev := roleOf(t, pool, "developer")
	var sys uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT created_by FROM role_versions LIMIT 1`).Scan(&sys); err != nil {
		t.Fatal(err)
	}
	text := "содержимое только у документа роли " + uuid.NewString()
	var rvID uuid.UUID
	if err := store.InTx(ctx, func(tx pgx.Tx) error {
		rv, err := store.CreateRoleVersion(ctx, tx, postgres.CreateRoleVersionInput{
			RoleID: dev, Instructions: text, CreatedBy: sys,
			Documents: []postgres.RoleDocumentInput{{Name: "main.md", Kind: "instructions", Content: []byte(text)}},
		})
		rvID = rv.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(text))
	sha := hex.EncodeToString(sum[:])
	// Блоб «старый»: сборщик с minAge = 0 видит его кандидатом по возрасту.
	if _, err := pool.Exec(ctx, `UPDATE blobs SET created_at = now() - interval '1 hour' WHERE sha256 = $1`, sha); err != nil {
		t.Fatal(err)
	}
	blobExists := func() bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blobs WHERE sha256 = $1)`, sha).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if _, err := store.CollectBlobs(ctx, postgres.BlobGCOptions{Limit: 10_000, MinAge: 0}, nil); err != nil {
		t.Fatal(err)
	}
	if !blobExists() {
		t.Fatal("сборщик унёс содержимое документа роли")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM role_versions WHERE id = $1`, rvID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := store.CollectBlobs(ctx, postgres.BlobGCOptions{Limit: 10_000, MinAge: 0}, nil); err != nil {
		t.Fatal(err)
	}
	if blobExists() {
		t.Fatal("блоб без ссылок пережил сборщик после удаления версии")
	}
}

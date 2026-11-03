package postgres_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/migrations"
)

// Миграция 000040_instruction_library (CLAB-351, решение 0069). Номера —
// критерии приёмки анализа CLAB-351.

const libraryMigration = "000040_instruction_library"

func execLibraryMigration(t *testing.T, pool *pgxpool.Pool, suffix string) {
	t.Helper()
	body, err := migrations.Schema.ReadFile(libraryMigration + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("%s%s: %v", libraryMigration, suffix, err)
	}
}

func regclass(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// AC-1: на базе с данными прежнего релиза миграция только добавляет —
// документы ролей целы и без закрепления; down снимает добавленное;
// повторный up проходит.
func TestInstructionLibraryMigration(t *testing.T) {
	ctx := context.Background()
	_, pool := testdb.Store(t)

	digest := func() string {
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(md5(string_agg(role_version_id::text || position || name || kind || blob_sha, ',' ORDER BY role_version_id, position)), '')
			FROM role_version_documents`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// Откат идёт в обратном порядке: документы предложений версий ролей
	// (000044) ссылаются на версии инструкций.
	execProposalsMigration(t, pool, ".down.sql")
	execLibraryMigration(t, pool, ".down.sql")
	if regclass(t, pool, "public.instructions") || regclass(t, pool, "public.instruction_versions") {
		t.Fatal("down не снял таблицы библиотеки")
	}
	var col bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_name = 'role_version_documents' AND column_name = 'instruction_version_id')`).Scan(&col); err != nil || col {
		t.Fatalf("down не снял колонку закрепления: %v %v", col, err)
	}
	// Документ роли прежнего релиза: сиды кладут версии без строк
	// документов, поэтому строку заводим сами.
	sha := strings.Repeat("c", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO blobs (sha256, size_bytes, storage, content) VALUES ($1, 1, 'inline', 'c')`, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_version_documents (role_version_id, position, name, kind, blob_sha)
		SELECT id, 7, 'legacy.md', 'instructions', $1 FROM role_versions LIMIT 1`, sha); err != nil {
		t.Fatal(err)
	}
	before := digest()
	if before == "" {
		t.Fatal("нет документов ролей для проверки")
	}
	execLibraryMigration(t, pool, ".up.sql")
	execProposalsMigration(t, pool, ".up.sql")
	if after := digest(); after != before {
		t.Fatalf("миграция изменила документы ролей: %s → %s", before, after)
	}
	var pinned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM role_version_documents WHERE instruction_version_id IS NOT NULL`).Scan(&pinned); err != nil || pinned != 0 {
		t.Fatalf("после миграции закреплённых документов %d: %v", pinned, err)
	}
	if !regclass(t, pool, "public.instruction_versions_blob_sha_idx") {
		t.Fatal("нет индекса пятой ссылки на blobs")
	}
}

// AC-1, AC-4: схема держит форму ключа, номер версии и «те же байты» у
// закреплённого документа.
func TestInstructionLibrarySchemaRejects(t *testing.T) {
	ctx := context.Background()
	_, pool := testdb.Store(t)
	var project, version uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM projects WHERE key = 'SDLC'`).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM role_versions LIMIT 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO blobs (sha256, size_bytes, storage, content)
		VALUES ($1, 1, 'inline', 'a'), ($2, 1, 'inline', 'b')`, a, b); err != nil {
		t.Fatal(err)
	}
	var inst, iv uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO instructions (project_id, key, name) VALUES ($1, 'git', 'git') RETURNING id`, project).Scan(&inst); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO instruction_versions (instruction_id, version, kind, blob_sha)
		VALUES ($1, 1, 'instructions', $2) RETURNING id`, inst, a).Scan(&iv); err != nil {
		t.Fatal(err)
	}
	pin := func(pos int, sha string) error {
		_, err := pool.Exec(ctx, `INSERT INTO role_version_documents (role_version_id, position, name, kind, blob_sha, instruction_version_id)
			VALUES ($1, $2, $3, 'instructions', $4, $5)`, version, pos, "p"+strings.Repeat("x", pos)+".md", sha, iv)
		return err
	}
	if err := pin(10, a); err != nil {
		t.Fatalf("закрепление с теми же байтами: %v", err)
	}
	for name, try := range map[string]func() error{
		"другие байты": func() error { return pin(11, b) },
		"ключ с точкой": func() error {
			_, err := pool.Exec(ctx, `INSERT INTO instructions (project_id, key, name) VALUES ($1, 'git.md', 'x')`, project)
			return err
		},
		"заглавная в ключе": func() error {
			_, err := pool.Exec(ctx, `INSERT INTO instructions (project_id, key, name) VALUES ($1, 'Git', 'x')`, project)
			return err
		},
		"дубль ключа": func() error {
			_, err := pool.Exec(ctx, `INSERT INTO instructions (project_id, key, name) VALUES ($1, 'git', 'x')`, project)
			return err
		},
		"дубль номера": func() error {
			_, err := pool.Exec(ctx, `INSERT INTO instruction_versions (instruction_id, version, kind, blob_sha) VALUES ($1, 1, 'x', $2)`, inst, b)
			return err
		},
		"версия 0": func() error {
			_, err := pool.Exec(ctx, `INSERT INTO instruction_versions (instruction_id, version, kind, blob_sha) VALUES ($1, 0, 'x', $2)`, inst, b)
			return err
		},
	} {
		if err := try(); err == nil {
			t.Errorf("%s: схема пропустила", name)
		}
	}
}

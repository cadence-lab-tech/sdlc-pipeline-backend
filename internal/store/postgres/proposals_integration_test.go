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

// Миграция 000044_role_version_proposals (CLAB-381, решение 0072 п. 5).
// Номера — критерии приёмки анализа CLAB-381.

const proposalsMigration = "000044_role_version_proposals"

func execProposalsMigration(t *testing.T, pool *pgxpool.Pool, suffix string) {
	t.Helper()
	body, err := migrations.Schema.ReadFile(proposalsMigration + suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(body)); err != nil {
		t.Fatalf("%s%s: %v", proposalsMigration, suffix, err)
	}
}

// proposalRow заводит предложение роли в обход сервиса: схема проверяется
// сама по себе. vals — SQL значений колонок cols; $1 — роль, $2 — автор.
func proposalRow(t *testing.T, pool *pgxpool.Pool, cols, vals string) error {
	t.Helper()
	ctx := context.Background()
	var role, actor uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT role_id, created_by FROM role_versions WHERE created_by IS NOT NULL LIMIT 1`).Scan(&role, &actor); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO role_version_proposals (role_id, created_by, `+cols+`)
		VALUES ($1, $2, `+vals+`)`, role, actor)
	return err
}

// AC-13: миграция только добавляет — версии и документы ролей целы; down
// снимает добавленное; повторный up проходит; индекс шестой ссылки на
// blobs на месте.
func TestRoleProposalsMigration(t *testing.T) {
	ctx := context.Background()
	_, pool := testdb.Store(t)
	digest := func() string {
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT coalesce(md5(string_agg(v.id::text || v.version || v.instructions, ',' ORDER BY v.id)), '') ||
			       (SELECT coalesce(md5(string_agg(role_version_id::text || position || blob_sha, ',' ORDER BY role_version_id, position)), '')
			        FROM role_version_documents)
			FROM role_versions v`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := digest()
	if err := proposalRow(t, pool, "base_version, changelog", "1, 'правка'"); err != nil {
		t.Fatal(err)
	}
	execProposalsMigration(t, pool, ".down.sql")
	if regclass(t, pool, "public.role_version_proposals") || regclass(t, pool, "public.role_version_proposal_documents") {
		t.Fatal("down не снял таблицы предложений")
	}
	execProposalsMigration(t, pool, ".up.sql")
	if after := digest(); after != before {
		t.Fatalf("миграция изменила версии ролей: %s → %s", before, after)
	}
	if !regclass(t, pool, "public.role_version_proposal_documents_blob_sha_idx") {
		t.Fatal("нет индекса шестой ссылки на blobs")
	}
}

// AC-13: схема держит состояние, обязательный changelog, согласованность
// решения и форму имени документа.
func TestRoleProposalsSchemaRejects(t *testing.T) {
	_, pool := testdb.Store(t)
	const cols = "base_version, changelog, state, decided_by, decided_at, decision_note, result_version, model_level"
	if err := proposalRow(t, pool, cols, "1, 'x', 'rejected', $2, now(), 'причина', NULL, 'high'"); err != nil {
		t.Fatalf("годная строка отклонена: %v", err)
	}
	for name, vals := range map[string]string{
		"неизвестное состояние":    "1, 'x', 'draft', NULL, NULL, NULL, NULL, NULL",
		"пустой changelog":         "1, '  ', 'pending', NULL, NULL, NULL, NULL, NULL",
		"база ноль":                "0, 'x', 'pending', NULL, NULL, NULL, NULL, NULL",
		"уровень вне набора":       "1, 'x', 'pending', NULL, NULL, NULL, NULL, 'max'",
		"pending с решившим":       "1, 'x', 'pending', $2, now(), NULL, NULL, NULL",
		"pending с номером версии": "1, 'x', 'pending', NULL, NULL, NULL, 2, NULL",
		"решено без решившего":     "1, 'x', 'withdrawn', NULL, now(), NULL, NULL, NULL",
		"утверждено без номера":    "1, 'x', 'approved', $2, now(), NULL, NULL, NULL",
		"отклонено с номером":      "1, 'x', 'rejected', $2, now(), 'причина', 2, NULL",
		"отклонено без заметки":    "1, 'x', 'rejected', $2, now(), '  ', NULL, NULL",
	} {
		if err := proposalRow(t, pool, cols, vals); err == nil {
			t.Errorf("%s: схема пропустила строку", name)
		}
	}

	ctx := context.Background()
	var id uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO role_version_proposals (role_id, created_by, base_version, changelog)
		SELECT role_id, created_by, 1, 'x' FROM role_versions WHERE created_by IS NOT NULL LIMIT 1
		RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("d", 64)
	if _, err := pool.Exec(ctx, `INSERT INTO blobs (sha256, size_bytes, storage, content) VALUES ($1, 1, 'inline', 'd')`, sha); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO role_version_proposal_documents (proposal_id, position, name, kind, blob_sha)
		VALUES ($1, 0, 'Main.md', 'instructions', $2)`, id, sha); err == nil {
		t.Error("имя документа не по форме прошло")
	}
	// Закрепление за версией инструкции с другими байтами — составной ключ.
	if _, err := pool.Exec(ctx, `INSERT INTO role_version_proposal_documents (proposal_id, position, name, kind, blob_sha, instruction_version_id)
		VALUES ($1, 0, 'git.md', 'instructions', $2, gen_random_uuid())`, id, sha); err == nil {
		t.Error("закрепление без версии инструкции прошло")
	}
}

// AC-14: сборщик мусора не удаляет байты документа предложения в любом
// состоянии и удаляет их, когда предложения больше нет.
func TestBlobGCKeepsRoleProposalDocuments(t *testing.T) {
	f := newGCFixture(t)
	var proposals []uuid.UUID
	var shas []string
	for i, state := range []string{"pending", "approved", "rejected", "withdrawn"} {
		var id uuid.UUID
		err := f.pool.QueryRow(f.ctx, `
			INSERT INTO role_version_proposals (role_id, created_by, base_version, changelog, state,
			            decided_by, decided_at, decision_note, result_version)
			SELECT role_id, created_by, 1, 'x', $1,
			       CASE WHEN $1 = 'pending' THEN NULL ELSE created_by END,
			       CASE WHEN $1 = 'pending' THEN NULL ELSE now() END,
			       CASE WHEN $1 = 'rejected' THEN 'нет' END,
			       CASE WHEN $1 = 'approved' THEN 2 END
			FROM role_versions WHERE created_by IS NOT NULL LIMIT 1
			RETURNING id`, state).Scan(&id)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		sha := f.put("документ предложения " + state + strings.Repeat("!", i))
		f.exec(`INSERT INTO role_version_proposal_documents (proposal_id, position, name, kind, blob_sha)
			VALUES ($1, 0, 'main.md', 'instructions', $2)`, id, sha)
		proposals = append(proposals, id)
		shas = append(shas, sha)
	}
	orphan := f.put("никому не нужное содержимое предложения")

	stats, _ := f.collect(gcOpts())
	if stats.Deleted != 1 || f.alive(orphan) {
		t.Fatalf("удалено %d, ожидали один сирота: %+v", stats.Deleted, stats)
	}
	for i, sha := range shas {
		if !f.alive(sha) {
			t.Fatalf("документ предложения %d удалён сборщиком", i)
		}
	}
	f.exec(`DELETE FROM role_version_proposals WHERE id = ANY($1)`, proposals)
	stats, _ = f.collect(gcOpts())
	if stats.Deleted != len(shas) {
		t.Fatalf("после удаления предложений удалено %d, ожидали %d", stats.Deleted, len(shas))
	}
}

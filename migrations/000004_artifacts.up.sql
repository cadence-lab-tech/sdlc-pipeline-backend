-- 000004_artifacts: результаты работы агентов по этапам.

-- Артефакт иммутабелен: правка создаёт новую версию через supersedes_id.
-- Содержимое лежит в blobs, поэтому одинаковые вложения не дублируются.
CREATE TABLE artifacts (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    stage_run_id  uuid REFERENCES stage_runs(id) ON DELETE SET NULL,
    -- 'analysis.brief', 'analysis.acceptance_criteria', 'arch.adr',
    -- 'arch.impact_map', 'dev.plan', 'dev.diff', 'qa.report', 'problem.report'
    kind          text NOT NULL,
    title         text NOT NULL,
    format        text NOT NULL DEFAULT 'markdown' CHECK (format IN (
                      'markdown', 'json', 'yaml', 'diff', 'text', 'html', 'binary')),
    blob_sha      char(64) NOT NULL REFERENCES blobs(sha256),
    version       integer NOT NULL DEFAULT 1,
    supersedes_id uuid REFERENCES artifacts(id),
    status        text NOT NULL DEFAULT 'draft' CHECK (status IN (
                      'draft', 'submitted', 'approved', 'rejected', 'superseded')),
    produced_by   uuid REFERENCES principals(id),
    approved_by   uuid REFERENCES principals(id),
    approved_at   timestamptz,
    meta          jsonb NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Связи между артефактами и с кодом: из чего выведен, чему противоречит,
-- какие файлы и символы затрагивает. Это основа обзора "что уже решено по теме".
CREATE TABLE artifact_links (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    artifact_id        uuid NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
    rel                text NOT NULL CHECK (rel IN (
                           'derives_from', 'supersedes', 'contradicts', 'implements',
                           'verifies', 'references_document', 'references_file',
                           'references_symbol', 'references_external')),
    target_artifact_id uuid REFERENCES artifacts(id) ON DELETE CASCADE,
    target_document_id uuid REFERENCES project_documents(id) ON DELETE CASCADE,
    -- для ссылок на код и внешние системы: 'repo://owner/name@sha/path#L10-L42'
    target_uri         text,
    note               text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT artifact_links_target_present CHECK (
        num_nonnulls(target_artifact_id, target_document_id, target_uri) = 1
    )
);

CREATE INDEX ON artifacts (task_id, kind, created_at DESC);
CREATE INDEX ON artifacts (stage_run_id);
CREATE INDEX ON artifacts (project_id, kind);
CREATE INDEX ON artifact_links (artifact_id);
CREATE INDEX ON artifact_links (target_artifact_id);

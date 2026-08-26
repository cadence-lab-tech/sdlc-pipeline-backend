-- Сценарий для проверки детектора дрейфа. Не миграция, запускается после
-- default_workflow.sql. Разворачивает две задачи в полёте, движение main
-- и ожидаемые сигналы.

DO $demo$
DECLARE
    sys uuid; proj uuid; repo uuid; wf uuid; st_ana uuid; st_dev uuid;
    t_be uuid; t_fe uuid; run0 uuid; run1 uuid;
    sha_a char(64); sha_b char(64);
    art_be uuid; art_fe uuid; sr_be uuid; sr_fe uuid;
    r_analyst uuid; rv_analyst uuid;
BEGIN
    SELECT id INTO sys  FROM principals WHERE handle = 'system';
    SELECT id INTO proj FROM projects   WHERE key = 'SDLC';
    SELECT id INTO repo FROM repositories WHERE project_id = proj AND name = 'sdlc-pipeline-backend';
    SELECT id INTO wf   FROM workflows  WHERE project_id = proj AND state = 'active';
    SELECT id INTO st_ana FROM workflow_statuses WHERE workflow_id = wf AND key = 'analysis';
    SELECT id INTO st_dev FROM workflow_statuses WHERE workflow_id = wf AND key = 'development';
    SELECT id INTO t_be FROM tasks WHERE project_id = proj AND key = 'SDLC-1';
    SELECT id INTO t_fe FROM tasks WHERE project_id = proj AND key = 'SDLC-2';
    SELECT id INTO r_analyst FROM roles WHERE project_id = proj AND key = 'analyst';
    SELECT id INTO rv_analyst FROM role_versions WHERE role_id = r_analyst AND version = 1;

    -- SDLC-2 однорепозиторная и живёт во фронтенде; для сценария дрейфа по
    -- бэкенду переводим её в мультирепозиторную и расширяем область.
    UPDATE tasks SET properties = properties || '{"multi_repo": true}' WHERE id = t_fe;
    INSERT INTO task_repositories (project_id, task_id, repository_id, added_by)
    VALUES (proj, t_fe, repo, sys) ON CONFLICT DO NOTHING;

    -- обе задачи в полёте
    UPDATE tasks SET status_id = st_ana WHERE id = t_be;
    UPDATE tasks SET status_id = st_dev WHERE id = t_fe;

    INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, state)
    VALUES (t_be, st_ana, r_analyst, rv_analyst, 'in_progress') RETURNING id INTO sr_be;
    INSERT INTO stage_runs (task_id, status_id, role_id, role_version_id, state)
    VALUES (t_fe, st_dev, r_analyst, rv_analyst, 'in_progress') RETURNING id INTO sr_fe;

    -- индексация main на коммите aaa…, затем на bbb…
    run0 := gen_random_uuid();
    INSERT INTO repo_index_runs (id, repository_id, commit_sha, branch, state, finished_at)
    VALUES (run0, repo, repeat('a', 40), 'main', 'completed', now() - interval '2 days');

    run1 := gen_random_uuid();
    INSERT INTO repo_index_runs (id, repository_id, commit_sha, branch, state, parent_index_run_id, finished_at)
    VALUES (run1, repo, repeat('b', 40), 'main', 'completed', run0, now());

    -- в новом коммите изменились два файла
    INSERT INTO code_file_changes (index_run_id, repository_id, path, change_type, changed_symbols, commit_sha) VALUES
        (run1, repo, 'internal/auth/token.go',  'modified', '{internal/auth.Rotate}', repeat('b', 40)),
        (run1, repo, 'internal/http/router.go', 'modified', '{internal/http.Register}', repeat('b', 40));

    -- обе задачи опираются на старый коммит
    INSERT INTO task_baselines (task_id, repository_id, branch, commit_sha, index_run_id, set_by) VALUES
        (t_be, repo, 'main', repeat('a', 40), run0, sys),
        (t_fe, repo, 'main', repeat('a', 40), run0, sys);

    -- артефакты со ссылками на код
    sha_a := encode(sha256('brief-be'::bytea), 'hex');
    sha_b := encode(sha256('brief-fe'::bytea), 'hex');
    INSERT INTO blobs (sha256, size_bytes, storage, content) VALUES
        (sha_a, 8, 'inline', 'brief-be'::bytea),
        (sha_b, 8, 'inline', 'brief-fe'::bytea);

    INSERT INTO artifacts (task_id, project_id, stage_run_id, kind, title, blob_sha, status, produced_by)
    VALUES (t_be, proj, sr_be, 'analysis.brief', 'Разбор требований', sha_a, 'submitted', sys)
    RETURNING id INTO art_be;
    INSERT INTO artifacts (task_id, project_id, stage_run_id, kind, title, blob_sha, status, produced_by)
    VALUES (t_fe, proj, sr_fe, 'dev.plan', 'План реализации', sha_b, 'submitted', sys)
    RETURNING id INTO art_fe;

    -- SDLC-1 зависит от контракта Rotate — того самого символа, что изменился
    INSERT INTO artifact_code_refs (artifact_id, task_id, repository_id, path, symbol_fqn, ref_kind, commit_sha) VALUES
        (art_be, t_be, repo, 'internal/auth/token.go',  'internal/auth.Rotate', 'depends_on', repeat('a', 40)),
        (art_be, t_be, repo, 'internal/http/router.go', NULL,                    'modifies',   repeat('a', 40));
    -- SDLC-2 собирается менять тот же router.go
    INSERT INTO artifact_code_refs (artifact_id, task_id, repository_id, path, symbol_fqn, ref_kind, commit_sha) VALUES
        (art_fe, t_fe, repo, 'internal/http/router.go', NULL, 'modifies', repeat('a', 40));
END
$demo$;

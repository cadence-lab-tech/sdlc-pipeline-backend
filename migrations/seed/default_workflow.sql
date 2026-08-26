-- Демонстрационный проект с полным SDLC-циклом: показывает, как статусы,
-- роли, переходы и гейты собираются вместе. Не миграция — запускается вручную.

DO $seed$
DECLARE
    sys        uuid;
    proj       uuid;
    wf         uuid;
    st_new     uuid; st_ana uuid; st_arc uuid; st_dev uuid;
    st_rev     uuid; st_qa  uuid; st_done uuid; st_cancel uuid;
    r_analyst  uuid; r_architect uuid; r_developer uuid; r_reviewer uuid; r_qa uuid;
    ep         uuid;
    repo_be    uuid; repo_fe uuid;
    t_be       uuid; t_fe uuid; t_multi uuid;
BEGIN
    SELECT id INTO sys FROM principals WHERE handle = 'system';

    INSERT INTO projects (key, name, description, created_by)
    VALUES ('SDLC', 'SDLC Pipeline', 'Сам себя разрабатывающий пайплайн', sys)
    RETURNING id INTO proj;

    INSERT INTO repositories (project_id, owner, name, clone_url, role_hint) VALUES
        (proj, 'stepaKaziatko', 'sdlc-pipeline-backend',
         'https://github.com/stepaKaziatko/sdlc-pipeline-backend.git', 'backend'),
        (proj, 'stepaKaziatko', 'sdlc-pipeline-frontend',
         'https://github.com/stepaKaziatko/sdlc-pipeline-frontend.git', 'frontend'),
        (proj, 'stepaKaziatko', 'sdlc-pipeline-docs',
         'https://github.com/stepaKaziatko/sdlc-pipeline-docs.git', 'docs');

    SELECT id INTO repo_be FROM repositories WHERE project_id = proj AND name = 'sdlc-pipeline-backend';
    SELECT id INTO repo_fe FROM repositories WHERE project_id = proj AND name = 'sdlc-pipeline-frontend';

    -- Свойства задач. multi_repo системное: его поведение зашито в движок,
    -- проект может поменять только значение по умолчанию.
    INSERT INTO task_property_defs
        (project_id, key, name, description, value_type, default_value, is_system, position)
    VALUES
        (proj, 'multi_repo', 'Мультирепозиторная задача',
         'Разрешает задаче охватывать несколько репозиториев: аналитика и '
         'архитектурное решение делаются сразу по всей области. Выключено — '
         'задача живёт в одном репозитории.',
         'boolean', 'false'::jsonb, true, 1),
        (proj, 'needs_migration', 'Требует миграции БД',
         'Подсказка ролям архитектора и разработчика.',
         'boolean', 'false'::jsonb, false, 2);

    INSERT INTO workflows (project_id, name, version, state, created_by)
    VALUES (proj, 'Default SDLC', 1, 'active', sys)
    RETURNING id INTO wf;

    INSERT INTO workflow_statuses (workflow_id, key, name, category, position, is_initial, is_terminal, description) VALUES
        (wf, 'new',          'New',           'backlog',   1, true,  false, 'Задача заведена, в цикл не запущена'),
        (wf, 'analysis',     'Analysis',      'active',    2, false, false, 'Разбор требований и критериев приёмки'),
        (wf, 'architecture', 'Architecture',  'active',    3, false, false, 'Сопоставление требований с кодовой базой'),
        (wf, 'development',  'Development',   'active',    4, false, false, 'Реализация по утверждённому плану'),
        (wf, 'code_review',  'Code Review',   'active',    5, false, false, 'Перепроверка изменений'),
        (wf, 'qa',           'QA',            'active',    6, false, false, 'Проверка против критериев приёмки'),
        (wf, 'done',         'Done',          'done',      7, false, true,  'Задача завершена'),
        (wf, 'cancelled',    'Cancelled',     'cancelled', 8, false, true,  'Задача снята');

    SELECT id INTO st_new    FROM workflow_statuses WHERE workflow_id = wf AND key = 'new';
    SELECT id INTO st_ana    FROM workflow_statuses WHERE workflow_id = wf AND key = 'analysis';
    SELECT id INTO st_arc    FROM workflow_statuses WHERE workflow_id = wf AND key = 'architecture';
    SELECT id INTO st_dev    FROM workflow_statuses WHERE workflow_id = wf AND key = 'development';
    SELECT id INTO st_rev    FROM workflow_statuses WHERE workflow_id = wf AND key = 'code_review';
    SELECT id INTO st_qa     FROM workflow_statuses WHERE workflow_id = wf AND key = 'qa';
    SELECT id INTO st_done   FROM workflow_statuses WHERE workflow_id = wf AND key = 'done';
    SELECT id INTO st_cancel FROM workflow_statuses WHERE workflow_id = wf AND key = 'cancelled';

    -- Роли
    INSERT INTO roles (project_id, key, name, summary, created_by) VALUES
        (proj, 'analyst',   'Аналитик',   'Превращает запрос в проверяемые требования', sys),
        (proj, 'architect', 'Архитектор', 'Сопоставляет требования с текущей кодовой базой', sys),
        (proj, 'developer', 'Разработчик','Реализует по утверждённому плану', sys),
        (proj, 'reviewer',  'Ревьюер',    'Независимая перепроверка чужого результата', sys),
        (proj, 'qa',        'QA',         'Проверяет результат против критериев приёмки', sys);

    SELECT id INTO r_analyst   FROM roles WHERE project_id = proj AND key = 'analyst';
    SELECT id INTO r_architect FROM roles WHERE project_id = proj AND key = 'architect';
    SELECT id INTO r_developer FROM roles WHERE project_id = proj AND key = 'developer';
    SELECT id INTO r_reviewer  FROM roles WHERE project_id = proj AND key = 'reviewer';
    SELECT id INTO r_qa        FROM roles WHERE project_id = proj AND key = 'qa';

    INSERT INTO role_versions (role_id, version, instructions, input_kinds, output_contract, context_budget, created_by) VALUES
        (r_analyst, 1,
         E'Ты аналитик. Разбери задачу до уровня, на котором её можно проверить.\n\n'
         '- Прочитай описание задачи и связанный внешний тикет, если он есть.\n'
         '- Найди в проектной документации и в коде уже существующее поведение по теме.\n'
         '- Сформулируй требования так, чтобы каждое можно было проверить фактом.\n'
         '- Явно перечисли, что в объём НЕ входит.\n'
         '- Если требование невозможно проверить — не выдумывай, вынеси в открытые вопросы.',
         '{}',
         '[{"kind":"analysis.brief","title":"Разбор требований","format":"markdown","required":true},
           {"kind":"analysis.acceptance_criteria","title":"Критерии приёмки","format":"markdown","required":true},
           {"kind":"analysis.open_questions","title":"Открытые вопросы","format":"markdown","required":false}]',
         '{"rag_top_k":12,"include_project_docs":true}', sys),

        (r_architect, 1,
         E'Ты архитектор. Сопоставь требования с тем, что уже есть в коде.\n\n'
         '- Возьми критерии приёмки аналитика как вход, не переписывай их.\n'
         '- Найди в связанных репозиториях места, которых коснётся изменение.\n'
         '- Опиши решение и зафиксируй, почему выбран именно этот вариант.\n'
         '- Если требование противоречит текущей архитектуре — это повод откатить задачу назад, а не молча переинтерпретировать.',
         '{analysis.brief,analysis.acceptance_criteria}',
         '[{"kind":"arch.adr","title":"Решение и обоснование","format":"markdown","required":true},
           {"kind":"arch.impact_map","title":"Затрагиваемые компоненты","format":"markdown","required":true}]',
         '{"rag_top_k":20,"include_code_graph":true}', sys),

        (r_developer, 1,
         E'Ты разработчик. Реализуй ровно то, что утверждено на предыдущих этапах.\n\n'
         '- План реализации выведи из ADR, не из собственных предположений.\n'
         '- Расхождение между ADR и кодом — повод откатить задачу к архитектору.\n'
         '- Каждый критерий приёмки должен быть покрыт проверкой.\n'
         '- Сам код живёт в PR: привяжи PR к задаче и сдай разбор изменений, а не патч.',
         '{analysis.acceptance_criteria,arch.adr,arch.impact_map}',
         '[{"kind":"dev.plan","title":"План реализации","format":"markdown","required":true},
           {"kind":"dev.change_summary","title":"Разбор изменений","format":"markdown","required":true},
           {"kind":"dev.test_report","title":"Результаты прогона тестов","format":"markdown","required":true}]',
         '{"rag_top_k":20,"include_code_graph":true}', sys),

        (r_reviewer, 1,
         E'Ты ревьюер. Ты НЕ исполнитель этапа — ты проверяешь чужой результат.\n\n'
         '- Проверь результат против входных артефактов, а не против своего вкуса.\n'
         '- Каждое замечание подкрепи ссылкой на файл, строку или пункт критериев.\n'
         '- Вердикт — pass или fail, без промежуточных формулировок. При fail объясни, что именно вернуть на доработку.',
         '{analysis.acceptance_criteria,arch.adr,dev.change_summary}',
         '[{"kind":"review.verdict","title":"Вердикт ревью","format":"json","required":true,
            "schema":{"type":"object","required":["verdict","findings"],
                      "properties":{"verdict":{"enum":["pass","fail"]},
                                    "findings":{"type":"array"}}}}]',
         '{"rag_top_k":15}', sys),

        (r_qa, 1,
         E'Ты QA. Проверь результат против критериев приёмки аналитика.\n\n'
         '- Каждый критерий отметь как выполненный или нет, с доказательством.\n'
         '- Не проверяй то, чего нет в критериях; недостающее — открытый вопрос.',
         '{analysis.acceptance_criteria,dev.change_summary,dev.test_report}',
         '[{"kind":"qa.report","title":"Отчёт QA","format":"markdown","required":true}]',
         '{"rag_top_k":10}', sys);

    -- Привязка ролей к статусам
    INSERT INTO status_roles (status_id, role_id, is_primary) VALUES
        (st_ana, r_analyst,   true),
        (st_arc, r_architect, true),
        (st_dev, r_developer, true),
        (st_rev, r_reviewer,  true),
        (st_qa,  r_qa,        true);

    -- Движение вперёд
    INSERT INTO workflow_transitions (workflow_id, key, name, from_status_id, to_status_id, kind, position) VALUES
        (wf, 'start',             'Запустить в цикл',   st_new, st_ana,  'advance', 1),
        (wf, 'analysis_done',     'Аналитика готова',   st_ana, st_arc,  'advance', 2),
        (wf, 'architecture_done', 'Архитектура готова', st_arc, st_dev,  'advance', 3),
        (wf, 'dev_done',          'Реализация готова',  st_dev, st_rev,  'advance', 4),
        (wf, 'review_passed',     'Ревью пройдено',     st_rev, st_qa,   'advance', 5),
        (wf, 'qa_passed',         'QA пройдено',        st_qa,  st_done, 'advance', 6);

    -- Откаты: при найденной проблеме задача уходит назад вместе с её описанием
    INSERT INTO workflow_transitions (workflow_id, key, name, from_status_id, to_status_id, kind, position) VALUES
        (wf, 'reject_to_analysis',     'Вернуть аналитику',    st_arc, st_ana, 'reject', 10),
        (wf, 'reject_to_architecture', 'Вернуть архитектору',  st_dev, st_arc, 'reject', 11),
        (wf, 'reject_to_development',  'Вернуть в разработку', st_rev, st_dev, 'reject', 12),
        (wf, 'qa_failed',              'QA не пройдено',       st_qa,  st_dev, 'reject', 13);

    -- Отмена доступна из любого статуса
    INSERT INTO workflow_transitions (workflow_id, key, name, from_status_id, to_status_id, kind, position)
    VALUES (wf, 'cancel', 'Снять задачу', NULL, st_cancel, 'cancel', 99);

    -- Гейты на движении вперёд
    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'artifacts', 'Артефакты аналитики сданы', 'artifacts_required',
           '{"kinds":["analysis.brief","analysis.acceptance_criteria"]}', true, 1
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'analysis_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'peer_review', 'Перепроверка ревьюером', 'agent_review',
           '{"reviewer_role_key":"reviewer","artifact_kind":"review.verdict","pass_on":"pass"}', true, 2
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'analysis_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'human_approval', 'Апрув человеком', 'human_approval',
           '{"min_approvals":1}', true, 3
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'analysis_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'artifacts', 'Артефакты архитектуры сданы', 'artifacts_required',
           '{"kinds":["arch.adr","arch.impact_map"]}', true, 1
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'architecture_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'traceability', 'Каждый критерий приёмки покрыт решением', 'checklist',
           '{"items":[{"id":"ac_covered","text":"Каждый критерий приёмки отражён в ADR","required":true},
                      {"id":"impact_complete","text":"Перечислены все затрагиваемые репозитории","required":true}]}',
           true, 2
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'architecture_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'human_approval', 'Апрув человеком', 'human_approval',
           '{"min_approvals":1}', true, 3
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'architecture_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'artifacts', 'Разбор изменений и тесты сданы', 'artifacts_required',
           '{"kinds":["dev.plan","dev.change_summary","dev.test_report"]}', true, 1
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'dev_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'pull_request', 'PR привязан к задаче', 'repo_ref_required',
           '{"ref_type":"pull_request","min_count":1,"require_open":true,"per_repository":true}', true, 2
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'dev_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'ci', 'CI зелёный', 'external_check',
           '{"provider":"github_checks","required_conclusion":"success"}', true, 3
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'dev_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'verdict', 'Вердикт ревью = pass', 'schema_validation',
           '{"artifact_kind":"review.verdict","expect":{"verdict":"pass"}}', true, 1
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'review_passed';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'artifacts', 'Отчёт QA сдан', 'artifacts_required',
           '{"kinds":["qa.report"]}', true, 1
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'qa_passed';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'human_approval', 'Финальный апрув', 'human_approval',
           '{"min_approvals":1}', true, 2
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'qa_passed';


    -- Область задачи: артефакты этапа не вышли за объявленные репозитории.
    -- Для однорепозиторной задачи это блокирует расползание, для
    -- мультирепозиторной — расширение области без явного решения.
    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'repo_scope', 'Артефакты не вышли за область задачи', 'repo_scope',
           '{"allow_expansion_when_multi_repo":true}', true, 5
    FROM workflow_transitions
    WHERE workflow_id = wf AND key IN ('analysis_done', 'architecture_done', 'dev_done');

    -- Дрейф базовой линии. Порог тем строже, чем ближе этап к коду:
    -- аналитику двигают только серьёзные расхождения, разработку — любые.
    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'drift', 'Кодовая база под задачей не уехала', 'drift_clear',
           '{"min_severity":"high"}', true, 4
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'analysis_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'drift', 'Кодовая база под задачей не уехала', 'drift_clear',
           '{"min_severity":"medium"}', true, 4
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'architecture_done';

    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'drift', 'Нет открытых расхождений и пересечений с другими задачами', 'drift_clear',
           '{"min_severity":"low","include_kinds":["baseline_drift","task_overlap"]}', true, 4
    FROM workflow_transitions WHERE workflow_id = wf AND key = 'dev_done';

    -- Гейт на откатах: назад задача уходит только вместе с описанием проблемы
    INSERT INTO gates (transition_id, key, name, type, config, blocking, position)
    SELECT id, 'problem_report', 'Описание проблемы приложено', 'artifacts_required',
           '{"kinds":["problem.report"]}', true, 1
    FROM workflow_transitions
    WHERE workflow_id = wf AND kind = 'reject';

    -- Демонстрационные задачи. Первые две однорепозиторные, третья охватывает
    -- бэкенд и фронтенд сразу — ровно тот случай, ради которого заведён multi_repo.
    INSERT INTO epics (project_id, key, title, description, created_by)
    VALUES (proj, 'SDLC-E1', 'Скелет системы', 'Первый сквозной проход цикла', sys)
    RETURNING id INTO ep;

    UPDATE projects SET task_counter = task_counter + 3 WHERE id = proj;

    INSERT INTO tasks (project_id, epic_id, key, seq, title, description, workflow_id, status_id, properties, created_by)
    VALUES (proj, ep, 'SDLC-1', 1, 'REST API проектов и задач',
            'Поднять Gin, миграции и CRUD.', wf, st_new, '{"multi_repo": false}', sys)
    RETURNING id INTO t_be;

    INSERT INTO tasks (project_id, epic_id, key, seq, title, description, workflow_id, status_id, properties, created_by)
    VALUES (proj, ep, 'SDLC-2', 2, 'Доска задач и карточка',
            'Vite + React 19 + Astryx поверх REST.', wf, st_new, '{"multi_repo": false}', sys)
    RETURNING id INTO t_fe;

    INSERT INTO tasks (project_id, epic_id, key, seq, title, description, workflow_id, status_id, properties, created_by)
    VALUES (proj, ep, 'SDLC-3', 3, 'Признак мультирепозиторности в карточке задачи',
            'Добавить поле в ручку задачи на бэкенде и отобразить его в карточке на фронте.',
            wf, st_new, '{"multi_repo": true, "needs_migration": true}', sys)
    RETURNING id INTO t_multi;

    INSERT INTO task_repositories (project_id, task_id, repository_id, is_primary, added_by) VALUES
        (proj, t_be,    repo_be, true,  sys),
        (proj, t_fe,    repo_fe, true,  sys),
        (proj, t_multi, repo_be, true,  sys),
        (proj, t_multi, repo_fe, false, sys);

    INSERT INTO task_links (from_task_id, to_task_id, rel, note, created_by)
    VALUES (t_be, t_fe, 'blocks', 'Фронтенд ждёт стабильного контракта API', sys);

    -- Попытка прохождения статуса заводится сразу вместе с задачей: движок
    -- исходит из того, что у задачи на текущем статусе всегда есть активный
    -- stage_run — к нему привязываются артефакты и результаты гейтов.
    -- У статуса 'new' исполнителя нет, поэтому роль пустая.
    INSERT INTO stage_runs (task_id, status_id, attempt, state)
    VALUES (t_be, st_new, 1, 'pending'),
           (t_fe, st_new, 1, 'pending'),
           (t_multi, st_new, 1, 'pending');

    INSERT INTO task_events (task_id, stage_run_id, type, actor_id, payload)
    SELECT sr.task_id, sr.id, 'task.created', sys, '{"source":"seed"}'
    FROM stage_runs sr WHERE sr.task_id IN (t_be, t_fe, t_multi);

END
$seed$;

-- 000020_invites: приглашение в команду или проект ссылкой (CLAB-2, решение 0035).
--
-- Человека, который ещё не входил, нельзя добавить по хэндлу (решение 0033):
-- на открытом инстансе это способ занять чужой логин. Приглашение — отложенное
-- добавление, где участник определяется тем, кто вошёл и предъявил ссылку.

CREATE TABLE invites (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Цель ровно одна. Удалённая цель уносит приглашения с собой: принять
    -- приглашение в то, чего нет, всё равно нельзя, а историю выдачи хранит
    -- аудит цели, а не висящая строка.
    team_id     uuid REFERENCES teams(id) ON DELETE CASCADE,
    project_id  uuid REFERENCES projects(id) ON DELETE CASCADE,
    -- У команды уровней доступа нет, они живут на project_teams: командное
    -- приглашение несёт роль в команде, проектное — уровень и роли специалистов.
    team_role   text CHECK (team_role IN ('member', 'lead')),
    level       text CHECK (level IN ('viewer', 'agent', 'maintainer', 'admin')),
    -- NULL — любые роли специалистов, как у project_members (решение 0030).
    role_keys   text[],
    -- Хеш, а не токен: ссылка — пропуск на предъявителя, и её утечка из базы
    -- была бы утечкой участия.
    token_hash  text NOT NULL UNIQUE,
    created_by  uuid NOT NULL REFERENCES principals(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL,
    accepted_at timestamptz,
    accepted_by uuid REFERENCES principals(id),
    revoked_at  timestamptz,
    revoked_by  uuid REFERENCES principals(id),

    -- Статуса колонкой нет: «протух» наступает со временем и разошёлся бы с
    -- колонкой в первую же минуту после срока. Статус вычисляется из времён.
    CONSTRAINT invites_one_target CHECK (num_nonnulls(team_id, project_id) = 1),
    CONSTRAINT invites_target_fields CHECK (
        (team_id IS NOT NULL AND team_role IS NOT NULL AND level IS NULL AND role_keys IS NULL)
        OR (project_id IS NOT NULL AND level IS NOT NULL AND team_role IS NULL)
    ),
    CONSTRAINT invites_accepted_pair CHECK ((accepted_at IS NULL) = (accepted_by IS NULL)),
    CONSTRAINT invites_revoked_pair CHECK ((revoked_at IS NULL) = (revoked_by IS NULL)),
    -- Конечное состояние одно: принятое не отзывается, отозванное не принимается.
    CONSTRAINT invites_closed_once CHECK (accepted_at IS NULL OR revoked_at IS NULL),
    CONSTRAINT invites_expiry CHECK (expires_at > created_at)
);

-- Списки приглашений цели — единственное чтение, кроме поиска по хешу.
CREATE INDEX invites_team ON invites (team_id, created_at DESC) WHERE team_id IS NOT NULL;
CREATE INDEX invites_project ON invites (project_id, created_at DESC) WHERE project_id IS NOT NULL;

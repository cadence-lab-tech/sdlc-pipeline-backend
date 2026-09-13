-- 000021_team_events: аудит уровня арендатора (CLAB-10).
--
-- Аудит до сих пор жил на задаче (task_events) и уходил каскадом вместе с ней.
-- Событие об удалении проекта обязано пережить сам проект, иначе следа не
-- остаётся вовсе: писать его некуда, кроме команды-владельца. Та же таблица
-- нужна удалению арендатора (CLAB-13).

CREATE TABLE team_events (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Уходит вместе с командой: аудит арендатора переживает его проекты, но не
    -- его самого — хранить события удалённой команды негде и незачем.
    team_id    uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    -- 'project.deleted', дальше — 'team.deleted' и прочее по мере надобности.
    type       text NOT NULL,
    actor_id   uuid REFERENCES principals(id),
    payload    jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON team_events (team_id, created_at DESC);

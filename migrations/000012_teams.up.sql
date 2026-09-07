-- 000012_teams: команды как группы принципалов, участие команды в проекте,
-- владение проектом командой (решение 0027).
--
-- Модель доступа фазы 4 поимённая: команда из десяти человек на три проекта —
-- тридцать строк в project_members, а уход человека означает обход всех
-- проектов. Команда — та же связка, но выданная группе целиком; поимённое
-- участие остаётся для исключений (внешний ревьюер, агент другой команды).

-- Команда — сущность инстанса, а не проекта: одна и та же команда участвует в
-- нескольких проектах, и её состав от проекта не зависит.
CREATE TABLE teams (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key         text NOT NULL UNIQUE,
    name        text NOT NULL,
    description text,
    -- Право заводить проекты выдаётся команде, а не человеку: лид уходит,
    -- команда остаётся. Глобальный администратор нужен только чтобы выдать
    -- это право.
    can_create_projects boolean NOT NULL DEFAULT false,
    created_by  uuid REFERENCES principals(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    archived_at timestamptz
);

-- Состав команды. `lead` управляет составом и заводит проекты команды;
-- уровень прав в проекте отсюда не выводится — его даёт project_teams.
CREATE TABLE team_members (
    team_id      uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    principal_id uuid NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    role         text NOT NULL CHECK (role IN ('member', 'lead')),
    added_by     uuid REFERENCES principals(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, principal_id)
);

CREATE INDEX team_members_principal ON team_members (principal_id);

-- Участие команды в проекте теми же уровнями, что у project_members:
-- действующий уровень принципала — максимум из прямого участия и участия
-- через команды. Оба пути складываются, а не подменяют друг друга: агент,
-- добавленный поимённо, не теряет прав от того, что его команда — viewer.
CREATE TABLE project_teams (
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    team_id    uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    level      text NOT NULL CHECK (level IN ('viewer', 'agent', 'maintainer', 'admin')),
    added_by   uuid REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (project_id, team_id)
);

CREATE INDEX project_teams_team ON project_teams (team_id);

-- Владелец проекта. Без него непонятно, кто отвечает за проект, когда его
-- создатель ушёл из компании. ON DELETE SET NULL: команду сносят, проект
-- остаётся и достаётся глобальному администратору.
ALTER TABLE projects ADD COLUMN owner_team_id uuid REFERENCES teams(id) ON DELETE SET NULL;

CREATE INDEX projects_owner_team ON projects (owner_team_id);

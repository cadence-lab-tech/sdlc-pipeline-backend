-- 000018_tenants: арендатор — команда (решение 0033).
--
-- Общий инстанс: коллеги приходят сами, входят через GitHub и заводят
-- проекты. Проект без команды-владельца (решение 0030) оказался единственным
-- путём мимо бюджета и будущих квот, а команду мог завести только
-- администратор инстанса. Здесь у команды появляется вид, у каждого
-- вошедшего — личная команда, у проекта — обязательный владелец, а у
-- арендатора — тариф с пределами.

-- Вид команды: личная (один человек, участников не принимает) или
-- организация. Личная команда помнит, чья она: один принципал — одна живая
-- личная команда. Превращённая в организацию личная команда теряет
-- `personal_of`, и следующий вход заведёт человеку новую личную.
ALTER TABLE teams ADD COLUMN kind text NOT NULL DEFAULT 'organization'
    CHECK (kind IN ('personal', 'organization'));
ALTER TABLE teams ADD COLUMN personal_of uuid REFERENCES principals(id);
ALTER TABLE teams ADD CONSTRAINT teams_personal_of_kind
    CHECK ((kind = 'personal') = (personal_of IS NOT NULL));
CREATE UNIQUE INDEX teams_personal_of ON teams (personal_of)
    WHERE kind = 'personal' AND archived_at IS NULL;

-- Тариф — набор пределов, не более того. NULL — без предела. Другие пределы
-- из SAAS.md добавятся своими миграциями вместе с проверками: столбец без
-- проверки обещал бы то, чего система не делает.
CREATE TABLE plans (
    code        text PRIMARY KEY,
    name        text NOT NULL,
    members_max integer CHECK (members_max IS NULL OR members_max > 0),
    created_at  timestamptz NOT NULL DEFAULT now()
);

INSERT INTO plans (code, name, members_max) VALUES
    ('free', 'Бесплатный', 3),
    ('business', 'Бизнес', NULL);

-- Подписка команды на тариф. Команда без активной подписки живёт на free.
-- external_ref — задел под платёжного провайдера, пустой, пока его нет.
CREATE TABLE subscriptions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    team_id      uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    plan_code    text NOT NULL REFERENCES plans(code),
    status       text NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active', 'past_due', 'canceled')),
    period_start timestamptz,
    period_end   timestamptz,
    external_ref text,
    created_by   uuid REFERENCES principals(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX subscriptions_active_team ON subscriptions (team_id)
    WHERE status = 'active';

-- Владелец существующим проектам. Личная команда самого раннего
-- администратора-человека проекта (или его создателя-человека); если
-- человека нет — организация с ключом проекта, лидами которой становятся
-- администраторы проекта. Так ни один проект не остаётся без владельца и
-- ни один человек не теряет управления.
DO $tenants$
DECLARE
    pr     RECORD;
    person uuid;
    team   uuid;
    k      text;
BEGIN
    FOR pr IN SELECT id, key, name, created_by FROM projects WHERE owner_team_id IS NULL LOOP
        person := NULL;
        team := NULL;

        SELECT m.principal_id INTO person
        FROM project_members m
        JOIN principals p ON p.id = m.principal_id
        WHERE m.project_id = pr.id AND m.role = 'admin' AND p.kind = 'user'
        ORDER BY m.created_at
        LIMIT 1;

        IF person IS NULL THEN
            SELECT p.id INTO person FROM principals p
            WHERE p.id = pr.created_by AND p.kind = 'user';
        END IF;

        IF person IS NOT NULL THEN
            SELECT t.id INTO team FROM teams t
            WHERE t.kind = 'personal' AND t.personal_of = person AND t.archived_at IS NULL;

            IF team IS NULL THEN
                SELECT handle INTO k FROM principals WHERE id = person;
                IF EXISTS (SELECT 1 FROM teams WHERE key = k) THEN
                    k := k || '-' || left(replace(person::text, '-', ''), 6);
                END IF;
                INSERT INTO teams (key, name, kind, personal_of, can_create_projects, created_by)
                SELECT k, display_name, 'personal', person, true, person
                FROM principals WHERE id = person
                RETURNING id INTO team;
                INSERT INTO team_members (team_id, principal_id, role, added_by)
                VALUES (team, person, 'lead', person);
            END IF;
        ELSE
            k := lower(pr.key);
            IF EXISTS (SELECT 1 FROM teams WHERE key = k) THEN
                k := k || '-' || left(replace(pr.id::text, '-', ''), 6);
            END IF;
            INSERT INTO teams (key, name, kind, can_create_projects, created_by)
            VALUES (k, pr.name, 'organization', true, pr.created_by)
            RETURNING id INTO team;
            INSERT INTO team_members (team_id, principal_id, role, added_by)
            SELECT team, m.principal_id, 'lead', pr.created_by
            FROM project_members m WHERE m.project_id = pr.id AND m.role = 'admin';
        END IF;

        UPDATE projects SET owner_team_id = team WHERE id = pr.id;
    END LOOP;
END
$tenants$;

-- Владелец обязателен. Команды не удаляются, а архивируются (ArchiveTeam),
-- поэтому ON DELETE SET NULL, несовместимый с NOT NULL, меняется на RESTRICT.
ALTER TABLE projects DROP CONSTRAINT projects_owner_team_id_fkey;
ALTER TABLE projects ADD CONSTRAINT projects_owner_team_id_fkey
    FOREIGN KEY (owner_team_id) REFERENCES teams(id) ON DELETE RESTRICT;
ALTER TABLE projects ALTER COLUMN owner_team_id SET NOT NULL;

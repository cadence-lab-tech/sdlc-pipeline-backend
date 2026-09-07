-- 000014_github_app: доступ к GitHub через установку приложения на проект
-- (решение 0028).
--
-- Один токен на инстанс работает, пока проект один. Репозитории разных команд
-- лежат в разных организациях, и общий токен либо не достаёт до них, либо
-- достаёт до всего сразу — тогда одна команда индексирует репозитории другой.
-- Хранить токен на проект нельзя: секретов в базе нет.

-- Идентификатор установки приложения в организации. Это ссылка, а не секрет:
-- её можно показывать в настройках проекта и переносить связкой. Токен по ней
-- выписывается на час и живёт в памяти сервиса.
ALTER TABLE projects ADD COLUMN github_installation_id bigint;

-- Запасной путь для GitHub Enterprise без приложения: имя секрета, по
-- которому бэкенд берёт токен из окружения (SDLC_GITHUB_TOKEN_<NAME>). В базе
-- имя, не значение — тот же приём, что у repositories.credential_ref.
ALTER TABLE projects ADD COLUMN github_credential_ref text;

CREATE INDEX projects_github_installation ON projects (github_installation_id)
    WHERE github_installation_id IS NOT NULL;

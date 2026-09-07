-- 000015_team_budgets: квоты по стоимости на команду (решение 0027).
--
-- Стоимость прогонов система считает с фазы 5 (решение 0026), но считать
-- мало: без предела одна команда способна сжечь бюджет инстанса за ночь
-- непрерывной кодогенерации. Предел ставится команде, а не проекту: платит
-- команда, и проекты у неё меняются чаще, чем бюджет.

CREATE TABLE team_budgets (
    team_id  uuid NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    -- Окно, за которое считается расход. Скользящего окна здесь нет
    -- сознательно: «сколько потрачено в этом месяце» человек проверяет по
    -- календарю, а не по последним тридцати дням.
    period   text NOT NULL CHECK (period IN ('day', 'week', 'month')),
    -- Предел в долларах. numeric, а не float: деньги.
    cost_usd_limit numeric(12, 2) NOT NULL CHECK (cost_usd_limit > 0),
    created_by uuid REFERENCES principals(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Один предел на окно: дневной и месячный можно держать вместе, два
    -- месячных — нет.
    PRIMARY KEY (team_id, period)
);

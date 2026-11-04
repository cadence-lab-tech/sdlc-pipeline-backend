-- Откат 000046: исхода moved больше нет. Сужающий CHECK не проходит
-- валидацию, пока в базе лежат прогоны с этим исходом, поэтому сначала
-- исход снимается (прогон остаётся закрытым: state и finished_at на месте).
UPDATE stage_runs SET outcome = NULL WHERE outcome = 'moved';
ALTER TABLE stage_runs DROP CONSTRAINT stage_runs_outcome_check;
ALTER TABLE stage_runs ADD CONSTRAINT stage_runs_outcome_check
    CHECK (outcome IN ('advanced', 'rejected', 'cancelled'));

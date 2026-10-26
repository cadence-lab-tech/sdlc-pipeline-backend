package main

import (
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// codeUsageReportRequired — отказ захвата по политике учёта проекта
// (CLAB-219): у принципала раннера есть закрытые прогоны без отчёта о
// стоимости.
const codeUsageReportRequired = "usage_report_required"

// debtRepeat — через сколько тот же долг пишется в журнал снова.
const debtRepeat = 10 * time.Minute

// usageDebt — долг из details отказа.
type usageDebt struct {
	Task       string    `json:"task"`
	StageRunID string    `json:"stage_run_id"`
	FinishedAt time.Time `json:"finished_at"`
}

type usageDebtDetails struct {
	Debts        []usageDebt `json:"debts"`
	Total        int         `json:"total"`
	GraceMinutes int         `json:"grace_minutes"`
}

// debtLog держит последнюю запись о долге: раннер пробует захват на каждом
// опросе, и без подавления журнал забился бы одним и тем же. Повтор — если
// изменился список долгов или прошло debtRepeat.
type debtLog struct {
	mu     sync.Mutex
	key    string
	logged time.Time
}

func (d *debtLog) note(log *slog.Logger, apiErr *apiError, now time.Time) {
	var det usageDebtDetails
	if len(apiErr.Details) > 0 {
		_ = json.Unmarshal(apiErr.Details, &det)
	}
	ids := make([]string, 0, len(det.Debts))
	tasks := make([]string, 0, len(det.Debts))
	for _, debt := range det.Debts {
		ids = append(ids, debt.StageRunID)
		tasks = append(tasks, debt.Task+" "+debt.StageRunID)
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	key := strings.Join(sorted, ",")
	if det.Total > len(det.Debts) || key == "" {
		key += "|" + apiErr.Message
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if key == d.key && now.Sub(d.logged) < debtRepeat {
		return
	}
	d.key, d.logged = key, now
	log.Error("захват отбит политикой учёта проекта: у принципала раннера прогоны без отчёта о стоимости; "+
		"новых задач раннер не берёт, пока долг не погашен — отчитайтесь токеном раннера: "+
		"clab usage <задача> --run <прогон> --model <модель> [--cost <доллары>], или администратор проекта выключит политику",
		"debts", tasks, "total", det.Total, "grace_minutes", det.GraceMinutes, "message", apiErr.Message)
}

// clear — захват прошёл, долга нет: следующий отказ пишется сразу.
func (d *debtLog) clear() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.key, d.logged = "", time.Time{}
}

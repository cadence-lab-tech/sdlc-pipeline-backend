package main

import "github.com/cadence-lab-tech/sdlc-pipeline-backend/agentkit/launch"

// Разбор вывода агента и решение «отчитается ли запускающий» — общие с
// clab exec (agentkit/launch, CLAB-470). Раннер называет себя в
// details.reported_by, как до выноса.
const reportedByRunner = "clab-runner"

// runnerReports — отчитается ли о стоимости раннер (launch.LauncherReports).
func runnerReports(out, model string) bool { return launch.LauncherReports(out, model) }

// parseOutput — отчёт о стоимости по выводу агента в формате привязки;
// nil — «стоимость неизвестна», а не ноль (решение 0026).
func parseOutput(out string, raw []byte, model string) map[string]any {
	return launch.ParseOutput(out, raw, model, reportedByRunner)
}

package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// CLAB-221: разбор длительности предела этапа — один на валидатор,
// вычислитель, захват и воркер.
func TestMaxDuration(t *testing.T) {
	for config, want := range map[string]time.Duration{
		`{"max_duration": "4h"}`:               4 * time.Hour,
		`{"max_duration": "90m"}`:              90 * time.Minute,
		`{"max_duration": "24h"}`:              24 * time.Hour,
		`{"max_duration": "36h", "x": true}`:   36 * time.Hour,
		`{"max_duration": "2s"}`:               2 * time.Second,
		`{"max_duration": " 1h30m "}`:          90 * time.Minute,
		`{"max_duration": "8784h"}`:            maxStageDuration,
		`{"max_duration": "1h", "other": [1]}`: time.Hour,
	} {
		d, text, err := maxDuration(json.RawMessage(config))
		if err != nil || d != want || text == "" {
			t.Errorf("%s: %v %q %v", config, d, text, err)
		}
	}
	for config, hint := range map[string]string{
		`{"max_duration": "0s"}`:    "больше нуля",
		`{"max_duration": "-1h"}`:   "больше нуля",
		`{"max_duration": "abc"}`:   "не длительность",
		`{"max_duration": "1d"}`:    "24h",
		`{"max_duration": 4}`:       "строка",
		`{"max_duration": null}`:    "строка",
		`{"max_duration": ""}`:      "пустая",
		`{"max_duration": "8785h"}`: "больше года",
		`[]`:                        "не объект",
	} {
		_, _, err := maxDuration(json.RawMessage(config))
		if err == nil || !strings.Contains(err.Error(), hint) {
			t.Errorf("%s должен отбиваться с подсказкой %q: %v", config, hint, err)
		}
	}
	for _, config := range []string{``, `{}`} {
		if _, _, err := maxDuration(json.RawMessage(config)); err != errNoMaxDuration {
			t.Errorf("%q: ждали errNoMaxDuration, а не %v", config, err)
		}
	}
}

// Правило 5: без числа или с негодным числом гейт не держит переход и в базу
// не ходит.
func TestStageTimeLimitWithoutLimitPasses(t *testing.T) {
	e := &Engine{}
	for _, config := range []string{`{}`, ``, `{"max_duration": "0s"}`, `{"max_duration": 4}`} {
		out, err := e.evalStageTimeLimit(context.Background(), nil, EvalInput{
			Gate: gate(domain.GateStageTimeLimit, config)})
		if err != nil || out.Status != domain.GateResultPassed || !strings.Contains(out.Reason, "предел этапа") {
			t.Fatalf("config %q: %+v %v", config, out, err)
		}
	}
}

func TestStageClock(t *testing.T) {
	entered := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	deadline, overdue := stageClock(entered, 4*time.Hour, entered.Add(4*time.Hour-time.Second))
	if overdue || !deadline.Equal(entered.Add(4*time.Hour)) {
		t.Fatalf("до срока: %v %v", deadline, overdue)
	}
	if _, overdue := stageClock(entered, 4*time.Hour, entered.Add(4*time.Hour)); !overdue {
		t.Fatal("в момент срока этап уже просрочен")
	}
	if (*StageLimit)(nil).Holds() || (&StageLimit{Overdue: true, Waived: true}).Holds() ||
		(&StageLimit{}).Holds() || !(&StageLimit{Overdue: true}).Holds() {
		t.Fatal("Holds: держит только просроченный и не снятый")
	}
}

// Критерии 1–3: предел — положительная длительность, переход ведёт не в
// терминальный статус, на переходе он один. Негодный предел — проблема, а не
// предупреждение.
func TestValidateStageTimeLimit(t *testing.T) {
	dev := status("development", true, false)
	qa := status("qa", false, false)
	done := status("done", false, true)

	timed := func(key, config string) domain.Gate {
		return domain.Gate{Key: key, Type: domain.GateStageTimeLimit,
			Blocking: true, Config: json.RawMessage(config)}
	}
	cycle := func(config string, onTerminal, twice bool) domain.Workflow {
		back := transition("qa_failed", &qa, dev, timed("time", config))
		back.Kind = "reject"
		if twice {
			back.Gates = append(back.Gates, timed("time2", `{"max_duration": "1h"}`))
		}
		finish := transition("finish", &qa, done)
		if onTerminal {
			finish.Gates = []domain.Gate{timed("time", config)}
		}
		return wf([]domain.Status{dev, qa, done},
			transition("dev_done", &dev, qa), back, finish)
	}

	cases := []struct {
		name, config    string
		terminal, twice bool
		want            string
	}{
		{"4h на откате", `{"max_duration": "4h"}`, false, false, ""},
		{"90m", `{"max_duration":"90m"}`, false, false, ""},
		{"сутки 24h", `{"max_duration": "24h"}`, false, false, ""},
		{"нет max_duration", `{}`, false, false, "stage_time_limit_invalid"},
		{"config отсутствует", ``, false, false, "stage_time_limit_invalid"},
		{"0s", `{"max_duration": "0s"}`, false, false, "stage_time_limit_invalid"},
		{"-1h", `{"max_duration": "-1h"}`, false, false, "stage_time_limit_invalid"},
		{"abc", `{"max_duration": "abc"}`, false, false, "stage_time_limit_invalid"},
		{"число", `{"max_duration": 4}`, false, false, "stage_time_limit_invalid"},
		{"null", `{"max_duration": null}`, false, false, "stage_time_limit_invalid"},
		{"пустая строка", `{"max_duration": ""}`, false, false, "stage_time_limit_invalid"},
		{"1d", `{"max_duration": "1d"}`, false, false, "stage_time_limit_invalid"},
		{"на терминальный статус", `{"max_duration": "4h"}`, true, false, "stage_time_limit_terminal"},
		{"два на одном переходе", `{"max_duration": "4h"}`, false, true, "stage_time_limit_duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := validateWorkflow(cycle(tc.config, tc.terminal, tc.twice), allImplemented, true)
			if tc.want == "" {
				if !v.Valid {
					t.Fatalf("цикл с пределом собран верно: %v", v.Problems)
				}
				return
			}
			if v.Valid {
				t.Fatalf("цикл с негодным пределом прошёл проверку")
			}
			var found *Problem
			for i, p := range v.Problems {
				if p.Code == tc.want {
					found = &v.Problems[i]
				}
			}
			if found == nil {
				t.Fatalf("нет проблемы %q: %v", tc.want, v.Problems)
			}
			if found.Gate == "" || found.Transition == "" {
				t.Fatalf("проблема без ключей перехода и гейта: %+v", found)
			}
			if tc.want == "stage_time_limit_invalid" && !strings.Contains(found.Message, "max_duration") {
				t.Fatalf("причина не называет max_duration: %q", found.Message)
			}
			for _, w := range v.Warnings {
				if w.Code == tc.want {
					t.Fatalf("негодный предел попал в предупреждения: %+v", w)
				}
			}
		})
	}
}

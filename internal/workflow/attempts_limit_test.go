package workflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
)

// CLAB-217, правило 9: предел не задан или задан неверно (валидатор обойдён)
// — гейт не держит переход и не падает; базы для этого не нужно.
func TestAttemptsLimitWithoutLimitPasses(t *testing.T) {
	e := &Engine{}
	for _, config := range []string{`{}`, ``, `{"max_attempts": 0}`, `{"max_attempts": "3"}`} {
		out, err := e.evalAttemptsLimit(context.Background(), nil, EvalInput{
			Gate: gate(domain.GateAttemptsLimit, config)})
		if err != nil || out.Status != domain.GateResultPassed {
			t.Fatalf("config %q: %+v %v", config, out, err)
		}
		if !strings.Contains(out.Reason, "предел") {
			t.Fatalf("config %q: причина %q", config, out.Reason)
		}
	}
}

func TestMaxAttempts(t *testing.T) {
	for config, want := range map[string]int{
		`{"max_attempts": 3}`: 3, `{"max_attempts":1,"other":true}`: 1,
	} {
		if n, err := maxAttempts(json.RawMessage(config)); err != nil || n != want {
			t.Errorf("%s: %d %v", config, n, err)
		}
	}
	for _, config := range []string{`[]`, `{"max_attempts": 1e2}`, `{"max_attempts": 3.0}`,
		`{"max_attempts": 99999999999}`, `{"max_attempts": true}`} {
		if _, err := maxAttempts(json.RawMessage(config)); err == nil {
			t.Errorf("%s должен отбиваться", config)
		}
	}
}

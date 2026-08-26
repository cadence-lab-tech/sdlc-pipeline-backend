package domain

import (
	"encoding/json"
	"testing"
)

// Область по репозиториям — свойство задачи (решение 0015). Значение по
// умолчанию — false: разрешение по умолчанию размывало бы зону изменений,
// на которую опирается детектор дрейфа.
func TestTaskMultiRepo(t *testing.T) {
	cases := []struct {
		name  string
		props string
		want  bool
	}{
		{"свойств нет", "", false},
		{"свойства пустые", `{}`, false},
		{"флаг выключен", `{"multi_repo": false}`, false},
		{"флаг включён", `{"multi_repo": true}`, true},
		{"рядом другие свойства", `{"needs_migration": true, "multi_repo": true}`, true},
		{"чужие свойства без флага", `{"needs_migration": true}`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := Task{Properties: json.RawMessage(tc.props)}
			if got := task.MultiRepo(); got != tc.want {
				t.Fatalf("получили %v, ожидали %v", got, tc.want)
			}
		})
	}
}

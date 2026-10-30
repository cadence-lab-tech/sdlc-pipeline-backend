package domain

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Ребёнок в карточке родителя (CLAB-324, AC-7б): без трёх новых полей JSON
// побайтно — тот же, что у задачи, с тем же порядком ключей; новые поля идут
// последними. Клиент прошлой версии читает ответ как раньше.
func TestChildTaskJSONOnlyAppends(t *testing.T) {
	parent, assignee := uuid.New(), uuid.New()
	parentKey, handle, desc, level := "P-1", "ann", "описание", "high"
	closed := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	full := Task{
		ID: uuid.New(), ProjectID: uuid.New(), ProjectKey: "P", ParentID: &parent, ParentKey: &parentKey,
		Key: "P-2", Seq: 2, Title: "Ребёнок", Description: &desc, WorkflowID: uuid.New(), StatusID: uuid.New(),
		StatusKey: "intake", StatusName: "Intake", Priority: "high", AssigneeID: &assignee, AssigneeHandle: &handle,
		ExternalRef: json.RawMessage(`{"system":"jira","key":"X-1"}`), Labels: []string{"a"},
		Properties: json.RawMessage(`{"type":"story"}`), ModelLevel: &level, StageSettings: json.RawMessage(`{}`),
		OptionalStages: json.RawMessage(`{"design":false}`), CreatedAt: closed, UpdatedAt: closed, ClosedAt: &closed,
	}
	bare := Task{ID: uuid.New(), Key: "P-3", ExternalRef: json.RawMessage(`null`), Properties: json.RawMessage(`{}`),
		StageSettings: json.RawMessage(`{}`), OptionalStages: json.RawMessage(`{}`), Labels: []string{}}

	for _, task := range []Task{full, bare} {
		before, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		after, err := json.Marshal(ChildTask{Task: task, StatusCategory: "active", AwaitingApproval: true, BlockedBy: []string{"P-1"}})
		if err != nil {
			t.Fatal(err)
		}
		tail := []byte(`,"status_category":"active","awaiting_approval":true,"blocked_by":["P-1"]}`)
		if !bytes.HasSuffix(after, tail) {
			t.Fatalf("новые поля не в конце: %s", after)
		}
		stripped := append(append([]byte{}, after[:len(after)-len(tail)]...), '}')
		if !bytes.Equal(stripped, before) {
			t.Fatalf("без новых полей ответ изменился:\n%s\n%s", stripped, before)
		}
	}

	// Пустой blocked_by — [], а не null.
	raw, _ := json.Marshal(ChildTask{Task: bare, StatusCategory: "backlog", BlockedBy: []string{}})
	if !bytes.HasSuffix(raw, []byte(`"blocked_by":[]}`)) {
		t.Fatalf("пустой blocked_by: %s", raw)
	}
}

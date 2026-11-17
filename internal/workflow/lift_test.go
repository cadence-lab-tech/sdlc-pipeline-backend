package workflow

import (
	"context"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// liftToTopLevel снимает задачи сида с родителя прямой записью: SDLC-1..3 в
// демонстрационном проекте — дети эпика SDLC-4, а с CLAB-196 вложенность —
// один уровень, и завести ребёнка у ребёнка нельзя. Тестам, которым нужна
// своя задача с детьми, она нужна на верхнем уровне.
func liftToTopLevel(t *testing.T, store *postgres.Store, keys ...string) {
	t.Helper()
	if _, err := store.DB().Exec(context.Background(),
		`UPDATE tasks SET parent_id = NULL WHERE key = ANY($1)`, keys); err != nil {
		t.Fatal(err)
	}
}

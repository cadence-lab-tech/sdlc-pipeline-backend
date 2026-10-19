package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/embed"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// OnRunDone видит каждый исполненный прогон (CLAB-126, AC-12): и
// завершённый, и провалившийся — второй RunNext наружу не возвращает, и без
// хука метрики воркера не отличили бы его от пустой очереди.
func TestOnRunDoneSeesCompletedAndFailedRuns(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	type done struct {
		failed bool
		took   time.Duration
	}
	var seen []done
	src := &fakeSource{
		repo: "sdlc-pipeline-backend",
		head: "c1",
		commits: map[string]map[string]string{
			"c1": {"internal/workflow/engine.go": engineV1},
		},
	}
	ix := New(store, src, embed.Hash{}, testdb.Logger(), Options{
		ChunkChars: 400, EmbedBatch: 8,
		OnRunDone: func(failed bool, took time.Duration) { seen = append(seen, done{failed, took}) },
	})

	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	if run, err := ix.RunNext(ctx); err != nil || run == nil {
		t.Fatalf("прогон: %v", err)
	}
	if len(seen) != 1 || seen[0].failed || seen[0].took <= 0 {
		t.Fatalf("завершённый прогон: %+v", seen)
	}

	// Голова ушла на коммит, которого источник не отдаёт: прогон встаёт в
	// очередь и проваливается на листинге.
	src.head = "c-missing"
	if _, err := ix.Enqueue(ctx); err != nil {
		t.Fatal(err)
	}
	if run, err := ix.RunNext(ctx); err != nil || run != nil {
		t.Fatalf("проваленный прогон: run=%v err=%v", run, err)
	}
	if len(seen) != 2 || !seen[1].failed {
		t.Fatalf("проваленный прогон не учтён: %+v", seen)
	}

	// Пустая очередь — не прогон.
	if run, err := ix.RunNext(ctx); err != nil || run != nil {
		t.Fatalf("пустая очередь: run=%v err=%v", run, err)
	}
	if len(seen) != 2 {
		t.Fatalf("пустая очередь посчитана прогоном: %+v", seen)
	}
}

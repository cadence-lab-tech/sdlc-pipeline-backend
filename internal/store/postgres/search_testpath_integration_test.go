package postgres_test

import (
	"context"
	"testing"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/testdb"
)

// AC-11 (CLAB-369): признак тестового файла в SQL — та же регулярка и те же
// пути, что в табличном тесте Go (TestIsTestPath): поиск применяет её как
// cf.path ~ $7, и разойтись RE2 и регулярки Postgres на перечне не должны.
func TestTestPathPatternInSQL(t *testing.T) {
	_, pool := testdb.Store(t)
	check := func(paths []string, want bool) {
		for _, p := range paths {
			var got bool
			if err := pool.QueryRow(context.Background(), `SELECT $1 ~ $2`, p, postgres.TestPathPatternForTest).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("%s: в SQL тестовый=%v, ждали %v", p, got, want)
			}
		}
	}
	check(postgres.TestPaths, true)
	check(postgres.NonTestPaths, false)
}

package postgres

// Признак тестового файла для внешних интеграционных тестов (AC-11 CLAB-369):
// те же пути гоняются через SQL, которым его применяет поиск.
const TestPathPatternForTest = testPathPattern

// TestPaths и NonTestPaths — таблица AC-11, общая для проверки в Go и в SQL.
var (
	TestPaths = []string{
		"a_test.go", "src/a.test.ts", "src/a.spec.tsx", "test_a.py", "a_test.py",
		"x/testdata/a.go", "src/__tests__/a.ts", "test/a.js", "tests/a.py", "e2e/a.mjs",
		"x/fixtures/a.json", "pkg/sub/test_b.py", "web/e2e/x/y.ts",
	}
	NonTestPaths = []string{
		"internal/testdb/db.go", "contest.go", "latest.ts", "attestation.go",
		"src/testing-utils.ts", "test.go", "src/test_utils.go", "mytest/a.go", "a_test.go.orig",
	}
)

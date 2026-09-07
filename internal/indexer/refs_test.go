package indexer

import "testing"

// Пустое заключение означает «проверки ещё идут»: гейт обязан отличать
// «не прошло» от «ещё не известно», иначе он завалит переход на каждом
// свежем пуше.
func TestConcludeChecks(t *testing.T) {
	cases := []struct {
		name string
		runs []CheckRun
		want string
	}{
		{"проверок нет", nil, "success"},
		{"все зелёные", []CheckRun{
			{Name: "build", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "success"},
		}, "success"},
		{"одна идёт", []CheckRun{
			{Name: "build", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "in_progress"},
		}, ""},
		{"одна упала", []CheckRun{
			{Name: "build", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "completed", Conclusion: "failure"},
		}, "failure"},
		{"пропущенная не портит", []CheckRun{
			{Name: "lint", Status: "completed", Conclusion: "skipped"},
			{Name: "build", Status: "completed", Conclusion: "success"},
		}, "success"},
	}
	for _, c := range cases {
		if got := conclude(c.runs); got != c.want {
			t.Fatalf("%s: получили %q, ожидали %q", c.name, got, c.want)
		}
	}
}

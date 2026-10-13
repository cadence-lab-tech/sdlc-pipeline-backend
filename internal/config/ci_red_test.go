package config

import "testing"

// Временный тест для проверки, что ci краснеет на сломанном тесте (CLAB-143,
// критерий 8). Откатывается следующим коммитом.
func TestCIRed(t *testing.T) {
	t.Fatal("намеренно красный тест: ci должен упасть на make test-integration")
}

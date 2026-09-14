package config

import (
	"testing"
	"time"
)

// Сборка мусора в блобах включена по умолчанию: место, на которое не осталось
// ссылок, освобождать больше некому — удаление проекта и репозитория блобы
// намеренно не трогают. Забытая переменная не должна означать растущий счёт
// за хранилище.
func TestBlobGCDefaults(t *testing.T) {
	t.Setenv("SDLC_DATABASE_URL", "postgres://clab:clab@localhost:55432/clab")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case !c.BlobGCEnabled:
		t.Fatal("сборка мусора выключена по умолчанию")
	case c.BlobGCInterval != time.Hour:
		t.Fatalf("период по умолчанию %s", c.BlobGCInterval)
	case c.BlobGCLimit != 500:
		t.Fatalf("предел за проход по умолчанию %d", c.BlobGCLimit)
	case c.BlobGCMinAge != 24*time.Hour:
		t.Fatalf("минимальный возраст по умолчанию %s", c.BlobGCMinAge)
	case c.BlobGCDryRun:
		t.Fatal("сухой прогон включён по умолчанию")
	}
}

func TestBlobGCFromEnv(t *testing.T) {
	t.Setenv("SDLC_DATABASE_URL", "postgres://clab:clab@localhost:55432/clab")
	t.Setenv("SDLC_BLOB_GC_INTERVAL", "90s")
	t.Setenv("SDLC_BLOB_GC_LIMIT", "10")
	t.Setenv("SDLC_BLOB_GC_MIN_AGE", "5m")
	t.Setenv("SDLC_BLOB_GC_DRY_RUN", "true")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case c.BlobGCInterval != 90*time.Second:
		t.Fatalf("период %s", c.BlobGCInterval)
	case c.BlobGCLimit != 10:
		t.Fatalf("предел за проход %d", c.BlobGCLimit)
	case c.BlobGCMinAge != 5*time.Minute:
		t.Fatalf("минимальный возраст %s", c.BlobGCMinAge)
	case !c.BlobGCDryRun:
		t.Fatal("сухой прогон не включился")
	}

	// Тикер с неположительным периодом роняет воркер паникой на старте:
	// ошибку лучше назвать в конфигурации.
	t.Setenv("SDLC_BLOB_GC_INTERVAL", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("нулевой период принят")
	}
	// Выключенная сборка период не проверяет: тикер не заводится.
	t.Setenv("SDLC_BLOB_GC_ENABLED", "false")
	if c, err := Load(); err != nil || c.BlobGCEnabled {
		t.Fatalf("выключенная сборка: %v, enabled=%v", err, c.BlobGCEnabled)
	}
}

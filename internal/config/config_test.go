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
	t.Setenv("SDLC_BLOB_GC_INTERVAL", "90s")

	// Непозитивный предел за проход оставлял бы сборку включённой, но
	// бездействующей: тикер ходит, ничего не удаляет, лог молчит.
	t.Setenv("SDLC_BLOB_GC_LIMIT", "0")
	if _, err := Load(); err == nil {
		t.Fatal("нулевой предел за проход принят")
	}
	t.Setenv("SDLC_BLOB_GC_LIMIT", "-1")
	if _, err := Load(); err == nil {
		t.Fatal("отрицательный предел за проход принят")
	}

	// Выключенная сборка ни периода, ни предела не проверяет: тикер не
	// заводится, и значения ни на что не влияют.
	t.Setenv("SDLC_BLOB_GC_ENABLED", "false")
	t.Setenv("SDLC_BLOB_GC_INTERVAL", "0s")
	if c, err := Load(); err != nil || c.BlobGCEnabled {
		t.Fatalf("выключенная сборка: %v, enabled=%v", err, c.BlobGCEnabled)
	}
}

// Вытеснение кэша эмбеддингов включено по умолчанию по той же причине, что и
// сборка блобов: кэш ключуется содержимым и владельца не имеет, поэтому ни
// удаление проекта, ни отвязка репозитория его не уменьшают — забытая
// переменная означала бы монотонно растущую таблицу на общем диске.
func TestEmbeddingCacheGCDefaults(t *testing.T) {
	t.Setenv("SDLC_DATABASE_URL", "postgres://clab:clab@localhost:55432/clab")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case !c.EmbeddingCacheGCEnabled:
		t.Fatal("вытеснение выключено по умолчанию")
	case c.EmbeddingCacheGCInterval != 6*time.Hour:
		t.Fatalf("период по умолчанию %s", c.EmbeddingCacheGCInterval)
	case c.EmbeddingCacheTTL != 720*time.Hour:
		t.Fatalf("срок по умолчанию %s", c.EmbeddingCacheTTL)
	case c.EmbeddingCacheMaxRows != 1_000_000:
		t.Fatalf("граница по умолчанию %d", c.EmbeddingCacheMaxRows)
	case c.EmbeddingCacheGCLimit != 10_000:
		t.Fatalf("предел за проход по умолчанию %d", c.EmbeddingCacheGCLimit)
	case c.EmbeddingCacheGCDryRun:
		t.Fatal("сухой прогон включён по умолчанию")
	case c.EmbeddingCacheTouchAfter != time.Hour:
		t.Fatalf("порог отметки по умолчанию %s", c.EmbeddingCacheTouchAfter)
	}
}

func TestEmbeddingCacheGCFromEnv(t *testing.T) {
	t.Setenv("SDLC_DATABASE_URL", "postgres://clab:clab@localhost:55432/clab")
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_INTERVAL", "90s")
	t.Setenv("SDLC_EMBEDDING_CACHE_TTL", "48h")
	t.Setenv("SDLC_EMBEDDING_CACHE_MAX_ROWS", "500")
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_LIMIT", "10")
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_DRY_RUN", "true")
	t.Setenv("SDLC_EMBEDDING_CACHE_TOUCH_AFTER", "5m")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case c.EmbeddingCacheGCInterval != 90*time.Second:
		t.Fatalf("период %s", c.EmbeddingCacheGCInterval)
	case c.EmbeddingCacheTTL != 48*time.Hour:
		t.Fatalf("срок %s", c.EmbeddingCacheTTL)
	case c.EmbeddingCacheMaxRows != 500:
		t.Fatalf("граница %d", c.EmbeddingCacheMaxRows)
	case c.EmbeddingCacheGCLimit != 10:
		t.Fatalf("предел за проход %d", c.EmbeddingCacheGCLimit)
	case !c.EmbeddingCacheGCDryRun:
		t.Fatal("сухой прогон не включился")
	case c.EmbeddingCacheTouchAfter != 5*time.Minute:
		t.Fatalf("порог отметки %s", c.EmbeddingCacheTouchAfter)
	}

	// Тикер с неположительным периодом роняет воркер паникой на старте.
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_INTERVAL", "0s")
	if _, err := Load(); err == nil {
		t.Fatal("нулевой период принят")
	}
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_INTERVAL", "90s")

	// Тикер, которому нечего удалять, лучше поймать на старте, чем искать
	// потом в логах, почему кэш не уменьшается.
	t.Setenv("SDLC_EMBEDDING_CACHE_TTL", "0s")
	t.Setenv("SDLC_EMBEDDING_CACHE_MAX_ROWS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("проход без срока и без границы принят")
	}
	// Одной границы достаточно: вытеснять по сроку необязательно.
	t.Setenv("SDLC_EMBEDDING_CACHE_MAX_ROWS", "500")
	if _, err := Load(); err != nil {
		t.Fatalf("проход по одной границе: %v", err)
	}
	// Непозитивный предел за проход оставлял бы вытеснение включённым, но
	// бездействующим — та же ловушка, что у сборки блобов.
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_LIMIT", "0")
	if _, err := Load(); err == nil {
		t.Fatal("нулевой предел за проход принят")
	}
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_LIMIT", "-5")
	if _, err := Load(); err == nil {
		t.Fatal("отрицательный предел за проход принят")
	}

	// Выключенное вытеснение ни периода, ни пределов не проверяет.
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_ENABLED", "false")
	t.Setenv("SDLC_EMBEDDING_CACHE_MAX_ROWS", "0")
	t.Setenv("SDLC_EMBEDDING_CACHE_GC_INTERVAL", "0s")
	if c, err := Load(); err != nil || c.EmbeddingCacheGCEnabled {
		t.Fatalf("выключенное вытеснение: %v, enabled=%v", err, c.EmbeddingCacheGCEnabled)
	}
}

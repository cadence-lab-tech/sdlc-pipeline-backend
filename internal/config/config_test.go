package config

import (
	"strings"
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

const testDatabaseURL = "postgres://clab:clab@localhost:55432/clab"

// Размеры и предохранители, у которых нет осмысленного нуля (CLAB-73). Слой
// ниже их не отвергает, а молча меняет: индексатор подставляет умолчание,
// хранилище артефактов поднимает порог inline до 4 МиБ или снимает предел,
// чистка прогонов выключается.
var positiveIntVars = []string{
	"SDLC_INDEX_MAX_FILE_BYTES",
	"SDLC_INDEX_MAX_FILES",
	"SDLC_INDEX_CHUNK_CHARS",
	"SDLC_EMBEDDINGS_BATCH",
	"SDLC_BLOB_INLINE_MAX_BYTES",
	"SDLC_BLOB_MAX_BYTES",
	"SDLC_INDEX_RETAIN_RUNS",
}

// Длительности класса A: непозитивное значение — ошибка на старте. Три из них —
// периоды тикеров воркера, которые иначе роняют его паникой.
var positiveDurationVars = []struct{ name, negative string }{
	{"SDLC_SESSION_TTL", "-12h"},
	{"SDLC_LEASE_TTL", "-30m"},
	{"SDLC_HEARTBEAT_INTERVAL", "-5m"},
	{"SDLC_LEASE_SWEEP_INTERVAL", "-1m"},
	{"SDLC_INDEX_INTERVAL", "-5m"},
	{"SDLC_INDEX_STUCK_AFTER", "-30m"},
	{"SDLC_INDEX_POLL_INTERVAL", "-15s"},
	{"SDLC_SHUTDOWN_TIMEOUT", "-15s"},
}

func TestNonPositiveSizesRejected(t *testing.T) {
	for _, name := range positiveIntVars {
		for _, v := range []string{"0", "-1"} {
			t.Run(name+"="+v, func(t *testing.T) {
				t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
				t.Setenv(name, v)
				_, err := Load()
				if err == nil {
					t.Fatalf("%s=%s принят", name, v)
				}
				if !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "("+v+")") {
					t.Fatalf("ошибка не называет переменную и значение: %v", err)
				}
			})
		}
	}
}

// Проверки размеров безусловны: опечатка в пределе блобов не должна лежать
// незамеченной до дня, когда включат внешнее хранилище, а глубина хранения
// прогонов — до дня, когда включат индексацию.
func TestSizeChecksAreUnconditional(t *testing.T) {
	t.Run("блобы без внешнего хранилища", func(t *testing.T) {
		t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
		t.Setenv("SDLC_BLOB_DIR", "")
		t.Setenv("SDLC_BLOB_S3_BUCKET", "")
		t.Setenv("SDLC_BLOB_MAX_BYTES", "0")
		if _, err := Load(); err == nil {
			t.Fatal("SDLC_BLOB_MAX_BYTES=0 без хранилища принят")
		}
	})
	t.Run("глубина хранения при выключенной индексации", func(t *testing.T) {
		t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
		t.Setenv("SDLC_INDEX_ENABLED", "false")
		t.Setenv("SDLC_INDEX_RETAIN_RUNS", "0")
		if _, err := Load(); err == nil {
			t.Fatal("SDLC_INDEX_RETAIN_RUNS=0 при выключенной индексации принят")
		}
	})
}

// Ноль в глубине хранения — дефект CLAB-71 слово в слово: оператор хотел «не
// чистить», а сервис должен сказать, что так чистка не выключается.
func TestRetainRunsErrorExplainsPrune(t *testing.T) {
	t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
	t.Setenv("SDLC_INDEX_RETAIN_RUNS", "0")
	_, err := Load()
	if err == nil {
		t.Fatal("SDLC_INDEX_RETAIN_RUNS=0 принят")
	}
	for _, want := range []string{"SDLC_INDEX_RETAIN_RUNS", "(0)", "не выключается"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("в ошибке нет %q: %v", want, err)
		}
	}
}

func TestNonPositiveDurationsRejected(t *testing.T) {
	for _, d := range positiveDurationVars {
		for _, v := range []string{"0s", d.negative} {
			t.Run(d.name+"="+v, func(t *testing.T) {
				t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
				t.Setenv(d.name, v)
				_, err := Load()
				if err == nil {
					t.Fatalf("%s=%s принят", d.name, v)
				}
				if !strings.Contains(err.Error(), d.name) || !strings.Contains(err.Error(), "не больше нуля") {
					t.Fatalf("ошибка не про непозитивное значение %s: %v", d.name, err)
				}
			})
		}
	}

	// Тикер индексации заводится и при выключенной индексации: нулевой период
	// ронял бы воркер паникой, флаг не спасает.
	t.Run("SDLC_INDEX_INTERVAL при выключенной индексации", func(t *testing.T) {
		t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
		t.Setenv("SDLC_INDEX_ENABLED", "false")
		t.Setenv("SDLC_INDEX_INTERVAL", "0s")
		if _, err := Load(); err == nil {
			t.Fatal("SDLC_INDEX_INTERVAL=0s при выключенной индексации принят")
		}
	})

	// Парная проверка аренды работает по-прежнему и говорит про протухание, а
	// не про ноль.
	t.Run("heartbeat не меньше аренды", func(t *testing.T) {
		t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
		t.Setenv("SDLC_HEARTBEAT_INTERVAL", "40m")
		t.Setenv("SDLC_LEASE_TTL", "30m")
		_, err := Load()
		if err == nil {
			t.Fatal("heartbeat длиннее аренды принят")
		}
		if !strings.Contains(err.Error(), "протухнет") {
			t.Fatalf("ошибка не про протухание аренды: %v", err)
		}
	})
}

// Длительности класса B: ноль задуман и описан в таблице переменных, а
// отрицательное значение — промах знаком.
func TestNegativeZeroMeaningfulDurations(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]string
	}{
		{"SDLC_BLOB_GC_MIN_AGE", nil},
		// Без границы по строкам нулевой срок оставил бы вытеснению нечего
		// удалять — это отдельная проверка CLAB-71.
		{"SDLC_EMBEDDING_CACHE_TTL", map[string]string{"SDLC_EMBEDDING_CACHE_MAX_ROWS": "500"}},
		{"SDLC_EMBEDDING_CACHE_TOUCH_AFTER", nil},
		{"SDLC_REQUEST_TIMEOUT", nil},
		{"SDLC_DB_CONNECT_TIMEOUT", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name+"=0s", func(t *testing.T) {
			t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
			for k, v := range tc.extra {
				t.Setenv(k, v)
			}
			t.Setenv(tc.name, "0s")
			if _, err := Load(); err != nil {
				t.Fatalf("нулевой %s отвергнут: %v", tc.name, err)
			}
		})
		t.Run(tc.name+"=-1s", func(t *testing.T) {
			t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
			for k, v := range tc.extra {
				t.Setenv(k, v)
			}
			t.Setenv(tc.name, "-1s")
			_, err := Load()
			if err == nil {
				t.Fatalf("%s=-1s принят", tc.name)
			}
			if !strings.Contains(err.Error(), tc.name) || !strings.Contains(err.Error(), "(-1s)") {
				t.Fatalf("ошибка не называет переменную и значение: %v", err)
			}
		})
	}
}

func TestNumericDefaults(t *testing.T) {
	t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ints := []struct {
		field     string
		got, want int
	}{
		{"IndexMaxFileBytes", c.IndexMaxFileBytes, 524288},
		{"IndexMaxFiles", c.IndexMaxFiles, 20000},
		{"ChunkMaxChars", c.ChunkMaxChars, 1600},
		{"EmbeddingBatch", c.EmbeddingBatch, 96},
		{"BlobInlineMaxBytes", c.BlobInlineMaxBytes, 262144},
		{"BlobMaxBytes", c.BlobMaxBytes, 67108864},
		{"IndexRetainRuns", c.IndexRetainRuns, 5},
	}
	for _, f := range ints {
		if f.got != f.want {
			t.Errorf("%s по умолчанию %d, ждали %d", f.field, f.got, f.want)
		}
	}
	durations := []struct {
		field     string
		got, want time.Duration
	}{
		{"SessionTTL", c.SessionTTL, 12 * time.Hour},
		{"LeaseTTL", c.LeaseTTL, 30 * time.Minute},
		{"HeartbeatInterval", c.HeartbeatInterval, 5 * time.Minute},
		{"LeaseSweepInterval", c.LeaseSweepInterval, time.Minute},
		{"IndexInterval", c.IndexInterval, 5 * time.Minute},
		{"IndexStuckAfter", c.IndexStuckAfter, 30 * time.Minute},
		{"IndexPollInterval", c.IndexPollInterval, 15 * time.Second},
		{"ShutdownTimeout", c.ShutdownTimeout, 15 * time.Second},
		{"RequestTimeout", c.RequestTimeout, 30 * time.Second},
		{"ConnectTimeout", c.ConnectTimeout, 30 * time.Second},
		{"BlobGCMinAge", c.BlobGCMinAge, 24 * time.Hour},
		{"EmbeddingCacheTTL", c.EmbeddingCacheTTL, 720 * time.Hour},
		{"EmbeddingCacheTouchAfter", c.EmbeddingCacheTouchAfter, time.Hour},
	}
	for _, f := range durations {
		if f.got != f.want {
			t.Errorf("%s по умолчанию %s, ждали %s", f.field, f.got, f.want)
		}
	}
}

// Положительные значения доходят до полей как есть: проверки ничего не
// подменяют и не округляют.
func TestPositiveNumericValuesPassThrough(t *testing.T) {
	t.Setenv("SDLC_DATABASE_URL", testDatabaseURL)
	env := map[string]string{
		"SDLC_INDEX_MAX_FILE_BYTES":        "1",
		"SDLC_INDEX_MAX_FILES":             "2",
		"SDLC_INDEX_CHUNK_CHARS":           "3",
		"SDLC_EMBEDDINGS_BATCH":            "4",
		"SDLC_BLOB_INLINE_MAX_BYTES":       "5",
		"SDLC_BLOB_MAX_BYTES":              "1",
		"SDLC_INDEX_RETAIN_RUNS":           "1",
		"SDLC_SESSION_TTL":                 "1s",
		"SDLC_LEASE_TTL":                   "3s",
		"SDLC_HEARTBEAT_INTERVAL":          "2s",
		"SDLC_LEASE_SWEEP_INTERVAL":        "4s",
		"SDLC_INDEX_INTERVAL":              "5s",
		"SDLC_INDEX_STUCK_AFTER":           "6s",
		"SDLC_INDEX_POLL_INTERVAL":         "7s",
		"SDLC_SHUTDOWN_TIMEOUT":            "8s",
		"SDLC_REQUEST_TIMEOUT":             "9s",
		"SDLC_DB_CONNECT_TIMEOUT":          "10s",
		"SDLC_BLOB_GC_MIN_AGE":             "11s",
		"SDLC_EMBEDDING_CACHE_TTL":         "12s",
		"SDLC_EMBEDDING_CACHE_TOUCH_AFTER": "13s",
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ints := []struct {
		name      string
		got, want int
	}{
		{"SDLC_INDEX_MAX_FILE_BYTES", c.IndexMaxFileBytes, 1},
		{"SDLC_INDEX_MAX_FILES", c.IndexMaxFiles, 2},
		{"SDLC_INDEX_CHUNK_CHARS", c.ChunkMaxChars, 3},
		{"SDLC_EMBEDDINGS_BATCH", c.EmbeddingBatch, 4},
		{"SDLC_BLOB_INLINE_MAX_BYTES", c.BlobInlineMaxBytes, 5},
		{"SDLC_BLOB_MAX_BYTES", c.BlobMaxBytes, 1},
		{"SDLC_INDEX_RETAIN_RUNS", c.IndexRetainRuns, 1},
	}
	for _, f := range ints {
		if f.got != f.want {
			t.Errorf("%s: %d, ждали %d", f.name, f.got, f.want)
		}
	}
	durations := []struct {
		name      string
		got, want time.Duration
	}{
		{"SDLC_SESSION_TTL", c.SessionTTL, time.Second},
		{"SDLC_LEASE_TTL", c.LeaseTTL, 3 * time.Second},
		{"SDLC_HEARTBEAT_INTERVAL", c.HeartbeatInterval, 2 * time.Second},
		{"SDLC_LEASE_SWEEP_INTERVAL", c.LeaseSweepInterval, 4 * time.Second},
		{"SDLC_INDEX_INTERVAL", c.IndexInterval, 5 * time.Second},
		{"SDLC_INDEX_STUCK_AFTER", c.IndexStuckAfter, 6 * time.Second},
		{"SDLC_INDEX_POLL_INTERVAL", c.IndexPollInterval, 7 * time.Second},
		{"SDLC_SHUTDOWN_TIMEOUT", c.ShutdownTimeout, 8 * time.Second},
		{"SDLC_REQUEST_TIMEOUT", c.RequestTimeout, 9 * time.Second},
		{"SDLC_DB_CONNECT_TIMEOUT", c.ConnectTimeout, 10 * time.Second},
		{"SDLC_BLOB_GC_MIN_AGE", c.BlobGCMinAge, 11 * time.Second},
		{"SDLC_EMBEDDING_CACHE_TTL", c.EmbeddingCacheTTL, 12 * time.Second},
		{"SDLC_EMBEDDING_CACHE_TOUCH_AFTER", c.EmbeddingCacheTouchAfter, 13 * time.Second},
	}
	for _, f := range durations {
		if f.got != f.want {
			t.Errorf("%s: %s, ждали %s", f.name, f.got, f.want)
		}
	}
}

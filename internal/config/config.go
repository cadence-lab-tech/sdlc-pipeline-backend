// Package config собирает настройки сервиса из окружения.
//
// Всё конфигурируется переменными с префиксом SDLC_, потому что сервис
// разворачивается в контейнере: файл конфигурации там пришлось бы монтировать
// отдельно, а секрет подключения к базе всё равно приходит окружением.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore"
)

type Config struct {
	// HTTP
	HTTPAddr        string
	ShutdownTimeout time.Duration
	RequestTimeout  time.Duration

	// База
	DatabaseURL    string
	MaxConns       int32
	ConnectTimeout time.Duration

	// Старт
	AutoMigrate bool
	// Имена сидов из migrations/seed без расширения, в порядке применения.
	// Применяются только на пустой базе — сиды не идемпотентны.
	Seeds []string

	// Аренды агентов. Аренда протухает без heartbeat, воркер возвращает
	// задачу в pending — иначе умерший локально агент держал бы её вечно.
	LeaseTTL           time.Duration
	HeartbeatInterval  time.Duration
	LeaseSweepInterval time.Duration

	// MCP-сервер поднимается в том же процессе, что и REST: это два
	// транспорта над одним ядром (решение 0011).
	MCPEnabled bool

	// Чтение GitHub. Система работает с репозиториями только на чтение
	// (repositories.access_mode), поэтому токену достаточно прав на чтение.
	GitHubAPIURL string
	// GitHub App: один на инстанс, приватный ключ — единственный секрет
	// (решение 0028). Без них доступ идёт общим токеном и именованными
	// секретами, как в фазах 3-5.
	GitHubAppID      string
	GitHubAppKeyFile string
	GitHubToken      string
	// Вход людей через GitHub (решение 0030): OAuth-учётные данные того же
	// приложения. Секрет клиента — второй секрет GitHub, и живёт он здесь
	// же, в окружении. Адрес сайта нужен GitHub Enterprise: у него
	// /login/oauth живёт не там, где API.
	GitHubWebURL          string
	GitHubAppClientID     string
	GitHubAppClientSecret string
	// Слаг приложения — имя в адресе github.com/apps/<slug>: из него
	// строится кнопка «Подключить GitHub» (решение 0034).
	GitHubAppSlug string

	// Индексация репозиториев. Кодовая база под задачами двигается непрерывно,
	// поэтому прогон наследует у предыдущего всё, что не изменилось, и
	// выкачивает только изменившееся.
	IndexEnabled      bool
	IndexInterval     time.Duration
	IndexStuckAfter   time.Duration
	IndexMaxFileBytes int
	IndexMaxFiles     int
	ChunkMaxChars     int

	// Эмбеддинги: voyage — модель, hash — детерминированная заглушка для
	// локального прогона без ключа, none — вектора не считаются и поиск
	// вырождается в полнотекстовый.
	EmbeddingProvider string
	EmbeddingModel    string
	EmbeddingBatch    int
	VoyageAPIURL      string
	VoyageAPIKey      string

	// Движок
	// StrictGates=false: гейт нереализованного типа отдаёт skipped и пропускает
	// переход. Это режим отладки цикла, пока фазы 2-3 не написаны.
	// StrictGates=true: такой гейт валит переход. Значение для продакшена.
	StrictGates bool

	// Аутентификация (решение 0021). header — актор приезжает заголовком
	// X-SDLC-Actor, права не проверяются: режим отладки цикла. token — только
	// предъявленный токен, права по участию в проекте: значение для продакшена.
	AuthMode string
	// Первый администратор: заводится из окружения, потому что в пустой базе
	// некому выдать первый токен.
	AdminHandle string
	AdminToken  string
	// Первая команда (решение 0027): её лидом становится администратор, и
	// право заводить проекты у неё есть сразу. Пусто — команд нет, проекты
	// заводит администратор системы.
	BootstrapTeam     string
	BootstrapTeamName string
	// Вход людей через корпоративный IdP (решение 0027). Без Issuer и
	// ClientID вход работает по выданным токенам, как в фазе 4.
	OIDCIssuer      string
	OIDCClientID    string
	OIDCGroupsClaim string
	// Кто заводит проекты (решение 0030): open — любой вошедший, создатель
	// становится администратором проекта; teams — администратор системы и
	// лиды команд с правом на это (решение 0027).
	ProjectCreation string
	// Профиль поставки (решение 0033): shared — общий инстанс, квоты тарифа
	// считаются; single — выделенный, пределы задаёт договор, квоты выключены.
	Tenancy string
	// Срок жизни сессии: она отзывается выходом, но и сама протухает —
	// вкладку, забытую открытой на неделю, входом заново не считают.
	SessionTTL time.Duration
	// Актор по умолчанию в режиме header, если заголовка нет.
	DefaultActor string

	// Вынос крупного содержимого из базы. Пустой BlobDir — всё лежит inline,
	// а содержимое больше InlineMaxBytes отбивается.
	BlobDir            string
	BlobInlineMaxBytes int
	BlobMaxBytes       int
	// Объектное хранилище вместо каталога: нужно, когда реплик API несколько
	// и общего тома между ними нет. Заданный бакет перекрывает BlobDir.
	// Ключи доступа берутся из окружения и в базу не попадают.
	BlobS3 blobstore.S3Config

	// Сборка мусора в блобах: блоб без ссылок из артефактов, файлов прогонов
	// индексации и документов проекта — место, которое никому не принадлежит.
	// Освобождать его обязан кто-то: удаление проекта и репозитория блобы
	// намеренно не трогают, они общие по sha256 между арендаторами.
	BlobGCEnabled  bool
	BlobGCInterval time.Duration
	BlobGCLimit    int
	// Минимальный возраст кандидата: только что записанный блоб может ждать
	// ссылки из ещё не закоммиченной транзакции, и сутки запаса стоят дешевле
	// потерянного содержимого. Заодно это время на разбор аварии.
	BlobGCMinAge time.Duration
	// Сухой прогон: посчитать и не удалять. Оператор смотрит в лог перед тем,
	// как включить сборку на засиженной базе.
	BlobGCDryRun bool

	// Вытеснение кэша эмбеддингов: таблица ключуется содержимым и владельца
	// не имеет намеренно, поэтому ни удаление проекта, ни отвязка
	// репозитория её не уменьшают — растёт она монотонно. Свой период и свои
	// пределы, а не общие со сборкой блобов: этот проход — чистый SQL, а
	// сборщик блобов ходит во внешнее хранилище на каждый блоб.
	EmbeddingCacheGCEnabled  bool
	EmbeddingCacheGCInterval time.Duration
	// Сколько строка живёт без использования; 0 — не вытеснять по сроку.
	EmbeddingCacheTTL time.Duration
	// Верхняя граница размера кэша в строках; 0 — без границы. В строках, а
	// не в байтах: размер строки задан размерностью вектора, а место на диске
	// после удаления возвращает автовакуум, и не сразу.
	EmbeddingCacheMaxRows  int
	EmbeddingCacheGCLimit  int
	EmbeddingCacheGCDryRun bool
	// Порог отметки попадания: чаще этого last_used_at не переписывается.
	// Иначе каждая пачка каждого прогона переписывала бы четырёхкилобайтные
	// строки ради времени, которое по существу не изменилось.
	EmbeddingCacheTouchAfter time.Duration

	// Вебхуки GitHub: без секрета ручка выключена — принимать неподписанные
	// события значит позволить кому угодно ставить прогоны в очередь.
	GitHubWebhookSecret string
	// Как часто воркер смотрит в очередь прогонов между опросами голов веток:
	// вебхук кладёт коммит в очередь, и ждать полного интервала незачем.
	IndexPollInterval time.Duration
	// Сколько завершённых прогонов хранить на ветку; на прогоны, на которые
	// ссылаются базовые линии задач, вычистка не распространяется.
	IndexRetainRuns int

	LogLevel string
	LogJSON  bool
	Env      string
}

func Load() (Config, error) {
	c := Config{
		HTTPAddr:              env("SDLC_HTTP_ADDR", ":8080"),
		ShutdownTimeout:       15 * time.Second,
		RequestTimeout:        30 * time.Second,
		DatabaseURL:           env("SDLC_DATABASE_URL", ""),
		MaxConns:              10,
		ConnectTimeout:        30 * time.Second,
		AutoMigrate:           true,
		StrictGates:           false,
		LeaseTTL:              30 * time.Minute,
		HeartbeatInterval:     5 * time.Minute,
		LeaseSweepInterval:    time.Minute,
		MCPEnabled:            true,
		GitHubAPIURL:          env("SDLC_GITHUB_API_URL", "https://api.github.com"),
		GitHubAppID:           env("SDLC_GITHUB_APP_ID", ""),
		GitHubAppKeyFile:      env("SDLC_GITHUB_APP_KEY_FILE", ""),
		GitHubToken:           env("SDLC_GITHUB_TOKEN", ""),
		GitHubWebURL:          env("SDLC_GITHUB_URL", "https://github.com"),
		GitHubAppClientID:     env("SDLC_GITHUB_APP_CLIENT_ID", ""),
		GitHubAppClientSecret: env("SDLC_GITHUB_APP_CLIENT_SECRET", ""),
		GitHubAppSlug:         env("SDLC_GITHUB_APP_SLUG", ""),
		ProjectCreation:       env("SDLC_PROJECT_CREATION", "open"),
		Tenancy:               env("SDLC_TENANCY", "shared"),
		IndexEnabled:          true,
		IndexInterval:         5 * time.Minute,
		IndexStuckAfter:       30 * time.Minute,
		IndexMaxFileBytes:     512 << 10,
		IndexMaxFiles:         20000,
		ChunkMaxChars:         1600,
		EmbeddingProvider:     env("SDLC_EMBEDDINGS_PROVIDER", ""),
		EmbeddingModel:        env("SDLC_EMBEDDINGS_MODEL", "voyage-code-3"),
		EmbeddingBatch:        96,
		VoyageAPIURL:          env("SDLC_VOYAGE_API_URL", "https://api.voyageai.com/v1/embeddings"),
		VoyageAPIKey:          env("SDLC_VOYAGE_API_KEY", ""),
		AuthMode:              env("SDLC_AUTH_MODE", ""),
		AdminHandle:           env("SDLC_ADMIN_HANDLE", "admin"),
		AdminToken:            env("SDLC_ADMIN_TOKEN", ""),
		BootstrapTeam:         env("SDLC_BOOTSTRAP_TEAM", ""),
		BootstrapTeamName:     env("SDLC_BOOTSTRAP_TEAM_NAME", ""),
		OIDCIssuer:            env("SDLC_OIDC_ISSUER", ""),
		OIDCClientID:          env("SDLC_OIDC_CLIENT_ID", ""),
		OIDCGroupsClaim:       env("SDLC_OIDC_GROUPS_CLAIM", "groups"),
		SessionTTL:            12 * time.Hour,
		DefaultActor:          env("SDLC_DEFAULT_ACTOR", "anonymous"),
		BlobDir:               env("SDLC_BLOB_DIR", ""),
		BlobS3: blobstore.S3Config{
			Endpoint:     env("SDLC_BLOB_S3_ENDPOINT", ""),
			Region:       env("SDLC_BLOB_S3_REGION", env("AWS_REGION", "")),
			Bucket:       env("SDLC_BLOB_S3_BUCKET", ""),
			Prefix:       env("SDLC_BLOB_S3_PREFIX", ""),
			AccessKey:    env("SDLC_BLOB_S3_ACCESS_KEY_ID", env("AWS_ACCESS_KEY_ID", "")),
			SecretKey:    env("SDLC_BLOB_S3_SECRET_ACCESS_KEY", env("AWS_SECRET_ACCESS_KEY", "")),
			SessionToken: env("SDLC_BLOB_S3_SESSION_TOKEN", env("AWS_SESSION_TOKEN", "")),
		},
		BlobInlineMaxBytes: 256 << 10,
		BlobMaxBytes:       64 << 20,
		BlobGCEnabled:      true,
		BlobGCInterval:     time.Hour,
		// Пятьсот за проход: тик не затягивается, а следующий всё равно
		// наступит — хвост разбирается за несколько проходов.
		BlobGCLimit:              500,
		BlobGCMinAge:             24 * time.Hour,
		EmbeddingCacheGCEnabled:  true,
		EmbeddingCacheGCInterval: 6 * time.Hour,
		// Месяц без использования: кусок кода, не встретившийся за месяц ни в
		// одном прогоне, скорее всего уже не встретится, а промах стоит
		// одного вызова модели.
		EmbeddingCacheTTL: 720 * time.Hour,
		// Миллион строк — это около четырёх гигабайт при vector(1024).
		EmbeddingCacheMaxRows: 1_000_000,
		// Десять тысяч за проход: тик не затягивается, а хвост разбирается за
		// несколько проходов.
		EmbeddingCacheGCLimit:    10_000,
		EmbeddingCacheTouchAfter: time.Hour,
		GitHubWebhookSecret:      env("SDLC_GITHUB_WEBHOOK_SECRET", ""),
		IndexPollInterval:        15 * time.Second,
		IndexRetainRuns:          5,
		LogLevel:                 env("SDLC_LOG_LEVEL", "info"),
		Env:                      env("SDLC_ENV", "development"),
	}

	if c.DatabaseURL == "" {
		return c, fmt.Errorf("SDLC_DATABASE_URL не задан")
	}

	var err error
	if c.AutoMigrate, err = envBool("SDLC_AUTO_MIGRATE", c.AutoMigrate); err != nil {
		return c, err
	}
	if c.StrictGates, err = envBool("SDLC_STRICT_GATES", c.StrictGates); err != nil {
		return c, err
	}
	if c.MCPEnabled, err = envBool("SDLC_MCP_ENABLED", c.MCPEnabled); err != nil {
		return c, err
	}
	if c.SessionTTL, err = envDuration("SDLC_SESSION_TTL", c.SessionTTL); err != nil {
		return c, err
	}
	if c.LeaseTTL, err = envDuration("SDLC_LEASE_TTL", c.LeaseTTL); err != nil {
		return c, err
	}
	if c.HeartbeatInterval, err = envDuration("SDLC_HEARTBEAT_INTERVAL", c.HeartbeatInterval); err != nil {
		return c, err
	}
	if c.LeaseSweepInterval, err = envDuration("SDLC_LEASE_SWEEP_INTERVAL", c.LeaseSweepInterval); err != nil {
		return c, err
	}
	if c.HeartbeatInterval >= c.LeaseTTL {
		return c, fmt.Errorf("SDLC_HEARTBEAT_INTERVAL (%s) не меньше SDLC_LEASE_TTL (%s): "+
			"аренда протухнет раньше первого продления", c.HeartbeatInterval, c.LeaseTTL)
	}
	if c.BlobGCEnabled, err = envBool("SDLC_BLOB_GC_ENABLED", c.BlobGCEnabled); err != nil {
		return c, err
	}
	if c.BlobGCInterval, err = envDuration("SDLC_BLOB_GC_INTERVAL", c.BlobGCInterval); err != nil {
		return c, err
	}
	if c.BlobGCLimit, err = envInt("SDLC_BLOB_GC_LIMIT", c.BlobGCLimit); err != nil {
		return c, err
	}
	if c.BlobGCMinAge, err = envDuration("SDLC_BLOB_GC_MIN_AGE", c.BlobGCMinAge); err != nil {
		return c, err
	}
	if c.BlobGCDryRun, err = envBool("SDLC_BLOB_GC_DRY_RUN", c.BlobGCDryRun); err != nil {
		return c, err
	}
	// Тикер с неположительным периодом роняет воркер паникой на старте, а
	// сборка включена по умолчанию: ошибку лучше назвать здесь.
	if c.BlobGCEnabled && c.BlobGCInterval <= 0 {
		return c, fmt.Errorf("SDLC_BLOB_GC_INTERVAL (%s) не больше нуля", c.BlobGCInterval)
	}
	if c.EmbeddingCacheGCEnabled, err = envBool("SDLC_EMBEDDING_CACHE_GC_ENABLED", c.EmbeddingCacheGCEnabled); err != nil {
		return c, err
	}
	if c.EmbeddingCacheGCInterval, err = envDuration("SDLC_EMBEDDING_CACHE_GC_INTERVAL", c.EmbeddingCacheGCInterval); err != nil {
		return c, err
	}
	if c.EmbeddingCacheTTL, err = envDuration("SDLC_EMBEDDING_CACHE_TTL", c.EmbeddingCacheTTL); err != nil {
		return c, err
	}
	if c.EmbeddingCacheMaxRows, err = envInt("SDLC_EMBEDDING_CACHE_MAX_ROWS", c.EmbeddingCacheMaxRows); err != nil {
		return c, err
	}
	if c.EmbeddingCacheGCLimit, err = envInt("SDLC_EMBEDDING_CACHE_GC_LIMIT", c.EmbeddingCacheGCLimit); err != nil {
		return c, err
	}
	if c.EmbeddingCacheGCDryRun, err = envBool("SDLC_EMBEDDING_CACHE_GC_DRY_RUN", c.EmbeddingCacheGCDryRun); err != nil {
		return c, err
	}
	if c.EmbeddingCacheTouchAfter, err = envDuration("SDLC_EMBEDDING_CACHE_TOUCH_AFTER", c.EmbeddingCacheTouchAfter); err != nil {
		return c, err
	}
	if c.EmbeddingCacheGCEnabled && c.EmbeddingCacheGCInterval <= 0 {
		return c, fmt.Errorf("SDLC_EMBEDDING_CACHE_GC_INTERVAL (%s) не больше нуля", c.EmbeddingCacheGCInterval)
	}
	// Тикер, которому нечего удалять, лучше поймать на старте, чем искать
	// потом в логах, почему кэш не уменьшается.
	if c.EmbeddingCacheGCEnabled && c.EmbeddingCacheTTL <= 0 && c.EmbeddingCacheMaxRows <= 0 {
		return c, fmt.Errorf("вытеснение кэша эмбеддингов включено, но и SDLC_EMBEDDING_CACHE_TTL, " +
			"и SDLC_EMBEDDING_CACHE_MAX_ROWS выключены: проходу нечего удалять")
	}
	if c.IndexEnabled, err = envBool("SDLC_INDEX_ENABLED", c.IndexEnabled); err != nil {
		return c, err
	}
	if c.IndexInterval, err = envDuration("SDLC_INDEX_INTERVAL", c.IndexInterval); err != nil {
		return c, err
	}
	if c.IndexStuckAfter, err = envDuration("SDLC_INDEX_STUCK_AFTER", c.IndexStuckAfter); err != nil {
		return c, err
	}
	if c.IndexMaxFileBytes, err = envInt("SDLC_INDEX_MAX_FILE_BYTES", c.IndexMaxFileBytes); err != nil {
		return c, err
	}
	if c.IndexMaxFiles, err = envInt("SDLC_INDEX_MAX_FILES", c.IndexMaxFiles); err != nil {
		return c, err
	}
	if c.ChunkMaxChars, err = envInt("SDLC_INDEX_CHUNK_CHARS", c.ChunkMaxChars); err != nil {
		return c, err
	}
	if c.EmbeddingBatch, err = envInt("SDLC_EMBEDDINGS_BATCH", c.EmbeddingBatch); err != nil {
		return c, err
	}
	// Провайдер по умолчанию выводится из наличия ключа: забыть переменную
	// провайдера при заданном ключе — типичная причина молча пустого индекса.
	if c.EmbeddingProvider == "" {
		if c.VoyageAPIKey != "" {
			c.EmbeddingProvider = "voyage"
		} else {
			c.EmbeddingProvider = "none"
		}
	}
	switch c.EmbeddingProvider {
	case "voyage":
		if c.VoyageAPIKey == "" {
			return c, fmt.Errorf("SDLC_EMBEDDINGS_PROVIDER=voyage без SDLC_VOYAGE_API_KEY")
		}
	case "hash", "none":
	default:
		return c, fmt.Errorf("SDLC_EMBEDDINGS_PROVIDER: %q — не voyage, hash или none", c.EmbeddingProvider)
	}

	// Режим аутентификации по умолчанию выводится из окружения: локальная
	// отладка идёт по заголовку, всё остальное — по токенам. Забыть включить
	// токены на проде — типичная причина открытого наружу сервиса.
	if c.AuthMode == "" {
		if c.Env == "development" {
			c.AuthMode = "header"
		} else {
			c.AuthMode = "token"
		}
	}
	switch c.AuthMode {
	case "header", "token":
	default:
		return c, fmt.Errorf("SDLC_AUTH_MODE: %q — не header или token", c.AuthMode)
	}
	switch c.ProjectCreation {
	case "open", "teams":
	default:
		return c, fmt.Errorf("SDLC_PROJECT_CREATION: %q — не open или teams", c.ProjectCreation)
	}
	switch c.Tenancy {
	case "shared", "single":
	default:
		return c, fmt.Errorf("SDLC_TENANCY: %q — не shared или single", c.Tenancy)
	}
	if (c.GitHubAppClientID == "") != (c.GitHubAppClientSecret == "") {
		return c, fmt.Errorf("SDLC_GITHUB_APP_CLIENT_ID и SDLC_GITHUB_APP_CLIENT_SECRET задаются вместе")
	}
	if c.AuthMode == "token" && c.AdminToken == "" {
		if c.Env == "development" {
			fmt.Fprintln(os.Stderr, "SDLC_AUTH_MODE=token без SDLC_ADMIN_TOKEN: первого администратора завести некому")
		}
	}
	if c.BlobInlineMaxBytes, err = envInt("SDLC_BLOB_INLINE_MAX_BYTES", c.BlobInlineMaxBytes); err != nil {
		return c, err
	}
	if c.BlobMaxBytes, err = envInt("SDLC_BLOB_MAX_BYTES", c.BlobMaxBytes); err != nil {
		return c, err
	}
	// MinIO и совместимые умеют только path-style, у AWS работает и он же.
	if c.BlobS3.PathStyle, err = envBool("SDLC_BLOB_S3_PATH_STYLE", true); err != nil {
		return c, err
	}
	if c.BlobS3.Bucket != "" && c.BlobS3.Endpoint == "" && c.BlobS3.Region != "" {
		c.BlobS3.Endpoint = "https://s3." + c.BlobS3.Region + ".amazonaws.com"
	}
	if c.IndexPollInterval, err = envDuration("SDLC_INDEX_POLL_INTERVAL", c.IndexPollInterval); err != nil {
		return c, err
	}
	if c.IndexRetainRuns, err = envInt("SDLC_INDEX_RETAIN_RUNS", c.IndexRetainRuns); err != nil {
		return c, err
	}

	if c.LogJSON, err = envBool("SDLC_LOG_JSON", c.Env != "development"); err != nil {
		return c, err
	}
	if c.ShutdownTimeout, err = envDuration("SDLC_SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return c, err
	}
	if c.RequestTimeout, err = envDuration("SDLC_REQUEST_TIMEOUT", c.RequestTimeout); err != nil {
		return c, err
	}
	if c.ConnectTimeout, err = envDuration("SDLC_DB_CONNECT_TIMEOUT", c.ConnectTimeout); err != nil {
		return c, err
	}
	if n, err := envInt("SDLC_DB_MAX_CONNS", int(c.MaxConns)); err != nil {
		return c, err
	} else {
		c.MaxConns = int32(n)
	}

	for _, s := range strings.Split(env("SDLC_SEED", ""), ",") {
		if s = strings.TrimSpace(s); s != "" {
			c.Seeds = append(c.Seeds, s)
		}
	}

	return c, nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q не булево значение", key, v)
	}
	return b, nil
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q не целое число", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("%s: %q не длительность (нужно 15s, 2m)", key, v)
	}
	return d, nil
}

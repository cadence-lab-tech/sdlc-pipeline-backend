// Package config собирает настройки сервиса из окружения.
//
// Всё конфигурируется переменными с префиксом SDLC_, потому что сервис
// разворачивается в контейнере: файл конфигурации там пришлось бы монтировать
// отдельно, а секрет подключения к базе всё равно приходит окружением.
package config

import (
	"fmt"
	"net"
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
	//
	// IndexMaxFileBytes — файлы крупнее не индексируются. Он не больше
	// действующего предела блоба: содержимое файла ложится в blobs
	// (checkIndexBlobLimit).
	IndexEnabled      bool
	IndexInterval     time.Duration
	IndexStuckAfter   time.Duration
	IndexMaxFileBytes int
	IndexMaxFiles     int
	ChunkMaxChars     int

	// Эмбеддинги: voyage — модель Voyage, openai — любой OpenAI-совместимый
	// эндпоинт (OpenAI, Ollama, TEI, vLLM), hash — детерминированная заглушка
	// для локального прогона без ключа, none — вектора не считаются и поиск
	// вырождается в полнотекстовый.
	//
	// EmbeddingModel после Load уже выбрана под провайдера: у voyage своя по
	// умолчанию, у openai умолчания нет — имя модели чужого сервера угадать
	// нельзя. EmbeddingURL — базовый адрес OpenAI-совместимого API (…/v1),
	// EmbeddingSendDimensions — отправлять ли в запросе dimensions: часть
	// серверов отвечает 400 на незнакомое поле.
	EmbeddingProvider       string
	EmbeddingModel          string
	EmbeddingBatch          int
	EmbeddingURL            string
	EmbeddingAPIKey         string
	EmbeddingSendDimensions bool
	VoyageAPIURL            string
	VoyageAPIKey            string

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
	// а содержимое больше min(blobstore.InlineMaxBytes, BlobMaxBytes)
	// отбивается. Соотношение пределов — checkBlobLimits.
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

	// Адрес слушателя /metrics в формате Prometheus (CLAB-126), общий для
	// api и воркера. Пусто — метрики выключены и порт не открывается.
	// Отдельный порт, а не путь основного: основной смотрит в интернет через
	// Caddy, а метрики отдаются без токена.
	MetricsAddr string

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
		EmbeddingModel:        env("SDLC_EMBEDDINGS_MODEL", ""),
		EmbeddingBatch:        96,
		EmbeddingURL:          strings.TrimRight(env("SDLC_EMBEDDINGS_URL", ""), "/"),
		EmbeddingAPIKey:       env("SDLC_EMBEDDINGS_API_KEY", ""),
		VoyageAPIURL:          env("SDLC_VOYAGE_API_URL", ""),
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
		MetricsAddr:              env("SDLC_METRICS_ADDR", ""),
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
	if c.SessionTTL, err = envPositiveDuration("SDLC_SESSION_TTL", c.SessionTTL); err != nil {
		return c, err
	}
	if c.LeaseTTL, err = envPositiveDuration("SDLC_LEASE_TTL", c.LeaseTTL); err != nil {
		return c, err
	}
	if c.HeartbeatInterval, err = envPositiveDuration("SDLC_HEARTBEAT_INTERVAL", c.HeartbeatInterval); err != nil {
		return c, err
	}
	if c.LeaseSweepInterval, err = envPositiveDuration("SDLC_LEASE_SWEEP_INTERVAL", c.LeaseSweepInterval); err != nil {
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
	if c.BlobGCMinAge, err = envNonNegativeDuration("SDLC_BLOB_GC_MIN_AGE", c.BlobGCMinAge, "без выдержки, кандидат удаляется сразу"); err != nil {
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
	// Предел за проход — предохранитель от затянувшегося тика, а не выключатель:
	// с непозитивным пределом сборка ходит по тикеру и молча ничего не удаляет,
	// и оператор узнаёт об этом по выросшей таблице. Ошибка на старте дешевле.
	if c.BlobGCEnabled && c.BlobGCLimit < 1 {
		return c, fmt.Errorf("сборка блобов включена, но SDLC_BLOB_GC_LIMIT (%d) не больше нуля: "+
			"проход ничего не удалит; выключается сборка через SDLC_BLOB_GC_ENABLED", c.BlobGCLimit)
	}
	if c.EmbeddingCacheGCEnabled, err = envBool("SDLC_EMBEDDING_CACHE_GC_ENABLED", c.EmbeddingCacheGCEnabled); err != nil {
		return c, err
	}
	if c.EmbeddingCacheGCInterval, err = envDuration("SDLC_EMBEDDING_CACHE_GC_INTERVAL", c.EmbeddingCacheGCInterval); err != nil {
		return c, err
	}
	if c.EmbeddingCacheTTL, err = envNonNegativeDuration("SDLC_EMBEDDING_CACHE_TTL", c.EmbeddingCacheTTL, "не вытеснять по сроку"); err != nil {
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
	if c.EmbeddingCacheTouchAfter, err = envNonNegativeDuration("SDLC_EMBEDDING_CACHE_TOUCH_AFTER", c.EmbeddingCacheTouchAfter, "отмечать каждое попадание"); err != nil {
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
	// Тот же предохранитель, что у сборки блобов: непозитивный предел за проход
	// оставляет вытеснение включённым, но бездействующим.
	if c.EmbeddingCacheGCEnabled && c.EmbeddingCacheGCLimit < 1 {
		return c, fmt.Errorf("вытеснение кэша эмбеддингов включено, но SDLC_EMBEDDING_CACHE_GC_LIMIT (%d) "+
			"не больше нуля: проход ничего не удалит; выключается вытеснение через "+
			"SDLC_EMBEDDING_CACHE_GC_ENABLED", c.EmbeddingCacheGCLimit)
	}
	if c.IndexEnabled, err = envBool("SDLC_INDEX_ENABLED", c.IndexEnabled); err != nil {
		return c, err
	}
	if c.IndexInterval, err = envPositiveDuration("SDLC_INDEX_INTERVAL", c.IndexInterval); err != nil {
		return c, err
	}
	if c.IndexStuckAfter, err = envPositiveDuration("SDLC_INDEX_STUCK_AFTER", c.IndexStuckAfter); err != nil {
		return c, err
	}
	if c.IndexMaxFileBytes, err = envPositiveInt("SDLC_INDEX_MAX_FILE_BYTES", c.IndexMaxFileBytes, indexSizeHint); err != nil {
		return c, err
	}
	if c.IndexMaxFiles, err = envPositiveInt("SDLC_INDEX_MAX_FILES", c.IndexMaxFiles, indexSizeHint); err != nil {
		return c, err
	}
	if c.ChunkMaxChars, err = envPositiveInt("SDLC_INDEX_CHUNK_CHARS", c.ChunkMaxChars, indexSizeHint); err != nil {
		return c, err
	}
	if c.EmbeddingBatch, err = envPositiveInt("SDLC_EMBEDDINGS_BATCH", c.EmbeddingBatch, indexSizeHint); err != nil {
		return c, err
	}
	if c.EmbeddingSendDimensions, err = envBool("SDLC_EMBEDDINGS_SEND_DIMENSIONS", true); err != nil {
		return c, err
	}
	if err := resolveEmbeddings(&c); err != nil {
		return c, err
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
	if c.BlobInlineMaxBytes, err = envPositiveInt("SDLC_BLOB_INLINE_MAX_BYTES", c.BlobInlineMaxBytes, ""); err != nil {
		return c, err
	}
	if c.BlobMaxBytes, err = envPositiveInt("SDLC_BLOB_MAX_BYTES", c.BlobMaxBytes, ""); err != nil {
		return c, err
	}
	if err := checkBlobLimits(c.BlobInlineMaxBytes, c.BlobMaxBytes); err != nil {
		return c, err
	}
	if err := checkIndexBlobLimit(c.IndexMaxFileBytes, c.BlobMaxBytes,
		c.BlobS3.Bucket != "" || c.BlobDir != ""); err != nil {
		return c, err
	}
	// MinIO и совместимые умеют только path-style, у AWS работает и он же.
	if c.BlobS3.PathStyle, err = envBool("SDLC_BLOB_S3_PATH_STYLE", true); err != nil {
		return c, err
	}
	if c.BlobS3.Bucket != "" && c.BlobS3.Endpoint == "" && c.BlobS3.Region != "" {
		c.BlobS3.Endpoint = "https://s3." + c.BlobS3.Region + ".amazonaws.com"
	}
	if c.IndexPollInterval, err = envPositiveDuration("SDLC_INDEX_POLL_INTERVAL", c.IndexPollInterval); err != nil {
		return c, err
	}
	if c.IndexRetainRuns, err = envPositiveInt("SDLC_INDEX_RETAIN_RUNS", c.IndexRetainRuns, retainRunsHint); err != nil {
		return c, err
	}

	if err := checkListenAddr("SDLC_METRICS_ADDR", c.MetricsAddr); err != nil {
		return c, err
	}

	if c.LogJSON, err = envBool("SDLC_LOG_JSON", c.Env != "development"); err != nil {
		return c, err
	}
	if c.ShutdownTimeout, err = envPositiveDuration("SDLC_SHUTDOWN_TIMEOUT", c.ShutdownTimeout); err != nil {
		return c, err
	}
	if c.RequestTimeout, err = envNonNegativeDuration("SDLC_REQUEST_TIMEOUT", c.RequestTimeout, "без таймаута"); err != nil {
		return c, err
	}
	if c.ConnectTimeout, err = envNonNegativeDuration("SDLC_DB_CONNECT_TIMEOUT", c.ConnectTimeout, "не ждать базу, одна попытка"); err != nil {
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

// Адреса и модели эмбеддингов по умолчанию.
const (
	defaultVoyageURL   = "https://api.voyageai.com/v1/embeddings"
	defaultVoyageModel = "voyage-code-3"
	defaultOpenAIURL   = "https://api.openai.com/v1"
)

// resolveEmbeddings выбирает провайдера эмбеддингов и доводит его настройки
// до рабочих.
//
// Провайдер по умолчанию выводится из ключа Voyage: забыть переменную
// провайдера при заданном ключе — типичная причина молча пустого индекса, а
// наш инстанс так и настроен. Общие SDLC_EMBEDDINGS_URL и _API_KEY служат
// Voyage запасными к SDLC_VOYAGE_*, но сами провайдера не выбирают: по ним не
// понять, чей это сервер.
func resolveEmbeddings(c *Config) error {
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
			c.VoyageAPIKey = c.EmbeddingAPIKey
		}
		if c.VoyageAPIKey == "" {
			return fmt.Errorf("SDLC_EMBEDDINGS_PROVIDER=voyage без SDLC_VOYAGE_API_KEY (или SDLC_EMBEDDINGS_API_KEY)")
		}
		// SDLC_VOYAGE_API_URL — адрес ручки целиком, SDLC_EMBEDDINGS_URL —
		// базовый адрес API, к которому ручка дописывается.
		if c.VoyageAPIURL == "" && c.EmbeddingURL != "" {
			c.VoyageAPIURL = c.EmbeddingURL + "/embeddings"
		}
		if c.VoyageAPIURL == "" {
			c.VoyageAPIURL = defaultVoyageURL
		}
		if c.EmbeddingModel == "" {
			c.EmbeddingModel = defaultVoyageModel
		}
	case "openai":
		// Модель у OpenAI-совместимого сервера своя у каждого: voyage-code-3
		// по умолчанию ушёл бы чужому серверу и вернулся бы 404 на каждом
		// прогоне индексации.
		if c.EmbeddingModel == "" {
			return fmt.Errorf("SDLC_EMBEDDINGS_PROVIDER=openai без SDLC_EMBEDDINGS_MODEL: " +
				"модель OpenAI-совместимого сервера по умолчанию не угадать, задайте её явно")
		}
		if c.EmbeddingURL == "" {
			c.EmbeddingURL = defaultOpenAIURL
		}
	case "hash", "none":
	default:
		return fmt.Errorf("SDLC_EMBEDDINGS_PROVIDER: %q — не voyage, openai, hash или none", c.EmbeddingProvider)
	}
	return nil
}

// checkBlobLimits сверяет пределы содержимого между собой. Порог inline —
// где содержимое лежит, общий предел — сколько принимается вовсе, поэтому
// порог не больше предела. И не больше потолка blobstore.InlineMaxBytes:
// мегабайты в строке таблицы — ровно то, от чего спасает вынос. Проверка
// безусловна, как и проверка на непозитивные значения: противоречивая пара не
// должна лежать незамеченной до дня, когда включат внешнее хранилище.
func checkBlobLimits(inline, maxBytes int) error {
	if inline > maxBytes {
		return fmt.Errorf("SDLC_BLOB_INLINE_MAX_BYTES (%d) больше SDLC_BLOB_MAX_BYTES (%d): "+
			"порог хранения в базе не может превышать предел принимаемого содержимого", inline, maxBytes)
	}
	if inline > blobstore.InlineMaxBytes {
		return fmt.Errorf("SDLC_BLOB_INLINE_MAX_BYTES (%d) больше потолка %d (4 МиБ): "+
			"крупнее в базе не хранится, такое содержимое уходит во внешнее хранилище", inline, blobstore.InlineMaxBytes)
	}
	return nil
}

// checkIndexBlobLimit сверяет предел индексатора с пределом блоба (CLAB-82):
// содержимое проиндексированного файла пишется в blobs, и файл, который
// хранилище не примет, ронял бы прогон, а не пропускался. Действующий предел
// блоба — SDLC_BLOB_MAX_BYTES, а без внешнего хранилища — ещё и потолок
// blobstore.InlineMaxBytes; external — то же условие, по которому
// blobstore.Open выбирает хранилище. Проверка безусловна по
// SDLC_INDEX_ENABLED, как и проверка пары блобов: противоречие не должно
// лежать незамеченным до дня, когда включат индексацию.
func checkIndexBlobLimit(indexMax, blobMax int, external bool) error {
	if indexMax > blobMax {
		return fmt.Errorf("SDLC_INDEX_MAX_FILE_BYTES (%d) больше SDLC_BLOB_MAX_BYTES (%d): "+
			"содержимое файла не примет хранилище, и прогон индексации упадёт на нём", indexMax, blobMax)
	}
	if !external && indexMax > blobstore.InlineMaxBytes {
		return fmt.Errorf("SDLC_INDEX_MAX_FILE_BYTES (%d) больше потолка %d (4 МиБ) без внешнего хранилища: "+
			"уменьшите предел или задайте SDLC_BLOB_DIR или SDLC_BLOB_S3_BUCKET", indexMax, blobstore.InlineMaxBytes)
	}
	return nil
}

// checkListenAddr проверяет адрес слушателя вида host:port или :port.
// Ошибка здесь останавливает старт: опечатка в адресе метрик иначе
// превратилась бы в процесс, который молча работает без них.
func checkListenAddr(key, addr string) error {
	if addr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: %q не адрес слушателя, нужно :9090 или host:9090", key, addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s: порт %q вне 1-65535", key, port)
	}
	return nil
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

// Подсказки к ошибкам непозитивных размеров: оператор, поставивший ноль, обычно
// хотел что-то выключить, и текст говорит, как это делается на самом деле.
const (
	indexSizeHint  = "индексация так не выключается, выключается она через SDLC_INDEX_ENABLED"
	retainRunsHint = "чистка старых прогонов индексации так не выключается; " +
		"чтобы хранить больше, задайте большое число"
)

// envPositiveInt читает размер или предохранитель, у которого нет осмысленного
// нуля. Слой ниже такие значения не отвергает, а молча меняет: индексатор и
// клиент эмбеддингов подставляют умолчание (indexer.New, Chunks, NewVoyage),
// хранилище артефактов поднимает порог inline до 4 МиБ вместо 256 КиБ или
// снимает предел размера (inlineLimit, PutBlob), чистка прогонов выключается
// (Prune). Оператор узнавал бы об этом по поведению — ошибка на старте дешевле.
// Охраны у потребителей остаются: их зовут и в обход конфигурации.
func envPositiveInt(key string, def int, hint string) (int, error) {
	n, err := envInt(key, def)
	if err != nil {
		return n, err
	}
	if n <= 0 {
		if hint != "" {
			return n, fmt.Errorf("%s (%d) не больше нуля: %s", key, n, hint)
		}
		return n, fmt.Errorf("%s (%d) не больше нуля", key, n)
	}
	return n, nil
}

// envPositiveDuration читает период или срок, у которого нет осмысленного
// нуля. Периоды тикеров воркера (SDLC_LEASE_SWEEP_INTERVAL,
// SDLC_INDEX_INTERVAL, SDLC_INDEX_POLL_INTERVAL) иначе роняют его паникой
// time.NewTicker — даже при выключенной индексации, сроки аренды, сессии и
// застрявшего прогона молча подменяются умолчанием слоем ниже, а нулевой
// SDLC_SHUTDOWN_TIMEOUT рвёт незавершённые запросы при остановке.
func envPositiveDuration(key string, def time.Duration) (time.Duration, error) {
	d, err := envDuration(key, def)
	if err != nil {
		return d, err
	}
	if d <= 0 {
		return d, fmt.Errorf("%s (%s) не больше нуля", key, d)
	}
	return d, nil
}

// envNonNegativeDuration читает длительность, у которой ноль задуман и описан в
// таблице переменных (zero — что он означает), а отрицательное значение —
// промах знаком, который иначе молча работал бы как ноль.
func envNonNegativeDuration(key string, def time.Duration, zero string) (time.Duration, error) {
	d, err := envDuration(key, def)
	if err != nil {
		return d, err
	}
	if d < 0 {
		return d, fmt.Errorf("%s (%s) меньше нуля; 0 — %s", key, d, zero)
	}
	return d, nil
}

var  _ = 1

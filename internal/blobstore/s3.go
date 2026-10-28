package blobstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// S3 — объектное хранилище, совместимое с S3 (AWS, MinIO, R2). Нужно там,
// где у API несколько реплик: общий том между ними держать негде, а
// содержимое, вынесенное одной репликой, читает другая.
//
// Подпись считается здесь же (SigV4 — это пара хэшей), клиентской библиотеки
// ради двух запросов, PUT и GET, в зависимостях не заводим.
type S3 struct {
	cfg    S3Config
	client *http.Client
	// stream — тот же клиент без общего срока на обмен: им идут запись и
	// чтение содержимого, у которых срок зависит от размера и не угадывается.
	// Зависший обмен обрывает idle — сколько поток может стоять без байта.
	stream *http.Client
	idle   time.Duration
}

// streamIdle — сколько поток содержимого может стоять без единого байта,
// прежде чем обмен с хранилищем оборвётся. Общего срока у потока нет: его
// длительность задаёт размер содержимого, а у отдачи клиенту — свой срок
// маршрута (SDLC_DOWNLOAD_TIMEOUT).
const streamIdle = 2 * time.Minute

// S3Config — адрес бакета и ключи доступа. Ключи приходят из окружения и в
// базе не оседают: в проекте хранятся только имена и ссылки.
type S3Config struct {
	// Endpoint — базовый адрес сервиса, например https://s3.eu-central-1.amazonaws.com
	// или http://minio:9000 для локальной установки.
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	// PathStyle — адресация вида endpoint/bucket/key. У MinIO иначе никак,
	// у AWS работает и виртуальный хост bucket.endpoint.
	PathStyle bool
	// HTTP подменяется в тестах.
	HTTP *http.Client
}

const (
	s3Scheme    = "s3://"
	s3Service   = "s3"
	s3Algorithm = "AWS4-HMAC-SHA256"
)

func NewS3(cfg S3Config) (*S3, error) {
	switch {
	case cfg.Bucket == "":
		return nil, errors.New("blobstore: бакет не задан")
	case cfg.Endpoint == "":
		return nil, errors.New("blobstore: адрес хранилища не задан")
	case cfg.Region == "":
		return nil, errors.New("blobstore: регион не задан")
	case cfg.AccessKey == "" || cfg.SecretKey == "":
		return nil, errors.New("blobstore: ключи доступа не заданы")
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("blobstore: адрес %s: %w", cfg.Endpoint, err)
	}
	cfg.Endpoint = strings.TrimSuffix(cfg.Endpoint, "/")
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	client := cfg.HTTP
	if client == nil {
		// Крупное содержимое передаётся целиком, поэтому запас по времени
		// больше обычного; отмена всё равно приходит через контекст.
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	stream := *client
	stream.Timeout = 0
	return &S3{cfg: cfg, client: client, stream: &stream, idle: streamIdle}, nil
}

// key раскладывает по первым двум символам sha — так же, как файловое
// хранилище: листинг по префиксу остаётся обозримым.
func (s *S3) key(sha string) string {
	key := sha[:2] + "/" + sha
	if s.cfg.Prefix != "" {
		key = s.cfg.Prefix + "/" + key
	}
	return key
}

func (s *S3) String() string {
	return s3Scheme + s.cfg.Bucket + "/" + s.cfg.Prefix + " (" + s.cfg.Endpoint + ")"
}

// TempDir — спул идёт во временный каталог процесса (TMPDIR): общего тома с
// хранилищем нет, и переименовывать некуда.
func (s *S3) TempDir() string { return "" }

// PutFile заливает файл одним PUT. Подпись берёт sha256 содержимого, уже
// посчитанный при спуле: SigV4 подписывает именно его, и второй проход по
// файлу ради хэша не нужен.
func (s *S3) PutFile(ctx context.Context, sha, path string, size int64, mediaType string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	return s.put(ctx, sha, file, size, sha, mediaType)
}

// Put — запись содержимого, уже лежащего в памяти.
func (s *S3) Put(ctx context.Context, sha string, content []byte, mediaType string) (string, error) {
	sum := sha256.Sum256(content)
	return s.put(ctx, sha, bytes.NewReader(content), int64(len(content)), hex.EncodeToString(sum[:]), mediaType)
}

// put кладёт size байт из body. Тело читается через ReaderAt, поэтому
// транспорт может повторить запрос с начала (GetBody) — память на это не
// тратится.
func (s *S3) put(ctx context.Context, sha string, body io.ReaderAt, size int64, payload, mediaType string) (string, error) {
	if len(sha) < 3 {
		return "", errors.New("blobstore: некорректный sha")
	}
	uri := s3Scheme + s.cfg.Bucket + "/" + s.key(sha)
	// Адресация по содержимому: если объект уже лежит, заливать те же байты
	// второй раз незачем.
	switch exists, err := s.head(ctx, s.key(sha)); {
	case err != nil:
		return "", err
	case exists:
		return uri, nil
	}
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	ctx, watch := newStall(ctx, s.idle)
	defer watch.stop()
	open := func() io.Reader { return stallReader{r: io.NewSectionReader(body, 0, size), s: watch} }
	req, err := s.newRequest(ctx, http.MethodPut, s.key(sha), nil, open())
	if err != nil {
		return "", err
	}
	req.ContentLength = size
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(open()), nil }
	req.Header.Set("Content-Type", mediaType)
	s.sign(req, payload, time.Now().UTC())
	resp, err := s.doWith(s.stream, req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", s3Error("запись", resp)
	}
	return uri, nil
}

// Open отдаёт тело ответа хранилища как есть: содержимое идёт клиенту
// потоком. Ответ 404 разбирается здесь, до первого Read.
func (s *S3) Open(ctx context.Context, uri string) (io.ReadCloser, error) {
	key, err := s.keyOf(uri)
	if err != nil {
		return nil, err
	}
	ctx, watch := newStall(ctx, s.idle)
	req, err := s.request(ctx, http.MethodGet, key, nil)
	if err != nil {
		watch.stop()
		return nil, err
	}
	resp, err := s.doWith(s.stream, req)
	if err != nil {
		watch.stop()
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		watch.touch()
		return &stallBody{body: resp.Body, s: watch}, nil
	case http.StatusNotFound:
		resp.Body.Close()
		watch.stop()
		return nil, ErrNotFound
	default:
		defer watch.stop()
		defer resp.Body.Close()
		return nil, s3Error("чтение", resp)
	}
}

// Get читает содержимое целиком.
func (s *S3) Get(ctx context.Context, uri string) ([]byte, error) {
	return readAll(s.Open(ctx, uri))
}

// Delete уносит объект. И 204, и 404 — успех: сборщик мусора приходит сюда
// после удаления строки в blobs и может прийти второй раз после падения
// между этими шагами.
func (s *S3) Delete(ctx context.Context, uri string) error {
	key, err := s.keyOf(uri)
	if err != nil {
		return err
	}
	req, err := s.request(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	resp, err := s.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil
	default:
		return s3Error("удаление", resp)
	}
}

// keyOf разбирает адрес s3://bucket/key. Чужой бакет не читаем: адрес мог
// остаться от прежней установки, и молча отдать пустоту хуже, чем отказать.
func (s *S3) keyOf(uri string) (string, error) {
	rest, ok := strings.CutPrefix(uri, s3Scheme)
	if !ok {
		return "", fmt.Errorf("blobstore: адрес %q не из объектного хранилища", uri)
	}
	bucket, key, ok := strings.Cut(rest, "/")
	if !ok || key == "" {
		return "", fmt.Errorf("blobstore: адрес %q без ключа", uri)
	}
	if bucket != s.cfg.Bucket {
		return "", fmt.Errorf("blobstore: адрес %q из чужого бакета", uri)
	}
	return key, nil
}

func (s *S3) head(ctx context.Context, key string) (bool, error) {
	req, err := s.request(ctx, http.MethodHead, key, nil)
	if err != nil {
		return false, err
	}
	resp, err := s.do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, s3Error("проверка", resp)
	}
}

func (s *S3) do(req *http.Request) (*http.Response, error) {
	return s.doWith(s.client, req)
}

func (s *S3) doWith(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blobstore: %s %s: %w", req.Method, req.URL.Path, err)
	}
	return resp, nil
}

func s3Error(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
	return fmt.Errorf("blobstore: %s: %s: %s", what, resp.Status, strings.TrimSpace(string(body)))
}

// request собирает запрос и подписывает его по SigV4.
func (s *S3) request(ctx context.Context, method, key string, body []byte) (*http.Request, error) {
	return s.requestQuery(ctx, method, key, nil, body)
}

// requestQuery — request с параметрами строки запроса (листинг, multipart).
// Строка собирается сразу в каноническом виде SigV4: ключи по порядку,
// кодирование RFC 3986 — и она же уходит по сети, так что подписано ровно
// то, что отправлено.
func (s *S3) requestQuery(ctx context.Context, method, key string, query url.Values, body []byte) (*http.Request, error) {
	req, err := s.newRequest(ctx, method, key, query, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	sum := sha256.Sum256(body)
	s.sign(req, hex.EncodeToString(sum[:]), time.Now().UTC())
	return req, nil
}

// newRequest собирает неподписанный запрос: подпись зависит от хэша тела,
// а его вызывающий знает по-разному — из байтов или из спула.
func (s *S3) newRequest(ctx context.Context, method, key string, query url.Values, body io.Reader) (*http.Request, error) {
	endpoint, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	path := "/" + key
	if s.cfg.PathStyle {
		path = "/" + s.cfg.Bucket + path
	} else {
		endpoint.Host = s.cfg.Bucket + "." + endpoint.Host
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + path
	endpoint.RawQuery = canonicalQuery(query)
	return http.NewRequestWithContext(ctx, method, endpoint.String(), body)
}

// sign — SigV4 с известным хэшем полезной нагрузки (payload — hex sha256
// тела). Потоковая подпись по кускам не нужна: тело либо в памяти, либо в
// спуле, и хэш его посчитан до запроса.
func (s *S3) sign(req *http.Request, payload string, now time.Time) {
	stamp := now.Format("20060102T150405Z")
	day := stamp[:8]

	req.Header.Set("Host", req.URL.Host)
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	if s.cfg.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.cfg.SessionToken)
	}

	signed, canonicalHeaders := headersToSign(req)
	canonical := strings.Join([]string{
		req.Method,
		canonicalPath(req.URL.Path),
		req.URL.RawQuery,
		canonicalHeaders,
		signed,
		payload,
	}, "\n")

	scope := day + "/" + s.cfg.Region + "/" + s3Service + "/aws4_request"
	hash := sha256.Sum256([]byte(canonical))
	toSign := strings.Join([]string{s3Algorithm, stamp, scope, hex.EncodeToString(hash[:])}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.cfg.SecretKey), day)
	key = hmacSHA256(key, s.cfg.Region)
	key = hmacSHA256(key, s3Service)
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, toSign))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s3Algorithm, s.cfg.AccessKey, scope, signed, signature))
}

// headersToSign подписывает host и все x-amz-*: остальные заголовки по пути
// может добавить прокси, и подпись развалится не там, где ошиблись.
func headersToSign(req *http.Request) (signed, canonical string) {
	names := []string{"host"}
	values := map[string]string{"host": req.URL.Host}
	for name, v := range req.Header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		names = append(names, lower)
		values[lower] = strings.TrimSpace(v[0])
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name + ":" + values[name] + "\n")
	}
	return strings.Join(names, ";"), b.String()
}

// canonicalQuery — параметры в каноническом виде SigV4: ключи по порядку,
// пробел — %20, а не «+», у параметра без значения — «=».
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		values := append([]string(nil), q[k]...)
		sort.Strings(values)
		for _, v := range values {
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}

func uriEncode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// canonicalPath кодирует каждый сегмент отдельно: слэши остаются слэшами.
func canonicalPath(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
	}
	return strings.Join(segments, "/")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

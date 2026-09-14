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
}

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
	return &S3{cfg: cfg, client: client}, nil
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

func (s *S3) Put(ctx context.Context, sha string, content []byte, mediaType string) (string, error) {
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
	req, err := s.request(ctx, http.MethodPut, s.key(sha), content)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mediaType)
	resp, err := s.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", s3Error("запись", resp)
	}
	return uri, nil
}

func (s *S3) Get(ctx context.Context, uri string) ([]byte, error) {
	key, err := s.keyOf(uri)
	if err != nil {
		return nil, err
	}
	req, err := s.request(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, s3Error("чтение", resp)
	}
	return io.ReadAll(resp.Body)
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
	resp, err := s.client.Do(req)
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

	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.ContentLength = int64(len(body))
	s.sign(req, body, time.Now().UTC())
	return req, nil
}

// sign — SigV4 с полезной нагрузкой в памяти: содержимое и так целиком в
// байтах, потоковую подпись городить не для чего.
func (s *S3) sign(req *http.Request, body []byte, now time.Time) {
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
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

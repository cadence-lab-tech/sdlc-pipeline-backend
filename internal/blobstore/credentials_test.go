package blobstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/blobstore/s3test"
)

// Проверки источников ключей CLAB-394: кэш и обновление, Pod Identity и IRSA
// на фейках s3test, выбор источника по окружению. Номера AC — из
// analysis.acceptance_criteria задачи.

const credRegion = "eu-central-1"

var (
	keyA = s3test.Key{AccessKeyID: "ASIAKEYA", SecretAccessKey: "secret-a/+", SessionToken: "token-a"}
	keyB = s3test.Key{AccessKeyID: "ASIAKEYB", SecretAccessKey: "secret-b/+", SessionToken: "token-b"}
)

// clock — управляемые часы теста.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func logBuffer() (*slog.Logger, *syncBuffer) {
	buf := &syncBuffer{}
	return slog.New(slog.NewJSONHandler(buf, nil)), buf
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// bucketS3 — фейк-бакет с проверкой подписи и S3 с источником ключей creds;
// seen копит Credential и X-Amz-Security-Token каждого запроса.
func bucketS3(t *testing.T, creds CredentialsProvider) (*S3, *s3test.Fake, *[]string) {
	t.Helper()
	fake := s3test.New("blobs", credRegion, testSecret)
	var mu sync.Mutex
	seen := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, cred, _ := strings.Cut(r.Header.Get("Authorization"), "Credential=")
		cred, _, _ = strings.Cut(cred, "/")
		mu.Lock()
		*seen = append(*seen, cred+" "+r.Header.Get("X-Amz-Security-Token"))
		mu.Unlock()
		fake.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	s3, err := NewS3(S3Config{Endpoint: server.URL, Region: credRegion, Bucket: "blobs",
		PathStyle: true, HTTP: server.Client(), Credentials: creds})
	if err != nil {
		t.Fatal(err)
	}
	return s3, fake, seen
}

// allOps гонит через хранилище все операции, которыми пользуется сервис и
// офф-сайт: PUT (с HEAD перед ним), GET, DELETE, листинг, multipart.
func allOps(t *testing.T, s3 *S3) {
	t.Helper()
	ctx := context.Background()
	content := []byte("вложение")
	sum := sha256.Sum256(content)
	uri, err := s3.Put(ctx, hex.EncodeToString(sum[:]), content, "text/plain")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if got, err := s3.Get(ctx, uri); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("get: %q %v", got, err)
	}
	big := make([]byte, MinPartSize+123)
	rand.Read(big)
	if err := s3.PutObject(ctx, "dumps/x.sql.gz", bytes.NewReader(big), int64(len(big)), MinPartSize); err != nil {
		t.Fatalf("multipart: %v", err)
	}
	var out bytes.Buffer
	if err := s3.GetObject(ctx, "dumps/x.sql.gz", &out); err != nil || !bytes.Equal(out.Bytes(), big) {
		t.Fatalf("get object: %v", err)
	}
	if objs, err := s3.ListObjects(ctx, "dumps/"); err != nil || len(objs) != 1 {
		t.Fatalf("list: %v %v", objs, err)
	}
	if err := s3.DeleteObject(ctx, "dumps/x.sql.gz"); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	if err := s3.Delete(ctx, uri); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// AC-1: при статических ключах фейки агента и STS не получают ни одного
// запроса, даже если переменные Pod Identity и IRSA тоже выставлены.
func TestStaticKeysNeverCallMetadata(t *testing.T) {
	agent := &s3test.Container{TokenFile: tokenFile(t, "pod-token")}
	sts := &s3test.STS{RoleArn: "arn:aws:iam::111122223333:role/x", TokenFile: tokenFile(t, "jwt")}
	agentSrv, stsSrv := httptest.NewServer(agent), httptest.NewServer(sts)
	defer agentSrv.Close()
	defer stsSrv.Close()
	creds, source, err := EnvCredentials(S3Config{Region: credRegion, AccessKey: "AKIAEXAMPLE", SecretKey: testSecret,
		STSEndpoint: stsSrv.URL}, envOf(map[string]string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     agentSrv.URL,
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": agent.TokenFile,
		"AWS_WEB_IDENTITY_TOKEN_FILE":            sts.TokenFile,
		"AWS_ROLE_ARN":                           sts.RoleArn,
	}), nil)
	if err != nil || source != SourceStatic {
		t.Fatalf("источник %q, %v", source, err)
	}
	s3, _, seen := bucketS3(t, creds)
	allOps(t, s3)
	if agent.Calls() != 0 || sts.Calls() != 0 {
		t.Fatalf("обращения к агенту %d, к STS %d — ожидали ноль", agent.Calls(), sts.Calls())
	}
	if (*seen)[0] != "AKIAEXAMPLE " {
		t.Fatalf("запрос ушёл не со статическим ключом: %q", (*seen)[0])
	}
}

// AC-2: сто подписей при пустом кэше — одно обращение к источнику.
func TestCredentialsSingleFlight(t *testing.T) {
	var calls atomic.Int32
	r := newRefreshing("test", func(context.Context) (Credentials, error) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		return Credentials{AccessKeyID: "A", SecretAccessKey: "s", Expires: time.Now().Add(time.Hour)}, nil
	}, nil)
	var wg sync.WaitGroup
	var failed atomic.Int32
	for range 100 {
		wg.Go(func() {
			if c, err := r.Retrieve(context.Background()); err != nil || c.AccessKeyID != "A" {
				failed.Add(1)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 || failed.Load() != 0 {
		t.Fatalf("обращений %d, неудач %d", calls.Load(), failed.Load())
	}
}

// AC-2, AC-9: обновление за refreshWindow до срока; сбой при живых ключах —
// работа на кэше и строка в логе; истёкшие ключи не отдаются никогда;
// источник вернулся — следующий вызов работает.
func TestCredentialsRefreshAndStale(t *testing.T) {
	c := &clock{t: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)}
	var calls atomic.Int32
	var broken atomic.Bool
	log, buf := logBuffer()
	r := newRefreshing("test", func(context.Context) (Credentials, error) {
		n := calls.Add(1)
		if broken.Load() {
			return Credentials{}, errors.New("agent down")
		}
		return Credentials{AccessKeyID: string(rune('A' + n - 1)), SecretAccessKey: "s", Expires: c.now().Add(time.Hour)}, nil
	}, log)
	r.now = c.now
	ctx := context.Background()
	get := func() (Credentials, error) { return r.Retrieve(ctx) }

	if k, _ := get(); k.AccessKeyID != "A" {
		t.Fatalf("первый вызов: %q", k.AccessKeyID)
	}
	c.add(time.Hour - refreshWindow - time.Second)
	if k, _ := get(); k.AccessKeyID != "A" || calls.Load() != 1 {
		t.Fatalf("до окна обновления: %q, обращений %d", k.AccessKeyID, calls.Load())
	}
	c.add(2 * time.Second)
	if k, _ := get(); k.AccessKeyID != "B" || calls.Load() != 2 {
		t.Fatalf("в окне обновления: %q, обращений %d", k.AccessKeyID, calls.Load())
	}

	// Сбой в окне обновления: ключи B ещё действуют.
	c.add(time.Hour - refreshWindow + time.Second)
	broken.Store(true)
	if k, err := get(); err != nil || k.AccessKeyID != "B" {
		t.Fatalf("сбой при живых ключах: %q %v", k.AccessKeyID, err)
	}
	if !strings.Contains(buf.String(), "agent down") {
		t.Fatalf("сбой обновления не попал в лог: %s", buf.String())
	}
	// Пауза после сбоя: следующий вызов в неё источник не трогает.
	before := calls.Load()
	if _, err := get(); err != nil || calls.Load() != before {
		t.Fatalf("в паузе после сбоя: обращений %d → %d, %v", before, calls.Load(), err)
	}
	// Ключи истекли, источник лежит — отказ, а не истёкшие ключи.
	c.add(refreshWindow)
	if k, err := get(); err == nil || !strings.Contains(err.Error(), "agent down") {
		t.Fatalf("после срока: %q %v", k.AccessKeyID, err)
	}
	// Источник вернулся — без пересоздания.
	broken.Store(false)
	if k, err := get(); err != nil || k.AccessKeyID == "B" {
		t.Fatalf("после возврата источника: %q %v", k.AccessKeyID, err)
	}
}

type sequence struct {
	mu   sync.Mutex
	keys []s3test.Key
}

func (s *sequence) Retrieve(context.Context) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.keys[0]
	if len(s.keys) > 1 {
		s.keys = s.keys[1:]
	}
	return Credentials{AccessKeyID: k.AccessKeyID, SecretAccessKey: k.SecretAccessKey, SessionToken: k.SessionToken}, nil
}

// AC-3: подпись берёт ключи на каждый запрос, а не при NewS3.
func TestSignTakesKeysPerRequest(t *testing.T) {
	s3, fake, seen := bucketS3(t, &sequence{keys: []s3test.Key{keyA, keyB}})
	fake.Allow(keyA, keyB)
	ctx := context.Background()
	if err := s3.DeleteObject(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s3.DeleteObject(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	want := []string{"ASIAKEYA token-a", "ASIAKEYB token-b"}
	if strings.Join(*seen, ",") != strings.Join(want, ",") {
		t.Fatalf("запросы ушли с %v, ожидали %v", *seen, want)
	}
	// Бакет сверяет токен с ключом: ключ A с токеном B — отказ подписи.
	mixed := keyA
	mixed.SessionToken = keyB.SessionToken
	s3, fake, _ = bucketS3(t, &sequence{keys: []s3test.Key{mixed}})
	fake.Allow(keyA)
	if err := s3.DeleteObject(ctx, "a"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("чужой токен сессии принят: %v", err)
	}
}

// AC-4: Pod Identity — Authorization из файла на момент запроса, токен
// перечитывается между обновлениями, все операции идут с выданными ключами.
func TestPodIdentity(t *testing.T) {
	c := &clock{t: time.Now()}
	a, b := keyA, keyB
	a.Expiration, b.Expiration = c.now().Add(time.Hour), c.now().Add(2*time.Hour)
	agent := &s3test.Container{TokenFile: tokenFile(t, "pod-token-1\n")}
	agent.Keys = []s3test.Key{a, b}
	srv := httptest.NewServer(agent)
	defer srv.Close()
	creds, source, err := EnvCredentials(S3Config{Region: credRegion}, envOf(map[string]string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI":     srv.URL + "/v1/credentials",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": agent.TokenFile,
		"AWS_WEB_IDENTITY_TOKEN_FILE":            "/nonexistent", // IRSA тоже выставлен — выигрывает Pod Identity
		"AWS_ROLE_ARN":                           "arn:aws:iam::111122223333:role/x",
	}), nil)
	if err != nil || source != SourcePodIdentity {
		t.Fatalf("источник %q, %v", source, err)
	}
	creds.(*refreshing).now = c.now
	s3, fake, seen := bucketS3(t, creds)
	fake.Allow(keyA, keyB)
	allOps(t, s3)
	for _, s := range *seen {
		if s != "ASIAKEYA token-a" {
			t.Fatalf("запрос ушёл с %q", s)
		}
	}
	// kubelet ротировал токен, ключи подошли к сроку.
	if err := os.WriteFile(agent.TokenFile, []byte("pod-token-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.add(time.Hour - refreshWindow + time.Second)
	if err := s3.DeleteObject(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if last := (*seen)[len(*seen)-1]; last != "ASIAKEYB token-b" {
		t.Fatalf("после обновления запрос ушёл с %q", last)
	}
	if strings.Join(agent.Tokens, ",") != "pod-token-1,pod-token-2" || agent.Calls() != 2 {
		t.Fatalf("токены агента %v, обращений %d", agent.Tokens, agent.Calls())
	}
}

// AC-5: IRSA — неподписанный AssumeRoleWithWebIdentity, имя сессии из
// AWS_ROLE_SESSION_NAME или непустое по умолчанию, адрес STS из настройки,
// токен перечитывается.
func TestIRSA(t *testing.T) {
	for _, session := range []string{"cadence-api", ""} {
		c := &clock{t: time.Now()}
		a, b := keyA, keyB
		a.Expiration, b.Expiration = c.now().Add(time.Hour), c.now().Add(2*time.Hour)
		sts := &s3test.STS{RoleArn: "arn:aws:iam::111122223333:role/cadence", SessionName: session,
			TokenFile: tokenFile(t, "jwt-1")}
		sts.Keys = []s3test.Key{a, b}
		srv := httptest.NewServer(sts)
		creds, source, err := EnvCredentials(S3Config{Region: credRegion, STSEndpoint: srv.URL}, envOf(map[string]string{
			"AWS_WEB_IDENTITY_TOKEN_FILE": sts.TokenFile,
			"AWS_ROLE_ARN":                sts.RoleArn,
			"AWS_ROLE_SESSION_NAME":       session,
		}), nil)
		if err != nil || source != SourceIRSA {
			t.Fatalf("источник %q, %v", source, err)
		}
		creds.(*refreshing).now = c.now
		s3, fake, seen := bucketS3(t, creds)
		fake.Allow(keyA, keyB)
		allOps(t, s3)
		os.WriteFile(sts.TokenFile, []byte("jwt-2"), 0o600)
		c.add(time.Hour - refreshWindow + time.Second)
		if err := s3.DeleteObject(context.Background(), "x"); err != nil {
			t.Fatal(err)
		}
		if last := (*seen)[len(*seen)-1]; last != "ASIAKEYB token-b" || (*seen)[0] != "ASIAKEYA token-a" {
			t.Fatalf("сессия %q: запросы %v", session, *seen)
		}
		if strings.Join(sts.Tokens, ",") != "jwt-1,jwt-2" || sts.Sessions[0] == "" {
			t.Fatalf("сессия %q: токены %v, имена %v", session, sts.Tokens, sts.Sessions)
		}
		srv.Close()
	}
	if got := stsEndpoint("eu-central-1"); got != "https://sts.eu-central-1.amazonaws.com" {
		t.Fatalf("адрес STS по региону: %s", got)
	}
	if got := stsEndpoint("cn-north-1"); got != "https://sts.cn-north-1.amazonaws.com.cn" {
		t.Fatalf("адрес STS Китая: %s", got)
	}
}

// AC-5, AC-8: отказ STS — код и текст STS в ошибке, без токена.
func TestIRSARejected(t *testing.T) {
	sts := &s3test.STS{RoleArn: "arn:aws:iam::111122223333:role/other", TokenFile: tokenFile(t, "jwt-secret-value")}
	srv := httptest.NewServer(sts)
	defer srv.Close()
	w := newWebIdentity(srv.URL, "arn:aws:iam::111122223333:role/cadence", "", sts.TokenFile)
	_, err := w.fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "InvalidParameterValue") || !strings.Contains(err.Error(), "400") ||
		!strings.Contains(err.Error(), "irsa") || strings.Contains(err.Error(), "jwt-secret-value") {
		t.Fatalf("отказ STS: %v", err)
	}
}

// AC-6: порядок «статические → Pod Identity → IRSA», отказы на половине пары
// и без источника.
func TestEnvCredentialsSelection(t *testing.T) {
	pod := map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://169.254.170.23/v1/credentials",
		"AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token"}
	irsa := map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": "/var/run/secrets/eks.amazonaws.com/serviceaccount/token",
		"AWS_ROLE_ARN": "arn:aws:iam::111122223333:role/x"}
	both := map[string]string{}
	for k, v := range pod {
		both[k] = v
	}
	for k, v := range irsa {
		both[k] = v
	}
	static := S3Config{AccessKey: "AKIA", SecretKey: "s", Region: credRegion}
	for name, tc := range map[string]struct {
		cfg     S3Config
		env     map[string]string
		source  string
		errHave []string
	}{
		"статические":                 {static, nil, SourceStatic, nil},
		"статические и всё прочее":    {static, both, SourceStatic, nil},
		"pod identity":                {S3Config{Region: credRegion}, pod, SourcePodIdentity, nil},
		"pod identity и irsa":         {S3Config{Region: credRegion}, both, SourcePodIdentity, nil},
		"irsa":                        {S3Config{Region: credRegion}, irsa, SourceIRSA, nil},
		"только id":                   {S3Config{AccessKey: "AKIA"}, both, "", []string{"SDLC_BLOB_S3_SECRET_ACCESS_KEY"}},
		"только secret":               {S3Config{SecretKey: "s"}, both, "", []string{"SDLC_BLOB_S3_ACCESS_KEY_ID"}},
		"токен сессии без пары":       {S3Config{SessionToken: "t"}, both, "", []string{"SDLC_BLOB_S3_SESSION_TOKEN"}},
		"irsa без роли":               {S3Config{}, map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": "/t"}, "", []string{"AWS_ROLE_ARN"}},
		"irsa без токена":             {S3Config{}, map[string]string{"AWS_ROLE_ARN": "arn"}, "", []string{"AWS_WEB_IDENTITY_TOKEN_FILE"}},
		"pod identity по чужому http": {S3Config{}, map[string]string{"AWS_CONTAINER_CREDENTIALS_FULL_URI": "http://10.0.0.5/creds"}, "", []string{"https"}},
		"ничего": {S3Config{}, nil, "", []string{"SDLC_BLOB_S3_ACCESS_KEY_ID", "SDLC_BLOB_S3_SECRET_ACCESS_KEY",
			"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN"}},
	} {
		_, source, err := EnvCredentials(tc.cfg, envOf(tc.env), nil)
		if source != tc.source || (err != nil) != (tc.errHave != nil) {
			t.Errorf("%s: источник %q, ошибка %v", name, source, err)
			continue
		}
		for _, s := range tc.errHave {
			if !strings.Contains(err.Error(), s) {
				t.Errorf("%s: в ошибке нет %s: %v", name, s, err)
			}
		}
	}
}

// AC-6: без бакета источник не выбирается и окружение не читается.
func TestOpenWithoutBucketSkipsCredentials(t *testing.T) {
	t.Setenv("AWS_ROLE_ARN", "arn") // половина IRSA уронила бы выбор источника
	store, err := Open(context.Background(), S3Config{}, "", nil)
	if err != nil || store != nil {
		t.Fatalf("без бакета и каталога: %v %v", store, err)
	}
}

// AC-7: одна строка старта с именем источника рядом с store, без ключей,
// секретов, токенов и содержимого файла токена.
func TestOpenLogsSource(t *testing.T) {
	a := keyA
	a.Expiration = time.Now().Add(time.Hour)
	agent := &s3test.Container{TokenFile: tokenFile(t, "pod-token-secret")}
	agent.Keys = []s3test.Key{a}
	srv := httptest.NewServer(agent)
	defer srv.Close()
	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", srv.URL)
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", agent.TokenFile)
	for source, cfg := range map[string]S3Config{
		SourceStatic:      {AccessKey: "AKIASTATICKEY", SecretKey: "static-secret", SessionToken: "static-token"},
		SourcePodIdentity: {},
	} {
		cfg.Endpoint, cfg.Region, cfg.Bucket = "http://s3.test", credRegion, "blobs"
		log, buf := logBuffer()
		if _, err := Open(context.Background(), cfg, "", log); err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		out := buf.String()
		if strings.Count(out, `"credentials":"`+source+`"`) != 1 || !strings.Contains(out, `"store":"s3://blobs/`) {
			t.Fatalf("%s: строка старта: %s", source, out)
		}
		for _, secret := range []string{"AKIASTATIC", "static-secret", "static-token", "ASIAKEYA", "secret-a", "token-a", "pod-token"} {
			if strings.Contains(out, secret) {
				t.Fatalf("%s: в логе %q: %s", source, secret, out)
			}
		}
	}
}

// Решение человека по CLAB-394: ключи Pod Identity и IRSA на старте ждутся
// с повтором; агент поднялся позже — старт идёт, не поднялся — отказ.
func TestAwaitCredentials(t *testing.T) {
	var calls atomic.Int32
	r := newRefreshing(SourcePodIdentity, func(context.Context) (Credentials, error) {
		if calls.Add(1) < 3 {
			return Credentials{}, errors.New("connection refused")
		}
		return Credentials{AccessKeyID: "A", SecretAccessKey: "s", Expires: time.Now().Add(time.Hour)}, nil
	}, nil)
	log, buf := logBuffer()
	if err := awaitCredentials(context.Background(), r, SourcePodIdentity, time.Second, 10*time.Millisecond, log); err != nil {
		t.Fatalf("агент поднялся на третьей попытке: %v", err)
	}
	if strings.Count(buf.String(), "повторим") != 2 {
		t.Fatalf("попытки в логе: %s", buf.String())
	}
	down := newRefreshing(SourceIRSA, func(context.Context) (Credentials, error) {
		return Credentials{}, errors.New("STS closed")
	}, nil)
	start := time.Now()
	err := awaitCredentials(context.Background(), down, SourceIRSA, 200*time.Millisecond, 20*time.Millisecond, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "irsa") || !strings.Contains(err.Error(), "STS closed") ||
		time.Since(start) > time.Second {
		t.Fatalf("источник не поднялся: %v за %s", err, time.Since(start))
	}
	if credentialsWait != 30*time.Second {
		t.Fatalf("ожидание на старте %s, договорились о 30 с", credentialsWait)
	}
}

// AC-8: зависший агент — ошибка за fetchTimeout, запросы S3 не висят;
// отмена контекста запроса прерывает ожидание сразу.
func TestPodIdentityHangs(t *testing.T) {
	agent := &s3test.Container{TokenFile: tokenFile(t, "pod-token-secret")}
	agent.Hang = true
	agent.Keys = []s3test.Key{keyA}
	srv := httptest.NewServer(agent)
	defer srv.Close()
	// Обновление, начатое отменённым запросом, ещё висит на агенте: рвём его.
	defer srv.CloseClientConnections()
	creds, _, err := EnvCredentials(S3Config{Region: credRegion}, envOf(map[string]string{
		"AWS_CONTAINER_CREDENTIALS_FULL_URI": srv.URL, "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE": agent.TokenFile}), nil)
	if err != nil {
		t.Fatal(err)
	}
	s3, _, _ := bucketS3(t, creds)
	start := time.Now()
	_, err = s3.Put(context.Background(), strings.Repeat("ab", 32), []byte("x"), "")
	if err == nil || time.Since(start) > fetchTimeout+2*time.Second || !strings.Contains(err.Error(), SourcePodIdentity) ||
		!strings.Contains(err.Error(), srv.URL) || strings.Contains(err.Error(), "pod-token-secret") {
		t.Fatalf("HEAD при зависшем агенте: %v за %s", err, time.Since(start))
	}
	if fetchTimeout > 10*time.Second {
		t.Fatalf("таймаут источника %s больше 10 с", fetchTimeout)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	for name, op := range map[string]func() error{
		"get":    func() error { _, err := s3.Get(ctx, "s3://blobs/ab/x"); return err },
		"delete": func() error { return s3.Delete(ctx, "s3://blobs/ab/x") },
		"put":    func() error { return s3.PutObject(ctx, "k", bytes.NewReader([]byte("x")), 1, MinPartSize) },
		"list":   func() error { _, err := s3.ListObjects(ctx, ""); return err },
	} {
		if err := op(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("отмена контекста не прервала ожидание: %s", time.Since(start))
	}
}

// AC-8: понятные ошибки агента — путь файла, HTTP-статус, без токена.
func TestPodIdentityErrors(t *testing.T) {
	good := tokenFile(t, "pod-token-secret")
	for name, tc := range map[string]struct {
		tokenFile string
		status    int
		body      string
		want      []string
	}{
		"нет файла":       {"/nonexistent/token", 0, "", []string{"/nonexistent/token"}},
		"пустой файл":     {tokenFile(t, "\n"), 0, "", []string{"пуст"}},
		"не json":         {good, 200, "<html>", []string{"200 OK", "без AccessKeyId"}},
		"без AccessKeyId": {good, 200, `{"SecretAccessKey":"x"}`, []string{"200 OK", "без AccessKeyId"}},
		"отказ агента":    {good, 403, `{"Message":"no association"}`, []string{"403 Forbidden", "no association"}},
		"длинное тело":    {good, 500, strings.Repeat("x", 4000), []string{"500"}},
	} {
		agent := &s3test.Container{TokenFile: good}
		agent.Status, agent.Body = tc.status, tc.body
		srv := httptest.NewServer(agent)
		p, err := newPodIdentity(srv.URL, tc.tokenFile, "")
		if err != nil {
			t.Fatal(err)
		}
		_, err = p.fetch(context.Background())
		srv.Close()
		if err == nil {
			t.Errorf("%s: ожидали ошибку", name)
			continue
		}
		msg := err.Error()
		for _, w := range append(tc.want, SourcePodIdentity) {
			if !strings.Contains(msg, w) {
				t.Errorf("%s: в ошибке нет %q: %s", name, w, msg)
			}
		}
		if strings.Contains(msg, "pod-token-secret") || len(msg) > errBodyMax+300 {
			t.Errorf("%s: в ошибке токен или длинное тело (%d байт)", name, len(msg))
		}
	}
}

// AC-10: адрес агента — https или http только до узла; запрос к агенту идёт
// мимо прокси из окружения, к STS — как обычный исходящий.
func TestPodIdentityAddressAndProxy(t *testing.T) {
	for uri, ok := range map[string]bool{
		"http://169.254.170.23/v1/credentials": true,
		"http://[fd00:ec2::23]/v1/credentials": true,
		"http://127.0.0.1:8080/creds":          true,
		"http://[::1]/creds":                   true,
		"http://localhost/creds":               true,
		"https://agent.example.com/creds":      true,
		"http://10.0.0.5/creds":                false,
		"http://agent.example.com/creds":       false,
		"ftp://169.254.170.23/":                false,
		"не адрес":                             false,
	} {
		if _, err := newPodIdentity(uri, "", ""); (err == nil) != ok {
			t.Errorf("%s: допустим %v, ошибка %v", uri, ok, err)
		}
	}

	p, err := newPodIdentity("http://169.254.170.23/v1/credentials", tokenFile(t, "pod-token"), "")
	if err != nil {
		t.Fatal(err)
	}
	transport := p.client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("клиент агента берёт прокси из окружения")
	}
	// Запрос к агенту уходит прямо на его адрес: соединение открывается к
	// 169.254.170.23:80, а не к прокси.
	agent := &s3test.Container{TokenFile: tokenFile(t, "pod-token")}
	a := keyA
	a.Expiration = time.Now().Add(time.Hour)
	agent.Keys = []s3test.Key{a}
	srv := httptest.NewServer(agent)
	defer srv.Close()
	var dialed string
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = addr
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	p.tokenFile = agent.TokenFile
	if _, err := p.fetch(context.Background()); err != nil || dialed != "169.254.170.23:80" {
		t.Fatalf("соединение с %q, %v", dialed, err)
	}
	if w := newWebIdentity("https://sts.example", "arn", "", ""); w.client.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("STS должен ходить через прокси окружения, если он задан")
	}
}

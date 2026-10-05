package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Приложение и выбор учётных данных (решение 0028). Проверяется то, ради чего
// это писалось: токен установки выписывается один раз и переиспользуется, а
// частная настройка репозитория побеждает общую настройку проекта.

func testAppKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

func TestInstallationTokenIsCachedAndSigned(t *testing.T) {
	var issued int
	var gotJWT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/42/access_tokens" || r.Method != http.MethodPost {
			t.Errorf("неожиданный запрос %s %s", r.Method, r.URL.Path)
		}
		issued++
		gotJWT = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_installation", "expires_at": time.Now().Add(time.Hour),
		})
	}))
	defer srv.Close()

	app, err := NewApp(srv.URL, "12345", testAppKey(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 3 {
		token, err := app.InstallationToken(ctx, 42)
		if err != nil {
			t.Fatal(err)
		}
		if token != "ghs_installation" {
			t.Fatalf("токен %q", token)
		}
	}
	// Токен живёт час: выписывать его на каждый запрос значило бы тратить
	// поход в сеть перед каждым походом в сеть.
	if issued != 1 {
		t.Fatalf("токен выписан %d раз вместо одного", issued)
	}

	// Утверждение приложения подписано его ключом и выписано от его имени.
	parts := strings.Split(gotJWT, ".")
	if len(parts) != 3 {
		t.Fatalf("утверждение приложения не JWT: %q", gotJWT)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Issuer string `json:"iss"`
		Issued int64  `json:"iat"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != "12345" {
		t.Fatalf("iss %q", claims.Issuer)
	}
	// iat в прошлое: провайдер отбивает утверждение, выписанное «в будущем»,
	// а часы сервиса и провайдера расходятся.
	if claims.Issued > time.Now().Unix() {
		t.Fatal("iat в будущем — GitHub такое утверждение отбивает")
	}
}

func TestProviderPrefersMoreSpecificCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_installation", "expires_at": time.Now().Add(time.Hour),
		})
	}))
	defer srv.Close()

	app, err := NewApp(srv.URL, "12345", testAppKey(t))
	if err != nil {
		t.Fatal(err)
	}
	base := New(srv.URL, "общий-токен-инстанса")
	provider := NewProvider(base, app, func(ref string) string {
		if ref == "acme" {
			return "секрет-акме"
		}
		return ""
	})
	ctx := context.Background()
	installation := int64(42)

	cases := []struct {
		name  string
		creds Credentials
		want  string
	}{
		{"без настроек — общий токен", Credentials{}, "общий-токен-инстанса"},
		{"установка проекта", Credentials{InstallationID: &installation}, "ghs_installation"},
		// Имя секрета задают ради исключения из общего правила, поэтому оно
		// побеждает установку.
		{"именованный секрет", Credentials{InstallationID: &installation, SecretRef: "acme"}, "секрет-акме"},
	}
	for _, tc := range cases {
		client, err := provider.Client(ctx, tc.creds)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if client.Token != tc.want {
			t.Fatalf("%s: токен %q вместо %q", tc.name, client.Token, tc.want)
		}
	}

	// Имя секрета есть, а значения в окружении нет — это ошибка настройки.
	// Молча уйти на общий токен значит дать доступ, которого не хотели.
	if _, err := provider.Client(ctx, Credentials{SecretRef: "нет-такого"}); err == nil {
		t.Fatal("ненайденный секрет не считается ошибкой")
	}
}

func TestResolveFollowsDecision0034(t *testing.T) {
	repoInstallation, projectInstallation := int64(42), int64(7)
	cases := []struct {
		name         string
		refs         CredentialRefs
		source       CredentialSource
		installation *int64
		secret       string
	}{
		{"секрет репозитория побеждает всё", CredentialRefs{
			RepositorySecret: "repo-x", RepositoryInstallation: &repoInstallation,
			ProjectInstallation: &projectInstallation, ProjectSecret: "proj",
		}, SourceRepositorySecret, nil, "repo-x"},
		{"установка репозитория раньше проекта", CredentialRefs{
			RepositoryInstallation: &repoInstallation,
			ProjectInstallation:    &projectInstallation, ProjectSecret: "proj",
		}, SourceRepositoryInstallation, &repoInstallation, ""},
		{"установка проекта раньше секрета проекта", CredentialRefs{
			ProjectInstallation: &projectInstallation, ProjectSecret: "proj",
		}, SourceProjectInstallation, &projectInstallation, ""},
		{"секрет проекта", CredentialRefs{ProjectSecret: "proj"}, SourceProjectSecret, nil, "proj"},
		{"ничего не объявлено — токен инстанса", CredentialRefs{}, SourceInstance, nil, ""},
	}
	for _, tc := range cases {
		creds, source := tc.refs.Resolve()
		if source != tc.source {
			t.Fatalf("%s: источник %q вместо %q", tc.name, source, tc.source)
		}
		if creds.SecretRef != tc.secret {
			t.Fatalf("%s: секрет %q вместо %q", tc.name, creds.SecretRef, tc.secret)
		}
		// Заполнено не больше одного поля: Provider.Client отдаёт секрету
		// приоритет, и лишняя установка рядом с ним была бы враньём о том,
		// чем читают.
		switch {
		case (creds.InstallationID == nil) != (tc.installation == nil):
			t.Fatalf("%s: установка %v вместо %v", tc.name, creds.InstallationID, tc.installation)
		case creds.InstallationID != nil && *creds.InstallationID != *tc.installation:
			t.Fatalf("%s: установка %d вместо %d", tc.name, *creds.InstallationID, *tc.installation)
		}
	}
}

// Значение секрета не попадает в текст отказа провайдера: отказ уходит в
// ответ проверки доступа и в интерфейс.
func TestSecretValueStaysOutOfErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	provider := NewProvider(New(srv.URL, ""), nil, func(string) string { return "ghp_значение-секрета" })
	client, err := provider.Client(context.Background(), Credentials{SecretRef: "repo-x"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Repo(context.Background(), Repo{Owner: "acme", Name: "api"})
	if err == nil {
		t.Fatal("404 не стал ошибкой")
	}
	if strings.Contains(err.Error(), "ghp_значение-секрета") {
		t.Fatalf("значение секрета в тексте ошибки: %v", err)
	}
}

// Часы машины уехали вперёд: GitHub отбивает утверждение, выписанное «в
// будущем», с 401 Bad credentials — на это однажды встала индексация, и
// отличить такой отказ от отозванного ключа было нельзя. Поправка снимается
// с заголовка Date ответа, и утверждение перевыписывается один раз.
func TestInstallationTokenSurvivesClockDrift(t *testing.T) {
	// Провайдер «отстаёт» на три минуты — столько набежало на машине без NTP.
	const drift = 3 * time.Minute
	var attempts int
	var issuedAt []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		now := time.Now().Add(-drift)
		iat := jwtIssuedAt(t, r)
		issuedAt = append(issuedAt, iat)
		w.Header().Set("Date", now.UTC().Format(http.TimeFormat))
		if iat > now.Unix() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_installation", "expires_at": now.Add(time.Hour),
		})
	}))
	defer srv.Close()

	app, err := NewApp(srv.URL, "12345", testAppKey(t))
	if err != nil {
		t.Fatal(err)
	}
	token, err := app.InstallationToken(context.Background(), 42)
	if err != nil {
		t.Fatalf("токен не выписан: %v", err)
	}
	if token != "ghs_installation" {
		t.Fatalf("токен %q", token)
	}
	if attempts != 2 {
		t.Fatalf("походов %d вместо двух: первый отбивается по времени, второй идёт с поправкой", attempts)
	}
	// Первое утверждение выписано «в будущем» по часам провайдера, второе —
	// в прошлом: поправка измерена, а не угадана.
	if issuedAt[0] <= time.Now().Add(-drift).Unix() {
		t.Fatal("первое утверждение не попало в будущее — проверка ничего не проверяет")
	}
	if issuedAt[1] > time.Now().Add(-drift).Unix() {
		t.Fatal("второе утверждение всё ещё в будущем по часам провайдера")
	}

	// Поправка запомнена: следующая выписка идёт сразу верной.
	app.mu.Lock()
	app.tokens = map[int64]installationToken{}
	app.mu.Unlock()
	if _, err := app.InstallationToken(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("походов %d вместо трёх: поправка не запомнена", attempts)
	}
}

// Ключ не принят, а часы при этом разошлись — в отказе должно быть сказано
// про часы, иначе поломку ищут в правах установки.
func TestInstallationTokenErrorNamesClockDrift(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Now().Add(-3*time.Minute).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	app, err := NewApp(srv.URL, "12345", testAppKey(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.InstallationToken(context.Background(), 42)
	if err == nil {
		t.Fatal("отказ не вернулся")
	}
	if !strings.Contains(err.Error(), "часы машины") {
		t.Fatalf("в отказе нет приписки про часы: %v", err)
	}
}

// Отказ в учётных данных приложения (CLAB-78) — класс ошибки, а не текст:
// движок отличает его от недоступности репозитория по метке ErrAppRejected.
// Метка ставится и после попытки с поправкой часов, и без неё; текст
// начинается с описания отказа и хранит статус, ответ провайдера и приписку
// про часы.
func TestInstallationTokenMarksAppRejection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drift time.Duration
		note  bool
	}{
		{name: "часы в порядке", drift: 0},
		{name: "часы разошлись", drift: 3 * time.Minute, note: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.Header().Set("Date", time.Now().Add(-tc.drift).UTC().Format(http.TimeFormat))
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			}))
			defer srv.Close()

			app, err := NewApp(srv.URL, "12345", testAppKey(t))
			if err != nil {
				t.Fatal(err)
			}
			_, err = app.InstallationToken(context.Background(), 42)
			if !errors.Is(err, ErrAppRejected) {
				t.Fatalf("401 на токен установки без метки отказа приложения: %v", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatalf("отказ приложения помечен как «не найдено»: %v", err)
			}
			text := err.Error()
			if !strings.HasPrefix(text, ErrAppRejected.Error()) {
				t.Fatalf("текст начинается не с описания отказа приложения: %q", text)
			}
			for _, want := range []string{"401", "токен установки 42", "Bad credentials"} {
				if !strings.Contains(text, want) {
					t.Fatalf("в отказе нет %q: %q", want, text)
				}
			}
			if got := strings.Contains(text, "часы машины"); got != tc.note {
				t.Fatalf("приписка про часы: %v, ожидали %v: %q", got, tc.note, text)
			}
			if want := 1 + map[bool]int{true: 1}[tc.note]; attempts != want {
				t.Fatalf("походов %d вместо %d", attempts, want)
			}
		})
	}
}

// Прочие отказы на токен установки метку отказа приложения не получают:
// 404 — установки нет у провайдера, 403 и прочее — прежним путём.
func TestInstallationTokenOtherRefusalsAreNotAppRejection(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden,
		http.StatusUnprocessableEntity, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
		}))
		app, err := NewApp(srv.URL, "12345", testAppKey(t))
		if err != nil {
			t.Fatal(err)
		}
		_, err = app.InstallationToken(context.Background(), 42)
		srv.Close()
		if err == nil {
			t.Fatalf("%d: отказ не вернулся", status)
		}
		if errors.Is(err, ErrAppRejected) {
			t.Fatalf("%d помечен как отказ приложения: %v", status, err)
		}
		if got := errors.Is(err, ErrNotFound); got != (status == http.StatusNotFound) {
			t.Fatalf("%d: ErrNotFound = %v: %v", status, got, err)
		}
	}
}

// Отказ на сам репозиторий — не отказ приложения, каким бы ни был статус.
func TestRepoRefusalIsNotAppRejection(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"nope"}`))
		}))
		_, err := New(srv.URL, "ghs_token").Repo(context.Background(), Repo{"acme", "api"})
		srv.Close()
		if err == nil {
			t.Fatalf("%d: отказ не вернулся", status)
		}
		if errors.Is(err, ErrAppRejected) {
			t.Fatalf("%d на репозиторий помечен как отказ приложения: %v", status, err)
		}
	}
}

// jwtIssuedAt — iat утверждения из заголовка запроса.
func jwtIssuedAt(t *testing.T, r *http.Request) int64 {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		t.Fatalf("утверждение приложения не JWT: %q", r.Header.Get("Authorization"))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Issued int64 `json:"iat"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims.Issued
}

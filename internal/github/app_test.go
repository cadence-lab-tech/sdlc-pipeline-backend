package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

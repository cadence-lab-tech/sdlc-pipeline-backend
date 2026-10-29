package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Параметры журнала без тел и секретов (CLAB-210, критерии 12–14): секреты
// на любой глубине — [redacted], content — размер и sha256, строка длиннее
// 4 КБ — тоже, запись целиком не больше 16 КБ.
func TestSanitizeAuditParams(t *testing.T) {
	body := "# Разбор\n\nтело артефакта"
	long := strings.Repeat("инструкция ", 600) // больше 4 КБ
	in := map[string]any{
		"task_key": "SDLC-1",
		"content":  body,
		"path":     map[string]any{"code": "ABCD-1234"},
		"body": map[string]any{
			"token":         "clab_inv_secret",
			"Authorization": "Bearer clab_xxx",
			"nested": []any{
				map[string]any{"client_secret": "s1", "password": "p1", "api_key": "k1", "private_key": "pk", "name": "ok"},
			},
			"roles": []any{map[string]any{"key": "analyst", "instructions": long}},
		},
	}
	raw := SanitizeAuditParams(in)
	text := string(raw)
	for _, secret := range []string{"ABCD-1234", "clab_inv_secret", "clab_xxx", `"s1"`, `"p1"`, `"k1"`, `"pk"`, body, "инструкция инструкция"} {
		if strings.Contains(text, secret) {
			t.Fatalf("в параметрах осталось %q: %s", secret, text)
		}
	}
	var out struct {
		TaskKey string `json:"task_key"`
		Content struct {
			Size   int    `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"content"`
		Body struct {
			Nested []map[string]any `json:"nested"`
			Roles  []struct {
				Instructions struct {
					Size   int    `json:"size"`
					SHA256 string `json:"sha256"`
				} `json:"instructions"`
			} `json:"roles"`
		} `json:"body"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	if out.TaskKey != "SDLC-1" || out.Content.Size != len(body) || out.Content.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("content и обычные поля: %s", text)
	}
	if out.Body.Nested[0]["name"] != "ok" || out.Body.Nested[0]["password"] != auditRedacted {
		t.Fatalf("вложенные секреты: %s", text)
	}
	if out.Body.Roles[0].Instructions.Size != len(long) || out.Body.Roles[0].Instructions.SHA256 == "" {
		t.Fatalf("длинная строка: %s", text)
	}
}

func TestSanitizeAuditParamsLimit(t *testing.T) {
	// Много коротких строк: каждая меньше 4 КБ, вместе — больше 16 КБ.
	items := make([]any, 0, 200)
	for range 200 {
		items = append(items, strings.Repeat("x", 200))
	}
	raw := SanitizeAuditParams(map[string]any{"task_key": "SDLC-1", "items": items})
	if len(raw) > auditMaxParams {
		t.Fatalf("параметры %d байт", len(raw))
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out["_truncated"] != true || out["task_key"] != "SDLC-1" {
		t.Fatalf("усечение без пометки или без простых полей: %s", raw)
	}
	if _, ok := out["items"].(map[string]any)["sha256"]; !ok {
		t.Fatalf("составное поле не заменено размером и sha256: %s", raw)
	}

	if got := string(SanitizeAuditParams(nil)); got != "{}" {
		t.Fatalf("пустые параметры: %s", got)
	}
}

// Учётные данные внутри ссылок (CLAB-226, критерии 1–6, 8): userinfo —
// "[redacted]@", значения секретных параметров запроса и фрагмента —
// "[redacted]"; имена, порядок и прочие значения остаются, а строка, в
// которой вырезать нечего, проходит байт в байт.
func TestSanitizeAuditParamsURLs(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// AC-1, AC-2: userinfo с паролем, только имя, ssh.
		{"userinfo с паролем", "https://alice:ghp_SECRETMARK1@github.com/acme/app.git", "https://[redacted]@github.com/acme/app.git"},
		{"userinfo из имени", "https://ghp_SECRETMARK1@github.com/acme/app.git", "https://[redacted]@github.com/acme/app.git"},
		{"ssh", "ssh://git@host/x.git", "ssh://[redacted]@host/x.git"},
		{"userinfo с портом", "https://u:PWMARK@git.example.com:8443/x.git", "https://[redacted]@git.example.com:8443/x.git"},
		{"@ в пароле", "https://u:p@ss@host/x", "https://[redacted]@host/x"},
		// AC-3: строка запроса.
		{"строка запроса",
			"https://files.example.com/spec.pdf?access_token=TOKMARK&v=2&Token=TOKMARK2&sig=SIGMARK&key=KEYMARK",
			"https://files.example.com/spec.pdf?access_token=[redacted]&v=2&Token=[redacted]&sig=[redacted]&key=[redacted]"},
		{"подписанная ссылка S3",
			"https://bucket.s3.amazonaws.com/o?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAMARK%2F20261029%2Fus-east-1&X-Amz-Date=20261029T000000Z&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Signature=S3SIGMARK2&response-content-type=pdf",
			"https://bucket.s3.amazonaws.com/o?X-Amz-Algorithm=[redacted]&X-Amz-Credential=[redacted]&X-Amz-Date=[redacted]&X-Amz-Expires=[redacted]&X-Amz-SignedHeaders=[redacted]&X-Amz-Signature=[redacted]&response-content-type=pdf"},
		{"подстроки имени",
			"https://x.example.com/u?upload_token=UPMARK&X-Goog-Signature=GOOGMARK&sas_token=SASMARK&page=1",
			"https://x.example.com/u?upload_token=[redacted]&X-Goog-Signature=[redacted]&sas_token=[redacted]&page=1"},
		{"имя в процентной записи", "https://x.example.com/?access%5Ftoken=PCTMARK&a=1", "https://x.example.com/?access%5Ftoken=[redacted]&a=1"},
		// AC-4: фрагмент.
		{"фрагмент", "https://app.example.com/cb#access_token=FRAGMARK&state=x", "https://app.example.com/cb#access_token=[redacted]&state=x"},
		{"запрос и фрагмент",
			"https://u:PWMARK@app.example.com/cb?code=CODEMARK&state=s#id_token=IDMARK",
			"https://[redacted]@app.example.com/cb?code=[redacted]&state=s#id_token=[redacted]"},
		// AC-5: ссылка внутри текста.
		{"ссылка в тексте",
			"клонировать из https://bob:PWMARK@git.example.com/x.git и проверить",
			"клонировать из https://[redacted]@git.example.com/x.git и проверить"},
		{"две ссылки в тексте",
			`см. "https://a:PWMARK@h1/x" и <https://h2/y?token=TOKMARK>`,
			`см. "https://[redacted]@h1/x" и <https://h2/y?token=[redacted]>`},
		// AC-5: ссылки без пробела между ними и ссылка внутри ссылки —
		// вторая лежит в «пути» первой, но чистится так же.
		{"две ссылки через запятую",
			"из https://a:PWMARK@x/r.git,https://c:PWMARK2@y/r.git и всё",
			"из https://[redacted]@x/r.git,https://[redacted]@y/r.git и всё"},
		{"две ссылки через точку с запятой", "https://a:PWMARK@x/r.git;https://c:PWMARK2@y/r.git?token=TOKMARK",
			"https://[redacted]@x/r.git;https://[redacted]@y/r.git?token=[redacted]"},
		{"ссылка в параметре запроса", "https://proxy.example/go?u=https://user:PWMARK@host/x%3Fa&v=1",
			"https://proxy.example/go?u=https://[redacted]@host/x%3Fa&v=1"},
		{"ссылка во фрагменте", "https://app.example.com/#/open/https://user:PWMARK@host/x",
			"https://app.example.com/#/open/https://[redacted]@host/x"},
		// Порт, IPv6, «@» не в authority: хост и порт остаются, лишнего не режем.
		{"IPv6 с userinfo", "https://u:PWMARK@[2001:db8::1]:8443/x.git", "https://[redacted]@[2001:db8::1]:8443/x.git"},
		{"IPv6 без userinfo", "https://[::1]:8080/x?a=1", "https://[::1]:8080/x?a=1"},
		{"процентное кодирование в userinfo", "https://us%40er:p%40PWMARK@host/p", "https://[redacted]@host/p"},
		{"@ в пути и запросе", "https://host/path@notuser/x?email=a@b.c&v=1", "https://host/path@notuser/x?email=a@b.c&v=1"},
		{"@ только во фрагменте", "https://host#f@x", "https://host#f@x"},
		{"порт без пути с секретом", "https://host:8080?token=TOKMARK", "https://host:8080?token=[redacted]"},
		// AC-8: ссылка, которую не берёт url.Parse.
		{"разбор не берёт", "https://u:PARSEMARK@host:99999x/p?token=PARSEMARK2", "https://[redacted]@host:99999x/p?token=[redacted]"},
		{"разбор не берёт: процент", "https://u:PARSEMARK@host/%zz?password=PARSEMARK2&q=%", "https://[redacted]@host/%zz?password=[redacted]&q=%"},
		// AC-6: вырезать нечего — байт в байт.
		{"без userinfo", "https://github.com/acme/app.git", "https://github.com/acme/app.git"},
		{"параметры не пересортированы", "https://example.com/a?b=1&a=2#top", "https://example.com/a?b=1&a=2#top"},
		{"кодирование прежнее", "https://example.com/a%20b?q=a+b&x=%2F&token=", "https://example.com/a%20b?q=a+b&x=%2F&token="},
		{"пустой userinfo", "https://@example.com/", "https://@example.com/"},
		{"без ссылок", "обычный текст, email a@b.c и owner/name", "обычный текст, email a@b.c и owner/name"},
		{"scp-ссылка git не ссылка", "git@github.com:acme/app.git", "git@github.com:acme/app.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := url.Parse(tc.in); strings.HasPrefix(tc.name, "разбор не берёт") && err == nil {
				t.Fatalf("url.Parse берёт %q — пример не про запасной путь", tc.in)
			}
			raw := SanitizeAuditParams(map[string]any{"url": tc.in})
			var out map[string]string
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if out["url"] != tc.want {
				t.Fatalf("\nвход    %s\nвыход   %s\nожидали %s", tc.in, out["url"], tc.want)
			}
		})
	}
}

// Перечень секретных имён параметров ссылки (CLAB-226, требования п. 4):
// ключи auditSecretKeys, точные имена, подстроки и префикс x-amz-, без
// учёта регистра.
func TestSecretURLParamNames(t *testing.T) {
	secret := []string{
		"authorization", "token", "secret", "password", "client_secret", "private_key", "api_key",
		"code", "code_verifier", "id_token", "access_token", "refresh_token",
		"key", "apikey", "sig", "signature", "passwd", "pwd", "auth", "credential", "session",
		"sas_token", "upload_token", "X-Goog-Signature", "client_secret_v2", "db_password", "aws_credentials",
		"X-Amz-Date", "x-amz-anything", "TOKEN", "Sig", "ApiKey",
		// Возврат с merge (CLAB-226, круг 2): имена вне прежнего перечня и
		// их родня — с дефисом, в CamelCase, с приставкой.
		"api-key", "x-api-key", "X-API-KEY", "Api_Key", "access_key", "AccessKeyId", "AWSAccessKeyId",
		"aws_secret_access_key", "secret_key", "jwt", "id_jwt", "pass", "bearer", "bearer_token",
		"private-key", "PHPSESSID", "jsessionid", "session_id", "sid", "otp", "hmac", "authkey", "auth_key",
		"user_passw", "X-Amz-Security-Token", "x_amz_date",
		// Круг 3: слово целиком — где угодно в имени, в любом написании.
		"authToken", "apiSecret", "GITHUB_TOKEN", "private_token", "personal_access_token",
		"SecretAccessKey", "accesstoken", "refreshtoken", "sessionId", "db_pass", "user_pwd",
		"ClientSecret", "clientsecret", "codeVerifier", "max_token", "access%5Ftoken",
	}
	for k := range auditSecretKeys {
		secret = append(secret, k)
	}
	for _, name := range secret {
		if !secretURLParam(name) {
			t.Errorf("%q не считается секретным", name)
		}
	}
	for _, name := range []string{
		"v", "page", "state", "ref", "q", "response-content-type", "keyword", "monkey", "author",
		"passport", "bypass", "compass", "sidebar", "session_count", "signed_in", "keyboard", "keys",
		"accessibility", "author_id", "design", "codec", "zip_code", "redirect_uri", "next", "email",
		// Круг 3 (решение человека): подстрока — не слово.
		"tokens", "max_tokens", "maxTokens", "tokenizer", "passenger", "passage",
		"authority", "authenticated", "authors", "signal", "signup", "sidekiq", "ssid", "kid", "turkey",
		"hashkey", "sortkey", "primary_key", "status_code", "key_id", "sig_alg", "hmac_alg",
		"auth_type", "session_timeout", "secretary", "keypass_hint",
	} {
		if secretURLParam(name) {
			t.Errorf("%q считается секретным", name)
		}
	}
}

// Быстрый отсев в secretURLParam не теряет слов перечня.
func TestSecretWordHints(t *testing.T) {
	for w := range auditSecretParamWords {
		if !containsAny(w, auditSecretWordHints) {
			t.Errorf("слово %q не содержит ни одного куска auditSecretWordHints", w)
		}
	}
}

// Ссылки на любой глубине параметров (критерий 5): вложенные объекты и
// элементы массива, как в импорте связки; ключи-секреты, тела и длинные
// строки — по-прежнему (критерий 7).
func TestSanitizeAuditParamsURLsNested(t *testing.T) {
	long := "https://u:LONGMARK@h/x " + strings.Repeat("ы", 3000) // больше 4 КБ
	in := map[string]any{
		"bundle": map[string]any{
			"repositories": []any{map[string]any{"name": "acme/app", "clone_url": "https://alice:ghp_SECRETMARK1@github.com/acme/app.git"}},
			"documents":    []any{map[string]any{"url": "https://files.example.com/spec.pdf?X-Amz-Signature=S3SIGMARK2&v=2"}},
		},
		"links":       []any{"https://bob:PWMARK@git.example.com/x.git"},
		"description": long,
		"token":       "https://plain.example.com/",
	}
	raw := SanitizeAuditParams(in)
	text := string(raw)
	for _, marker := range []string{"ghp_SECRETMARK1", "alice", "S3SIGMARK2", "PWMARK", "LONGMARK", "plain.example.com"} {
		if strings.Contains(text, marker) {
			t.Fatalf("в параметрах осталось %q: %s", marker, text)
		}
	}
	for _, kept := range []string{"https://[redacted]@github.com/acme/app.git", `spec.pdf?X-Amz-Signature=[redacted]\u0026v=2`, "https://[redacted]@git.example.com/x.git"} {
		if !strings.Contains(text, kept) {
			t.Fatalf("нет %q: %s", kept, text)
		}
	}
	var out struct {
		Description struct {
			Size   int    `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"description"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(long))
	if out.Description.Size != len(long) || out.Description.SHA256 != hex.EncodeToString(sum[:]) || out.Token != auditRedacted {
		t.Fatalf("длинная строка или ключ-секрет: %s", text)
	}
}

// Возврат с merge (CLAB-226, круг 2): ссылки без схемы, процентная запись,
// неэкранированные "/" и кавычка в пароле.
func TestSanitizeAuditParamsURLsAwkward(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		// Ссылки без схемы.
		{"userinfo без схемы", "u:PWMARK@host/x", "[redacted]@host/x"},
		{"userinfo без схемы с портом", "u:PWMARK@git.example.com:8443", "[redacted]@git.example.com:8443"},
		{"userinfo без схемы в тексте",
			"клон из alice:ghp_PWMARK@github.com/acme/app.git, ветка main",
			"клон из [redacted]@github.com/acme/app.git, ветка main"},
		{"userinfo без схемы с @ в пароле", "u:p@PWMARK@host/x", "[redacted]@host/x"},
		{"userinfo без схемы и запрос", "u:PWMARK@host/x?token=TOKMARK&v=1", "[redacted]@host/x?token=[redacted]&v=1"},
		{"userinfo без схемы в параметре ссылки", "https://h/go?to=u:PWMARK@host/x&v=1", "https://h/go?to=[redacted]@host/x&v=1"},
		{"ссылка от корня схемы", "//u:PWMARK@host", "//[redacted]@host"},
		{"ссылка от корня схемы в тексте", "см. //u:PWMARK@host/x и всё", "см. //[redacted]@host/x и всё"},
		{"запрос без схемы", "github.com/x?token=TOKMARK&v=1", "github.com/x?token=[redacted]&v=1"},
		{"запрос и фрагмент без схемы", "/cb?code=CODEMARK&state=s#id_token=IDMARK&x=1", "/cb?code=[redacted]&state=s#id_token=[redacted]&x=1"},
		{"фрагмент без схемы", "app.example.com/cb#access_token=FRAGMARK&state=x", "app.example.com/cb#access_token=[redacted]&state=x"},
		// Процентная запись.
		{"закодированная ссылка в параметре",
			"https://h/go?next=https%3A%2F%2Fuser%3APWMARK%40host%2Fx&v=1",
			"https://h/go?next=https%3A%2F%2F%5Bredacted%5D%40host%2Fx&v=1"},
		{"закодированная ссылка с запросом",
			"https://h/cb?next=%2Fapp%3Ftoken%3DTOKMARK%26v%3D1&v=2",
			"https://h/cb?next=%2Fapp%3Ftoken%3D%5Bredacted%5D%26v%3D1&v=2"},
		{"двойная процентная запись",
			"https://h/go?next=https%253A%252F%252Fu%253APWMARK%2540h",
			"https://h/go?next=https%253A%252F%252F%255Bredacted%255D%2540h"},
		{"закодированная ссылка строкой целиком", "https%3A%2F%2Fu%3APWMARK%40host%2Fx", "https%3A%2F%2F%5Bredacted%5D%40host%2Fx"},
		{"закодированное =", "https://h/x?token%3DTOKMARK&v=1", "https://h/x?token%3D[redacted]&v=1"},
		{"закодированное = в нижнем регистре", "https://h/x?a=1&api_key%3dKEYMARK", "https://h/x?a=1&api_key%3d[redacted]"},
		{"закодированные параметры строкой целиком", "a%3D1%26token%3DTOKMARK", "a%3D1%26token%3D%5Bredacted%5D"},
		// Неэкранированные "/" и кавычка в пароле.
		{"/ в пароле", "https://u:p/PWMARK@h/x", "https://[redacted]@h/x"},
		{"кавычка в пароле", `https://u:p"PWMARK@h/x?token=TOKMARK`, "https://[redacted]@h/x?token=[redacted]"},
		{"апостроф в пароле", "https://u:p'PWMARK@h", "https://[redacted]@h"},
		{"/ в пароле, ссылка в тексте", "из https://u:a/b/PWMARK@h/x и всё", "из https://[redacted]@h/x и всё"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := SanitizeAuditParams(map[string]any{"url": tc.in})
			var out map[string]string
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatal(err)
			}
			if out["url"] != tc.want {
				t.Fatalf("\nвход    %s\nвыход   %s\nожидали %s", tc.in, out["url"], tc.want)
			}
			for _, marker := range []string{"PWMARK", "TOKMARK", "CODEMARK", "IDMARK", "FRAGMARK", "KEYMARK"} {
				if strings.Contains(string(raw), marker) {
					t.Fatalf("в параметрах осталось %q: %s", marker, raw)
				}
			}
		})
	}
}

// Обычный текст без секретов проходит байт в байт (CLAB-226, круг 2):
// почта, время, host:port, путь с «@», проценты, «//» и «#» в тексте.
func TestSanitizeAuditParamsPlainTextUntouched(t *testing.T) {
	for _, s := range []string{
		"ivan@example.com",
		"Почта:ivan@example.com",
		"Email:ivan@example.com, телефон 12:30",
		"mailto:ivan@example.com",
		"встреча в 12:30, комната 4; перенос на 14:00",
		"localhost:8080",
		"db.internal:5432/cadence",
		"user@host:22",
		"ratio 3:2 @ 60fps",
		"http://localhost:3000/@vite/client",
		"https://medium.com/@user/post-1",
		`["https://h:8080","me@x.io"]`,
		"a//b и // комментарий",
		"C# и issue #12, раздел #top",
		"что?ключ=1",
		// Круг 3 (решение человека): перед "?" не хост и не путь — проза.
		"что?key=1",
		"x?token=abc",
		"Вопрос?pass=1, а дальше#token=2",
		"https://h/?tokens=5&max_tokens=100&tokenizer=bpe",
		"api.example.com/v1/chat?max_tokens=100&tokenizer=bpe",
		"100% готово, скидка 50%25",
		"%D0%BF%D1%80%D0%B8%D0%B2%D0%B5%D1%82",
		"https://example.com/search?q=%D0%BF&page=2&next=%2Fhome",
		"https://h/?passport=1&bypass=2&pass=&token=",
		"git@github.com:acme/app.git",
		"owner/name@v1.2.3",
		// Строки из проверки круга 2 — после круга 3 так же байт в байт.
		"http://localhost:3000/@vite/client?x=1",
		"@scope/pkg@1.2.3",
		"ghcr.io/org/img@sha256:0123abcd",
		"v1.2.3@sha256:0123abcd",
		"a:b@c",
		"%s %d и date +%Y-%m-%d",
		"https://github.com/o/r/blob/main/a.go#L10-L20",
		"https://h:8080/x@y",
		"https://h:abc/x",
		"tel: +7 (999) 123-45-67",
		"https://h/x?q=%D0%BF%D1%80",
		"?q=%D0%BF&next=%2Fhome",
		"https://h/x?password=",
		"https://[::1]:8080/x?a=1#f@x",
	} {
		raw := SanitizeAuditParams(map[string]any{"s": s})
		var out map[string]string
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		if out["s"] != s {
			t.Errorf("строка без секретов изменилась:\nвход  %s\nвыход %s", s, out["s"])
		}
	}
}

// Секреты, вырезанные на круге 2 (проверка тестировщика), вырезаются и
// после круга 3: предел глубины и узкие имена их не задевают.
func TestRedactURLsRound2Still(t *testing.T) {
	for _, in := range []string{
		"//MARK@host/p", "u:MARK@[::1]:80/p", "u:MARK@10.0.0.1/p", "/cb?access_token=MARK",
		"host/cb#access_token=MARK", "u:MARK@host?x=1", "u:MARK@host#f", "u:p@MARK@host/x",
		"u:MARK%40x@host/x", "см. token%3DMARK в логе", "https://h/x?a=1#access_token%3DMARK",
		"https://u%3AMARK@host/x", "https://u:MARK%40@host/x", "https://h/x?%74oken=MARK",
		"https://h/go?next=https%3A%2F%2Fu%3Ap%2540MARK%40host%2Fx%3Ftoken%3DMARK2",
		`https://u:p"MARK@h`, "https://u:p'MARK@h", "https://u:p`MARK@h", "https://u:p<MARK@h",
		"https://u:p/MARK/z@h/x", "https://u:p/ss@h/p?token=MARK",
		"git clone https://alice:pa/MARK@github.com/a/b.git",
		"postgres://app:MARK@db:5432/cadence?sslmode=disable", "redis://:MARK@h:6379",
		"git+ssh://u:MARK@h/x.git", "HTTPS://U:MARK@H/X", "https://u:MARK:more@h/x",
		`{"url":"https://u:MARK@h/x"}`, "(https://u:MARK@h/x)", "a;https://u:MARK@h/x,b",
		"строка\nhttps://u:MARK@h/x\nещё", "https://h/?api-key=MARK&x-api-key=MARK2&AWSAccessKeyId=MARK3",
		"https://h/?PHPSESSID=MARK&jsessionid=MARK2&X-Amz-Security-Token=MARK3&pass=MARK4",
	} {
		if got := redactURLs(in); strings.Contains(got, "MARK") {
			t.Errorf("секрет остался:\nвход  %s\nвыход %s", in, got)
		}
	}
}

// Известные пробелы (решение человека, круг 3): не закрываются и описаны в
// CLAUDE.md, MCP_CONTRACT.md и admin.md пакета. Тест держит описание
// честным: закрыли пробел — строку отсюда и из трёх документов убрать.
func TestRedactURLsKnownGaps(t *testing.T) {
	for _, in := range []string{
		"u:GAPMARK@host",                // без схемы, без пути и порта
		"https://u:12/GAPMARK@h",        // пароль из цифр с "/" — как порт
		"https://u:p/a@GAP/MARK@h/x",    // пароль с "/" после "@"
		"https://u:p GAPMARK@h",         // пробел в пароле
		"см. %74oken=GAPMARK",           // имя в процентной записи вне ссылки
		"localhost:8080?token=GAPMARK",  // без схемы, перед "?" нет точки и "/"
		"https://h/?dbpassword=GAPMARK", // склеенное имя вне перечня
	} {
		if got := redactURLs(in); !strings.Contains(got, "MARK") {
			t.Errorf("пробел закрыт, обновите документацию: %q → %q", in, got)
		}
	}
}

// Строка без "@", "=" и "%" не меняется никогда: вырезать в ней нечего — ни
// userinfo, ни значения параметра. Разбор не паникует на любом входе.
func FuzzRedactURLs(f *testing.F) {
	for _, s := range []string{
		"https://u:p@h/x?token=t#a=b", "//u:p/s@h", "u:p@h:1", "a%3D1%26token%3Dx",
		"https%253A%252F%252Fu%253Ap%2540h", `https://u:p"s@h`, "https://", "://@", "%", "%%3D=",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := redactURLs(s)
		if !strings.ContainsAny(s, "@=%") && out != s {
			t.Fatalf("строка без секретов изменилась: %q → %q", s, out)
		}
	})
}

// auditWorstUnits — куски, из повторов которых строка разбиралась дольше
// всего: «//» и «/» — вложенность ссылок, «%25» и «%2525» — процентная
// запись в процентной записи, остальные — userinfo, запрос и фрагмент на
// каждом шаге (CLAB-226, круг 3).
var auditWorstUnits = []string{
	"//", "/", "//:@", "///@", "https://", "https://u:p@h/", "//u@h?token=x&", "//a?x=//a#y=",
	"%25", "%2525", "%2F%2F", "%3D", "a%3D", "%40%3A",
	"a:b@", "a:b@c/", "u:p/s@h/", ":@", "@", "?a=", "#a=", "?", "a.b/c?token=", "x?",
}

// worstString — строка длиной не больше n из повторов unit; "%2525"
// повторяется как "%" и хвост "25", чтобы раскодировалась на каждом уровне.
func worstString(unit string, n int) string {
	if unit == "%2525" {
		return "%" + strings.Repeat("25", (n-1)/2)
	}
	return strings.Repeat(unit, n/len(unit))
}

// Разбор ссылок линеен по длине строки (CLAB-226, круг 3): шестнадцать
// строк по 1 КБ и одна в 16 КБ из того же куска чистятся за сравнимое
// время. Квадратичный разбор даёт на длинной строке в шестнадцать раз
// больше, линейный — столько же; порог 4 оставляет место шуму машины.
// Минимум из пяти замеров убирает паузы сборщика мусора.
func TestRedactURLsLinear(t *testing.T) {
	measure := func(fn func()) time.Duration {
		best := time.Duration(1 << 62)
		for range 5 {
			start := time.Now()
			fn()
			best = min(best, time.Since(start))
		}
		return best
	}
	for _, unit := range auditWorstUnits {
		small, big := worstString(unit, 1<<10), worstString(unit, 16<<10)
		tSmall := measure(func() {
			for range 16 {
				_ = redactURLs(small)
			}
		})
		tBig := measure(func() { _ = redactURLs(big) })
		// Доли миллисекунды меряются с шумом: сравниваем, только когда
		// длинная строка заметно дольше 1 мс.
		if tBig > time.Millisecond && tBig > 4*tSmall {
			t.Errorf("%q: 16 × 1 КБ — %s, 16 КБ — %s: рост быстрее линейного", unit, tSmall, tBig)
		}
	}
}

// Тело в 4 МБ из тысячи строк по 4 КБ худшего вида — то, что api
// принимает и на отказе 401 (CLAB-226, круг 3: было 44 с). Порог щедрый на
// медленную машину CI; замер на верстаке — в разборе изменений.
func TestSanitizeAuditParamsWorstBody(t *testing.T) {
	for _, unit := range []string{"//", "//:@", "%2525", "https://u:p@h/", "//u@h?token=x&"} {
		s := worstString(unit, auditMaxString-2)
		items := make([]any, 1000)
		for i := range items {
			items[i] = s
		}
		start := time.Now()
		_ = SanitizeAuditParams(map[string]any{"x": items})
		d := time.Since(start)
		t.Logf("%q: тело из 1000 строк по %d байт — %s", unit, len(s), d)
		if d > 3*time.Second {
			t.Errorf("%q: тело из 1000 строк по %d байт чистилось %s", unit, len(s), d)
		}
	}
}

// Глубже auditMaxDepth разбор не идёт: кусок, где может лежать секрет,
// заменяется целиком, остальное остаётся байт в байт.
func TestRedactURLsTooDeep(t *testing.T) {
	deep := "https://h/" + strings.Repeat("a//", auditMaxDepth+2)
	if got := redactURLs(deep + "x"); got != deep+"x" {
		t.Fatalf("глубокая ссылка без секретов изменилась: %q", got)
	}
	for _, in := range []string{
		deep + "ghp_DEEPMARK@host/x",
		deep + "x?token=DEEPMARK",
		strings.Repeat("https://h/?u=", auditMaxDepth+2) + "https://h/x?a=1&token=DEEPMARK",
		"https://h/?n=%" + strings.Repeat("25", auditMaxDepth+2) + "3D",
	} {
		if got := redactURLs(in); strings.Contains(got, "DEEPMARK") || !strings.Contains(got, auditRedacted) && strings.Contains(in, "MARK") {
			t.Errorf("глубже предела секрет остался:\nвход  %s\nвыход %s", in, got)
		}
	}
	// В пределах глубины разбор прежний: вырезается только секрет.
	in := "https://h/go?u=https://h2/go?u=https://u:SHALLOWMARK@h3/x&v=1"
	if got, want := redactURLs(in), "https://h/go?u=https://h2/go?u=https://[redacted]@h3/x&v=1"; got != want {
		t.Fatalf("\nвход    %s\nвыход   %s\nожидали %s", in, got, want)
	}
}

func BenchmarkRedactURLs(b *testing.B) {
	for _, unit := range auditWorstUnits {
		s := worstString(unit, auditMaxString)
		b.Run(unit, func(b *testing.B) {
			b.SetBytes(int64(len(s)))
			for b.Loop() {
				_ = redactURLs(s)
			}
		})
	}
}

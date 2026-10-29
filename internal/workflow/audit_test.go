package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
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
	}
	for k := range auditSecretKeys {
		secret = append(secret, k)
	}
	for _, name := range secret {
		if !secretURLParam(name) {
			t.Errorf("%q не считается секретным", name)
		}
	}
	for _, name := range []string{"v", "page", "state", "ref", "q", "response-content-type", "keyword", "monkey", "author"} {
		if secretURLParam(name) {
			t.Errorf("%q считается секретным", name)
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

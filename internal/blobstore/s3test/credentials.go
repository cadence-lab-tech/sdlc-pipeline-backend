package s3test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Key — временные ключи, которые выдают фейки агента Pod Identity и STS.
type Key struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expiration      time.Time
}

// issuer — общая часть фейков: очередь ключей (последний повторяется),
// счётчик обращений, зависание и отказ по требованию.
type issuer struct {
	mu    sync.Mutex
	Keys  []Key
	calls int
	// Hang — не отвечать, пока клиент не оборвёт запрос.
	Hang bool
	// Status — ответить этим кодом и телом Body вместо ключей.
	Status int
	Body   string
	// Tokens — какие токены пришли, по порядку.
	Tokens []string
}

func (i *issuer) next(token string) (Key, int, string, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	i.Tokens = append(i.Tokens, token)
	if i.Status != 0 {
		return Key{}, i.Status, i.Body, i.Hang
	}
	if len(i.Keys) == 0 {
		return Key{}, http.StatusInternalServerError, "no keys", i.Hang
	}
	k := i.Keys[0]
	if len(i.Keys) > 1 {
		i.Keys = i.Keys[1:]
	}
	return k, 0, "", i.Hang
}

// Calls — сколько раз к фейку обращались.
func (i *issuer) Calls() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

// Set меняет поведение фейка между запросами теста.
func (i *issuer) Set(f func(*issuer)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	f(i)
}

func currentToken(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

// Container — агент EKS Pod Identity: отвечает только на GET с заголовком
// Authorization, равным содержимому TokenFile на момент запроса.
type Container struct {
	issuer
	TokenFile string
}

func (c *Container) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := r.Header.Get("Authorization")
	if r.Method != http.MethodGet || got == "" || got != currentToken(c.TokenFile) {
		c.mu.Lock()
		c.calls++
		c.mu.Unlock()
		http.Error(w, `{"Message":"invalid token"}`, http.StatusUnauthorized)
		return
	}
	k, status, body, hang := c.next(got)
	if hang {
		<-r.Context().Done()
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
		return
	}
	json.NewEncoder(w).Encode(map[string]string{
		"AccessKeyId": k.AccessKeyID, "SecretAccessKey": k.SecretAccessKey,
		"Token": k.SessionToken, "Expiration": k.Expiration.UTC().Format(time.RFC3339),
	})
}

// STS — AssumeRoleWithWebIdentity: запрос без подписи, параметры сверяются с
// RoleArn, RoleSessionName (пусто — любое непустое) и содержимым TokenFile.
type STS struct {
	issuer
	RoleArn     string
	SessionName string
	TokenFile   string
	// Sessions — какие RoleSessionName пришли.
	Sessions []string
}

func (s *STS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f := r.Form
	s.mu.Lock()
	s.Sessions = append(s.Sessions, f.Get("RoleSessionName"))
	s.mu.Unlock()
	var problem string
	switch {
	case r.Header.Get("Authorization") != "":
		problem = "request must not be signed"
	case f.Get("Action") != "AssumeRoleWithWebIdentity" || f.Get("Version") != "2011-06-15":
		problem = "wrong Action or Version"
	case f.Get("RoleArn") != s.RoleArn:
		problem = "wrong RoleArn"
	case f.Get("RoleSessionName") == "" || s.SessionName != "" && f.Get("RoleSessionName") != s.SessionName:
		problem = "wrong RoleSessionName"
	case f.Get("WebIdentityToken") == "" || f.Get("WebIdentityToken") != currentToken(s.TokenFile):
		problem = "wrong WebIdentityToken"
	}
	if problem != "" {
		s.mu.Lock()
		s.calls++
		s.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "<ErrorResponse><Error><Type>Sender</Type><Code>InvalidParameterValue</Code><Message>%s</Message></Error></ErrorResponse>", problem)
		return
	}
	k, status, body, hang := s.next(f.Get("WebIdentityToken"))
	if hang {
		<-r.Context().Done()
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		fmt.Fprint(w, body)
		return
	}
	fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
<AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>%s</SecretAccessKey>
<SessionToken>%s</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult>
</AssumeRoleWithWebIdentityResponse>`, k.AccessKeyID, k.SecretAccessKey, k.SessionToken, k.Expiration.UTC().Format(time.RFC3339))
}

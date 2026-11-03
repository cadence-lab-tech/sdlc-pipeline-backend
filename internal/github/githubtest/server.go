// Package githubtest — подставной GitHub для проверок опроса установок
// (CLAB-395): установки приложения, их репозитории, токены установок, головы
// веток. Считает запросы по видам и умеет отказывать по виду запроса, чтобы
// проверки видели и число походов к провайдеру, и поведение на сбое.
package githubtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/github"
)

// Виды запросов — ключи Requests и Fail.
const (
	KindList   = "list"   // GET /app/installations
	KindGet    = "get"    // GET /app/installations/{id}
	KindToken  = "token"  // POST /app/installations/{id}/access_tokens
	KindRepos  = "repos"  // GET /installation/repositories
	KindRepo   = "repo"   // GET /repos/{owner}/{name}
	KindBranch = "branch" // GET /repos/{owner}/{name}/branches/{branch}
)

// Drop — значение Fail, при котором соединение рвётся без ответа.
const Drop = -1

// Installation — установка на стороне подставного GitHub.
type Installation struct {
	Suspended bool
	// Repos — owner/name репозиториев, отданных установке.
	Repos []string
}

// Server — подставной GitHub. Поля меняются под Lock/Unlock или до первого
// запроса.
type Server struct {
	*httptest.Server

	mu            sync.Mutex
	Installations map[int64]*Installation
	// Renamed — старое owner/name → новое: GET /repos по старому имени
	// отвечает новым full_name, как переадресация GitHub.
	Renamed map[string]string
	// Public — репозитории, которые читаются любым токеном, даже вне
	// установки.
	Public map[string]bool
	// Fail — вид запроса → код ответа (Drop — оборвать соединение).
	Fail map[string]int
	// UnderreportTotal — /installation/repositories отдаёт total_count
	// больше, чем репозиториев: список «сдвинулся» между страницами.
	UnderreportTotal bool
	requests         map[string]int
}

// New поднимает сервер; он гасится в конце теста.
func New(t *testing.T) *Server {
	t.Helper()
	s := &Server{
		Installations: map[int64]*Installation{},
		Renamed:       map[string]string{},
		Public:        map[string]bool{},
		Fail:          map[string]int{},
		requests:      map[string]int{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// Requests — сколько запросов вида kind пришло.
func (s *Server) Requests(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[kind]
}

// ResetRequests обнуляет счётчики.
func (s *Server) ResetRequests() {
	s.mu.Lock()
	s.requests = map[string]int{}
	s.mu.Unlock()
}

// Provider — github.Provider с приложением на этом сервере. Повторов по
// лимиту у клиента нет (MaxWait мал): сбой виден сразу, а не через секунды
// ожидания.
func (s *Server) Provider(t *testing.T) *github.Provider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := github.NewApp(s.URL, "12345", pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
	if err != nil {
		t.Fatal(err)
	}
	base := github.New(s.URL, "")
	base.MaxWait = time.Millisecond
	return github.NewProvider(base, app, nil)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	kind, id, rest := s.classify(r)
	if kind == "" {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	s.requests[kind]++
	switch code := s.Fail[kind]; {
	case code == Drop:
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		http.Error(w, "drop", http.StatusBadGateway)
		return
	case code == http.StatusForbidden:
		w.Header().Set("X-RateLimit-Remaining", "0")
		http.Error(w, `{"message":"API rate limit exceeded"}`, code)
		return
	case code != 0:
		http.Error(w, `{"message":"injected"}`, code)
		return
	}

	switch kind {
	case KindList:
		perPage, page := paging(r)
		ids := s.ids()
		from, to := min((page-1)*perPage, len(ids)), min(page*perPage, len(ids))
		out := []map[string]any{}
		for _, id := range ids[from:to] {
			out = append(out, s.installationJSON(id))
		}
		writeJSON(w, http.StatusOK, out)
	case KindGet:
		if _, ok := s.Installations[id]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, s.installationJSON(id))
	case KindToken:
		inst, ok := s.Installations[id]
		switch {
		case !ok:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		case inst.Suspended:
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "This installation has been suspended"})
		default:
			writeJSON(w, http.StatusCreated, map[string]any{
				"token": "tok-" + strconv.FormatInt(id, 10), "expires_at": time.Now().Add(time.Hour),
			})
		}
	case KindRepos:
		inst, ok := s.Installations[id]
		if !ok || inst.Suspended {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		perPage, page := paging(r)
		from, to := min((page-1)*perPage, len(inst.Repos)), min(page*perPage, len(inst.Repos))
		repos := []map[string]any{}
		for _, full := range inst.Repos[from:to] {
			owner, name, _ := strings.Cut(full, "/")
			repos = append(repos, map[string]any{"name": name, "full_name": full, "owner": map[string]string{"login": owner}})
		}
		total := len(inst.Repos)
		if s.UnderreportTotal {
			total++
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": total, "repositories": repos})
	case KindRepo:
		full := rest
		if renamed, ok := s.Renamed[full]; ok {
			writeJSON(w, http.StatusOK, map[string]any{"full_name": renamed, "default_branch": "main"})
			return
		}
		if s.Public[full] || s.tokenSees(id, full) {
			writeJSON(w, http.StatusOK, map[string]any{"full_name": full, "default_branch": "main"})
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	case KindBranch:
		writeJSON(w, http.StatusOK, map[string]any{"commit": map[string]string{"sha": strings.Repeat("c", 40)}})
	}
}

// classify разбирает запрос: вид, установку (из пути или из токена) и
// owner/name для запросов репозитория.
func (s *Server) classify(r *http.Request) (kind string, id int64, rest string) {
	path := r.URL.Path
	tokenID := func() int64 {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		n, _ := strconv.ParseInt(tok, 10, 64)
		return n
	}
	switch {
	case r.Method == http.MethodGet && path == "/app/installations":
		return KindList, 0, ""
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/app/installations/") && strings.HasSuffix(path, "/access_tokens"):
		n, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(path, "/app/installations/"), "/access_tokens"), 10, 64)
		return KindToken, n, ""
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/app/installations/"):
		n, _ := strconv.ParseInt(strings.TrimPrefix(path, "/app/installations/"), 10, 64)
		return KindGet, n, ""
	case r.Method == http.MethodGet && path == "/installation/repositories":
		return KindRepos, tokenID(), ""
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/repos/"):
		parts := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
		if len(parts) >= 4 && parts[2] == "branches" {
			return KindBranch, tokenID(), parts[0] + "/" + parts[1]
		}
		if len(parts) == 2 {
			return KindRepo, tokenID(), parts[0] + "/" + parts[1]
		}
	}
	return "", 0, ""
}

func (s *Server) tokenSees(id int64, full string) bool {
	inst, ok := s.Installations[id]
	if !ok || inst.Suspended {
		return false
	}
	for _, r := range inst.Repos {
		if strings.EqualFold(r, full) {
			return true
		}
	}
	return false
}

func (s *Server) ids() []int64 {
	ids := make([]int64, 0, len(s.Installations))
	for id := range s.Installations {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Server) installationJSON(id int64) map[string]any {
	out := map[string]any{"id": id, "account": map[string]string{"login": fmt.Sprintf("acct-%d", id)}, "suspended_at": nil}
	if s.Installations[id].Suspended {
		out["suspended_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	return out
}

func paging(r *http.Request) (perPage, page int) {
	perPage, _ = strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 30
	}
	page, _ = strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}
	return perPage, page
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

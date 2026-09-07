package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestHeadTreeBlob(t *testing.T) {
	repo := Repo{Owner: "acme", Name: "api"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t0ken" {
			t.Errorf("токен не уехал в запрос: %q", got)
		}
		switch r.URL.Path {
		case "/repos/acme/api/branches/main":
			_, _ = w.Write([]byte(`{"commit":{"sha":"abc123"}}`))
		case "/repos/acme/api/git/trees/abc123":
			if r.URL.Query().Get("recursive") != "1" {
				t.Error("листинг обязан быть рекурсивным")
			}
			_, _ = w.Write([]byte(`{"truncated":false,"tree":[
				{"path":"main.go","sha":"f1","type":"blob","size":10},
				{"path":"internal","sha":"d1","type":"tree"},
				{"path":"sub","sha":"c1","type":"commit"}]}`))
		case "/repos/acme/api/git/blobs/f1":
			if r.Header.Get("Accept") != "application/vnd.github.raw" {
				t.Errorf("содержимое должно запрашиваться сырым: %q", r.Header.Get("Accept"))
			}
			_, _ = w.Write([]byte("package main\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "t0ken")
	ctx := context.Background()

	head, err := c.Head(ctx, repo, "main")
	if err != nil || head != "abc123" {
		t.Fatalf("голова ветки: %q, %v", head, err)
	}

	entries, truncated, err := c.Tree(ctx, repo, head)
	if err != nil || truncated {
		t.Fatalf("листинг: truncated=%v, %v", truncated, err)
	}
	// Каталоги и субмодули содержимого не имеют, в индекс им не за чем.
	if len(entries) != 1 || entries[0].Path != "main.go" || entries[0].SHA != "f1" {
		t.Fatalf("в листинге должен остаться один файл: %+v", entries)
	}

	content, err := c.Blob(ctx, repo, "f1")
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("содержимое: %q, %v", content, err)
	}
}

// Удалённая ветка — обычный ход событий, а не сбой: прогон индексации должен
// уметь отличить её от «провайдер лежит».
func TestNotFoundIsDistinguishable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").Head(context.Background(), Repo{"acme", "api"}, "gone")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("ожидали ErrNotFound, получили %v", err)
	}
}

func TestRetriesOnServerError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"commit":{"sha":"ok"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.MaxWait = time.Minute
	head, err := c.Head(context.Background(), Repo{"acme", "api"}, "main")
	if err != nil || head != "ok" {
		t.Fatalf("после повтора ожидали успех: %q, %v", head, err)
	}
	if calls != 2 {
		t.Fatalf("ожидали ровно один повтор, вызовов %d", calls)
	}
}

// Упереться в почасовой лимит и ждать его сброса внутри прогона хуже, чем
// перенести прогон на следующий тик.
func TestRateLimitBeyondMaxWaitFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	c.MaxWait = time.Second
	start := time.Now()
	if _, err := c.Head(context.Background(), Repo{"acme", "api"}, "main"); err == nil {
		t.Fatal("при лимите до конца часа прогон обязан отложиться")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("ждать сброса лимита внутри запроса нельзя")
	}
}

package oauth

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Заглушка документа клиента: сервер TLS httptest (сертификат на
// example.com), имя разрешает подставной резолвер в «публичный» адрес, а
// соединение с ним ведёт на заглушку. Так проверяется настоящий путь —
// разрешение, проверка адреса, соединение на проверенный IP, TLS по имени.

type stubResolver struct {
	mu    sync.Mutex
	calls int
	// answers — ответ на каждый вызов; последний повторяется.
	answers [][]netip.Addr
}

func (r *stubResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i := min(r.calls, len(r.answers)-1)
	r.calls++
	return r.answers[i], nil
}

type cimdStub struct {
	srv     *httptest.Server
	hits    atomic.Int32
	dialed  []string
	mu      sync.Mutex
	handler http.HandlerFunc
}

const stubPublic = "203.0.113.10"

func newCIMDStub(t *testing.T) (*cimdStub, *Fetcher, *stubResolver) {
	t.Helper()
	s := &cimdStub{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.handler(w, r)
	}))
	t.Cleanup(s.srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(s.srv.Certificate())
	res := &stubResolver{answers: [][]netip.Addr{{netip.MustParseAddr(stubPublic)}}}
	f := &Fetcher{
		Resolver: res, RootCAs: pool,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			s.mu.Lock()
			s.dialed = append(s.dialed, addr)
			s.mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, s.srv.Listener.Addr().String())
		},
	}
	return s, f, res
}

const stubClientID = "https://example.com/client.json"

func docJSON(clientID string, redirects ...string) string {
	return fmt.Sprintf(`{"client_id":%q,"client_name":"Claude","redirect_uris":["%s"]}`, clientID, strings.Join(redirects, `","`))
}

func serveJSON(body string, header map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for k, v := range header {
			w.Header().Set(k, v)
		}
		_, _ = w.Write([]byte(body))
	}
}

// AC-6: документ загружен, проверен и лежит в кэше по max-age.
func TestFetchClientDocument(t *testing.T) {
	stub, f, _ := newCIMDStub(t)
	now := time.Now()
	f.Now = func() time.Time { return now }
	stub.handler = serveJSON(docJSON(stubClientID, "https://claude.ai/cb"), map[string]string{"Cache-Control": "public, max-age=60"})

	doc, err := f.Fetch(context.Background(), stubClientID)
	if err != nil || doc.Name != "Claude" || doc.RedirectURIs[0] != "https://claude.ai/cb" {
		t.Fatalf("документ: %+v %v", doc, err)
	}
	if stub.dialed[0] != stubPublic+":443" {
		t.Fatalf("соединение не на проверенный адрес: %v", stub.dialed)
	}
	// max-age=60 поднимается до пяти минут.
	now = now.Add(4 * time.Minute)
	if _, err := f.Fetch(context.Background(), stubClientID); err != nil || stub.hits.Load() != 1 {
		t.Fatalf("кэш: запросов %d, %v", stub.hits.Load(), err)
	}
	now = now.Add(2 * time.Minute)
	if _, ok := f.Cached(stubClientID); ok {
		t.Fatal("кэш пережил свой срок")
	}
	if cacheTTL("") != time.Hour || cacheTTL("max-age=999999") != 24*time.Hour || cacheTTL("max-age=7200") != 2*time.Hour {
		t.Fatal("сроки кэша")
	}
}

// AC-7: каждый отказ — категория, без запроса к цели там, где адрес плох.
func TestFetchClientDocumentRefusals(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{
		"http://example.com/client.json", "https://example.com/client.json#frag", "https://u:p@example.com/client.json",
		"https://example.com", "https://example.com/",
	} {
		stub, f, _ := newCIMDStub(t)
		stub.handler = serveJSON(docJSON(bad, "https://claude.ai/cb"), nil)
		_, err := f.Fetch(ctx, bad)
		var ce *CIMDError
		if !errors.As(err, &ce) || ce.Category != CIMDBadURL || stub.hits.Load() != 0 {
			t.Errorf("%s: %v, запросов %d", bad, err, stub.hits.Load())
		}
	}

	private := []string{"10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.16.0.1", "192.168.1.1",
		"198.18.0.1", "0.0.0.1", "224.0.0.1", "240.0.0.1", "::1", "fc00::1", "fe80::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "::",
		// Встроенный IPv4: NAT64 известного префикса с запрещённым IPv4,
		// локальный NAT64, 6to4, Teredo и IPv4-compatible — любые.
		"64:ff9b::a00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", "64:ff9b:1::808:808", "2002:808:808::1",
		"2001:0:4136:e378:8000:63bf:3fff:fdd2", "::808:808", "fe80::1%eth0"}
	for _, ip := range private {
		stub, f, res := newCIMDStub(t)
		res.answers = [][]netip.Addr{{netip.MustParseAddr(stubPublic), netip.MustParseAddr(ip)}}
		stub.handler = serveJSON(docJSON(stubClientID, "https://claude.ai/cb"), nil)
		_, err := f.Fetch(ctx, stubClientID)
		var ce *CIMDError
		if !errors.As(err, &ce) || ce.Category != CIMDForbidden || len(stub.dialed) != 0 {
			t.Errorf("%s: %v, соединений %v", ip, err, stub.dialed)
		}
	}
	// Адрес литералом в client_id — та же проверка, и для адресов со
	// встроенным IPv4 и с зоной (RFC 6874, %25 в URL).
	for _, lit := range []string{"https://10.0.0.1/client.json", "https://[64:ff9b::a00:1]/c", "https://[64:ff9b::a9fe:a9fe]/c",
		"https://[64:ff9b:1::808:808]/c", "https://[2002:a00:1::1]/c", "https://[2001:0:4136:e378:8000:63bf:3fff:fdd2]/c",
		"https://[::a00:1]/c", "https://[::1%25lo]/c", "https://[fe80::1%25eth0]/c"} {
		stub, f, _ := newCIMDStub(t)
		stub.handler = serveJSON(docJSON(lit, "https://claude.ai/cb"), nil)
		if _, err := f.Fetch(ctx, lit); !isCategory(err, CIMDForbidden) || len(stub.dialed) != 0 {
			t.Errorf("литерал %s: %v, соединений %v", lit, err, stub.dialed)
		}
	}

	// DNS rebinding: второе разрешение дало бы приватный адрес, но его нет —
	// соединение идёт на первый проверенный.
	stub, f, res := newCIMDStub(t)
	res.answers = [][]netip.Addr{{netip.MustParseAddr(stubPublic)}, {netip.MustParseAddr("127.0.0.1")}}
	stub.handler = serveJSON(docJSON(stubClientID, "https://claude.ai/cb"), nil)
	if _, err := f.Fetch(ctx, stubClientID); err != nil {
		t.Fatalf("rebinding: %v", err)
	}
	if res.calls != 1 || len(stub.dialed) != 1 || stub.dialed[0] != stubPublic+":443" {
		t.Fatalf("rebinding: разрешений %d, соединения %v", res.calls, stub.dialed)
	}

	big := `{"client_id":"` + stubClientID + `","redirect_uris":["https://claude.ai/cb"],"pad":"` + strings.Repeat("x", CIMDMaxBytes) + `"}`
	for name, c := range map[string]struct {
		handler  http.HandlerFunc
		category string
	}{
		"3xx": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://example.com/other.json", http.StatusFound)
		}, CIMDBadResponse},
		"не JSON": {serveHTML(docJSON(stubClientID, "https://claude.ai/cb")), CIMDBadResponse},
		"5 КиБ":   {serveJSON(big, nil), CIMDTooLarge},
		"404": {func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "секрет внутренней сети", 404)
		}, CIMDBadResponse},
		"client_id":      {serveJSON(docJSON("https://example.com/other.json", "https://claude.ai/cb"), nil), CIMDInvalid},
		"пустой":         {serveJSON(`{"client_id":"`+stubClientID+`","redirect_uris":[]}`, nil), CIMDInvalid},
		"секретный":      {serveJSON(`{"client_id":"`+stubClientID+`","redirect_uris":["https://claude.ai/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, nil), CIMDInvalid},
		"http-адрес":     {serveJSON(docJSON(stubClientID, "http://evil.example/cb"), nil), CIMDInvalid},
		"не разбирается": {serveJSON(`{"client_id":`, nil), CIMDInvalid},
	} {
		stub, f, _ := newCIMDStub(t)
		stub.handler = c.handler
		_, err := f.Fetch(ctx, stubClientID)
		if !isCategory(err, c.category) {
			t.Errorf("%s: %v, ожидали %q", name, err, c.category)
		}
		if err != nil && (strings.Contains(err.Error(), "секрет") || strings.Contains(err.Error(), "pad")) {
			t.Errorf("%s: тело цели в ошибке: %v", name, err)
		}
		if _, ok := f.Cached(stubClientID); ok {
			t.Errorf("%s: отказ попал в кэш", name)
		}
	}

	// Ответ дольше срока — отказ; срок сокращён, чтобы тест не ждал 5 секунд.
	stub, f, _ = newCIMDStub(t)
	f.Timeout = 200 * time.Millisecond
	stub.handler = func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		serveJSON(docJSON(stubClientID, "https://claude.ai/cb"), nil)(w, r)
	}
	start := time.Now()
	if _, err := f.Fetch(ctx, stubClientID); !isCategory(err, CIMDUnreachable) || time.Since(start) > time.Second {
		t.Fatalf("срок: %v за %s", err, time.Since(start))
	}

	// Сетевая ошибка наружу — только категория.
	_, f, _ = newCIMDStub(t)
	f.Dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("dial tcp 203.0.113.10:443: connect: connection refused")
	}
	_, err := f.Fetch(ctx, stubClientID)
	if !isCategory(err, CIMDUnreachable) || strings.Contains(err.Error(), "refused") {
		t.Fatalf("сеть: %v", err)
	}
}

func serveHTML(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}
}

func isCategory(err error, category string) bool {
	var ce *CIMDError
	return errors.As(err, &ce) && ce.Category == category
}

func TestForbiddenAddr(t *testing.T) {
	for _, ok := range []string{"203.0.113.10", "8.8.8.8", "2001:4860:4860::8888", "160.79.104.1",
		// Соседи новых диапазонов и NAT64 известного префикса с публичным IPv4.
		"2003::1", "64:ff9a:ffff:ffff:ffff:ffff:ffff:ffff", "2001:4860::1", "2001:1::1", "64:ff9b::808:808",
		"64:ff9b::cb00:710a", "::1:0:0:0", "::ffff:8.8.8.8"} {
		if ForbiddenAddr(netip.MustParseAddr(ok)) {
			t.Errorf("%s запрещён", ok)
		}
	}
	for _, bad := range []string{
		// NAT64 известного префикса (RFC 6052): встроенный IPv4 из каждого
		// запрещённого диапазона.
		"64:ff9b::a00:1", "64:ff9b::6440:1", "64:ff9b::7f00:1", "64:ff9b::a9fe:a9fe", "64:ff9b::ac10:1",
		"64:ff9b::c0a8:101", "64:ff9b::c612:1", "64:ff9b::1", "64:ff9b::e000:1", "64:ff9b::f000:1",
		"64:ff9b::", "64:ff9b::ffff:ffff", "64:ff9b::10.0.0.1",
		// NAT64 локального использования (RFC 8215) — целиком.
		"64:ff9b:1::a00:1", "64:ff9b:1::808:808", "64:ff9b:1::", "64:ff9b:1:ffff:ffff:ffff:ffff:ffff",
		// 6to4 (RFC 3056) — целиком.
		"2002:a00:1::1", "2002:808:808::1", "2002::", "2002:ffff:ffff:ffff:ffff:ffff:ffff:ffff",
		// Teredo (RFC 4380) и IPv4-compatible (RFC 4291) — целиком.
		"2001:0:4136:e378:8000:63bf:3fff:fdd2", "2001::", "2001:0:ffff:ffff:ffff:ffff:ffff:ffff",
		"::a00:1", "::808:808", "::ffff:ffff",
		// Зона: Prefix.Contains с ней не совпадает ни с чем.
		"fe80::1%eth0", "::1%lo", "2001:4860:4860::8888%eth0",
		// IPv4-mapped от встроенного: запись не открывает путь в обход.
		"::ffff:10.0.0.1",
	} {
		if !ForbiddenAddr(netip.MustParseAddr(bad)) {
			t.Errorf("%s не запрещён", bad)
		}
	}
	if !ForbiddenAddr(netip.Addr{}) {
		t.Error("пустой адрес не запрещён")
	}
}

// Годный NAT64 известного префикса: соединение идёт на сам IPv6-адрес, а не
// на встроенный IPv4, и документ загружается как обычно.
func TestFetchClientDocumentNAT64(t *testing.T) {
	stub, f, res := newCIMDStub(t)
	res.answers = [][]netip.Addr{{netip.MustParseAddr("64:ff9b::808:808")}}
	stub.handler = serveJSON(docJSON(stubClientID, "https://claude.ai/cb"), nil)
	doc, err := f.Fetch(context.Background(), stubClientID)
	if err != nil || doc.Name != "Claude" {
		t.Fatalf("NAT64: %+v %v", doc, err)
	}
	if len(stub.dialed) != 1 || stub.dialed[0] != "[64:ff9b::808:808]:443" {
		t.Fatalf("NAT64: соединения %v", stub.dialed)
	}
}

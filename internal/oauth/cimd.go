package oauth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Пределы загрузки документа клиента (решение 0079, п. 5).
const (
	CIMDMaxBytes = 5 << 10
	CIMDTimeout  = 5 * time.Second
	cimdMinTTL   = 5 * time.Minute
	cimdMaxTTL   = 24 * time.Hour
	cimdDefTTL   = time.Hour
	// Предел записей кэша: client_id приходит из запроса, и без предела
	// память процесса росла бы от перебора адресов.
	cimdCacheMax = 1000
)

// CIMDError — отказ документа клиента. Наружу уходит только категория:
// ни тела ответа цели, ни текста сетевой ошибки — иначе authorize стал бы
// сканером внутренней сети.
type CIMDError struct {
	Category string
	// Cause — для лога сервера, не для ответа.
	Cause error
}

func (e *CIMDError) Error() string { return "документ клиента: " + e.Category }

func (e *CIMDError) Unwrap() error { return e.Cause }

// Категории отказа.
const (
	CIMDBadURL      = "client_id не годится как адрес документа"
	CIMDForbidden   = "адрес документа запрещён: внутренняя или служебная сеть"
	CIMDUnreachable = "документ не загрузился"
	CIMDBadResponse = "ответ сервера клиента не годится: не 200 с JSON"
	CIMDTooLarge    = "документ больше 5 КиБ"
	CIMDInvalid     = "документ клиента не прошёл проверку"
)

func cimdFail(category string, cause error) error {
	return &CIMDError{Category: category, Cause: cause}
}

// ClientDocument — принятый документ клиента.
type ClientDocument struct {
	ClientID     string
	Name         string
	RedirectURIs []string
}

// CheckClientIDURL — client_id годится как адрес CIMD: https, хост, путь
// не пустой, без фрагмента, логина и пароля.
func CheckClientIDURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.Fragment != "" || strings.Contains(raw, "#") || u.Path == "" || u.Path == "/" {
		return nil, cimdFail(CIMDBadURL, err)
	}
	return u, nil
}

// LooksLikeURL — client_id — адрес, а не id регистрации DCR: id DCR —
// base64url без двоеточия.
func LooksLikeURL(clientID string) bool { return strings.Contains(clientID, "://") }

// Resolver — разрешение имени; подменяется в тестах.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Fetcher загружает и кэширует документы клиентов. Один на процесс api:
// реплики кэшируют каждая своё (решение 0079, п. 5).
type Fetcher struct {
	Resolver Resolver
	// Dial — соединение с уже проверенным адресом; nil — обычный net.Dialer.
	// Тест подменяет его, чтобы «публичный» адрес вёл на заглушку.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// RootCAs — корни TLS; nil — системные. Тест кладёт сертификат заглушки.
	RootCAs *x509.CertPool
	Now     func() time.Time
	// Timeout — срок загрузки целиком; ноль — CIMDTimeout. Тест сокращает
	// его, чтобы проверить отказ по сроку без пятисекундного ожидания.
	Timeout time.Duration

	mu    sync.Mutex
	cache map[string]cimdEntry
}

type cimdEntry struct {
	doc     ClientDocument
	expires time.Time
}

func NewFetcher() *Fetcher {
	return &Fetcher{Resolver: net.DefaultResolver}
}

func (f *Fetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Cached — документ из кэша без похода в сеть; false — нет или истёк.
func (f *Fetcher) Cached(clientID string) (ClientDocument, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.cache[clientID]
	if !ok || !f.now().Before(e.expires) {
		return ClientDocument{}, false
	}
	return e.doc, true
}

// Fetch отдаёт документ клиента: из кэша или загрузкой с проверками.
func (f *Fetcher) Fetch(ctx context.Context, clientID string) (ClientDocument, error) {
	if doc, ok := f.Cached(clientID); ok {
		return doc, nil
	}
	u, err := CheckClientIDURL(clientID)
	if err != nil {
		return ClientDocument{}, err
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = CIMDTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	addr, err := f.resolve(ctx, u.Hostname())
	if err != nil {
		return ClientDocument{}, err
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	target := net.JoinHostPort(addr.String(), port)

	dial := f.Dial
	if dial == nil {
		d := &net.Dialer{}
		dial = d.DialContext
	}
	transport := &http.Transport{
		// Прокси из окружения не берётся: проверенный адрес — это адрес
		// соединения, а прокси увёл бы его туда, где проверки нет.
		Proxy: nil,
		// Адрес соединения — проверенный IP, без повторного разрешения
		// имени: защита от DNS rebinding. TLS проверяется по имени из URL.
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dial(ctx, network, target)
		},
		TLSClientConfig:        &tls.Config{RootCAs: f.RootCAs, MinVersion: tls.VersionTLS12},
		ResponseHeaderTimeout:  timeout,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// Редиректы не следуются: цель редиректа не проверялась.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return ClientDocument{}, cimdFail(CIMDBadURL, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ClientDocument{}, cimdFail(CIMDUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ClientDocument{}, cimdFail(CIMDBadResponse, errors.New(resp.Status))
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if media != "application/json" {
		return ClientDocument{}, cimdFail(CIMDBadResponse, errors.New("content-type "+media))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, CIMDMaxBytes+1))
	if err != nil {
		return ClientDocument{}, cimdFail(CIMDUnreachable, err)
	}
	if len(body) > CIMDMaxBytes {
		return ClientDocument{}, cimdFail(CIMDTooLarge, nil)
	}
	doc, err := parseClientDocument(clientID, u.Hostname(), body)
	if err != nil {
		return ClientDocument{}, err
	}
	f.store(clientID, doc, cacheTTL(resp.Header.Get("Cache-Control")))
	return doc, nil
}

// resolve разрешает имя и проверяет каждый полученный адрес: хоть один
// запрещённый — отказ целиком. Соединение пойдёт на первый.
func (f *Fetcher) resolve(ctx context.Context, host string) (netip.Addr, error) {
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if ForbiddenAddr(ip) {
			return netip.Addr{}, cimdFail(CIMDForbidden, nil)
		}
		return ip, nil
	}
	r := f.Resolver
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return netip.Addr{}, cimdFail(CIMDUnreachable, err)
	}
	for _, a := range addrs {
		if ForbiddenAddr(a) {
			return netip.Addr{}, cimdFail(CIMDForbidden, nil)
		}
	}
	return addrs[0].Unmap(), nil
}

// forbiddenPrefixes — петля, частные и служебные диапазоны (решение 0079,
// п. 5). 224/4 «и выше» — 224.0.0.0/3 целиком.
//
// Адреса IPv6 со встроенным IPv4, позицию которого по адресу не
// определить или который незачем разбирать, запрещены целиком:
// 64:ff9b:1::/48 — NAT64 локального использования (RFC 8215, длину
// префикса выбирает оператор), 2002::/16 — 6to4 (RFC 3056, выведен из
// употребления RFC 7526), 2001::/32 — Teredo (RFC 4380), ::/96 —
// IPv4-compatible (RFC 4291, выведен из употребления; покрывает и ::, ::1).
// Известный префикс NAT64 — nat64WellKnown ниже.
var forbiddenPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3",
		"::/128", "::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
		"64:ff9b:1::/48", "2002::/16", "2001::/32", "::/96",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// nat64WellKnown — известный префикс NAT64 (RFC 6052): IPv4 — последние
// 32 бита адреса. Адрес проверяется по встроенному IPv4 тем же списком, а
// не запрещается целиком: так DNS64 в сети только с IPv6 синтезирует адрес
// любого хоста без AAAA, и полный запрет выключил бы там CIMD. RFC 6052
// (п. 3.1) и сам запрещает транслировать этот префикс в не-глобальные IPv4.
var nat64WellKnown = netip.MustParsePrefix("64:ff9b::/96")

// ForbiddenAddr — адрес, куда сервер документа клиента не ходит.
// IPv4-mapped (::ffff:10.0.0.1) и известный префикс NAT64
// (64:ff9b::10.0.0.1) проверяются по встроенному IPv4. Адрес с зоной
// (fe80::1%eth0) запрещён: зона бывает только у адресов внутри канала, а
// netip.Prefix.Contains с зоной не совпадает ни с одним префиксом.
func ForbiddenAddr(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return true
	}
	a = a.Unmap()
	if nat64WellKnown.Contains(a) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	for _, p := range forbiddenPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

type clientDocumentJSON struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func parseClientDocument(clientID, host string, body []byte) (ClientDocument, error) {
	var raw clientDocumentJSON
	if err := json.Unmarshal(body, &raw); err != nil {
		return ClientDocument{}, cimdFail(CIMDInvalid, err)
	}
	if raw.ClientID != clientID {
		return ClientDocument{}, cimdFail(CIMDInvalid, errors.New("client_id документа не равен его адресу"))
	}
	if len(raw.RedirectURIs) == 0 || len(raw.RedirectURIs) > MaxRedirectURIs {
		return ClientDocument{}, cimdFail(CIMDInvalid, errors.New("redirect_uris пуст или слишком длинный"))
	}
	for _, r := range raw.RedirectURIs {
		if err := CheckRedirectURI(r); err != nil {
			return ClientDocument{}, cimdFail(CIMDInvalid, err)
		}
	}
	if raw.TokenEndpointAuthMethod != "" && raw.TokenEndpointAuthMethod != "none" {
		return ClientDocument{}, cimdFail(CIMDInvalid, errors.New("клиент не публичный"))
	}
	name := CleanName(raw.ClientName)
	if name == "" {
		name = host
	}
	return ClientDocument{ClientID: clientID, Name: name, RedirectURIs: raw.RedirectURIs}, nil
}

// cacheTTL — срок кэша из Cache-Control: max-age в пределах 5 минут — 24
// часов, без него — час.
func cacheTTL(header string) time.Duration {
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || !strings.EqualFold(k, "max-age") {
			continue
		}
		n, err := strconv.Atoi(strings.Trim(v, `"`))
		if err != nil {
			break
		}
		return min(max(time.Duration(n)*time.Second, cimdMinTTL), cimdMaxTTL)
	}
	return cimdDefTTL
}

func (f *Fetcher) store(clientID string, doc ClientDocument, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		f.cache = map[string]cimdEntry{}
	}
	now := f.now()
	if len(f.cache) >= cimdCacheMax {
		for k, e := range f.cache {
			if !now.Before(e.expires) {
				delete(f.cache, k)
			}
		}
		// Всё ещё полон — уходит произвольная запись: промах стоит одной
		// загрузки, а не отказа.
		for k := range f.cache {
			if len(f.cache) < cimdCacheMax {
				break
			}
			delete(f.cache, k)
		}
	}
	f.cache[clientID] = cimdEntry{doc: doc, expires: now.Add(ttl)}
}

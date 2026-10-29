package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Каналы журнала: каким транспортом пришёл вызов.
const (
	AuditMCP  = "mcp"
	AuditREST = "rest"
)

// AuditOK — исход успешного вызова; иначе в исходе код apperr.
const AuditOK = "ok"

// Пределы параметров записи журнала. Журнал — след, а не копия запроса:
// длинную строку (инструкцию роли в связке, описание задачи) заменяет её
// размер и sha256, и запись не разрастается до тела запроса.
const (
	auditMaxString = 4 << 10
	auditMaxParams = 16 << 10
)

// auditRedacted подставляется вместо значения секрета.
const auditRedacted = "[redacted]"

// auditSecretKeys — ключи, значения которых не пишутся никогда, на любой
// глубине параметров. Сравнение без учёта регистра.
var auditSecretKeys = map[string]bool{
	"authorization": true, "token": true, "secret": true, "password": true,
	"client_secret": true, "private_key": true, "api_key": true, "code": true,
	"code_verifier": true, "id_token": true, "access_token": true, "refresh_token": true,
}

// auditSecretParams — имена параметров строки запроса и фрагмента ссылки,
// значения которых не пишутся (CLAB-226): все ключи auditSecretKeys и эти.
// Имя сравнивается нормализованным — в нижнем регистре и без знаков, только
// буквы и цифры: api-key, api_key и ApiKey — одно имя apikey.
var auditSecretParams = map[string]bool{
	"key": true, "sig": true, "pass": true, "pwd": true, "auth": true, "authkey": true,
	"authorization": true, "code": true, "codeverifier": true, "session": true, "sid": true,
	"otp": true, "hmac": true,
}

// auditSecretParamParts — подстроки нормализованного имени, по которым
// параметр ссылки считается секретным: sas_token, X-Goog-Signature,
// x-api-key, AWSAccessKeyId, client_secret, PHPSESSID.
var auditSecretParamParts = []string{
	"token", "secret", "passw", "signature", "credential", "apikey", "accesskey",
	"privatekey", "sessionid", "sessid", "jwt", "bearer",
}

// auditSecretParamPrefix — подписанная ссылка S3 (X-Amz-*, нормализованно
// xamz…): вырезаются все параметры, половина подписи без даты и ключа
// бесполезна, а оставлять её незачем.
const auditSecretParamPrefix = "xamz"

// Ссылки внутри строки параметра. Граница ссылки — пробел, кавычка или
// угловая скобка.
var (
	// auditURL — ссылка со схемой по RFC 3986 или без неё, начиная с "//".
	auditURL = regexp.MustCompile("(?:[A-Za-z][A-Za-z0-9+.-]*:)?//[^\\s\"'`<>«»]+")
	// auditLooseUserinfo — userinfo с паролем, в котором стоят
	// неэкранированные "/" или кавычка: url.Parse такой пароль не берёт, а
	// человек вставляет. Пароль — до первой "@" после двоеточия.
	auditLooseUserinfo = regexp.MustCompile("((?:[A-Za-z][A-Za-z0-9+.-]*:)?//)[^\\s/?#@\"'`<>«»:]+:[^\\s?#@]*@")
	// auditBareUserinfo — ссылка без схемы с user:password@host, за которым
	// идёт путь, запрос, фрагмент или порт. Без этого хвоста user:x@host не
	// отличить от «Почта:ivan@example.com», и строка остаётся как есть.
	auditBareUserinfo = regexp.MustCompile("(^|[^A-Za-z0-9._~%+:/@-])[A-Za-z0-9._~%+-]+:[^\\s/?#\"'`<>«»]+@((?:\\[[0-9A-Fa-f:.]+\\]|[A-Za-z0-9.-]+)(?::[0-9]+[/?#]?|[/?#]))")
	// auditBareQuery — ссылка без схемы со строкой запроса или фрагментом:
	// github.com/x?token=…, /cb#access_token=….
	auditBareQuery = regexp.MustCompile("[^\\s\"'`<>«»?#]+[?#][^\\s\"'`<>«»]+")
	// auditEncoded — кусок в процентной записи между разделителями ссылки:
	// закодированная ссылка в параметре (?next=https%3A%2F%2Fu%3Ap%40h) или
	// закодированное "=" (token%3Dvalue).
	auditEncoded = regexp.MustCompile("[^\\s\"'`<>«»&=?#/]*%[0-9A-Fa-f]{2}[^\\s\"'`<>«»&=?#/]*")
)

// auditBodyKeys — тела артефактов и документов: вместо содержимого — размер
// и sha256, по которому запись сверяется с blobs.
var auditBodyKeys = map[string]bool{"content": true}

// AuditEntry — одно действие к записи.
type AuditEntry struct {
	Target   postgres.AuditTarget
	Identity Identity
	Channel  string
	Action   string
	// Параметры как пришли: писатель сам чистит их от секретов, тел и
	// учётных данных внутри ссылок.
	Params     any
	Outcome    string
	HTTPStatus int
	RequestID  string
}

// auditSink — куда пишется журнал. Интерфейс, а не *postgres.Store, ради
// одного теста: отказ хранилища журнала не должен менять ответ вызова.
type auditSink interface {
	InsertAudit(ctx context.Context, q postgres.Querier, r postgres.NewAuditRecord) error
	DB() postgres.Querier
}

// Audit — единственный писатель журнала действий (CLAB-210, решение 0049).
// Через него пишут оба транспорта — обёртка тулов MCP и middleware REST — и
// сервисы, у которых есть свои доменные события (выдача секрета, ротация
// токена): своей таблицы им заводить не нужно.
//
// Журнал — след, а не транзакция действия: запись идёт после ответа
// обработчика, отдельным запросом, и её сбой вызов не роняет — только лог
// уровня error с request_id и счётчик сбоев.
type Audit struct {
	store     *postgres.Store
	sink      auditSink
	log       *slog.Logger
	onFailure func()
}

func NewAudit(store *postgres.Store, log *slog.Logger) *Audit {
	return &Audit{store: store, sink: store, log: log}
}

// FailingSinkForTest подменяет хранилище записи журнала отказывающим —
// только для проверки, что сбой журнала не меняет ответ вызова.
func (a *Audit) FailingSinkForTest(err error) {
	a.sink = failingSink{store: a.store, err: err}
}

type failingSink struct {
	store *postgres.Store
	err   error
}

func (f failingSink) InsertAudit(context.Context, postgres.Querier, postgres.NewAuditRecord) error {
	return f.err
}

func (f failingSink) DB() postgres.Querier { return f.store.DB() }

// List отдаёт страницу журнала. Права проверяет транспорт guard'ом до
// вызова: задача и проект — maintainer, инстанс — администратор системы.
func (a *Audit) List(ctx context.Context, f postgres.AuditFilter) (postgres.AuditPage, error) {
	return a.store.ListAudit(ctx, a.store.DB(), f)
}

// RejectedIdentity — чей токен отбит при аутентификации: отозванный,
// истёкший, отключённого принципала. Нужен только журналу, чтобы отказ 401
// был записан со ссылкой на токен; токена нет в таблице — false.
func (a *Auth) RejectedIdentity(ctx context.Context, token string) (Identity, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Identity{}, false
	}
	t, p, err := a.store.TokenByHashAny(ctx, a.store.DB(), HashToken(token))
	if err != nil {
		return Identity{}, false
	}
	return Identity{Principal: p, Token: &t}, true
}

// OnFailure задаёт счётчик сбоев записи (метрика api).
func (a *Audit) OnFailure(fn func()) { a.onFailure = fn }

// Record пишет действие. Ошибок наружу не отдаёт.
func (a *Audit) Record(ctx context.Context, e AuditEntry) {
	if a == nil {
		return
	}
	// Запрос мог уже закончиться (клиент закрыл соединение) — запись о нём
	// всё равно нужна.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	rec := postgres.NewAuditRecord{
		Target:    e.Target,
		Channel:   e.Channel,
		Action:    e.Action,
		Params:    SanitizeAuditParams(e.Params),
		Outcome:   e.Outcome,
		RequestID: e.RequestID,
	}
	if rec.Outcome == "" {
		rec.Outcome = AuditOK
	}
	if e.Identity.Principal.ID != uuid.Nil {
		id := e.Identity.Principal.ID
		rec.PrincipalID = &id
	}
	if t := e.Identity.Token; t != nil {
		id, name, prefix, kind := t.ID, t.Name, t.Prefix, t.TokenKind
		if id != uuid.Nil {
			rec.TokenID = &id
		}
		rec.TokenName, rec.TokenPrefix = &name, &prefix
		if kind != "" {
			rec.TokenKind = &kind
		}
	}
	if e.HTTPStatus != 0 {
		st := e.HTTPStatus
		rec.HTTPStatus = &st
	}
	if err := a.sink.InsertAudit(ctx, a.sink.DB(), rec); err != nil {
		a.log.Error("запись журнала действий не удалась",
			"error", err, "action", e.Action, "channel", e.Channel,
			"request_id", e.RequestID)
		if a.onFailure != nil {
			a.onFailure()
		}
	}
}

// SanitizeAuditParams превращает параметры вызова в то, что можно хранить:
// секреты — "[redacted]", тела (content) и строки длиннее 4 КБ — размер и
// sha256, всё целиком — не больше 16 КБ. Секрет ищется по имени ключа, а в
// коротких строках — ещё и по содержимому: у ссылок — со схемой, от "//",
// без схемы и в процентной записи — вырезаются userinfo и значения
// секретных параметров запроса и фрагмента (CLAB-226).
func SanitizeAuditParams(params any) json.RawMessage {
	if params == nil {
		return json.RawMessage(`{}`)
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return json.RawMessage(`{"_unserializable":true}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Числа как есть: float64 исказил бы большие идентификаторы.
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return digestJSON(raw)
	}
	v = sanitizeValue(v, "")
	out, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{"_unserializable":true}`)
	}
	if len(out) <= auditMaxParams {
		return out
	}
	return truncateParams(v, out)
}

func sanitizeValue(v any, key string) any {
	lk := strings.ToLower(key)
	if auditSecretKeys[lk] {
		if v == nil {
			return nil
		}
		return auditRedacted
	}
	if auditBodyKeys[lk] && v != nil {
		if s, ok := v.(string); ok {
			return digest([]byte(s))
		}
		raw, _ := json.Marshal(v)
		return digest(raw)
	}
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			x[k] = sanitizeValue(val, k)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = sanitizeValue(val, "")
		}
		return x
	case string:
		if len(x) > auditMaxString {
			return digest([]byte(x))
		}
		return redactURLs(x)
	}
	return v
}

// redactURLs чистит ссылки внутри строки: и строку-ссылку целиком, и ссылку,
// вставленную в текст. Текст вокруг ссылки не меняется; если вырезать
// нечего, строка возвращается та же, байт в байт: каждый проход меняет
// только секрет — userinfo или значение секретного параметра.
func redactURLs(s string) string {
	if !strings.ContainsAny(s, "/@?#%") {
		return s
	}
	if strings.Contains(s, "//") {
		s = auditLooseUserinfo.ReplaceAllStringFunc(s, redactLooseUserinfo)
		s = redactNestedURLs(s)
	}
	// Остальные проходы идут по строке целиком один раз: регулярное
	// выражение находит и то, что лежит внутри ссылки.
	if strings.Contains(s, "@") {
		s = auditBareUserinfo.ReplaceAllString(s, "${1}"+auditRedacted+"@${2}")
	}
	if strings.ContainsAny(s, "?#") {
		s = auditBareQuery.ReplaceAllStringFunc(s, redactBareQuery)
	}
	if strings.Contains(s, "%") {
		s = auditEncoded.ReplaceAllStringFunc(s, redactEncoded)
	}
	return s
}

// redactNestedURLs — проход по ссылкам со схемой и от "//": redactURL
// вызывает его на пути, запросе и фрагменте, чтобы найти ссылку внутри
// ссылки. Остаток короче, рекурсия кончается.
func redactNestedURLs(s string) string {
	if !strings.Contains(s, "//") {
		return s
	}
	return auditURL.ReplaceAllStringFunc(s, redactURL)
}

// redactLooseUserinfo вырезает userinfo, в пароле которого стоит "/" или
// кавычка (https://u:p/ss@h). Если до первого такого знака после двоеточия
// одни цифры, это порт, а "@" дальше — в пути или в соседней строке
// (http://localhost:3000/@vite/x, "https://h:8080","me@x"): такая ссылка
// не трогается.
func redactLooseUserinfo(m string) string {
	head := m[:strings.Index(m, "//")+2]
	rest := m[len(head):]
	pass := rest[strings.IndexByte(rest, ':')+1 : len(rest)-1]
	if j := strings.IndexAny(pass, "/\"'`<>«»"); j >= 0 && strings.Trim(pass[:j], "0123456789") == "" {
		return m
	}
	return head + auditRedacted + "@"
}

// redactURL режет ссылку по RFC 3986 на сырые сегменты и меняет только
// секреты: userinfo — на "[redacted]@", значения секретных параметров — на
// "[redacted]". Разбор строковый, без url.Parse: ссылка, которую стандартный
// разбор не берёт (порт не числом, неэкранированные символы), чистится так
// же, а прочие параметры не перекодируются и не пересортировываются.
func redactURL(u string) string {
	i := strings.Index(u, "//")
	head, rest := u[:i+2], u[i+2:]
	rest, fragment, hasFragment := strings.Cut(rest, "#")
	rest, query, hasQuery := strings.Cut(rest, "?")
	authority, path := rest, ""
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		authority, path = rest[:j], rest[j:]
	}
	// Userinfo — до последней "@" в authority, как у url.Parse: в пароле
	// "@" бывает неэкранированной. Токен GitHub работает и именем
	// пользователя, поэтому вырезается и userinfo без пароля.
	if at := strings.LastIndexByte(authority, '@'); at > 0 {
		authority = auditRedacted + authority[at:]
	}
	// Ссылка внутри ссылки — вторая через запятую или точку с запятой без
	// пробела, адрес возврата в параметре запроса (?next=https://u:p@h/) —
	// чистится так же: redactNestedURLs идёт по остатку, он короче,
	// рекурсия кончается.
	var b strings.Builder
	b.Grow(len(u))
	b.WriteString(head)
	b.WriteString(authority)
	b.WriteString(redactNestedURLs(path))
	if hasQuery {
		b.WriteByte('?')
		b.WriteString(redactNestedURLs(redactURLParams(query)))
	}
	if hasFragment {
		// Фрагмент вида k=v&… (неявный поток OAuth) — как строка запроса;
		// сегмент без "=" не трогается.
		b.WriteByte('#')
		b.WriteString(redactNestedURLs(redactURLParams(fragment)))
	}
	return b.String()
}

// redactBareQuery чистит строку запроса и фрагмент ссылки без схемы
// (github.com/x?token=…): всё до первого "?" или "#" остаётся как было.
func redactBareQuery(m string) string {
	i := strings.IndexAny(m, "?#")
	head, rest := m[:i+1], m[i+1:]
	if m[i] == '?' {
		if query, fragment, ok := strings.Cut(rest, "#"); ok {
			return head + redactURLParams(query) + "#" + redactURLParams(fragment)
		}
	}
	return head + redactURLParams(rest)
}

// redactEncoded раскодирует кусок в процентной записи и чистит его как
// обычную строку; голый k=v&k=v — как строку запроса. Вырезать нечего —
// кусок остаётся как был, байт в байт; иначе возвращается очищенный и
// закодированный заново (url.QueryEscape). Раскодированный кусок короче,
// поэтому двойная запись (%253A) раскрывается рекурсией и та кончается.
func redactEncoded(m string) string {
	dec, err := url.PathUnescape(m)
	if err != nil || dec == m {
		return m
	}
	red := redactURLs(dec)
	if !strings.ContainsAny(red, "/?#\t\n\r ") {
		red = redactURLParams(red)
	}
	if red == dec {
		return m
	}
	return url.QueryEscape(red)
}

// redactURLParams заменяет значения секретных параметров в сырой строке
// k=v&k=v; имена, прочие значения и порядок остаются как были. Сегмент с
// закодированным "=" (token%3Dvalue) разбирается так же.
func redactURLParams(raw string) string {
	segs := strings.Split(raw, "&")
	for i, seg := range segs {
		name, value, ok := strings.Cut(seg, "=")
		sep := "="
		if !ok {
			if j := indexEncodedEq(seg); j >= 0 {
				name, value, ok, sep = seg[:j], seg[j+3:], true, seg[j:j+3]
			}
		}
		if ok && value != "" && secretURLParam(name) {
			segs[i] = name + sep + auditRedacted
		}
	}
	return strings.Join(segs, "&")
}

// indexEncodedEq — позиция "%3D" или "%3d" в строке, -1 — нет.
func indexEncodedEq(s string) int {
	for i := 0; i+2 < len(s); i++ {
		if s[i] == '%' && s[i+1] == '3' && (s[i+2] == 'D' || s[i+2] == 'd') {
			return i
		}
	}
	return -1
}

// secretURLParam — имя параметра ссылки секретное: из auditSecretKeys или
// auditSecretParams, с секретной подстрокой или префиксом X-Amz-. Имя
// сравнивается раскодированным (access%5Ftoken) и нормализованным: нижний
// регистр, только буквы и цифры.
func secretURLParam(name string) bool {
	if dec, err := url.QueryUnescape(name); err == nil {
		name = dec
	}
	if auditSecretKeys[strings.ToLower(strings.TrimSpace(name))] {
		return true
	}
	n := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, name)
	if auditSecretParams[n] || strings.HasPrefix(n, auditSecretParamPrefix) {
		return true
	}
	for _, part := range auditSecretParamParts {
		if strings.Contains(n, part) {
			return true
		}
	}
	return false
}

func digest(b []byte) map[string]any {
	sum := sha256.Sum256(b)
	return map[string]any{"size": len(b), "sha256": hex.EncodeToString(sum[:])}
}

func digestJSON(b []byte) json.RawMessage {
	out, _ := json.Marshal(map[string]any{"_digest": digest(b)})
	return out
}

// truncateParams укладывает параметры в предел: простые поля верхнего уровня
// остаются, составные заменяются размером и sha256. Не уложилось и так —
// остаются только имена полей. Пометка _truncated говорит, что запись
// неполная.
func truncateParams(v any, full []byte) json.RawMessage {
	m, ok := v.(map[string]any)
	if !ok {
		out, _ := json.Marshal(map[string]any{"_truncated": true, "_digest": digest(full)})
		return out
	}
	short := map[string]any{"_truncated": true}
	keys := make([]string, 0, len(m))
	for k, val := range m {
		keys = append(keys, k)
		switch val.(type) {
		case map[string]any, []any:
			raw, _ := json.Marshal(val)
			short[k] = digest(raw)
		default:
			short[k] = val
		}
	}
	out, _ := json.Marshal(short)
	if len(out) <= auditMaxParams {
		return out
	}
	sort.Strings(keys)
	out, _ = json.Marshal(map[string]any{"_truncated": true, "_digest": digest(full), "_keys": keys})
	return out
}

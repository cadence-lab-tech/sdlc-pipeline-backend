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
// Здесь имя сравнивается целиком, нормализованным — в нижнем регистре и без
// знаков, только буквы и цифры: ?key=, ?sig=, ?Auth= — секрет, а key_id,
// sig_alg, auth_type и session_timeout — нет.
var auditSecretParams = map[string]bool{
	"key": true, "sig": true, "pass": true, "pwd": true, "auth": true, "authkey": true,
	"authorization": true, "code": true, "codeverifier": true, "session": true, "sid": true,
	"otp": true, "hmac": true,
}

// auditSecretParamWords — слова, по которым параметр ссылки считается
// секретным, где бы в имени они ни стояли: sas_token, X-Goog-Signature,
// db_password, GITHUB_TOKEN. Имя режется на слова по знакам и по смене
// регистра (AccessKeyId — access, key, id), и слово совпадает целиком или
// склейкой нескольких подряд: x-api-key и ApiKey — apikey. Подстрокой слово
// не ищется (решение человека, круг 3): tokens, max_tokens, tokenizer,
// passport и bypass — не секреты. Склеенные в одно слово имена, которые
// встречаются как есть (PHPSESSID, accesstoken), перечислены отдельно.
var auditSecretParamWords = map[string]bool{
	"token": true, "secret": true, "password": true, "passwd": true, "passw": true,
	"pass": true, "pwd": true, "signature": true, "credential": true, "credentials": true,
	"jwt": true, "bearer": true,
	"apikey": true, "accesskey": true, "privatekey": true, "secretkey": true, "authkey": true,
	"sessionid": true, "sessid": true, "phpsessid": true, "jsessionid": true,
	"accesstoken": true, "refreshtoken": true, "idtoken": true, "authtoken": true,
	"apitoken": true, "clientsecret": true, "apisecret": true,
}

// auditSecretWordMax — длина самого длинного слова перечня: склейка слов
// имени длиннее него уже ни с чем не совпадёт, и перебор на ней кончается.
var auditSecretWordMax = func() int {
	n := 0
	for w := range auditSecretParamWords {
		n = max(n, len(w))
	}
	return n
}()

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
	// github.com/x?token=…, /cb#access_token=…; что перед "?" не похоже на
	// хост или путь, отсеивает redactBareQuery.
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

// auditMaxDepth — предел вложенности разбора: ссылка внутри ссылки и
// процентная запись внутри процентной записи (CLAB-226, круг 3). Куски одного
// уровня не пересекаются, поэтому работа над строкой — несколько проходов по
// её длине на уровень, а уровней не больше auditMaxDepth: время растёт
// линейно с длиной строки и с размером тела. Без предела каждое "//" в пути
// было ещё одним уровнем, и строка из одних "//" разбиралась квадратично —
// 4 КБ за 50 мс, тело в 4 МБ из таких строк за 44 с. Живая ссылка глубже
// трёх-четырёх уровней не бывает (двойная процентная запись ссылки в
// параметре — четыре); что глубже — redactTooDeep.
const auditMaxDepth = 6

// redactURLs чистит ссылки внутри строки: и строку-ссылку целиком, и ссылку,
// вставленную в текст. Текст вокруг ссылки не меняется; если вырезать
// нечего, строка возвращается та же, байт в байт: каждый проход меняет
// только секрет — userinfo или значение секретного параметра.
func redactURLs(s string) string {
	return redactURLsAt(s, 0)
}

// redactURLsAt — redactURLs на уровне вложенности depth: с него
// начинается разбор раскодированного куска процентной записи.
func redactURLsAt(s string, depth int) string {
	if !strings.ContainsAny(s, "/@?#%") {
		return s
	}
	if strings.Contains(s, "//") {
		s = auditLooseUserinfo.ReplaceAllStringFunc(s, redactLooseUserinfo)
		s = redactNestedURLs(s, depth)
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
		s = auditEncoded.ReplaceAllStringFunc(s, func(m string) string {
			return redactEncoded(m, depth)
		})
	}
	return s
}

// redactNestedURLs — проход по ссылкам со схемой и от "//": redactURL
// вызывает его на пути, запросе и фрагменте уровнем глубже, чтобы найти
// ссылку внутри ссылки.
func redactNestedURLs(s string, depth int) string {
	if !strings.Contains(s, "//") {
		return s
	}
	if depth >= auditMaxDepth {
		return redactTooDeep(s)
	}
	return auditURL.ReplaceAllStringFunc(s, func(u string) string {
		return redactURL(u, depth)
	})
}

// redactTooDeep — кусок глубже auditMaxDepth уже не разбирается. Секрет в
// нём может лежать только при "@", "=" или "%" — тогда кусок заменяется
// целиком: в журнале лучше лишний [redacted], чем секрет. Без этих знаков
// кусок остаётся байт в байт, как и любая строка без секретов.
func redactTooDeep(s string) string {
	if !strings.ContainsAny(s, "@=%") {
		return s
	}
	return auditRedacted
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
func redactURL(u string, depth int) string {
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
	// чистится так же, уровнем глубже.
	var b strings.Builder
	b.Grow(len(u))
	b.WriteString(head)
	b.WriteString(authority)
	b.WriteString(redactNestedURLs(path, depth+1))
	if hasQuery {
		b.WriteByte('?')
		b.WriteString(redactNestedURLs(redactURLParams(query), depth+1))
	}
	if hasFragment {
		// Фрагмент вида k=v&… (неявный поток OAuth) — как строка запроса;
		// сегмент без "=" не трогается.
		b.WriteByte('#')
		b.WriteString(redactNestedURLs(redactURLParams(fragment), depth+1))
	}
	return b.String()
}

// redactBareQuery чистит строку запроса и фрагмент ссылки без схемы
// (github.com/x?token=…, /cb#access_token=…): всё до первого "?" или "#"
// остаётся как было. Ссылкой считается только то, перед чем стоит хост или
// путь — есть точка или "/" (решение человека, круг 3): в прозе «что?key=1»
// и «x?token=abc» строка не меняется.
func redactBareQuery(m string) string {
	i := strings.IndexAny(m, "?#")
	head, rest := m[:i+1], m[i+1:]
	if !strings.ContainsAny(head[:i], "./") {
		return m
	}
	if m[i] == '?' {
		if query, fragment, ok := strings.Cut(rest, "#"); ok {
			return head + redactURLParams(query) + "#" + redactURLParams(fragment)
		}
	}
	return head + redactURLParams(rest)
}

// redactEncoded раскодирует кусок в процентной записи и чистит его как
// обычную строку уровнем глубже; голый k=v&k=v — как строку запроса.
// Вырезать нечего — кусок остаётся как был, байт в байт; иначе возвращается
// очищенный и закодированный заново (url.QueryEscape). Двойная запись
// (%253A) раскрывается следующим уровнем; глубже auditMaxDepth кусок с
// процентами заменяется целиком, как в redactTooDeep.
func redactEncoded(m string, depth int) string {
	dec, err := url.PathUnescape(m)
	if err != nil || dec == m {
		return m
	}
	if depth >= auditMaxDepth {
		return auditRedacted
	}
	red := redactURLsAt(dec, depth+1)
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

// secretURLParam — имя параметра ссылки секретное: ключ auditSecretKeys,
// имя целиком из auditSecretParams, префикс X-Amz- или слово из
// auditSecretParamWords. Имя сравнивается раскодированным (access%5Ftoken).
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
	// Каждое слово перечня содержит один из этих кусков: без них имя не
	// режется на слова — так дешевле на длинной строке параметров.
	if !containsAny(n, auditSecretWordHints) {
		return false
	}
	words := paramWords(name)
	for i := range words {
		joined := ""
		for _, w := range words[i:] {
			joined += w
			if len(joined) > auditSecretWordMax {
				break
			}
			if auditSecretParamWords[joined] {
				return true
			}
		}
	}
	return false
}

// auditSecretWordHints — куски, из которых состоит каждое слово
// auditSecretParamWords; TestSecretWordHints держит их в согласии.
var auditSecretWordHints = []string{"token", "secret", "pass", "pwd", "signature", "credential", "jwt", "bearer", "key", "sess"}

func containsAny(s string, parts []string) bool {
	for _, p := range parts {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// paramWords режет имя параметра на слова в нижнем регистре: по любому
// знаку, кроме букв и цифр, и по смене регистра — accessToken, AccessKeyId,
// AWSAccessKeyId (aws, access, key, id). Подряд идущие заглавные — одно
// слово: PHPSESSID, API.
func paramWords(name string) []string {
	rs := []rune(name)
	var words []string
	start := -1
	flush := func(end int) {
		if start >= 0 {
			words = append(words, strings.ToLower(string(rs[start:end])))
			start = -1
		}
	}
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush(i)
			continue
		}
		if start >= 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush(i)
			}
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(rs))
	return words
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

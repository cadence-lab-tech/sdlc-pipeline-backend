package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"time"

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

// auditBodyKeys — тела артефактов и документов: вместо содержимого — размер
// и sha256, по которому запись сверяется с blobs.
var auditBodyKeys = map[string]bool{"content": true}

// AuditEntry — одно действие к записи.
type AuditEntry struct {
	Target   postgres.AuditTarget
	Identity Identity
	Channel  string
	Action   string
	// Параметры как пришли: писатель сам чистит их от секретов и тел.
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
// sha256, всё целиком — не больше 16 КБ.
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
		return x
	}
	return v
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

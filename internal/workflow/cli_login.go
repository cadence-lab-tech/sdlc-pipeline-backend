// Вход CLI через браузер (решение 0032).
//
// Схема та же, что device flow у OAuth: CLI заводит запрос и получает код
// для человека и секрет для себя; человек подтверждает код в интерфейсе,
// где он уже вошёл; CLI опрашивает запрос секретом и в момент подтверждения
// получает обычный долгоживущий токен — такой же, как выданный руками в
// настройках. Провайдер входа здесь не важен: CLI доверяет тому, что
// интерфейс уже проверил, кто перед ним.
package workflow

import (
	"context"
	"crypto/rand"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

const (
	// Сколько живёт запрос: человеку хватит открыть браузер и нажать кнопку.
	cliLoginTTL = 10 * time.Minute
	// Как часто CLI можно опрашивать запрос.
	cliLoginPollInterval = 3 * time.Second
	// Через сколько после закрытия строка запроса убирается.
	cliLoginPurgeAfter = time.Hour
	// Алфавит кода без букв и цифр, которые путают на слух и на глаз.
	cliCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	// Верхняя граница срока токена, о котором может попросить CLI.
	cliTokenMaxDays = 3660
)

// CLILoginStart — что CLI получает при старте входа. Секрет опроса виден
// один раз, как и токен: в базе только его хеш.
type CLILoginStart struct {
	UserCode   string    `json:"user_code"`
	PollSecret string    `json:"poll_secret"`
	ExpiresAt  time.Time `json:"expires_at"`
	// Путь страницы подтверждения в интерфейсе; CLI подставляет свой хост.
	VerifyPath      string `json:"verify_path"`
	IntervalSeconds int    `json:"interval_seconds"`
}

// CLILoginPoll — ответ на опрос. Токен приходит один раз, при status =
// approved; дальше запрос закрыт.
type CLILoginPoll struct {
	Status string `json:"status"`
	// Заполнено при approved.
	Token     string              `json:"token,omitempty"`
	Record    *domain.AccessToken `json:"record,omitempty"`
	Principal *domain.Principal   `json:"principal,omitempty"`
}

// StartCLILogin заводит запрос входа. Открыт для всех: у CLI токена ещё
// нет, а завести запрос — не значит войти.
func (a *Auth) StartCLILogin(ctx context.Context, label string, tokenDays int) (CLILoginStart, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "clab"
	}
	if len(label) > 120 {
		label = label[:120]
	}
	if tokenDays < 0 || tokenDays > cliTokenMaxDays {
		return CLILoginStart{}, apperr.InvalidInput("срок токена: от 0 (бессрочно) до %d дней", cliTokenMaxDays)
	}

	code, err := newUserCode()
	if err != nil {
		return CLILoginStart{}, err
	}
	secret, hash, _, err := NewToken()
	if err != nil {
		return CLILoginStart{}, err
	}
	expires := time.Now().Add(cliLoginTTL)

	q := a.store.DB()
	if err := a.store.PurgeCLILogins(ctx, q, cliLoginPurgeAfter); err != nil {
		a.log.Warn("старые запросы входа CLI не убраны", "error", err)
	}
	if _, err := a.store.CreateCLILogin(ctx, q, postgres.CreateCLILoginInput{
		UserCode: code, PollHash: hash, Label: label, TokenDays: tokenDays, ExpiresAt: expires,
	}); err != nil {
		return CLILoginStart{}, err
	}
	return CLILoginStart{
		UserCode: code, PollSecret: secret, ExpiresAt: expires,
		VerifyPath: "/login/cli", IntervalSeconds: int(cliLoginPollInterval / time.Second),
	}, nil
}

// CLILoginByCode — запрос для страницы подтверждения: подпись, срок и
// состояние. Требует вошедшего: чужие коды перебирать без входа нельзя.
func (a *Auth) CLILoginByCode(ctx context.Context, id Identity, code string) (domain.CLILoginRequest, error) {
	if err := a.requireLogin(id); err != nil {
		return domain.CLILoginRequest{}, err
	}
	return a.store.CLILoginByCode(ctx, a.store.DB(), NormalizeUserCode(code))
}

// ApproveCLILogin подтверждает запрос от имени вошедшего: токен достанется
// именно ему. Отказ закрывает запрос — CLI увидит denied и выйдет.
func (a *Auth) ApproveCLILogin(ctx context.Context, id Identity, code string, approve bool) (domain.CLILoginRequest, error) {
	if err := a.requireLogin(id); err != nil {
		return domain.CLILoginRequest{}, err
	}
	q := a.store.DB()
	req, err := a.store.CLILoginByCode(ctx, q, NormalizeUserCode(code))
	if err != nil {
		return domain.CLILoginRequest{}, err
	}
	if req.Status(time.Now()) == "expired" {
		return req, apperr.Conflict("код устарел: запустите clab auth login заново")
	}
	if approve {
		err = a.store.ApproveCLILogin(ctx, q, req.ID, id.Principal.ID)
	} else {
		err = a.store.DenyCLILogin(ctx, q, req.ID)
	}
	if err != nil {
		return req, err
	}
	a.log.Info("вход CLI подтверждён", "label", req.Label, "principal", id.Principal.Handle, "approved", approve)
	return a.store.CLILoginByCode(ctx, q, req.UserCode)
}

// PollCLILogin — опрос запроса секретом CLI. Пока человек не подтвердил —
// pending; после подтверждения выдаётся токен и запрос закрывается, так что
// второй опрос с тем же секретом токена уже не получит.
func (a *Auth) PollCLILogin(ctx context.Context, secret string) (CLILoginPoll, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return CLILoginPoll{}, apperr.InvalidInput("нужен секрет опроса")
	}
	q := a.store.DB()
	req, err := a.store.CLILoginByPollHash(ctx, q, HashToken(secret))
	if err != nil {
		return CLILoginPoll{}, err
	}
	status := req.Status(time.Now())
	if status != "approved" {
		return CLILoginPoll{Status: status}, nil
	}

	var out CLILoginPoll
	err = a.store.InTx(ctx, func(tx pgx.Tx) error {
		taken, err := a.store.ConsumeCLILogin(ctx, tx, req.ID)
		if err != nil {
			return err
		}
		if !taken {
			out = CLILoginPoll{Status: "consumed"}
			return nil
		}
		p, err := a.store.GetPrincipal(ctx, tx, *req.ApprovedBy)
		if err != nil {
			return err
		}
		raw, hash, prefix, err := NewToken()
		if err != nil {
			return err
		}
		var expires *time.Time
		if req.TokenDays > 0 {
			t := time.Now().Add(time.Duration(req.TokenDays) * 24 * time.Hour)
			expires = &t
		}
		record, err := a.store.CreateToken(ctx, tx, postgres.CreateTokenInput{
			PrincipalID: p.ID, Name: req.Label, TokenHash: hash, Prefix: prefix,
			ExpiresAt: expires, CreatedBy: p.ID,
		})
		if err != nil {
			return err
		}
		out = CLILoginPoll{Status: "approved", Token: raw, Record: &record, Principal: &p}
		return nil
	})
	return out, err
}

// requireLogin — действие доступно только вошедшему. В режиме заголовка
// доказательства нет ни у кого, и проверка сводится к наличию принципала.
func (a *Auth) requireLogin(id Identity) error {
	if a.Enforced() && !id.Authenticated() {
		return apperr.Unauthenticated("подтвердить вход CLI может только вошедший")
	}
	return nil
}

// NormalizeUserCode приводит код к виду из базы: заглавные буквы, дефис
// посередине. Человек может набрать его строчными или без дефиса.
func NormalizeUserCode(raw string) string {
	clean := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(raw)))
	if len(clean) != 8 {
		return strings.ToUpper(strings.TrimSpace(raw))
	}
	return clean[:4] + "-" + clean[4:]
}

func newUserCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", apperr.Internal(err, "генерация кода входа")
	}
	var b strings.Builder
	for i, v := range raw {
		if i == 4 {
			b.WriteByte('-')
		}
		b.WriteByte(cliCodeAlphabet[int(v)%len(cliCodeAlphabet)])
	}
	return b.String(), nil
}

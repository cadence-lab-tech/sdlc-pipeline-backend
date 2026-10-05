// Package apperr — ошибки домена с кодом, который транспорт превращает в
// HTTP-статус, а MCP-сервер (фаза 2) — в код ошибки из MCP_CONTRACT.md.
// Поэтому коды строковые и совпадают с контрактом агента, а не с net/http.
package apperr

import (
	"errors"
	"fmt"
)

type Code string

const (
	CodeNotFound             Code = "not_found"
	CodeInvalidInput         Code = "invalid_input"
	CodeConflict             Code = "conflict"
	CodeGatesNotSatisfied    Code = "gates_not_satisfied"
	CodeTransitionNotAllowed Code = "transition_not_allowed"
	CodeOutOfTaskScope       Code = "out_of_task_scope"
	CodeInternal             Code = "internal"

	// Коды работы агента (фаза 2). Названия — из таблицы ошибок
	// MCP_CONTRACT.md: агент разбирает ответ одинаково, пришёл он по MCP,
	// через CLI или из REST.
	CodeTaskNotClaimable        Code = "task_not_claimable"
	CodeLeaseHeldByOther        Code = "lease_held_by_other"
	CodeLeaseExpired            Code = "lease_expired"
	CodeContractViolation       Code = "contract_violation"
	CodeDriftUnresolved         Code = "drift_unresolved"
	CodeUnknownRepository       Code = "unknown_repository"
	CodeScopeExpansionForbidden Code = "scope_expansion_forbidden"

	// Аутентификация и права (фаза 4): токен не предъявлен или недействителен,
	// либо предъявлен, но прав на действие нет.
	CodeUnauthenticated Code = "unauthenticated"
	CodeForbidden       Code = "forbidden"

	// Провайдер не принял учётные данные самого инстанса (CLAB-78): например,
	// ключ GitHub App или часы машины. Не invalid_input — вызывающий это не
	// починит, и не internal — текст и есть польза: в нём сказано, что
	// проверить администратору инстанса.
	CodeProviderMisconfigured Code = "provider_misconfigured"
)

type Error struct {
	Code    Code
	Message string
	// Машиночитаемые подробности: какие гейты не прошли, каких артефактов нет.
	// Уходит в тело ответа как есть — по нему агент понимает, что доделать.
	Details any
	cause   error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.cause }

func (e *Error) WithDetails(d any) *Error {
	e.Details = d
	return e
}

func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func Wrap(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), cause: cause}
}

func NotFound(format string, args ...any) *Error {
	return New(CodeNotFound, format, args...)
}

func InvalidInput(format string, args ...any) *Error {
	return New(CodeInvalidInput, format, args...)
}

func Conflict(format string, args ...any) *Error {
	return New(CodeConflict, format, args...)
}

func Unauthenticated(format string, args ...any) *Error {
	return New(CodeUnauthenticated, format, args...)
}

func Forbidden(format string, args ...any) *Error {
	return New(CodeForbidden, format, args...)
}

func Internal(cause error, format string, args ...any) *Error {
	return Wrap(CodeInternal, cause, format, args...)
}

// As достаёт *Error из цепочки; всё незнакомое считается внутренней ошибкой,
// чтобы наружу случайно не утекла формулировка драйвера базы.
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Wrap(CodeInternal, err, "внутренняя ошибка")
}

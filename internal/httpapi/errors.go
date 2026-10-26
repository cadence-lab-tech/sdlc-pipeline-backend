package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// errorBody — единая форма ошибки для UI и (в фазе 2) для MCP.
// Код строковый и совпадает с таблицей ошибок в MCP_CONTRACT.md, чтобы агент
// разбирал ответ одинаково, каким бы транспортом ни пришёл.
type errorBody struct {
	Error struct {
		Code    apperr.Code `json:"code"`
		Message string      `json:"message"`
		Details any         `json:"details,omitempty"`
	} `json:"error"`
}

func statusFor(code apperr.Code) int {
	switch code {
	case apperr.CodeNotFound:
		return http.StatusNotFound
	case apperr.CodeInvalidInput:
		return http.StatusBadRequest
	case apperr.CodeUnknownRepository:
		return http.StatusNotFound
	case apperr.CodeContractViolation, apperr.CodeValidationFailed:
		return http.StatusBadRequest
	case apperr.CodeUnauthenticated:
		return http.StatusUnauthorized
	case apperr.CodeForbidden:
		return http.StatusForbidden
	case apperr.CodeProviderMisconfigured:
		// Отказ пришёл от провайдера за сервисом, а не от запроса: тот же
		// смысл, что у шлюза, получившего негодный ответ от вышестоящего.
		return http.StatusBadGateway
	case apperr.CodeConflict, apperr.CodeGatesNotSatisfied,
		apperr.CodeTransitionNotAllowed, apperr.CodeOutOfTaskScope,
		apperr.CodeTaskNotClaimable, apperr.CodeLeaseHeldByOther,
		apperr.CodeLeaseExpired, apperr.CodeDriftUnresolved,
		apperr.CodeScopeExpansionForbidden, apperr.CodeStageTimeExceeded,
		apperr.CodeUsageReportRequired:
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// fail отвечает ошибкой. Внутренние ошибки логируются целиком, а наружу уходит
// нейтральный текст: формулировки драйвера базы клиенту знать незачем.
func fail(c *gin.Context, log *slog.Logger, err error) {
	e := apperr.As(err)

	var body errorBody
	body.Error.Code = e.Code
	body.Error.Message = e.Message
	body.Error.Details = e.Details

	// Код ошибки нужен журналу действий: по статусу его не восстановить
	// (409 — это и conflict, и gates_not_satisfied).
	c.Set(ctxErrorCode, string(e.Code))

	if e.Code == apperr.CodeInternal {
		log.Error("внутренняя ошибка",
			"error", err,
			"path", c.FullPath(),
			"request_id", c.GetString(ctxRequestID))
		body.Error.Message = "внутренняя ошибка"
		body.Error.Details = nil
	}

	c.AbortWithStatusJSON(statusFor(e.Code), body)
}

func badRequest(c *gin.Context, log *slog.Logger, err error) {
	fail(c, log, apperr.Wrap(apperr.CodeInvalidInput, err, "некорректный запрос: %v", errText(err)))
}

func errText(err error) string {
	var e *apperr.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

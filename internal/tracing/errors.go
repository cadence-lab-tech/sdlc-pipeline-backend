package tracing

import (
	"log/slog"
	"sync"
	"time"
)

// errorLogInterval — не чаще одной строки об ошибках SDK в этот срок.
// Недоступный сборщик даёт ошибку на каждую пакетную отправку, и без
// ограничения журнал api превратился бы в журнал сборщика.
const errorLogInterval = time.Minute

// errorHandler — otel.ErrorHandler в slog сервиса с ограничением частоты:
// первая ошибка — сразу, следующие в пределах интервала считаются и
// попадают в журнал числом вместе с очередной строкой.
type errorHandler struct {
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time

	mu         sync.Mutex
	last       time.Time
	suppressed int
}

func newErrorHandler(log *slog.Logger, interval time.Duration) *errorHandler {
	return &errorHandler{log: log, interval: interval, now: time.Now}
}

func (h *errorHandler) Handle(err error) {
	h.mu.Lock()
	now := h.now()
	if !h.last.IsZero() && now.Sub(h.last) < h.interval {
		h.suppressed++
		h.mu.Unlock()
		return
	}
	suppressed := h.suppressed
	h.last, h.suppressed = now, 0
	h.mu.Unlock()

	h.log.Warn("трассировки: ошибка экспорта или SDK", "error", err,
		"suppressed", suppressed)
}

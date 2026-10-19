package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server — отдельный слушатель /metrics.
//
// Отдельный порт, а не путь на основном: основной порт api смотрит в
// интернет через Caddy, а метрики без токена туда попадать не должны. У
// воркера это вообще единственный слушатель.
type Server struct {
	srv *http.Server
	ln  net.Listener
}

// Start слушает addr и отдаёт на нём только /metrics. Пустой addr — метрики
// выключены: ни одного порта, nil без ошибки.
//
// Порт занимается синхронно: занятый адрес останавливает старт процесса, а
// не оставляет его молча без метрик, о которых администратор думает, что
// они есть.
func Start(addr string, reg *Registry, log *slog.Logger) (*Server, error) {
	if addr == "" {
		return nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("слушатель метрик SDLC_METRICS_ADDR=%s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", reg.Handler())
	s := &Server{
		srv: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      30 * time.Second,
		},
		ln: ln,
	}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("слушатель метрик остановился", "error", err)
		}
	}()
	log.Info("метрики слушают", "addr", ln.Addr().String(), "path", "/metrics")
	return s, nil
}

// Addr — фактический адрес слушателя (нужен тестам с портом 0).
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Shutdown останавливает слушатель; на nil ничего не делает.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

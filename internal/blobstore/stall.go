package blobstore

import (
	"context"
	"io"
	"time"
)

// stall обрывает обмен с хранилищем, если поток стоит дольше idle. Общий
// срок у потока содержимого не угадать — его задаёт размер, — а зависшее
// хранилище без такого сторожа держало бы запрос, пока жив контекст.
type stall struct {
	cancel context.CancelFunc
	timer  *time.Timer
	idle   time.Duration
}

func newStall(ctx context.Context, idle time.Duration) (context.Context, *stall) {
	ctx, cancel := context.WithCancel(ctx)
	return ctx, &stall{cancel: cancel, timer: time.AfterFunc(idle, cancel), idle: idle}
}

// touch — поток сдвинулся, отсчёт простоя заново.
func (s *stall) touch() { s.timer.Reset(s.idle) }

func (s *stall) stop() {
	s.timer.Stop()
	s.cancel()
}

// stallReader — тело запроса записи: каждый прочитанный транспортом кусок
// сдвигает сторожа.
type stallReader struct {
	r io.Reader
	s *stall
}

func (r stallReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.s.touch()
	}
	return n, err
}

// stallBody — тело ответа чтения: сторож живёт, пока читатель не закрыт.
type stallBody struct {
	body io.ReadCloser
	s    *stall
}

func (b *stallBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.s.touch()
	}
	return n, err
}

func (b *stallBody) Close() error {
	err := b.body.Close()
	b.s.stop()
	return err
}

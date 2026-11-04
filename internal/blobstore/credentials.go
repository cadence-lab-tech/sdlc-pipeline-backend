package blobstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Credentials — ключи подписи на момент запроса. Expires нулевой у
// статических ключей: они не истекают, пока их не сменит администратор.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Expires         time.Time
}

// CredentialsProvider отдаёт ключи на каждый подписываемый запрос (CLAB-394):
// временные ключи Pod Identity и IRSA живут часы, а сервис — недели, и
// ключи, зафиксированные при старте, однажды перестали бы подходить.
type CredentialsProvider interface {
	Retrieve(ctx context.Context) (Credentials, error)
}

// StaticCredentials — ключи из окружения, как до CLAB-394: без обращений к
// сети и без срока.
type StaticCredentials Credentials

func (c StaticCredentials) Retrieve(context.Context) (Credentials, error) { return Credentials(c), nil }

const (
	// refreshWindow — за сколько до Expiration ключи обновляются заранее. Пять
	// минут с запасом покрывают таймаут обращения к источнику и расхождение
	// часов, а самый короткий срок ключей STS — пятнадцать минут.
	refreshWindow = 5 * time.Minute
	// fetchTimeout — предел одного обращения к агенту Pod Identity или STS.
	// Оба отвечают за миллисекунды; дольше — значит, адрес закрыт сетью, и
	// загрузка вложения не должна висеть на нём дольше этого.
	fetchTimeout = 5 * time.Second
	// failedRetry — пауза после неудачного обновления, пока прежние ключи ещё
	// действуют: иначе каждый запрос к S3 стучался бы в упавший источник.
	failedRetry = 10 * time.Second
	// noExpiryTTL — сколько держать ключи из ответа без Expiration.
	noExpiryTTL = 15 * time.Minute
)

// refreshing — источник временных ключей с кэшем. Обновление одно на всех:
// пока оно идёт, остальные берут ещё действующие ключи или ждут его исхода.
// Обращение к источнику идёт без отмены вызвавшего запроса, со своим
// таймаутом: отменённая загрузка одного пользователя не должна валить
// обновление для всех, а сам вызвавший перестаёт ждать по своему контексту.
type refreshing struct {
	name  string // pod-identity или irsa — для сообщений и лога
	fetch func(ctx context.Context) (Credentials, error)
	now   func() time.Time
	log   *slog.Logger

	mu       sync.Mutex
	cur      Credentials
	failedAt time.Time
	lastErr  error
	inflight chan struct{}
}

func newRefreshing(name string, fetch func(context.Context) (Credentials, error), log *slog.Logger) *refreshing {
	if log == nil {
		log = slog.Default()
	}
	return &refreshing{name: name, fetch: fetch, now: time.Now, log: log}
}

func (r *refreshing) Retrieve(ctx context.Context) (Credentials, error) {
	r.mu.Lock()
	now := r.now()
	valid := r.cur.AccessKeyID != "" && now.Before(r.cur.Expires)
	fresh := valid && now.Before(r.cur.Expires.Add(-refreshWindow))
	recentlyFailed := !r.failedAt.IsZero() && now.Sub(r.failedAt) < failedRetry
	if fresh || (valid && (r.inflight != nil || recentlyFailed)) {
		c := r.cur
		r.mu.Unlock()
		return c, nil
	}
	wait := r.inflight
	if wait == nil {
		wait = make(chan struct{})
		r.inflight = wait
		go r.refresh(wait)
	}
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return Credentials{}, fmt.Errorf("blobstore: учётные данные S3 (%s): %w", r.name, ctx.Err())
	case <-wait:
	}
	r.mu.Lock()
	c, err := r.cur, r.lastErr
	now = r.now()
	r.mu.Unlock()
	switch {
	case c.AccessKeyID != "" && now.Before(c.Expires):
		return c, nil
	case err != nil:
		return Credentials{}, err
	}
	// Источник отдал ключи, которые уже истекли (часы разошлись): подписать
	// ими нечего, второй круг не поможет.
	return Credentials{}, fmt.Errorf("blobstore: учётные данные S3 (%s): выданные ключи уже истекли (%s)",
		r.name, c.Expires.Format(time.RFC3339))
}

func (r *refreshing) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	c, err := r.fetch(ctx)
	if err == nil && c.Expires.IsZero() {
		c.Expires = r.now().Add(noExpiryTTL)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.failedAt, r.lastErr = r.now(), err
		if r.now().Before(r.cur.Expires) {
			// Ключи ещё действуют: запросы идут на них, сбой — только в лог.
			r.log.Warn("учётные данные S3 не обновлены, работаем на прежних до их срока",
				"credentials", r.name, "expires", r.cur.Expires, "error", err)
		}
	} else {
		r.cur, r.failedAt, r.lastErr = c, time.Time{}, nil
	}
	r.inflight = nil
	close(done)
}

// Учётные данные Pod Identity и IRSA на старте ждутся недолго: агент Pod
// Identity — DaemonSet и после перезапуска узла может подняться позже пода.
// Не дождались — сервис не стартует (решение человека по CLAB-394): под
// падает с понятной ошибкой, а не принимает загрузки, которые не сохранит.
const (
	credentialsWait  = 30 * time.Second
	credentialsRetry = 2 * time.Second
)

// awaitCredentials пробует источник до первого успеха, но не дольше wait.
func awaitCredentials(ctx context.Context, p CredentialsProvider, name string, wait, every time.Duration, log *slog.Logger) error {
	deadline := time.Now().Add(wait)
	for attempt := 1; ; attempt++ {
		_, err := p.Retrieve(ctx)
		if err == nil {
			return nil
		}
		if time.Now().Add(every).After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("blobstore: учётные данные S3 (%s) не получены за %s, попыток %d: %w",
				name, wait, attempt, err)
		}
		log.Warn("учётные данные S3 пока не получены, повторим", "credentials", name,
			"attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(every):
		}
	}
}

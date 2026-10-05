package github

// Состояние приложения у провайдера (CLAB-79).
//
// «Задан идентификатор и ключ прочитался на старте» (HasApp) ничего не
// говорит о том, принимает ли провайдер этот ключ: приложение восемнадцать
// дней отбивалось по времени, а настройки показывали его настроенным.
// Спросить можно только у самого провайдера — утверждение приложения на
// GET /app.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AppState — исход проверки приложения. Значения — часть протокола: их
// отдают ручки настроек и сводка проекта, читают интерфейс и CLI.
type AppState string

const (
	// AppOK — провайдер принял утверждение приложения.
	AppOK AppState = "ok"
	// AppRejected — провайдер отбил утверждение: 401 и после поправки часов
	// либо 404 — приложения с таким идентификатором у него нет.
	AppRejected AppState = "rejected"
	// AppUnknown — проверить не удалось: сеть, таймаут, 5xx, лимит. Это не
	// отказ ключа, и показывать его как отказ значило бы повторить подмену
	// класса, которую убирала CLAB-78.
	AppUnknown AppState = "unknown"
	// AppNotConfigured — приложения у инстанса нет; провайдера не спрашивают.
	AppNotConfigured AppState = "not_configured"
)

// AppStatus — состояние приложения, каким его отдают наружу.
type AppStatus struct {
	State AppState `json:"state"`
	// Error — ответ провайдера при rejected и причина при unknown.
	Error     string     `json:"error,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	// ClockOffsetSeconds — измеренное расхождение часов, если оно не меньше
	// допуска; плюс — часы машины отстают от провайдера.
	ClockOffsetSeconds *int `json:"clock_offset_seconds,omitempty"`
}

const (
	// appStatusTTL — сколько живёт исход проверки. Страницы настроек и сводка
	// проекта читают кэш, а не провайдера: сводку зовут на каждой странице
	// проекта все участники сразу.
	appStatusTTL = 5 * time.Minute
	// appStatusUnknownTTL — срок для unknown короче: недоступность провайдера
	// проходит быстрее, чем чинят ключ. Но не нулевой — иначе при висящем
	// провайдере каждый переход по страницам ждал бы таймаут и ходил в сеть.
	appStatusUnknownTTL = 30 * time.Second
	// appStatusCachedTimeout — сколько путь «из кэша» ждёт провайдера, когда
	// кэш пуст. Заход на страницу не должен висеть на GitHub.
	appStatusCachedTimeout = 5 * time.Second
)

// appStatusFlight — идущая проверка, которой дожидаются остальные: при
// пустом кэше одновременные запросы делают один поход к провайдеру.
type appStatusFlight struct {
	done   chan struct{}
	status AppStatus
}

// Status отдаёт состояние приложения из кэша, а при пустом или истёкшем —
// спрашивает провайдера один раз на всех ожидающих и не дольше
// statusTimeout. Кэшируется любой исход, включая unknown.
//
// Проверка идёт в запросе того, кто пришёл первым, но от его отмены не
// зависит: ушедший со страницы человек не должен оставить остальным
// «не удалось проверить».
func (a *App) Status(ctx context.Context) AppStatus {
	a.statusMu.Lock()
	if a.statusCached && time.Now().Before(a.statusExpires) {
		status := a.status
		a.statusMu.Unlock()
		return status
	}
	if flight := a.statusFlight; flight != nil {
		a.statusMu.Unlock()
		select {
		case <-flight.done:
			return flight.status
		case <-ctx.Done():
			return a.unknown(ctx.Err())
		}
	}
	flight := &appStatusFlight{done: make(chan struct{})}
	a.statusFlight = flight
	a.statusMu.Unlock()

	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), a.statusTimeout)
	flight.status = a.probe(probeCtx)
	cancel()

	a.statusMu.Lock()
	a.statusFlight = nil
	a.remember(flight.status)
	a.statusMu.Unlock()
	close(flight.done)
	return flight.status
}

// Check — свежая проверка мимо кэша: её зовёт кнопка «Проверить доступ» и
// `clab project github --check`. Исход обновляет кэш, чтобы починенный ключ
// перестал числиться непринятым, не дожидаясь срока.
func (a *App) Check(ctx context.Context) AppStatus {
	status := a.probe(ctx)
	a.statusMu.Lock()
	a.remember(status)
	a.statusMu.Unlock()
	return status
}

// remember кладёт исход в кэш. Более ранний исход поздний не затирает:
// свежая проверка и проверка «из кэша» могут идти одновременно. Зовётся под
// statusMu.
func (a *App) remember(status AppStatus) {
	if a.statusCached && status.CheckedAt != nil && a.status.CheckedAt != nil &&
		status.CheckedAt.Before(*a.status.CheckedAt) {
		return
	}
	ttl := appStatusTTL
	if status.State == AppUnknown {
		ttl = appStatusUnknownTTL
	}
	a.status, a.statusCached, a.statusExpires = status, true, time.Now().Add(ttl)
}

// probe — один вопрос провайдеру: принимает ли он утверждение приложения.
//
// Правило то же, что у InstallationToken: 401 один раз перевыписывается
// после измеренной поправки часов, 401 после неё — отказ. 404 — тоже отказ:
// так провайдер отвечает на утверждение с идентификатором приложения,
// которого у него нет («Integration not found»), и чинится это там же, где
// ключ, — в настройке инстанса; часы тут ни при чём, перевыписывать нечего.
// Всё остальное — «не удалось проверить»: по такому ответу о ключе судить
// нельзя.
func (a *App) probe(ctx context.Context) AppStatus {
	for attempt := 0; ; attempt++ {
		resp, body, err := a.getApp(ctx)
		if err != nil {
			return a.unknown(err)
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return a.stamp(AppStatus{State: AppOK})
		case resp.StatusCode == http.StatusUnauthorized && attempt == 0 &&
			a.observeClock(resp.Header.Get("Date")):
			continue
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusNotFound:
			return a.stamp(AppStatus{State: AppRejected,
				Error: fmt.Sprintf("github %d на утверждение приложения: %s", resp.StatusCode, snippet(body))})
		default:
			return a.stamp(AppStatus{State: AppUnknown,
				Error: fmt.Sprintf("github %d на утверждение приложения: %s", resp.StatusCode, snippet(body))})
		}
	}
}

func (a *App) unknown(err error) AppStatus {
	return a.stamp(AppStatus{State: AppUnknown, Error: err.Error()})
}

// stamp дописывает время проверки и расхождение часов. Расхождение отдаётся
// при любом исходе: «ключ принят, но часы уехали» — повод чинить время до
// того, как поправка перестанет справляться.
func (a *App) stamp(status AppStatus) AppStatus {
	now := time.Now().UTC()
	status.CheckedAt = &now
	a.mu.Lock()
	offset := a.offset
	a.mu.Unlock()
	if offset.Abs() >= clockTolerance {
		seconds := int(offset.Round(time.Second) / time.Second)
		status.ClockOffsetSeconds = &seconds
	}
	return status
}

// getApp — один поход за описанием приложения. Тело прочитано целиком, у
// ответа остаются статус и заголовки: по Date снимается поправка часов.
func (a *App) getApp(ctx context.Context) (*http.Response, []byte, error) {
	appJWT, err := a.jwt()
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/app", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("Authorization", "Bearer "+appJWT)

	resp, err := a.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("github: проверка приложения: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("github: проверка приложения: %w", err)
	}
	return resp, body, nil
}

// AppStatus — состояние приложения инстанса. fresh — свежая проверка мимо
// кэша; без неё ответ берётся из кэша. Без приложения провайдера не
// спрашивают вовсе.
func (p *Provider) AppStatus(ctx context.Context, fresh bool) AppStatus {
	if !p.HasApp() {
		return AppStatus{State: AppNotConfigured}
	}
	if fresh {
		return p.app.Check(ctx)
	}
	return p.app.Status(ctx)
}

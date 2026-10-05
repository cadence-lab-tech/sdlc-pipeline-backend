package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Состояние приложения у провайдера (CLAB-79): четыре исхода, кэш и один
// поход на всех ожидающих. Провайдер здесь — заглушка, которая считает
// запросы /app: по счётчику и видно, что кэш работает.

type appStub struct {
	srv   *httptest.Server
	calls atomic.Int32
	// handler меняется по ходу теста: «провайдер починен».
	handler atomic.Value
}

func newAppStub(t *testing.T, handler http.HandlerFunc) *appStub {
	t.Helper()
	stub := &appStub{}
	stub.handler.Store(handler)
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" || r.Method != http.MethodGet {
			t.Errorf("неожиданный запрос %s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("запрос /app без утверждения приложения")
		}
		stub.calls.Add(1)
		stub.handler.Load().(http.HandlerFunc)(w, r)
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *appStub) app(t *testing.T) *App {
	t.Helper()
	app, err := NewApp(s.srv.URL, "12345", testAppKey(t))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

const badCredentials = `{"message":"Bad credentials"}`

// A2: провайдер принял утверждение.
func TestAppStatusOK(t *testing.T) {
	stub := newAppStub(t, respond(http.StatusOK, `{"id":12345,"slug":"clab"}`))
	status := stub.app(t).Status(context.Background())
	if status.State != AppOK || status.Error != "" || status.CheckedAt == nil {
		t.Fatalf("состояние %+v", status)
	}
	// A5: часы в порядке — поля расхождения нет.
	if status.ClockOffsetSeconds != nil {
		t.Fatalf("расхождение часов %d при исправных часах", *status.ClockOffsetSeconds)
	}
}

// A3: провайдер отбил утверждение — отказ с его текстом.
func TestAppStatusRejected(t *testing.T) {
	stub := newAppStub(t, respond(http.StatusUnauthorized, badCredentials))
	status := stub.app(t).Status(context.Background())
	if status.State != AppRejected || status.CheckedAt == nil {
		t.Fatalf("состояние %+v", status)
	}
	if !strings.Contains(status.Error, "Bad credentials") {
		t.Fatalf("в отказе нет ответа провайдера: %q", status.Error)
	}
	// Часы не разошлись — вторая попытка не делается: неверный ключ не
	// крутится по кругу.
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("запросов /app %d вместо одного", got)
	}
}

// A4: утверждение отбито по времени — поправка часов снимается с Date,
// утверждение перевыписывается один раз, расхождение отдаётся наружу.
func TestAppStatusSurvivesClockDrift(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drift time.Duration // на сколько провайдер впереди машины
	}{
		{"часы машины отстают", 3 * time.Minute},
		{"часы машины спешат", -3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen atomic.Int32
			stub := newAppStub(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Date", time.Now().Add(tc.drift).UTC().Format(http.TimeFormat))
				if seen.Add(1) == 1 {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(badCredentials))
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			status := stub.app(t).Status(context.Background())
			if got := stub.calls.Load(); got != 2 {
				t.Fatalf("запросов /app %d вместо двух: отбитое по времени утверждение перевыписывается один раз", got)
			}
			if status.State != AppOK {
				t.Fatalf("состояние %+v", status)
			}
			if status.ClockOffsetSeconds == nil {
				t.Fatal("расхождение часов не отдано")
			}
			// Плюс — часы машины отстают от провайдера.
			want := int(tc.drift / time.Second)
			if got := *status.ClockOffsetSeconds; got < want-5 || got > want+5 {
				t.Fatalf("расхождение %d с вместо ≈%d", got, want)
			}
		})
	}
}

// 401 и после поправки часов — отказ, и расхождение в нём названо.
func TestAppStatusRejectedAfterClockCorrection(t *testing.T) {
	stub := newAppStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Now().Add(-3*time.Minute).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(badCredentials))
	})
	status := stub.app(t).Status(context.Background())
	if status.State != AppRejected {
		t.Fatalf("состояние %+v", status)
	}
	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("запросов /app %d вместо двух", got)
	}
	if status.ClockOffsetSeconds == nil || *status.ClockOffsetSeconds > -170 {
		t.Fatalf("расхождение часов %v при отказе с уехавшими часами", status.ClockOffsetSeconds)
	}
}

// A5: расхождение меньше допуска не измеряется и не отдаётся.
func TestAppStatusIgnoresSmallClockDrift(t *testing.T) {
	stub := newAppStub(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Now().Add(10*time.Second).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(badCredentials))
	})
	status := stub.app(t).Status(context.Background())
	if status.State != AppRejected || status.ClockOffsetSeconds != nil {
		t.Fatalf("состояние %+v", status)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("запросов /app %d вместо одного", got)
	}
}

// A6: 5xx и лимит — «не удалось проверить», а не отказ ключа.
func TestAppStatusUnknownOnProviderFailure(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusTooManyRequests, http.StatusForbidden, http.StatusNotFound} {
		stub := newAppStub(t, respond(code, `{"message":"провайдеру плохо"}`))
		status := stub.app(t).Status(context.Background())
		if status.State != AppUnknown || status.Error == "" || status.CheckedAt == nil {
			t.Fatalf("%d: состояние %+v", code, status)
		}
	}
}

// A7, F7: провайдер висит — путь «из кэша» отвечает unknown за свой предел,
// а второй запрос не ждёт и в сеть не ходит: unknown кэшируется.
func TestAppStatusHangingProviderIsUnknownAndCached(t *testing.T) {
	release := make(chan struct{})
	stub := newAppStub(t, func(http.ResponseWriter, *http.Request) { <-release })
	defer close(release)

	app := stub.app(t)
	app.statusTimeout = 200 * time.Millisecond

	started := time.Now()
	first := app.Status(context.Background())
	if first.State != AppUnknown || first.Error == "" {
		t.Fatalf("состояние %+v", first)
	}
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("путь «из кэша» ждал провайдера %s", took)
	}

	started = time.Now()
	second := app.Status(context.Background())
	if second.State != AppUnknown {
		t.Fatalf("состояние %+v", second)
	}
	if took := time.Since(started); took > 50*time.Millisecond {
		t.Fatalf("второй запрос ждал %s: unknown не закэширован", took)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("запросов /app %d вместо одного", got)
	}
}

// Пределы — константы из требований: кэш не дольше пяти минут, ожидание
// провайдера на пути «из кэша» не дольше пяти секунд, срок unknown короче
// общего, но не нулевой.
func TestAppStatusLimits(t *testing.T) {
	if appStatusTTL <= 0 || appStatusTTL > 5*time.Minute {
		t.Fatalf("срок кэша %s", appStatusTTL)
	}
	if appStatusCachedTimeout <= 0 || appStatusCachedTimeout > 5*time.Second {
		t.Fatalf("предел ожидания %s", appStatusCachedTimeout)
	}
	if appStatusUnknownTTL <= 0 || appStatusUnknownTTL >= appStatusTTL {
		t.Fatalf("срок unknown %s", appStatusUnknownTTL)
	}
	stub := newAppStub(t, respond(http.StatusOK, `{}`))
	if got := stub.app(t).statusTimeout; got != appStatusCachedTimeout {
		t.Fatalf("предел ожидания у приложения %s", got)
	}
}

// A8: в пределах срока кэша провайдера спрашивают один раз; по истечении —
// снова.
func TestAppStatusIsCached(t *testing.T) {
	stub := newAppStub(t, respond(http.StatusOK, `{}`))
	app := stub.app(t)
	ctx := context.Background()
	first := app.Status(ctx)
	for range 3 {
		if got := app.Status(ctx); got.State != AppOK || !got.CheckedAt.Equal(*first.CheckedAt) {
			t.Fatalf("состояние %+v", got)
		}
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("запросов /app %d вместо одного", got)
	}

	app.statusMu.Lock()
	app.statusExpires = time.Now().Add(-time.Second)
	app.statusMu.Unlock()
	app.Status(ctx)
	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("запросов /app %d вместо двух: истёкший кэш не перепроверен", got)
	}
}

// A9, A10: свежая проверка ходит к провайдеру на каждый вызов и обновляет
// кэш — починенный ключ перестаёт числиться непринятым, не дожидаясь срока.
func TestAppCheckIsFreshAndUpdatesCache(t *testing.T) {
	stub := newAppStub(t, respond(http.StatusUnauthorized, badCredentials))
	app := stub.app(t)
	ctx := context.Background()

	if got := app.Status(ctx); got.State != AppRejected {
		t.Fatalf("состояние %+v", got)
	}
	stub.handler.Store(respond(http.StatusOK, `{}`))
	// Кэш ещё держит отказ.
	if got := app.Status(ctx); got.State != AppRejected {
		t.Fatalf("состояние из кэша %+v", got)
	}
	if got := app.Check(ctx); got.State != AppOK {
		t.Fatalf("свежая проверка %+v", got)
	}
	if got := app.Status(ctx); got.State != AppOK {
		t.Fatalf("кэш после свежей проверки %+v", got)
	}
	if got := app.Check(ctx); got.State != AppOK {
		t.Fatalf("свежая проверка %+v", got)
	}
	// Один поход за отказом и по одному на каждую свежую проверку.
	if got := stub.calls.Load(); got != 3 {
		t.Fatalf("запросов /app %d вместо трёх", got)
	}
}

// F6: при пустом кэше двадцать одновременных запросов делают одну проверку
// и получают один и тот же ответ.
func TestAppStatusSingleFlight(t *testing.T) {
	stub := newAppStub(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	app := stub.app(t)

	const n = 20
	results := make([]AppStatus, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = app.Status(context.Background())
		}()
	}
	wg.Wait()

	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("запросов /app %d вместо одного", got)
	}
	for i, got := range results {
		if got.State != AppOK || !got.CheckedAt.Equal(*results[0].CheckedAt) {
			t.Fatalf("ответ %d отличается: %+v против %+v", i, got, results[0])
		}
	}
}

// Отмена запроса того, кто пришёл первым, не портит исход остальным.
func TestAppStatusSurvivesLeaderCancel(t *testing.T) {
	stub := newAppStub(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	app := stub.app(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if got := app.Status(ctx); got.State != AppOK {
		t.Fatalf("состояние %+v: проверка оборвалась вместе с запросом", got)
	}
}

// A1, F9, A13: без приложения провайдера не спрашивают; сам по себе, без
// запросов, процесс к провайдеру не ходит.
func TestProviderAppStatusNotConfigured(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer srv.Close()

	provider := NewProvider(New(srv.URL, ""), nil, nil)
	for _, fresh := range []bool{false, true} {
		status := provider.AppStatus(context.Background(), fresh)
		if status.State != AppNotConfigured || status.CheckedAt != nil || status.Error != "" {
			t.Fatalf("состояние %+v", status)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("провайдера спросили %d раз без приложения", got)
	}
}

func TestProviderAppStatusHasNoBackgroundChecks(t *testing.T) {
	stub := newAppStub(t, respond(http.StatusOK, `{}`))
	app := stub.app(t)
	provider := NewProvider(New(stub.srv.URL, ""), app, nil)

	// Кэш истёк, а к провайдеру никто не обращался: проверка идёт по запросу,
	// не по расписанию.
	app.statusMu.Lock()
	app.statusExpires = time.Now().Add(-time.Hour)
	app.statusMu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if got := stub.calls.Load(); got != 0 {
		t.Fatalf("запросов /app %d без обращений к состоянию", got)
	}
	if got := provider.AppStatus(context.Background(), false); got.State != AppOK {
		t.Fatalf("состояние %+v", got)
	}
	if got := provider.AppStatus(context.Background(), true); got.State != AppOK {
		t.Fatalf("состояние %+v", got)
	}
	if got := stub.calls.Load(); got != 2 {
		t.Fatalf("запросов /app %d вместо двух", got)
	}
}

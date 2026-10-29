package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/domain"
	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/store/postgres"
)

// Ручки событий долгим опросом (CLAB-323, решение 0067 п. 4):
// GET /v1/tasks/:key/events и GET /v1/projects/:key/events.
//
// Ожидающий запрос не держит соединение пула и сам базу не опрашивает. Он
// один раз читает ленту после курсора и засыпает на канале; будит его общий
// будильник реплики (feedHub) — один короткий индексный запрос в такт на
// реплику, сколько бы ожидающих в ней ни было. По сроку запрос отвечает
// пустым списком без похода в базу. Событие только будит: клиент перечитывает
// задачу, истина — она, а не лента.

const (
	// Страница ответа: больше событий после курсора — следующая сразу.
	feedPage = 100
	// Потолок ожидания: меньше SDLC_REQUEST_TIMEOUT с запасом в 5 с, чтобы
	// WriteTimeout сервера не оборвал ответ, и не дольше 25 с при любом.
	feedMaxWait   = 25 * time.Second
	feedWaitSlack = 5 * time.Second
	// Такт будильника и окно назад. Окно больше такта: событие с ранней
	// меткой, зафиксированное позже соседнего, ещё попадёт в окно.
	feedTick     = time.Second
	feedLookback = 3 * time.Second
	// Предел следов за такт; упёрлись — будим всех, а не теряем.
	feedTickLimit = 2000
)

type feedResponse struct {
	Events []domain.FeedEvent `json:"events"`
	Cursor string             `json:"cursor"`
}

func (s *Server) taskEvents(c *gin.Context) {
	task, err := s.store.GetTaskByKey(c.Request.Context(), s.store.DB(), c.Param("taskKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	s.serveFeed(c, postgres.FeedScope{TaskID: task.ID}, task.ID)
}

func (s *Server) projectEvents(c *gin.Context) {
	project, err := s.store.GetProjectByKey(c.Request.Context(), s.store.DB(), c.Param("projectKey"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	s.serveFeed(c, postgres.FeedScope{ProjectID: project.ID}, project.ID)
}

// feedWaitLimit — потолок ожидания по сроку запроса сервера:
// min(25 с, SDLC_REQUEST_TIMEOUT − 5 с), без срока — 25 с. Ноль и меньше —
// ответ сразу.
func (s *Server) feedWaitLimit() time.Duration {
	if s.cfg.RequestTimeout <= 0 {
		return feedMaxWait
	}
	return min(feedMaxWait, s.cfg.RequestTimeout-feedWaitSlack)
}

func (s *Server) serveFeed(c *gin.Context, scope postgres.FeedScope, wakeKey uuid.UUID) {
	ctx, q := c.Request.Context(), s.store.DB()

	wait, err := parseFeedWait(c.Query("wait"))
	if err != nil {
		fail(c, s.log, err)
		return
	}
	raw, hasAfter := c.GetQuery("after")
	if !hasAfter {
		// Рукопожатие: «с этого момента», без выгрузки истории.
		head, err := s.store.FeedHead(ctx, q)
		if err != nil {
			fail(c, s.log, err)
			return
		}
		c.JSON(http.StatusOK, feedResponse{Events: []domain.FeedEvent{}, Cursor: head.String()})
		return
	}
	after, err := postgres.ParseFeedCursor(raw)
	if err != nil {
		fail(c, s.log, err)
		return
	}
	wait = min(wait, s.feedWaitLimit())

	var wake <-chan struct{}
	if wait > 0 {
		// Подписка до чтения: событие между чтением и подпиской иначе
		// проспали бы до срока.
		sub := s.feed.subscribe(wakeKey)
		defer s.feed.unsubscribe(sub)
		wake = sub.ch
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()

	for {
		events, err := s.store.FeedAfter(ctx, q, scope, after, feedPage)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fail(c, s.log, err)
			return
		}
		if len(events) > 0 {
			last := events[len(events)-1]
			c.JSON(http.StatusOK, feedResponse{Events: events,
				Cursor: postgres.FeedCursor{At: last.CreatedAt, ID: last.ID}.String()})
			return
		}
		if wait <= 0 {
			break
		}
		select {
		case <-wake:
			continue
		case <-deadline.C:
		case <-s.feed.stopping():
		case <-ctx.Done():
			// Клиент ушёл: отвечать некому.
			return
		}
		break
	}
	c.JSON(http.StatusOK, feedResponse{Events: []domain.FeedEvent{}, Cursor: after.String()})
}

// parseFeedWait — секунды ожидания: пусто — ноль, не целое или меньше нуля —
// invalid_input. Больше потолка урезается молча вызывающим.
func parseFeedWait(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, apperr.InvalidInput("wait — целое число секунд не меньше нуля, а не %q", raw)
	}
	// Предел до умножения: огромное число не переполняет Duration.
	if n > int(feedMaxWait/time.Second) {
		n = int(feedMaxWait / time.Second)
	}
	return time.Duration(n) * time.Second, nil
}

// StopWaiting отвечает всем ожидающим сразу пустым списком: остановку API
// (srv.RegisterOnShutdown) долгий опрос не держит, клиент переподключится к
// другой реплике. Повторный вызов безвреден.
func (s *Server) StopWaiting() { s.feed.stop() }

// feedHub — будильник ожидающих одной реплики. Ключ подписки — id задачи
// или проекта (uuid разных таблиц не совпадают). Опрос базы идёт только
// пока есть хоть один ожидающий: без них реплика базу не трогает вовсе.
type feedHub struct {
	store *postgres.Store
	log   *slog.Logger
	tick  time.Duration

	mu      sync.Mutex
	subs    map[uuid.UUID]map[*feedSub]struct{}
	total   int
	running bool
	done    chan struct{}
	once    sync.Once
}

type feedSub struct {
	key uuid.UUID
	ch  chan struct{}
}

func newFeedHub(store *postgres.Store, log *slog.Logger) *feedHub {
	return &feedHub{store: store, log: log, tick: feedTick,
		subs: map[uuid.UUID]map[*feedSub]struct{}{}, done: make(chan struct{})}
}

func (h *feedHub) stopping() <-chan struct{} { return h.done }

func (h *feedHub) stop() { h.once.Do(func() { close(h.done) }) }

func (h *feedHub) subscribe(key uuid.UUID) *feedSub {
	sub := &feedSub{key: key, ch: make(chan struct{}, 1)}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[key] == nil {
		h.subs[key] = map[*feedSub]struct{}{}
	}
	h.subs[key][sub] = struct{}{}
	h.total++
	if !h.running {
		h.running = true
		go h.run()
	}
	return sub
}

func (h *feedHub) unsubscribe(sub *feedSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.subs[sub.key]; set != nil {
		if _, ok := set[sub]; ok {
			delete(set, sub)
			h.total--
		}
		if len(set) == 0 {
			delete(h.subs, sub.key)
		}
	}
}

// run — такт будильника: следы событий за окно назад, новые (не виденные
// прошлым тактом) будят подписчиков своей задачи и своего проекта. Выходит,
// когда подписчиков не осталось; следующий подписчик запустит заново.
func (h *feedHub) run() {
	seen := map[uuid.UUID]struct{}{}
	t := time.NewTicker(h.tick)
	defer t.Stop()
	for {
		select {
		case <-h.done:
			h.mu.Lock()
			h.running = false
			h.mu.Unlock()
			return
		case <-t.C:
		}
		h.mu.Lock()
		if h.total == 0 {
			h.running = false
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		marks, err := h.store.FeedRecent(ctx, h.store.DB(), feedLookback, feedTickLimit)
		cancel()
		if err != nil {
			// Сбой такта — не повод будить или ронять: следующий такт
			// застанет те же события, окно больше такта.
			h.log.Warn("будильник событий: такт не прочитан", "error", err)
			continue
		}
		keys := map[uuid.UUID]struct{}{}
		next := make(map[uuid.UUID]struct{}, len(marks))
		for _, m := range marks {
			next[m.ID] = struct{}{}
			if _, old := seen[m.ID]; old {
				continue
			}
			keys[m.TaskID] = struct{}{}
			keys[m.ProjectID] = struct{}{}
		}
		seen = next
		h.wake(keys, len(marks) >= feedTickLimit)
	}
}

func (h *feedHub) wake(keys map[uuid.UUID]struct{}, all bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, set := range h.subs {
		if _, hit := keys[key]; !hit && !all {
			continue
		}
		for sub := range set {
			select {
			case sub.ch <- struct{}{}:
			default:
			}
		}
	}
}

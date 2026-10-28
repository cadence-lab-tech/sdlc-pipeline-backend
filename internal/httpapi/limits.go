package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cadence-lab-tech/sdlc-pipeline-backend/internal/apperr"
)

// ctxBodyTooLarge — заявленная длина тела больше предела: тело подменено
// отказом, и rejectTooLarge отвечает 413, не читая его.
const ctxBodyTooLarge = "body_too_large"

// bodyLimitSlack — запас общего предела тела на всё, кроме самого
// содержимого: поля JSON, заголовок, мелкие параметры.
const bodyLimitSlack = 1 << 20

// bodyLimit — общий предел тела запроса на /v1 и /mcp (CLAB-295):
// 2×SDLC_BLOB_MAX_BYTES + 1 МиБ.
//
// Предел не ниже самого крупного законного запроса: JSON-сдачи текстового
// артефакта с содержимым ровно SDLC_BLOB_MAX_BYTES. Экранирование JSON
// раздувает содержимое: перевод строки и кавычка — вдвое (\n, \"), а
// <, > и & кодировщик Go пишет как < — вшестеро. Шестикратный запас
// на каждый байт держал бы предел сотнями МиБ ради текста, которого не
// бывает: markdown — это буквы и в худшем случае каждый второй символ под
// экранированием. Вдвое — с запасом на такой текст; содержимое, которое
// раздувается сильнее, — файл, и ему место в загрузке вложений (часть 3),
// а не в JSON. Импорт связки и MCP укладываются в тот же предел: в них
// то же содержимое в том же JSON.
//
// Ноль — предела нет: так бывает только у сервера, собранного в обход
// config.Load (SDLC_BLOB_MAX_BYTES там всегда положителен), — в тестах.
func (s *Server) bodyLimit() int64 { return BodyLimit(s.cfg.BlobMaxBytes) }

// BodyLimit — тот же предел по SDLC_BLOB_MAX_BYTES; его пишет в журнал
// старта cmd/api.
func BodyLimit(blobMaxBytes int) int64 {
	if blobMaxBytes <= 0 {
		return 0
	}
	return 2*int64(blobMaxBytes) + bodyLimitSlack
}

// limitRequestBody ставит общий предел на тело каждого запроса к /v1 и
// /mcp. Стоит на движке до групп: открытые маршруты /v1/auth/*,
// /v1/instance и /v1/agent-kit зарегистрированы вне группы v1, и предел
// обязан накрыть и их. Стоит и до журнала действий: bodyTap читает тело
// уже через предел и дочитать сверх него не может.
//
// Отказ по заявленной длине здесь не отвечает, а только подменяет тело
// отказом и помечает запрос: ответ 413 даёт rejectTooLarge после журнала
// (на группе v1 — чтобы отказ попал в журнал, на MCP — до SDK). Открытые
// маршруты отвечают 413 сами, при первом чтении тела. Тело без заявленной
// длины (chunked) ограничивает http.MaxBytesReader: обработчик получит
// *http.MaxBytesError, и fail превратит его в тот же 413.
//
// Вебхуки GitHub (/webhooks) предел не трогает: у них свой, 4 МиБ, в
// ручке, и в эту часть они не входят.
func (s *Server) limitRequestBody() gin.HandlerFunc {
	limit := s.bodyLimit()
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if limit <= 0 || !(path == "/v1" || strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/mcp/")) {
			c.Next()
			return
		}
		if wrapBody(c, limit) {
			c.Set(ctxBodyTooLarge, limit)
		}
		c.Next()
	}
}

// rejectTooLarge отвечает 413 запросу, чья заявленная длина больше предела
// (пометка limitRequestBody). Тело при этом не читается вовсе.
func (s *Server) rejectTooLarge() gin.HandlerFunc {
	return func(c *gin.Context) {
		if v, ok := c.Get(ctxBodyTooLarge); ok {
			limit, _ := v.(int64)
			fail(c, s.log, bodyTooLarge(limit))
			return
		}
		c.Next()
	}
}

// limitBody — предел тела уже общего на отдельном маршруте (часть 3:
// multipart — предел файла плюс запас на поля формы). Тот же механизм:
// заявленная длина больше — 413 сразу, без чтения; без длины — предел на
// чтении. Ставится на маршрут, то есть после журнала действий группы.
func (s *Server) limitBody(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if limit > 0 && wrapBody(c, limit) {
			fail(c, s.log, bodyTooLarge(limit))
			return
		}
		c.Next()
	}
}

// wrapBody ограничивает тело запроса limit байтами и отвечает, больше ли
// предела заявленная длина. Тогда тело подменяется отказом: читать его нет
// смысла, а обработчик, который всё же прочтёт, получит ту же ошибку
// предела, что и при чтении сверх него.
func wrapBody(c *gin.Context, limit int64) bool {
	body := c.Request.Body
	if body == nil || body == http.NoBody {
		return false
	}
	if c.Request.ContentLength > limit {
		c.Request.Body = tooLargeBody{limit: limit}
		return true
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, body, limit)
	return false
}

// tooLargeBody — тело, которое заведомо больше предела: любое чтение — та
// же ошибка, что у http.MaxBytesReader, и ни одного байта из сети.
type tooLargeBody struct{ limit int64 }

func (b tooLargeBody) Read([]byte) (int, error) { return 0, &http.MaxBytesError{Limit: b.limit} }
func (b tooLargeBody) Close() error             { return nil }

var _ io.ReadCloser = tooLargeBody{}

func bodyTooLarge(limit int64) *apperr.Error {
	return apperr.PayloadTooLarge("тело запроса", limit)
}

// asTooLarge находит в цепочке ошибку предела тела. Обработчики читают
// тело по-разному (ShouldBindJSON, io.ReadAll, поток содержимого) и
// оборачивают ошибку чтения кто как; ответ на неё один — 413.
func asTooLarge(err error) (*apperr.Error, bool) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return bodyTooLarge(mbe.Limit), true
	}
	return nil, false
}

// uploadDeadline — срок маршрута, принимающего содержимое
// (SDLC_UPLOAD_TIMEOUT), downloadDeadline — отдающего его
// (SDLC_DOWNLOAD_TIMEOUT). Прочие маршруты живут по SDLC_REQUEST_TIMEOUT.
func (s *Server) uploadDeadline() gin.HandlerFunc {
	return routeDeadline(s.log, s.cfg.RequestTimeout, s.cfg.UploadTimeout, true)
}

func (s *Server) downloadDeadline() gin.HandlerFunc {
	return routeDeadline(s.log, s.cfg.RequestTimeout, s.cfg.DownloadTimeout, false)
}

// routeDeadline продлевает сроки соединения на одном маршруте.
//
// ReadTimeout и WriteTimeout сервера (SDLC_REQUEST_TIMEOUT) отсчитаны от
// начала запроса и касаются всего запроса целиком. Крупное содержимое на
// медленном канале в них не укладывается, поэтому маршрут переставляет
// границы соединения через http.ResponseController: загрузке — и чтения, и
// записи (иначе ответ 201 не уйдёт: пишущая граница сервера к концу приёма
// уже прошла), отдаче — только записи. Ноль — без срока.
//
// Срок маршрута только продлевает. Короче серверного он не ставится, и при
// SDLC_REQUEST_TIMEOUT=0 не ставится вовсе: сократить срок, заданный
// сервером, — не дело маршрута. Отсюда же и то, что срок маршрута не
// достаётся следующему запросу того же keep-alive-соединения: при
// ненулевом SDLC_REQUEST_TIMEOUT сервер сам переставляет обе границы в
// начале каждого запроса, а при нулевом здесь ничего и не ставилось.
//
// Соединение, которое не умеет сроков (рекордер в тестах), — не ошибка
// запроса: предупреждение в журнал, и запрос идёт дальше по сроку сервера.
func routeDeadline(log *slog.Logger, server, route time.Duration, read bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if server <= 0 || (route > 0 && route <= server) {
			c.Next()
			return
		}
		var at time.Time
		if route > 0 {
			at = time.Now().Add(route)
		}
		rc := http.NewResponseController(c.Writer)
		if read {
			if err := rc.SetReadDeadline(at); err != nil {
				deadlineUnsupported(log, c, err)
			}
		}
		if err := rc.SetWriteDeadline(at); err != nil {
			deadlineUnsupported(log, c, err)
		}
		c.Next()
	}
}

func deadlineUnsupported(log *slog.Logger, c *gin.Context, err error) {
	log.Warn("срок маршрута не поставлен: соединение не умеет сроков",
		"path", c.FullPath(), "error", err, "request_id", c.GetString(ctxRequestID))
}

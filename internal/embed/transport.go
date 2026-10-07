package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Общий транспорт HTTP-провайдеров. У Voyage и OpenAI-совместимых серверов
// один и тот же ответ (data[].index/embedding) и одни и те же правила повтора;
// различается только тело запроса.

const (
	defaultBatch = 96
	maxAttempts  = 4
)

// StatusError — провайдер ответил не 200. Код нужен пробе при старте: неверный
// ключ и неизвестная модель — ошибка настройки, а 5xx — временная
// недоступность.
type StatusError struct {
	Provider string
	Code     int
	Body     string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s %d: %s", e.Provider, e.Code, e.Body)
}

// DimensionError — модель вернула вектор не той длины, что колонка vector(1024).
// От повтора не исправится: это выбор модели, а не сбой.
type DimensionError struct {
	Provider string
	Got      int
}

func (e *DimensionError) Error() string {
	return fmt.Sprintf("%s вернул вектор размерности %d вместо %d", e.Provider, e.Got, Dim)
}

// endpoint — адрес ручки эмбеддингов с ключом.
type endpoint struct {
	provider string
	url      string
	key      string
	client   *http.Client
}

func newClient() *http.Client {
	// Таймаут больше обычного: пачка в сотню кусков кода считается
	// секундами, и обрывать её на середине дороже, чем подождать.
	return &http.Client{Timeout: 2 * time.Minute}
}

// embed отправляет тело и повторяет на 429 и 5xx. want — сколько векторов
// обязано вернуться; attempts ограничивает число попыток (проба при старте
// ходит один раз, чтобы не держать старт на ретраях).
func (e endpoint) embed(ctx context.Context, body []byte, want, attempts int) ([][]float32, error) {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		vectors, retryAfter, err := e.call(ctx, body)
		if err == nil {
			if len(vectors) != want {
				return nil, fmt.Errorf("%s вернул %d векторов на %d текстов", e.provider, len(vectors), want)
			}
			return vectors, nil
		}
		lastErr = err
		if retryAfter < 0 || attempt == attempts {
			return nil, err
		}
		if retryAfter == 0 {
			retryAfter = time.Duration(1<<attempt) * time.Second
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryAfter):
		}
	}
	return nil, lastErr
}

type embeddingsResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// call возвращает retryAfter < 0, если повторять бессмысленно: неверный ключ
// или отбитый запрос от повтора не исправятся.
func (e endpoint) call(ctx context.Context, body []byte) ([][]float32, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	// Свой сервер (Ollama, TEI) часто без авторизации: пустой Bearer ему
	// не нужен, а некоторым прокси он мешает.
	if e.key != "" {
		req.Header.Set("Authorization", "Bearer "+e.key)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("запрос к %s: %w: %w", e.provider, errTransport, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, 0, err
	}

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		wait := time.Duration(0)
		if s := resp.Header.Get("Retry-After"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				wait = time.Duration(n) * time.Second
			}
		}
		return nil, wait, &StatusError{Provider: e.provider, Code: resp.StatusCode, Body: snippet(payload)}
	default:
		return nil, -1, &StatusError{Provider: e.provider, Code: resp.StatusCode, Body: snippet(payload)}
	}

	var parsed embeddingsResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, -1, fmt.Errorf("разбор ответа %s: %w", e.provider, err)
	}

	// Порядок в ответе не гарантирован — раскладываем по index.
	out := make([][]float32, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, -1, fmt.Errorf("%s вернул индекс %d вне пачки", e.provider, d.Index)
		}
		if len(d.Embedding) != Dim {
			return nil, -1, &DimensionError{Provider: e.provider, Got: len(d.Embedding)}
		}
		out[d.Index] = d.Embedding
	}
	return out, 0, nil
}

// transient — сбой, который проходит сам: сеть, таймаут, перегрузка или 5xx.
// Всё остальное — неверный ключ, неизвестная модель, отбитый запрос, чужой
// формат ответа — ошибка настройки.
func transient(err error) bool {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Code == http.StatusTooManyRequests ||
			status.Code == http.StatusRequestTimeout || status.Code >= 500
	}
	var dim *DimensionError
	if errors.As(err, &dim) {
		return false
	}
	// Ошибки транспорта (отказ в соединении, DNS, таймаут) помечены
	// errTransport; истёкший контекст — тот же таймаут.
	return errors.Is(err, errTransport) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// errTransport помечает ошибку сети — до ответа сервера дело не дошло.
var errTransport = errors.New("сеть")

func snippet(b []byte) string {
	const limit = 300
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}

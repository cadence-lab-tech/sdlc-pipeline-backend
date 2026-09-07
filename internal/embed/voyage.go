package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Voyage — voyage-code-3 размерности 1024: модель, которую Anthropic
// рекомендует для кода, собственного embeddings API у Anthropic нет
// (решение 0008).
type Voyage struct {
	URL    string
	Key    string
	Name   string
	Batch  int
	Client *http.Client
}

const (
	defaultVoyageURL = "https://api.voyageai.com/v1/embeddings"
	defaultBatch     = 96
	maxAttempts      = 4
)

func NewVoyage(url, key, model string, batch int) *Voyage {
	if url == "" {
		url = defaultVoyageURL
	}
	if model == "" {
		model = "voyage-code-3"
	}
	if batch <= 0 {
		batch = defaultBatch
	}
	return &Voyage{
		URL: url, Key: key, Name: model, Batch: batch,
		// Таймаут больше обычного: пачка в сотню кусков кода считается
		// секундами, и обрывать её на середине дороже, чем подождать.
		Client: &http.Client{Timeout: 2 * time.Minute},
	}
}

func (v *Voyage) Model() string { return v.Name }

func (v *Voyage) Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += v.Batch {
		end := min(start+v.Batch, len(texts))
		part, err := v.embedBatch(ctx, texts[start:end], kind)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

type voyageResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

func (v *Voyage) embedBatch(ctx context.Context, texts []string, kind Kind) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{
		"input":            texts,
		"model":            v.Name,
		"input_type":       string(kind),
		"output_dimension": Dim,
		// Кусок, не влезший в контекст модели, обрезается, а не валит всю
		// пачку: один длинный файл не должен останавливать индексацию.
		"truncation": true,
	})
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		vectors, retryAfter, err := v.call(ctx, body)
		if err == nil {
			if len(vectors) != len(texts) {
				return nil, fmt.Errorf("voyage вернул %d векторов на %d текстов", len(vectors), len(texts))
			}
			return vectors, nil
		}
		lastErr = err
		if retryAfter < 0 || attempt == maxAttempts {
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

// call возвращает retryAfter < 0, если повторять бессмысленно: неверный ключ
// или отбитый запрос от повтора не исправятся.
func (v *Voyage) call(ctx context.Context, body []byte) ([][]float32, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.URL, bytes.NewReader(body))
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("Authorization", "Bearer "+v.Key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.Client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("запрос к voyage: %w", err)
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
		return nil, wait, fmt.Errorf("voyage %d: %s", resp.StatusCode, snippet(payload))
	default:
		return nil, -1, fmt.Errorf("voyage %d: %s", resp.StatusCode, snippet(payload))
	}

	var parsed voyageResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return nil, -1, fmt.Errorf("разбор ответа voyage: %w", err)
	}

	// Порядок в ответе не гарантирован — раскладываем по index.
	out := make([][]float32, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, -1, fmt.Errorf("voyage вернул индекс %d вне пачки", d.Index)
		}
		if len(d.Embedding) != Dim {
			return nil, -1, fmt.Errorf("voyage вернул вектор размерности %d вместо %d", len(d.Embedding), Dim)
		}
		out[d.Index] = d.Embedding
	}
	return out, 0, nil
}

func snippet(b []byte) string {
	const limit = 300
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}

package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// OpenAI — любой сервер с OpenAI-совместимой ручкой POST {base}/embeddings:
// сам OpenAI, Ollama, TEI, vLLM. Клиент self-hosted выбирает модель сам, и
// единственное, что от неё требуется, — вектор размерности 1024 (решение 0008).
//
// input_type у этого API нет: модели, которым нужны префиксы запроса и
// документа (e5, nomic), работают через него хуже — это их выбор, а не наш.
type OpenAI struct {
	// BaseURL — адрес API без ручки, вида https://api.openai.com/v1.
	BaseURL string
	// Key необязателен: свой сервер часто без авторизации, и тогда заголовок
	// Authorization не отправляется вовсе.
	Key   string
	Name  string
	Batch int
	// SendDimensions добавляет в запрос dimensions: 1024. Модели с
	// matryoshka (text-embedding-3-*) так урезают вектор до схемы; часть
	// серверов (vLLM без matryoshka, некоторые сборки Ollama и TEI) отвечает
	// на незнакомое поле 400, и для них поле выключается.
	SendDimensions bool
	Client         *http.Client
}

const defaultOpenAIURL = "https://api.openai.com/v1"

func NewOpenAI(baseURL, key, model string, batch int, sendDimensions bool) *OpenAI {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = defaultOpenAIURL
	}
	if batch <= 0 {
		batch = defaultBatch
	}
	return &OpenAI{
		BaseURL: baseURL, Key: key, Name: model, Batch: batch,
		SendDimensions: sendDimensions, Client: newClient(),
	}
}

func (o *OpenAI) Model() string { return o.Name }

// Embed не различает документ и запрос: у OpenAI-совместимого API нет
// input_type.
func (o *OpenAI) Embed(ctx context.Context, texts []string, _ Kind) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += o.Batch {
		end := min(start+o.Batch, len(texts))
		part, err := o.embedBatch(ctx, texts[start:end], maxAttempts)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (o *OpenAI) probe(ctx context.Context) error {
	_, err := o.embedBatch(ctx, []string{probeText}, 1)
	return err
}

func (o *OpenAI) embedBatch(ctx context.Context, texts []string, attempts int) ([][]float32, error) {
	req := map[string]any{"model": o.Name, "input": texts}
	if o.SendDimensions {
		req["dimensions"] = Dim
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	ep := endpoint{provider: "openai", url: o.BaseURL + "/embeddings", key: o.Key, client: o.Client}
	return ep.embed(ctx, body, len(texts), attempts)
}

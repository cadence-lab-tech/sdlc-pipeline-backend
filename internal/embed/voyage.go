package embed

import (
	"context"
	"encoding/json"
	"net/http"
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

const defaultVoyageURL = "https://api.voyageai.com/v1/embeddings"

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
	return &Voyage{URL: url, Key: key, Name: model, Batch: batch, Client: newClient()}
}

func (v *Voyage) Model() string { return v.Name }

func (v *Voyage) Embed(ctx context.Context, texts []string, kind Kind) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += v.Batch {
		end := min(start+v.Batch, len(texts))
		part, err := v.embedBatch(ctx, texts[start:end], kind, maxAttempts)
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}

func (v *Voyage) probe(ctx context.Context) error {
	_, err := v.embedBatch(ctx, []string{probeText}, KindQuery, 1)
	return err
}

func (v *Voyage) embedBatch(ctx context.Context, texts []string, kind Kind, attempts int) ([][]float32, error) {
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
	ep := endpoint{provider: "voyage", url: v.URL, key: v.Key, client: v.Client}
	return ep.embed(ctx, body, len(texts), attempts)
}

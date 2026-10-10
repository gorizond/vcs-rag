package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Ollama — клиент Ollama (POST /api/embed).
type Ollama struct {
	base
}

// NewOllama создаёт клиент; baseURL — например http://localhost:11434.
func NewOllama(baseURL, model string, dim, batch int) *Ollama {
	return &Ollama{base{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		batch:   batch,
		dim:     dim,
		http:    &http.Client{},
	}}
}

// Embed вычисляет эмбеддинги батчами по batch текстов.
// keep_alive=10m — модель не выгружается между проходами.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, part := range o.batches(texts) {
		body, err := json.Marshal(map[string]any{
			"model":      o.model,
			"input":      part,
			"keep_alive": "10m",
		})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := o.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("ollama: %w", err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("ollama: чтение ответа: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("ollama: HTTP %d: %s", resp.StatusCode, truncate(payload))
		}
		var parsed struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := json.Unmarshal(payload, &parsed); err != nil {
			return nil, fmt.Errorf("ollama: разбор ответа: %w", err)
		}
		if err := o.validate(parsed.Embeddings, len(part)); err != nil {
			return nil, err
		}
		out = append(out, parsed.Embeddings...)
	}
	return out, nil
}

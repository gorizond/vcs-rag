package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// LMStudio — LM Studio client (OpenAI-compatible POST /v1/embeddings).
type LMStudio struct {
	base
}

// NewLMStudio creates a client; baseURL — e.g. http://localhost:1234.
func NewLMStudio(baseURL, model string, dim, batch int) *LMStudio {
	return &LMStudio{base{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		batch:   batch,
		dim:     dim,
		http:    &http.Client{},
	}}
}

// Embed computes embeddings in batches; the response is sorted by the index field.
func (l *LMStudio) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, part := range l.batches(texts) {
		body, err := json.Marshal(map[string]any{
			"model": l.model,
			"input": part,
		})
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/v1/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer lm-studio")

		resp, err := l.http.Do(req)
		if err != nil {
			return nil, netError("lm-studio", err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("lm-studio: reading response: %w", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, httpError("lm-studio", resp.StatusCode, payload)
		}
		var parsed struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
		}
		if err := json.Unmarshal(payload, &parsed); err != nil {
			return nil, fmt.Errorf("lm-studio: parsing response: %w", err)
		}
		sort.Slice(parsed.Data, func(i, j int) bool { return parsed.Data[i].Index < parsed.Data[j].Index })
		vecs := make([][]float32, 0, len(parsed.Data))
		for _, d := range parsed.Data {
			vecs = append(vecs, d.Embedding)
		}
		if err := l.validate(vecs, len(part)); err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

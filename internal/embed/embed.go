// Package embed — embedding clients (Ollama, LM Studio) with a common interface
// and dimension validation (FR-009).
package embed

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Provider — an embedding provider.
type Provider interface {
	Dim() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// base — common part of the clients: address, model, batch, dimension, HTTP.
type base struct {
	baseURL string
	model   string
	batch   int
	dim     int
	http    *http.Client
}

// Dim returns the vector dimension.
func (b *base) Dim() int { return b.dim }

// batches splits texts into batches of size batch.
func (b *base) batches(texts []string) [][]string {
	var out [][]string
	for start := 0; start < len(texts); start += b.batch {
		out = append(out, texts[start:min(start+b.batch, len(texts))])
	}
	return out
}

// validate checks the number of vectors and the dimension of each (FR-009).
func (b *base) validate(vecs [][]float32, want int) error {
	if len(vecs) != want {
		return fmt.Errorf("embeddings: got %d vectors, expected %d", len(vecs), want)
	}
	for i, v := range vecs {
		if len(v) != b.dim {
			return fmt.Errorf(
				"embeddings: vector %d has dimension %d, expected %d — check EMBEDDINGS_MODEL/DIM; changing the dimension requires reindex (quickstart §7)",
				i, len(v), b.dim)
		}
	}
	return nil
}

// truncate cuts the response body for error messages.
func truncate(data []byte) string {
	const maxLen = 300
	s := strings.TrimSpace(string(data))
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

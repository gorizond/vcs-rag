// Package embed — клиенты эмбеддингов (Ollama, LM Studio) с общим интерфейсом
// и валидацией размерности (FR-009).
package embed

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Provider — провайдер эмбеддингов.
type Provider interface {
	Dim() int
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// base — общая часть клиентов: адрес, модель, батч, размерность, HTTP.
type base struct {
	baseURL string
	model   string
	batch   int
	dim     int
	http    *http.Client
}

// Dim возвращает размерность векторов.
func (b *base) Dim() int { return b.dim }

// batches нарезает тексты на батчи размера batch.
func (b *base) batches(texts []string) [][]string {
	var out [][]string
	for start := 0; start < len(texts); start += b.batch {
		out = append(out, texts[start:min(start+b.batch, len(texts))])
	}
	return out
}

// validate проверяет число векторов и размерность каждого (FR-009).
func (b *base) validate(vecs [][]float32, want int) error {
	if len(vecs) != want {
		return fmt.Errorf("эмбеддинги: получено %d векторов, ожидалось %d", len(vecs), want)
	}
	for i, v := range vecs {
		if len(v) != b.dim {
			return fmt.Errorf(
				"эмбеддинги: вектор %d имеет размерность %d, ожидалась %d — проверьте EMBEDDINGS_MODEL/DIM; смена размерности требует переиндексации (quickstart §7)",
				i, len(v), b.dim)
		}
	}
	return nil
}

// truncate обрезает тело ответа для сообщений об ошибках.
func truncate(data []byte) string {
	const maxLen = 300
	s := strings.TrimSpace(string(data))
	if len(s) > maxLen {
		return s[:maxLen] + "…"
	}
	return s
}

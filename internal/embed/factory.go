package embed

import (
	"fmt"

	"github.com/gorizond/vcs-rag/internal/config"
)

// New creates an embedding provider from the config (FR-009).
// Each vector's dimension is validated in Embed (before writing to Qdrant).
func New(cfg *config.Config) (Provider, error) {
	switch cfg.Embeddings.Provider {
	case "ollama":
		return NewOllama(cfg.Embeddings.BaseURL, cfg.Embeddings.Model, cfg.Embeddings.Dim, cfg.Embeddings.Batch), nil
	case "lm-studio":
		return NewLMStudio(cfg.Embeddings.BaseURL, cfg.Embeddings.Model, cfg.Embeddings.Dim, cfg.Embeddings.Batch), nil
	default:
		return nil, fmt.Errorf("unknown embeddings provider %q", cfg.Embeddings.Provider)
	}
}

package embed

import (
	"fmt"

	"github.com/gorizond/vcs-rag/internal/config"
)

// New создаёт провайдера эмбеддингов по конфигурации (FR-009).
// Валидация размерности каждого вектора — в Embed (до записи в Qdrant).
func New(cfg *config.Config) (Provider, error) {
	switch cfg.Embeddings.Provider {
	case "ollama":
		return NewOllama(cfg.Embeddings.BaseURL, cfg.Embeddings.Model, cfg.Embeddings.Dim, cfg.Embeddings.Batch), nil
	case "lm-studio":
		return NewLMStudio(cfg.Embeddings.BaseURL, cfg.Embeddings.Model, cfg.Embeddings.Dim, cfg.Embeddings.Batch), nil
	default:
		return nil, fmt.Errorf("неизвестный провайдер эмбеддингов %q", cfg.Embeddings.Provider)
	}
}

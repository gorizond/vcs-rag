// Package worker — фоновые линии: discovery, зеркалирование, индексация (FR-005/006/018).
package worker

import (
	"context"
	"log/slog"

	"github.com/gorizond/vcs-rag/internal/config"
)

// Run запускает воркер и блокируется до отмены контекста.
// Сейчас реализована линия discovery (US1); mirror/index — US2+.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	return runDiscovery(ctx, cfg, logger)
}

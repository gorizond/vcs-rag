// Package worker — фоновый цикл зеркалирования и индексации (FR-005/006/018).
package worker

import (
	"context"
	"log/slog"

	"github.com/gorizond/vcs-rag/internal/config"
)

// Run запускает worker и блокируется до отмены контекста.
// Заглушка: реальная реализация — в US1+.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("worker: ещё не реализовано (US1+)")
	return nil
}

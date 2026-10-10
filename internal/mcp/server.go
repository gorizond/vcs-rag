// Package mcp — MCP-сервер поиска по коду и метаданным (FR-013).
package mcp

import (
	"context"
	"log/slog"

	"github.com/gorizond/vcs-rag/internal/config"
)

// Run запускает MCP-сервер и блокируется до отмены контекста.
// Заглушка: реальная реализация — в US1+.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger.Info("mcp: ещё не реализовано (US1+)")
	return nil
}

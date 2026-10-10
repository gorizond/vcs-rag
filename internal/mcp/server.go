// Package mcp — MCP-сервер поиска (FR-013): stdio (default) и streamable HTTP,
// инструменты поверх store/embed. Сервер стартует и отвечает даже при
// недоступном Qdrant — ошибки бэкендов возвращаются структурированными
// ответами инструментов, процесс не падает.
package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/store"
)

// Run запускает MCP-сервер: stdio по умолчанию, streamable HTTP при
// MCP_TRANSPORT=sse (адрес MCP_ADDR). Блокируется до отмены контекста.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string) error {
	emb, err := embed.New(cfg)
	if err != nil {
		return err
	}
	st, err := store.New(cfg.Qdrant)
	if err != nil {
		return err
	}
	defer st.Close()

	server := mcp.NewServer(
		&mcp.Implementation{Name: "vcs-rag", Version: version},
		&mcp.ServerOptions{Logger: logger},
	)
	registerTools(server, &tools{embedder: emb, searcher: st})

	if cfg.MCP.Transport == "sse" {
		return runHTTP(ctx, cfg, logger, server)
	}
	logger.Info("MCP: транспорт stdio")
	return server.Run(ctx, &mcp.StdioTransport{})
}

// runHTTP обслуживает streamable HTTP на MCP_ADDR.
func runHTTP(ctx context.Context, cfg *config.Config, logger *slog.Logger, server *mcp.Server) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := &http.Server{Addr: cfg.MCP.Addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	logger.Info("MCP: транспорт streamable HTTP", "addr", cfg.MCP.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

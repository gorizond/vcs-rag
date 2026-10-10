// Package mcp — MCP search server (FR-013): stdio (default) and streamable HTTP,
// tools on top of store/embed. The server starts and responds even when
// Qdrant is unavailable — backend errors are returned as structured
// tool responses, the process does not crash.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/embed"
	"github.com/gorizond/vcs-rag/internal/provider/factory"
	"github.com/gorizond/vcs-rag/internal/store"
)

// Run starts the MCP server: stdio by default, streamable HTTP when
// MCP_TRANSPORT=sse (address MCP_ADDR). Blocks until the context is canceled.
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

	// T113/T119: refresh_repo is registered only with NATS_URL set; the
	// subject/KV names come from the instance id; search itself keeps working
	// regardless of the NATS availability.
	tl := newTools(emb, st, nil)
	// T132: the repository list index resolves short-form repo filters without
	// extra API calls; the provider constructor makes no network calls, and a
	// failure only degrades to the raw path (the search keeps working).
	if p, err := factory.New(cfg); err != nil {
		logger.Warn("repo index disabled", "err", err)
	} else {
		tl.repos = newRepoIndex(p)
	}
	if cfg.NATS.URL != "" {
		names := config.RefreshNames(cfg.NATS.InstanceID)
		ref, err := connectRefreshClient(cfg.NATS.URL, names)
		if err != nil {
			return fmt.Errorf("nats: %w", err)
		}
		defer ref.Close()
		if ref.Connected() {
			logger.Info("NATS connected", "instance", names.InstanceID)
		} else {
			logger.Warn("NATS unreachable, reconnecting in background", "instance", names.InstanceID)
		}
		tl.refresher = ref
		tl.inflight = newFlightGroup()
		tl.auto = newAutoClient(ref, logger) // T134: auto-freshness before repo searches
	}
	registerTools(server, tl)

	if cfg.MCP.Transport == "sse" {
		return runHTTP(ctx, cfg, logger, server)
	}
	logger.Info("MCP: transport stdio")
	return server.Run(ctx, &mcp.StdioTransport{})
}

// runHTTP serves streamable HTTP on MCP_ADDR.
func runHTTP(ctx context.Context, cfg *config.Config, logger *slog.Logger, server *mcp.Server) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	srv := &http.Server{Addr: cfg.MCP.Addr, Handler: handler}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	logger.Info("MCP: transport streamable HTTP", "addr", cfg.MCP.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

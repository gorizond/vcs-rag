// Package mcp — MCP-сервер поиска по коду и метаданным (FR-013).
package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gorizond/vcs-rag/internal/config"
)

// Run запускает MCP-сервер: stdio по умолчанию, streamable HTTP при
// MCP_TRANSPORT=sse (адрес MCP_ADDR). Блокируется до отмены контекста.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger, version string) error {
	server := mcp.NewServer(
		&mcp.Implementation{Name: "vcs-rag", Version: version},
		&mcp.ServerOptions{Logger: logger},
	)
	registerTools(server)

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

// stubOutput — структурированный ответ заглушки (US3/US4).
type stubOutput struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

var stub = stubOutput{Status: "not_implemented", Note: "not implemented (US3/US4)"}

// searchCodeInput — вход search_code (contracts/mcp-tools.md).
type searchCodeInput struct {
	Query    string `json:"query" jsonschema:"текст поискового запроса"`
	VCSType  string `json:"vcs_type,omitempty" jsonschema:"фильтр: gitlab | github"`
	GroupOrg string `json:"group_org,omitempty" jsonschema:"группа GitLab или организация GitHub"`
	Repo     string `json:"repo,omitempty" jsonschema:"путь репозитория"`
	Branch   string `json:"branch,omitempty" jsonschema:"ветка"`
	TopK     int    `json:"top_k,omitempty" jsonschema:"сколько результатов вернуть (по умолчанию 5)"`
}

// metadataQueryInput — общий вход search_pull_requests / search_issues.
type metadataQueryInput struct {
	Query    string `json:"query" jsonschema:"текст поискового запроса"`
	Repo     string `json:"repo,omitempty" jsonschema:"путь репозитория"`
	GroupOrg string `json:"group_org,omitempty" jsonschema:"группа GitLab или организация GitHub"`
	State    string `json:"state,omitempty" jsonschema:"состояние: opened | closed | merged"`
	TopK     int    `json:"top_k,omitempty" jsonschema:"сколько результатов вернуть (по умолчанию 5)"`
}

// ciFailuresInput — вход search_ci_failures.
type ciFailuresInput struct {
	Query  string `json:"query" jsonschema:"текст поискового запроса"`
	Repo   string `json:"repo,omitempty" jsonschema:"путь репозитория"`
	Branch string `json:"branch,omitempty" jsonschema:"ветка"`
	TopK   int    `json:"top_k,omitempty" jsonschema:"сколько результатов вернуть (по умолчанию 5)"`
}

// registerTools регистрирует инструменты из contracts/mcp-tools.md.
// Обработчики — заглушки: реальный поиск появится в US3/US4.
func registerTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_code",
		Description: "Семантический поиск по проиндексированному коду",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ searchCodeInput) (*mcp.CallToolResult, stubOutput, error) {
		return nil, stub, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_pull_requests",
		Description: "Семантический поиск по pull/merge requests",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ metadataQueryInput) (*mcp.CallToolResult, stubOutput, error) {
		return nil, stub, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_issues",
		Description: "Семантический поиск по issues (индексация — US4/FR-012)",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ metadataQueryInput) (*mcp.CallToolResult, stubOutput, error) {
		return nil, stub, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_ci_failures",
		Description: "Семантический поиск по логам упавших CI-джобов",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ ciFailuresInput) (*mcp.CallToolResult, stubOutput, error) {
		return nil, stub, nil
	})
}

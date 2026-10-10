// Command vcs-rag — единый бинарник сервиса: worker и MCP-сервер (FR-001).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorizond/vcs-rag/internal/config"
	"github.com/gorizond/vcs-rag/internal/log"
	"github.com/gorizond/vcs-rag/internal/mcp"
	"github.com/gorizond/vcs-rag/internal/worker"
)

func main() {
	os.Exit(run())
}

// run возвращает код возврата: 0 — штатно, 1 — ошибка запуска, 2 — ошибка конфигурации.
func run() int {
	mode := flag.String("mode", "", "режим работы: worker | mcp")
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	flag.Parse()

	if *showVersion {
		fmt.Printf("vcs-rag %s\n", version)
		return 0
	}
	if *mode != "worker" && *mode != "mcp" {
		fmt.Fprintln(os.Stderr, "usage: vcs-rag -mode=worker|mcp [-version]")
		return 2
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	// Логи — в stderr: stdout принадлежит MCP-транспорту (stdio).
	logger := log.New(os.Stderr, false)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var runErr error
	switch *mode {
	case "worker":
		runErr = worker.Run(ctx, cfg, logger)
	case "mcp":
		runErr = mcp.Run(ctx, cfg, logger, version)
	}
	if runErr != nil {
		logger.Error("завершение с ошибкой", "err", runErr)
		return 1
	}
	return 0
}

// Command vcs-rag is the single service binary: worker and MCP server (FR-001).
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

// run returns the exit code: 0 — success, 1 — startup error, 2 — configuration error.
func run() int {
	mode := flag.String("mode", "", "operating mode: worker | mcp")
	showVersion := flag.Bool("version", false, "print the version and exit")
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

	// Logs go to stderr: stdout belongs to the MCP transport (stdio).
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
		logger.Error("exiting with error", "err", runErr)
		return 1
	}
	return 0
}

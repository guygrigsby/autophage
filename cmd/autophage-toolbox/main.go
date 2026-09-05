// Command autophage-toolbox serves the coding tools over MCP stdio inside the
// sandbox. It has no network, no secrets and no opinion; it executes what
// the daemon's agent asks, rooted at the work dir.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/guygrigsby/autophage/internal/toolbox"
)

func main() {
	workDir := flag.String("workdir", "/work", "directory every tool is rooted at")
	bashTimeout := flag.Duration("bash-timeout", 15*time.Minute, "per-command limit for the bash tool")
	flag.Parse()
	log.SetOutput(os.Stderr)

	srv, err := toolbox.New(toolbox.Options{WorkDir: *workDir, BashTimeout: *bashTimeout})
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/internal/mcpstdio"
)

// runMCP implements `fabrik mcp`: a stdio MCP server (launched per session by
// Claude Code — `claude mcp add fabrik -- fabrik mcp`) that proxies the
// read-only fabrik_status / fabrik_board / fabrik_health tools to the running
// daemon's local socket (#1967, ADR-1966-a).
//
// stdout is the protocol channel and carries JSON-RPC frames only. This command
// deliberately loads no config or .env, prints no banner and starts no TUI; all
// diagnostics go to stderr.
func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "", "Fabrik directory of the daemon to talk to (default: the current directory)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: fabrik mcp [--dir <fabrik-dir>]\n\n")
		fmt.Fprintf(os.Stderr, "Run a stdio MCP server exposing read-only overseer tools backed by the running\n")
		fmt.Fprintf(os.Stderr, "daemon's local socket. Register it with: claude mcp add fabrik -- fabrik mcp\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("fabrik mcp: unexpected argument %q", fs.Arg(0))
	}

	fabrikDir := *dir
	if fabrikDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("fabrik mcp: determining the current directory: %w", err)
		}
		fabrikDir = wd
	}
	fabrikDir, err := filepath.Abs(fabrikDir)
	if err != nil {
		return fmt.Errorf("fabrik mcp: resolving --dir: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &mcpstdio.Server{
		SocketPath: localapi.SocketPath(fabrikDir),
		Version:    Version,
		In:         os.Stdin,
		Out:        os.Stdout,
		Err:        os.Stderr,
	}
	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("fabrik mcp: %w", err)
	}
	return nil
}

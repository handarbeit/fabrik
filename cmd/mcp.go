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
	subscriber := fs.String("subscriber", "", "stable name this server attaches to the daemon under for pushed channel events — your topic or session name, never a PID (default: $FABRIK_SUBSCRIBER; empty disables push)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: fabrik mcp [--dir <fabrik-dir>] [--subscriber <name>]\n\n")
		fmt.Fprintf(os.Stderr, "Run a stdio MCP server exposing read-only overseer tools backed by the running\n")
		fmt.Fprintf(os.Stderr, "daemon's local socket. Register it with: claude mcp add fabrik -- fabrik mcp\n\n")
		fmt.Fprintf(os.Stderr, "With --subscriber (or FABRIK_SUBSCRIBER) it also pushes events into the session via\n")
		fmt.Fprintf(os.Stderr, "Claude Code Channels (research preview); start claude with\n")
		fmt.Fprintf(os.Stderr, "--dangerously-load-development-channels server:fabrik to receive them.\n\n")
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
		Subscriber: subscriberName(*subscriber),
		In:         os.Stdin,
		Out:        os.Stdout,
		Err:        os.Stderr,
	}
	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("fabrik mcp: %w", err)
	}
	return nil
}

// subscriberName resolves the subscriber: the flag, else FABRIK_SUBSCRIBER. There
// is deliberately no generated default — the name keys the daemon's held queue,
// so it must be stable across sessions (#1968).
func subscriberName(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv("FABRIK_SUBSCRIBER")
}

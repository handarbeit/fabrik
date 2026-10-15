package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/handarbeit/fabrik/internal/localapi"
	"github.com/handarbeit/fabrik/internal/mcpstdio"
)

// runMCP implements `fabrik mcp`: a stdio MCP server (launched per session by
// Claude Code — via the `fabrik` plugin's bundled .mcp.json, or by hand with
// `claude mcp add fabrik -- fabrik mcp`) that proxies the
// read-only fabrik_status / fabrik_board / fabrik_health / fabrik_situation tools,
// and the six overseer action tools (#1969, ADR-1966-c), to the running daemon's local
// socket (#1967, ADR-1966-a).
//
// stdout is the protocol channel and carries JSON-RPC frames only. This command
// deliberately loads no config or .env, prints no banner and starts no TUI; all
// diagnostics go to stderr.
func runMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "", "Fabrik directory of the daemon to talk to, used as given (default: $FABRIK_MCP_DIR, used as given, else the nearest ancestor of $CLAUDE_PROJECT_DIR or the current directory that contains .fabrik/)")
	subscriber := fs.String("subscriber", "", "stable name this server attaches to the daemon under for pushed channel events and names in action audit comments — your topic or session name, never a PID (default: $FABRIK_SUBSCRIBER, else $CLAUDE_TOPIC, else the OS username; the OS-username default names audit comments but does not start the push stream)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: fabrik mcp [--dir <fabrik-dir>] [--subscriber <name>]\n\n")
		fmt.Fprintf(os.Stderr, "Run a stdio MCP server exposing overseer tools backed by the running daemon's\n")
		fmt.Fprintf(os.Stderr, "local socket: read-only status/board/health, plus promote / set_autonomy /\n")
		fmt.Fprintf(os.Stderr, "revalidate / clear_claude_limit actions (they leave an audit comment naming the\n")
		fmt.Fprintf(os.Stderr, "subscriber: --subscriber, else FABRIK_SUBSCRIBER, else CLAUDE_TOPIC, else the OS\n")
		fmt.Fprintf(os.Stderr, "username). Register it with: claude mcp add fabrik -- fabrik mcp\n\n")
		fmt.Fprintf(os.Stderr, "With --subscriber, FABRIK_SUBSCRIBER or CLAUDE_TOPIC it also pushes events into the session via\n")
		fmt.Fprintf(os.Stderr, "Claude Code Channels (research preview); start claude with\n")
		fmt.Fprintf(os.Stderr, "--dangerously-load-development-channels server:fabrik (plugin install:\n")
		fmt.Fprintf(os.Stderr, "plugin:fabrik@fabrik) to receive them.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("fabrik mcp: unexpected argument %q", fs.Arg(0))
	}

	srv, err := newMCPServer(*dir, *subscriber)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := srv.Serve(ctx); err != nil && ctx.Err() == nil {
		return fmt.Errorf("fabrik mcp: %w", err)
	}
	return nil
}

// newMCPServer resolves the Fabrik directory and builds the server for it. The
// channel stream is disabled unless the resolved directory contains .fabrik/:
// outside a Fabrik project there is no daemon to reach, so a stream (and its
// daemon-unreachable notice) would only mislead (#2207). The subscriber name is
// kept either way — it is also the audit name for the action tools.
func newMCPServer(flagDir, flagSubscriber string) (*mcpstdio.Server, error) {
	fabrikDir, err := resolveMCPDir(flagDir)
	if err != nil {
		return nil, err
	}
	fabrikDir, err = filepath.Abs(fabrikDir)
	if err != nil {
		return nil, fmt.Errorf("fabrik mcp: resolving --dir: %w", err)
	}
	name, source := resolveSubscriber(flagSubscriber, osUsername)
	return &mcpstdio.Server{
		SocketPath:       localapi.SocketPath(fabrikDir),
		Version:          Version,
		Subscriber:       name,
		SubscriberSource: source,
		DisableStream:    !hasFabrikDir(fabrikDir),
		In:               os.Stdin,
		Out:              os.Stdout,
		Err:              os.Stderr,
	}, nil
}

// resolveSubscriber resolves the subscriber name and says which input produced
// it (#2322, ADR 2322): --subscriber, else FABRIK_SUBSCRIBER, else CLAUDE_TOPIC
// (exported by `topic` on every session it launches, stable across restarts),
// else the fixed default — the OS username. Every source goes through
// subscriberValue, so an unexpanded "${...}" placeholder or a blank value counts
// as unset. lookupUser supplies the username when $USER is empty; no name at all
// comes back as ("", SourceNone).
//
// There is deliberately no PID- or session-id-derived default: the name keys the
// daemon's held queue and must be stable across restarts (#1968). A fixed
// default is stable; its only cost is that default-named sessions share a name.
func resolveSubscriber(flagValue string, lookupUser func() string) (string, mcpstdio.NameSource) {
	if v := subscriberValue(flagValue); v != "" {
		return v, mcpstdio.SourceFlag
	}
	if v := subscriberValue(os.Getenv("FABRIK_SUBSCRIBER")); v != "" {
		return v, mcpstdio.SourceEnv
	}
	if v := subscriberValue(os.Getenv("CLAUDE_TOPIC")); v != "" {
		return v, mcpstdio.SourceTopic
	}
	if v := subscriberValue(os.Getenv("USER")); v != "" {
		return v, mcpstdio.SourceDefault
	}
	if lookupUser != nil {
		if v := subscriberValue(lookupUser()); v != "" {
			return v, mcpstdio.SourceDefault
		}
	}
	return "", mcpstdio.SourceNone
}

// osUsername is the os/user fallback for the default name; "" when the lookup
// fails (static binaries and containers without a passwd entry).
func osUsername() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// subscriberValue is settingValue for a subscriber name: it also trims, and a
// blank value is unset.
func subscriberValue(v string) string {
	return strings.TrimSpace(settingValue(v))
}

// settingValue returns v, or "" when v is an unexpanded "${...}" placeholder.
// The fabrik plugin's .mcp.json passes optional user settings through the
// environment; if Claude Code leaves the literal ${user_config.X} text in place
// for a setting the user never filled in, that text must read as "not set", not
// as a subscriber name or a directory (#2141).
func settingValue(v string) string {
	if isUnexpandedPlaceholder(v) {
		return ""
	}
	return v
}

// isUnexpandedPlaceholder reports whether v still contains a literal "${...}"
// reference that Claude Code did not expand.
func isUnexpandedPlaceholder(v string) bool {
	return strings.Contains(v, "${")
}

// resolveMCPDir resolves the Fabrik directory: --dir, else $FABRIK_MCP_DIR (the
// fabrik plugin's optional directory setting, passed through the environment so
// an unset setting is an empty variable rather than an empty argument), else
// $CLAUDE_PROJECT_DIR (the session's project, which Claude Code exports to
// plugin MCP servers whatever their own working directory is), else the
// current directory (#2141).
//
// --dir and $FABRIK_MCP_DIR are used literally. A directory taken from
// $CLAUDE_PROJECT_DIR or the working directory is replaced by the nearest
// ancestor containing .fabrik/, so a session started in a subdirectory of a
// Fabrik project still finds the project's daemon (#2207). With no such
// ancestor the originally picked directory is kept.
func resolveMCPDir(flagValue string) (string, error) {
	dir, literal, err := pickMCPDir(flagValue)
	if err != nil || literal {
		return dir, err
	}
	home, _ := os.UserHomeDir() // an error leaves "": only the root bounds the walk
	if root, ok := findFabrikRoot(dir, home); ok {
		return root, nil
	}
	return dir, nil
}

// pickMCPDir applies the source precedence and reports whether the winner is a
// literal source (--dir, $FABRIK_MCP_DIR) rather than a derived one.
func pickMCPDir(flagValue string) (dir string, literal bool, err error) {
	for _, v := range []string{flagValue, os.Getenv("FABRIK_MCP_DIR")} {
		if v = settingValue(v); v != "" {
			return v, true, nil
		}
	}
	if v := settingValue(os.Getenv("CLAUDE_PROJECT_DIR")); v != "" {
		return v, false, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", false, fmt.Errorf("fabrik mcp: determining the current directory: %w", err)
	}
	return wd, false, nil
}

// hasFabrikDir reports whether dir contains a directory named .fabrik (a plain
// file of that name does not count).
func hasFabrikDir(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".fabrik"))
	return err == nil && fi.IsDir()
}

// findFabrikRoot walks up from start to the nearest directory containing
// .fabrik/. When start lies inside home, home itself is the last directory
// checked and nothing above it is looked at; otherwise (or when home is "") the
// walk runs to the filesystem root. It does stat calls only.
func findFabrikRoot(start, home string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	stopAtHome := false
	if home != "" {
		if h, err := filepath.Abs(home); err == nil {
			home = h
			if rel, err := filepath.Rel(home, dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				stopAtHome = true
			}
		}
	}
	for {
		if hasFabrikDir(dir) {
			return dir, true
		}
		if stopAtHome && dir == home {
			return "", false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

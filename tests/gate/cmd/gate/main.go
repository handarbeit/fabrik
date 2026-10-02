// Command gate is the live e2e release-gate runner: the Go port of
// scripts/e2e/run.sh and scripts/e2e/reset.sh (#1994, ADR-1994). Those two paths
// are now thin shims that exec it.
//
//	gate [run] [--clean] [go test flags...]   # the gate (what run.sh did)
//	gate reset [--worktrees]                  # clear the bed (what reset.sh did)
//
// It is not part of the fabrik binary.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/handarbeit/fabrik/tests/gate"
)

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(argv []string) int {
	sub, argv := splitSubcommand(argv)

	// INT/TERM cancel the context; every child the gate started is then reaped
	// (internal/sessionreap), so nothing is left behind headless. The exit code
	// is the shell convention, 128+signum (130 INT, 143 TERM).
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var got os.Signal
	gotCh := make(chan struct{})
	go func() {
		got = <-sigs
		close(gotCh)
		cancel()
	}()

	code := run(ctx, sub, argv)

	select {
	case <-gotCh:
		if s, ok := got.(syscall.Signal); ok {
			return 128 + int(s)
		}
		return 130
	default:
	}
	return code
}

// splitSubcommand peels an explicit "run" or "reset" off the front; anything
// else is the gate's own arguments (--clean, go test flags).
func splitSubcommand(argv []string) (string, []string) {
	if len(argv) > 0 && (argv[0] == "run" || argv[0] == "reset") {
		return argv[0], argv[1:]
	}
	return "run", argv
}

func run(ctx context.Context, sub string, argv []string) int {
	probe := gate.NewGate(gate.Config{}, os.Stdout, os.Stderr)
	root, err := probe.RepoRoot(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gate: %v\n", err)
		return gate.ExitUsage
	}
	cfg, err := gate.LoadConfig(os.Getenv, root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gate: %v\n", err)
		return gate.ExitUsage
	}
	g := gate.NewGate(cfg, os.Stdout, os.Stderr)

	if sub == "reset" {
		var opts gate.ResetOptions
		for _, a := range argv {
			if a == "--worktrees" {
				opts.Worktrees = true
			}
		}
		if err := g.Reset(ctx, opts); err != nil {
			if ee, ok := err.(*gate.ExitError); ok {
				fmt.Fprintln(os.Stderr, ee.Msg)
				return ee.Code
			}
			fmt.Fprintf(os.Stderr, "gate: %v\n", err)
			return 1
		}
		return 0
	}
	return g.Run(ctx, argv)
}

// Command gate is the live e2e release-gate runner: the Go port of
// scripts/e2e/run.sh and scripts/e2e/reset.sh (#1994, ADR-1994). Those two paths
// are now thin shims that exec it.
//
//	gate [run] [--clean] [go test flags...]   # the gate (what run.sh did)
//	gate reset [--worktrees] [--bed <dir>]    # clear the bed(s) (what reset.sh did; every E2E_BEDS bed by default, #1976)
//	gate [run] --resume [--clean] ...         # run only what the coverage ledger lacks (#1972)
//	gate coverage [--sha S] [--format notes]  # is live coverage complete for S? (exit 0 / 8)
//	gate report [--sha S] [--baseline B]      # measured per-test runtime from the archive (#1992)
//
// It is not part of the fabrik binary.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
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

// splitSubcommand peels an explicit "run", "reset", "coverage" or "report" off the front; anything
// else is the gate's own arguments (--clean, go test flags).
func splitSubcommand(argv []string) (string, []string) {
	if len(argv) > 0 && (argv[0] == "run" || argv[0] == "reset" || argv[0] == "coverage" || argv[0] == "report") {
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

	if sub == "coverage" {
		return g.Coverage(ctx, argv)
	}
	if sub == "report" {
		return g.ReportCmd(ctx, argv)
	}
	if sub == "reset" {
		opts, bed, err := parseResetArgs(argv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gate reset: %v\n", err)
			return gate.ExitUsage
		}
		if err := g.ResetBeds(ctx, opts, bed); err != nil {
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

// parseResetArgs reads `gate reset`'s flags: --worktrees, and --bed <dir> /
// --bed=<dir> to reset only that bed (#1976; the default is every configured
// bed). Other arguments are ignored, as reset.sh ignored them.
func parseResetArgs(argv []string) (opts gate.ResetOptions, bed string, err error) {
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--worktrees":
			opts.Worktrees = true
		case a == "--bed":
			if i+1 >= len(argv) || argv[i+1] == "" {
				return opts, "", fmt.Errorf("--bed needs a bed directory")
			}
			i++
			bed = argv[i]
		case strings.HasPrefix(a, "--bed="):
			if bed = strings.TrimPrefix(a, "--bed="); bed == "" {
				return opts, "", fmt.Errorf("--bed needs a bed directory")
			}
		}
	}
	return opts, bed, nil
}

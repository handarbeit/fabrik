package gate

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Plan is what the preconditions and the scheduler agree on for one invocation.
type Plan struct {
	// AuthModes are the auth legs to run ("pat", "app"), in order.
	AuthModes []string
	// Args are the caller's arguments with a leading --clean already consumed;
	// they are passed through to `go test`.
	Args []string
	// RawArgs are the arguments as given (--clean included), which is what the
	// reviewer-reachable check scanned in bash.
	RawArgs []string
	// Clean: reset the bed before the run.
	Clean bool
}

// Preflight is one precondition probe. The list is ordered and runs before ANY
// live spend (before the pre-gate, let alone the bed). A probe fails the run
// by returning an *ExitError, normally ExitPreconditionFailed. #1974 adds its
// environment probes by appending here; nothing else changes.
type Preflight struct {
	Name string
	Run  func(ctx context.Context, g *Gate, p *Plan) error
}

// Gate is one gate invocation: configuration plus every seam a test needs.
type Gate struct {
	Cfg  Config
	Out  io.Writer // stdout
	Err  io.Writer // stderr
	Exec Commander

	// Env is the environment children inherit (the gate's own, with per-leg
	// overrides layered on top by withEnv). Getenv reads the same set.
	Env []string

	// Preflights is the ordered probe list; NewGate fills in the defaults.
	Preflights []Preflight
	// Scheduler runs the cells; default is the serial auth × train loop.
	Scheduler Scheduler
	// OnLeg, if set, observes every finished leg (the #1972 ledger seam).
	OnLeg func(LegResult)

	// Seams for time and the OS.
	Sleep func(time.Duration)
	Now   func() time.Time
	// ProcCwd resolves a process's working directory ("" if unknown).
	ProcCwd func(ctx context.Context, pid int) string
	// Self is this process's PID (excluded from competing-consumer discovery).
	Self int

	// runLegFn replaces RunLeg for scheduler tests.
	runLegFn func(context.Context, Cell) error
}

// NewGate wires a Gate around the real OS.
func NewGate(cfg Config, out, errw io.Writer) *Gate {
	g := &Gate{
		Cfg:   cfg,
		Out:   out,
		Err:   errw,
		Exec:  OSExec{},
		Env:   os.Environ(),
		Sleep: time.Sleep,
		Now:   time.Now,
		Self:  os.Getpid(),
	}
	g.ProcCwd = g.defaultProcCwd
	g.Scheduler = SerialScheduler{}
	g.Preflights = DefaultPreflights()
	return g
}

// DefaultPreflights is the run.sh precondition order: auth-mode, competing
// token consumer, reviewer reachable, then the (informational) parity summary.
// The parity summary never gates and prints even when the pre-gate is skipped.
func DefaultPreflights() []Preflight {
	return []Preflight{
		{Name: "auth-mode", Run: func(_ context.Context, g *Gate, p *Plan) error { return g.CheckAuthModePreconditions(p.AuthModes) }},
		{Name: "competing-token-consumers", Run: func(ctx context.Context, g *Gate, p *Plan) error {
			return g.CheckCompetingTokenConsumers(ctx, p.AuthModes)
		}},
		{Name: "reviewer-reachable", Run: func(_ context.Context, g *Gate, p *Plan) error { return g.CheckReviewerReachable(p.RawArgs) }},
		{Name: "sim-parity", Run: func(_ context.Context, g *Gate, _ *Plan) error { g.PrintSimParitySummary(); return nil }},
	}
}

// Getenv looks a variable up in g.Env (last assignment wins, as in a real
// environment block).
func (g *Gate) Getenv(key string) string {
	v := ""
	prefix := key + "="
	for _, kv := range g.Env {
		if strings.HasPrefix(kv, prefix) {
			v = kv[len(prefix):]
		}
	}
	return v
}

// withEnv returns env with each KEY=VALUE in kvs set, replacing any existing
// assignment of the same key.
func withEnv(env []string, kvs ...string) []string {
	keys := map[string]bool{}
	for _, kv := range kvs {
		if i := strings.IndexByte(kv, '='); i > 0 {
			keys[kv[:i]] = true
		}
	}
	out := make([]string, 0, len(env)+len(kvs))
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 && keys[e[:i]] {
			continue
		}
		out = append(out, e)
	}
	return append(out, kvs...)
}

// withoutEnv drops the named keys.
func withoutEnv(env []string, keys ...string) []string {
	drop := map[string]bool{}
	for _, k := range keys {
		drop[k] = true
	}
	out := make([]string, 0, len(env))
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 && drop[e[:i]] {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (g *Gate) outf(format string, args ...any) { fmt.Fprintf(g.Out, format, args...) }
func (g *Gate) errf(format string, args ...any) { fmt.Fprintf(g.Err, format, args...) }
func (g *Gate) outln(s string)                  { fmt.Fprintln(g.Out, s) }
func (g *Gate) errln(s string)                  { fmt.Fprintln(g.Err, s) }

// Run executes the whole gate and returns the process exit code. The order is
// load-bearing and mirrors run.sh's dispatch guard exactly:
//
//	bed token → auth modes → preflight probes → parity summary → pre-gate →
//	bed preflight → --clean reset → bed start → legs
//
// The pre-gate runs strictly before any bed preflight, build or live call
// (ADR-1454: a pre-gate failure is proven, by construction, to have spent
// nothing); preflight leaves the bed STOPPED, --clean's reset runs against the
// stopped bed, and only then is the engine started.
func (g *Gate) Run(ctx context.Context, argv []string) int {
	err := g.run(ctx, argv)
	if err == nil {
		return 0
	}
	if ee, ok := err.(*ExitError); ok {
		if ee.Msg != "" {
			g.errln(ee.Msg)
		}
		return ee.Code
	}
	if ctx.Err() != nil {
		return 130
	}
	g.errf("gate: %v\n", err)
	return 1
}

func (g *Gate) run(ctx context.Context, argv []string) error {
	args := ParseRunArgs(argv)

	// BED_TOKEN: a missing token only degrades the budget report and the
	// competing-consumer check — it is a warning, not fatal.
	g.Cfg.BedToken = EnvFileValue(g.Cfg.TestBed+"/.env", "FABRIK_TOKEN")
	if g.Cfg.BedToken == "" {
		g.errf("warning: could not read FABRIK_TOKEN from %s/.env — GraphQL budget reporting and the competing-token-consumer check will be skipped\n", g.Cfg.TestBed)
	}

	modes, err := ResolveAuthModes(g.Getenv("E2E_AUTH_MODE"))
	if err != nil {
		g.errln(err.Error())
		return &ExitError{Code: ExitPreconditionFailed}
	}
	plan := &Plan{AuthModes: modes, Args: args.Rest, RawArgs: argv, Clean: args.Clean}

	for _, pf := range g.Preflights {
		if err := pf.Run(ctx, g, plan); err != nil {
			return err
		}
	}

	if err := g.RunPregate(ctx); err != nil {
		return err
	}
	if err := g.PrepareBedAndReset(ctx, plan.Clean); err != nil {
		return err
	}

	cells := PlanCells(PlanInput{
		AuthModes:    plan.AuthModes,
		TrainMode:    g.Getenv("E2E_TRAIN_MODE"),
		CallerHasRun: HasRunFlag(plan.Args),
		Parallel:     g.Cfg.Parallel,
		ParallelOn:   g.Cfg.ParallelOn,
		Args:         plan.Args,
	})
	return g.Scheduler.Run(ctx, g, cells)
}

// gitToplevel resolves the repo root from the current directory.
func gitToplevel(ctx context.Context, x Commander) (string, error) {
	so, se, res := output(ctx, x, Cmd{Name: "git", Args: []string{"rev-parse", "--show-toplevel"}})
	if res.ExitCode != 0 || res.Err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %s%v", se, res.Err)
	}
	return strings.TrimSpace(so), nil
}

// RepoRoot resolves the git toplevel of the current directory, like run.sh's
// `git rev-parse --show-toplevel`.
func (g *Gate) RepoRoot(ctx context.Context) (string, error) { return gitToplevel(ctx, g.Exec) }

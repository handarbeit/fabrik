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
	// Resume: run only what the per-SHA coverage ledger still lacks (#1972).
	Resume bool
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
	// OnLeg, if set, observes each leg that ran to a normal post-suite result.
	// (#1972's coverage ledger does NOT hang off it: it records from the suite's
	// event stream so a killed leg keeps what had finished.) It is NOT called for
	// a leg that ended in the RUN INVALID backoff banner (ExitBudgetExhausted), the post-suite watchdog
	// (ExitPostSuiteWatchdog), or a failed restart step: bash gave those no
	// per-leg result either. The ledger's reading of them: RUN INVALID voids the
	// cell's records, a watchdog leg's completed PASSes count (ADR-1972).
	OnLeg func(LegResult)

	// Seams for time and the OS.
	Sleep func(time.Duration)
	Now   func() time.Time
	// ProcCwd resolves a process's working directory ("" if unknown).
	ProcCwd func(ctx context.Context, pid int) string
	// Self is this process's PID (excluded from competing-consumer discovery).
	Self int

	// LoadAvg reads the host's 1-minute load average for the leg archive
	// (default: the per-OS probe; ok=false means unavailable).
	LoadAvg func() (float64, bool)

	// engineSHA is the FULL SHA of the engine under test, set by PreflightBed: the
	// coverage ledger's key (#1972).
	engineSHA string
	// resume mirrors Plan.Resume for RunPregate, which consults the pre-gate record.
	resume bool
	// cov is the live coverage ledger context; nil when the ledger is disabled or
	// unavailable (existing behaviour, unchanged).
	cov *covState

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
	plan := &Plan{AuthModes: modes, Args: args.Rest, RawArgs: argv, Clean: args.Clean, Resume: args.Resume}

	for _, pf := range g.Preflights {
		if err := pf.Run(ctx, g, plan); err != nil {
			return err
		}
	}

	// The coverage inputs (live set, source hashes, registry) are computed before
	// ANY live spend, so a source file that cannot be parsed fails here (exit 4)
	// rather than hours into a gate.
	var inputs *covInputs
	if g.Cfg.CoverageDir != "" {
		if inputs, err = g.loadCoverageInputs(); err != nil {
			return exitErr(ExitPreflightFailed, "coverage ledger: %v", err)
		}
	} else if plan.Resume {
		return exitErr(ExitUsage, "--resume needs the coverage ledger, which is disabled (E2E_COVERAGE_DIR resolved to nothing)")
	}
	if plan.Resume && hasSubtestFilter(plan.Args) {
		return exitErr(ExitUsage, "--resume cannot be combined with a subtest-level -run/-skip: a subtest-filtered run executes only part of a test, so it could never be credited")
	}
	g.resume = plan.Resume

	if err := g.RunPregate(ctx); err != nil {
		return err
	}
	preflightText, err := g.teeOutput(func() error { return g.PrepareBedAndReset(ctx, plan.Clean) })
	if err != nil {
		return err
	}

	planInput := PlanInput{
		AuthModes:    plan.AuthModes,
		TrainMode:    g.Getenv("E2E_TRAIN_MODE"),
		CallerHasRun: HasRunFlag(plan.Args),
		Parallel:     g.Cfg.Parallel,
		ParallelOn:   g.Cfg.ParallelOn,
		Args:         plan.Args,
	}
	cells := PlanCells(planInput)

	var (
		legs     []string
		required map[string][]string
	)
	if inputs != nil {
		cs, err := g.openCoverage(ctx, inputs, args, plan.Args, preflightText)
		if err != nil {
			return err
		}
		g.cov = cs
	}
	if g.cov != nil {
		// The REQUIRED set is the plan the gate would build with no caller
		// arguments at all — a caller's -run narrows what this invocation runs,
		// never what the gate requires.
		full := planInput
		full.CallerHasRun, full.Args = false, nil
		if legs, required, err = RequiredTests(inputs.live, PlanCells(full)); err != nil {
			return exitErr(ExitPreflightFailed, "coverage ledger: %v", err)
		}
		if plan.Resume {
			var resolved int
			ev := &Evaluator{Snap: g.cov.ledger.Load(), Hashes: inputs.hashes, Entries: inputs.entries, States: g.newIssueStates()}
			if cells, resolved, err = ResumeCells(ctx, cells, inputs.live, ev); err != nil {
				return exitErr(ExitUsage, "%v", err)
			}
			g.outf("== --resume: %d (test, leg) pair(s) already resolved; running %d cell(s) ==\n", resolved, len(cells))
		}
	}

	var runErr error
	if g.cov != nil && plan.Resume && len(cells) == 0 {
		g.outln("== --resume: nothing left to run for the selected tests ==")
	} else {
		runErr = g.Scheduler.Run(ctx, g, cells)
	}

	if g.cov == nil {
		return runErr
	}
	// Print the coverage summary on every path — pass, fail, kill. A cancelled
	// run still gets one, on a fresh, bounded context for the gh lookups.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	rep := g.coverageReport(sctx, g.cov.ledger, inputs, legs, required, true)
	rep.Pregate = g.pregateLine(sctx)
	g.outln(rep.Format())
	if runErr == nil && plan.Resume && !rep.Complete() {
		return &ExitError{Code: ExitCoverageIncomplete, Msg: "every leg this invocation ran passed, but required live coverage is still incomplete — run scripts/e2e/run.sh --resume again"}
	}
	return runErr
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

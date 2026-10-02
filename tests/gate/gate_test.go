package gate

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive Gate.Run end to end over a fake Commander, one per exit
// code scripts/cut-release.sh's interpret_e2e_exit_code interprets. They are the
// acceptance gate for the exit-code contract: 3, 4, 5, 6, 7, the suite's own
// code, and 0.

const bedSHA = "abc1234def5678abc1234def5678abc1234def56"

// runFake scripts the whole invocation: the bed's git, the sim pre-gate, the
// bed binary, and (via legFake) the legs.
type runFake struct {
	*legFake
	mu       sync.Mutex
	order    []string // coarse-grained milestones in the order they happened
	simRC    int
	goTestRC int // `go test -race ./github/...`
	headSHA  string
	wantSHA  string
}

func (r *runFake) note(s string) {
	r.mu.Lock()
	r.order = append(r.order, s)
	r.mu.Unlock()
}

func (r *runFake) handle(ctx context.Context, c Cmd) Result {
	line := argsLine(c)
	switch {
	case strings.HasSuffix(c.Name, "scripts/sim/run.sh"):
		r.note("pregate:sim")
		return Result{ExitCode: r.simRC}
	case c.Name == "go" && strings.Contains(line, "-race") && strings.Contains(line, "./github/..."):
		r.note("pregate:github")
		return Result{ExitCode: r.goTestRC}
	case c.Name == "go" && strings.HasPrefix(line, "go build"):
		r.note("bed:build")
		return Result{}
	case c.Name == "git" && c.Dir != "" && len(c.Args) > 0:
		switch c.Args[0] {
		case "status":
			if c.Dir == bedDir(c) {
				r.note("bed:git-status")
			}
			return Result{}
		case "fetch":
			r.note("bed:git-fetch")
		case "rev-parse":
			if c.Args[1] == "HEAD" {
				writeStdout(c, r.headSHA+"\n")
			} else {
				writeStdout(c, r.wantSHA+"\n")
			}
		}
		return Result{}
	case c.Name == "./fabrik":
		if len(c.Args) > 0 && c.Args[0] == "--version" {
			writeStdout(c, "fabrik version 0.0.0-dev ("+r.wantSHA[:7]+")\n")
		}
		return Result{}
	case c.Name == "gh" && strings.Contains(line, "pr list"):
		r.note("reset")
		return r.legFake.handle(ctx, c)
	case isSwitch(c):
		r.note("leg:switch:" + envValue(c.Env, "E2E_AUTH_MODE") + "/" + envValue(c.Env, "E2E_TRAIN_MODE"))
	case isSuite(c):
		r.note("leg:suite")
	}
	return r.legFake.handle(ctx, c)
}

func bedDir(c Cmd) string { return c.Dir }

func envValue(env []string, key string) string {
	v := ""
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			v = e[len(key)+1:]
		}
	}
	return v
}

func (r *runFake) milestones() string { return strings.Join(r.order, " | ") }

// newRunGate builds a Gate whose invocation can complete: a bed with .env and a
// .git dir, a Start that prints the banner, auth pinned to pat so the app-leg
// preconditions are out of the way.
func newRunGate(t *testing.T, rf *runFake, env ...string) (*Gate, *startWriter) {
	t.Helper()
	g, fe, _, _ := testGate(t, append([]string{"E2E_AUTH_MODE=pat", "E2E_SKIP_TOKEN_CHECK=1"}, env...)...)
	fe.handler = rf.handle
	sw := &startWriter{fakeExec: fe, banner: "Fabrik starting 0.0.0-dev (" + bedSHA[:7] + ")"}
	g.Exec = sw
	bed := g.Cfg.TestBed
	mustWrite(t, bed+"/.env", "FABRIK_TOKEN=bed-token\n")
	if err := os.MkdirAll(bed+"/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	g.Cfg.GHAPITimeout = 2 * time.Second
	g.Cfg.PostSuiteWatchdog = 10 * time.Second
	return g, sw
}

func newRunFake() *runFake {
	return &runFake{legFake: &legFake{suiteOut: passStream, budgets: []string{"4000", "3000", "3000", "2000", "2000", "1000"}}, headSHA: bedSHA, wantSHA: bedSHA}
}

func TestRunExitCodeSuccess(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_TRAIN_MODE=off")
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d\n%s", code, g.Err.(interface{ String() string }).String())
	}
}

func TestRunExitCode7Preconditions(t *testing.T) {
	t.Run("an invalid E2E_AUTH_MODE", func(t *testing.T) {
		rf := newRunFake()
		g, _ := newRunGate(t, rf, "E2E_AUTH_MODE=both")
		if code := g.Run(context.Background(), nil); code != ExitPreconditionFailed {
			t.Errorf("exit %d", code)
		}
		if !strings.Contains(g.Err.(interface{ String() string }).String(), `E2E_AUTH_MODE="both" is invalid`) {
			t.Error("missing the message")
		}
		if len(rf.order) != 0 {
			t.Errorf("nothing may run: %s", rf.milestones())
		}
	})
	t.Run("an unmet app-auth precondition", func(t *testing.T) {
		rf := newRunFake()
		g, _ := newRunGate(t, rf, "E2E_AUTH_MODE=app")
		if code := g.Run(context.Background(), nil); code != ExitPreconditionFailed {
			t.Errorf("exit %d", code)
		}
		if len(rf.order) != 0 {
			t.Errorf("nothing may run: %s", rf.milestones())
		}
	})
	t.Run("a competing token consumer", func(t *testing.T) {
		rf := newRunFake()
		g, _ := newRunGate(t, rf, "E2E_SKIP_TOKEN_CHECK=")
		other := t.TempDir()
		mustWrite(t, other+"/.env", "FABRIK_TOKEN=bed-token\n")
		g.Exec.(*startWriter).fakeExec.handler = func(ctx context.Context, c Cmd) Result {
			if c.Name == "pgrep" {
				writeStdout(c, "4242\n")
				return Result{}
			}
			return rf.handle(ctx, c)
		}
		g.ProcCwd = func(context.Context, int) string { return other }
		if code := g.Run(context.Background(), nil); code != ExitPreconditionFailed {
			t.Errorf("exit %d\n%s", code, g.Err.(interface{ String() string }).String())
		}
		if len(rf.order) != 0 {
			t.Errorf("nothing may run: %s", rf.milestones())
		}
	})
	t.Run("Pruefer confidently down", func(t *testing.T) {
		rf := newRunFake()
		g, _ := newRunGate(t, rf)
		g.Cfg.PrueferDir = t.TempDir() // exists, but has no lock file
		if code := g.Run(context.Background(), nil); code != ExitPreconditionFailed {
			t.Errorf("exit %d", code)
		}
		if len(rf.order) != 0 {
			t.Errorf("nothing may run: %s", rf.milestones())
		}
	})
}

func TestRunExitCode5PregateFailureSpendsNothing(t *testing.T) {
	rf := newRunFake()
	rf.simRC = 1
	g, sw := newRunGate(t, rf, "E2E_TRAIN_MODE=off")
	if code := g.Run(context.Background(), nil); code != ExitPregateFailed {
		t.Fatalf("exit %d", code)
	}
	// ADR-1454: a pre-gate failure is proven, by construction, to have made no
	// bed preflight, no build, no restart, and no live GitHub/Claude call.
	if got := rf.milestones(); got != "pregate:sim" {
		t.Errorf("milestones: %s", got)
	}
	if len(sw.started) != 0 {
		t.Error("the bed must not have been started")
	}
}

func TestRunExitCode4PreflightFailure(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1")
	os.RemoveAll(g.Cfg.TestBed + "/.git") // not a git checkout
	if code := g.Run(context.Background(), nil); code != ExitPreflightFailed {
		t.Fatalf("exit %d", code)
	}
	if len(rf.order) != 0 {
		t.Errorf("no leg may run after a failed preflight: %s", rf.milestones())
	}

	// A bed whose binary does not carry the ref under test (the 194-commit hole).
	rf = newRunFake()
	rf.wantSHA = "fffffff" + bedSHA[7:]
	rf.headSHA = rf.wantSHA
	g, sw := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_BED_NO_BUILD=1")
	g.Exec.(*startWriter).fakeExec.handler = func(ctx context.Context, c Cmd) Result {
		if c.Name == "./fabrik" && len(c.Args) > 0 && c.Args[0] == "--version" {
			writeStdout(c, "fabrik version 0.0.0-dev (0000000)\n")
			return Result{}
		}
		return rf.handle(ctx, c)
	}
	_ = sw
	if code := g.Run(context.Background(), nil); code != ExitPreflightFailed {
		t.Fatalf("a stale binary: exit %d", code)
	}
}

func TestRunExitCode3BackoffEndsTheRunBeforeLaterLegs(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1") // default two-mode gate: 3 legs per auth mode
	mustWrite(t, g.Cfg.EngineLog, "… activating rate-limit backoff\n")
	if code := g.Run(context.Background(), nil); code != ExitBudgetExhausted {
		t.Fatalf("exit %d", code)
	}
	if n := strings.Count(rf.milestones(), "leg:suite"); n != 1 {
		t.Errorf("RUN INVALID must short-circuit the remaining legs, suites run: %d (%s)", n, rf.milestones())
	}
}

func TestRunExitCode6PostSuiteWatchdog(t *testing.T) {
	rf := newRunFake()
	rf.budgets = []string{"4000", "HANG"}
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	g.Cfg.PostSuiteWatchdog = 300 * time.Millisecond
	if code := g.Run(context.Background(), nil); code != ExitPostSuiteWatchdog {
		t.Fatalf("exit %d", code)
	}
}

func TestRunSuiteFailureExitCodeIsPropagatedAndEndsTheRun(t *testing.T) {
	rf := newRunFake()
	rf.suiteRC = 1
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1") // would be 3 legs
	if code := g.Run(context.Background(), nil); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if n := strings.Count(rf.milestones(), "leg:suite"); n != 1 {
		t.Errorf("the first failing leg ends the run (set -e), suites run: %d", n)
	}
	rf = newRunFake()
	rf.suiteRC = 2
	g, _ = newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	if code := g.Run(context.Background(), nil); code != 2 {
		t.Fatalf("a suite's own code passes through unchanged, got %d", code)
	}
}

// The order is load-bearing (ADR-1454): parity summary and preconditions, then
// the pre-gate, then bed preflight, then --clean's reset against the STOPPED
// bed, then the bed start, then the legs.
func TestRunOrderAndCleanHandling(t *testing.T) {
	rf := newRunFake()
	g, sw := newRunGate(t, rf, "E2E_TRAIN_MODE=off", "E2E_PARITY_REGISTRY=/nonexistent")
	out := g.Out
	_ = out
	if code := g.Run(context.Background(), []string{"--clean", "-v"}); code != 0 {
		t.Fatalf("exit %d\n%s", code, g.Err.(interface{ String() string }).String())
	}
	want := "pregate:sim | pregate:github | bed:git-fetch | bed:build | reset | reset | leg:switch:pat/off | leg:suite"
	var got []string
	for _, m := range rf.order {
		if m != "bed:git-status" {
			got = append(got, m)
		}
	}
	if strings.Join(got, " | ") != want {
		t.Errorf("order:\n got: %s\nwant: %s", strings.Join(got, " | "), want)
	}
	if len(sw.started) != 1 {
		t.Fatalf("the bed must be started exactly once, after the reset: %d", len(sw.started))
	}
	// --clean is consumed by the runner, never passed to go test; other args are.
	su := rf.suiteCmds[0]
	if got := strings.Join(su.Args, " "); strings.Contains(got, "--clean") || !strings.HasSuffix(got, "./tests/e2e/... -v") {
		t.Errorf("suite argv = %q", got)
	}
	// The parity line is printed before the pre-gate banner, even though it is not part of it.
	stdout := g.Out.(interface{ String() string }).String()
	if i, j := strings.Index(stdout, "sim parity:"), strings.Index(stdout, "== pre-gate:"); i < 0 || j < 0 || i > j {
		t.Errorf("the parity summary must precede the pre-gate:\n%s", stdout)
	}
}

func TestRunWithoutCleanDoesNotReset(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_TRAIN_MODE=off", "E2E_SKIP_PREGATE=1")
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(rf.milestones(), "reset") {
		t.Errorf("no --clean, no reset: %s", rf.milestones())
	}
}

func TestRunSkipPrepSkipsPreflightAndStart(t *testing.T) {
	rf := newRunFake()
	g, sw := newRunGate(t, rf, "E2E_TRAIN_MODE=off", "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1")
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(rf.milestones(), "bed:") || len(sw.started) != 0 {
		t.Errorf("E2E_SKIP_PREP skips preflight and start: %s", rf.milestones())
	}
}

func TestRunNoBuildVerifiesOnly(t *testing.T) {
	rf := newRunFake()
	g, sw := newRunGate(t, rf, "E2E_TRAIN_MODE=off", "E2E_SKIP_PREGATE=1", "E2E_BED_NO_BUILD=1")
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d\n%s", code, g.Err.(interface{ String() string }).String())
	}
	if strings.Contains(rf.milestones(), "bed:build") || len(sw.started) != 0 {
		t.Errorf("E2E_BED_NO_BUILD never builds or starts: %s", rf.milestones())
	}
}

func TestRunUnreadableBedTokenOnlyWarns(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_TRAIN_MODE=off", "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1")
	mustWrite(t, g.Cfg.TestBed+"/.env", "OTHER=1\n")
	if code := g.Run(context.Background(), nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(g.Err.(interface{ String() string }).String(), "could not read FABRIK_TOKEN") {
		t.Error("missing the warning")
	}
}

func TestRunProbeListIsExtensible(t *testing.T) {
	// R5: #1974 appends environment probes; a failing probe stops the run before
	// the pre-gate, with the probe's own code.
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_TRAIN_MODE=off")
	g.Preflights = append(g.Preflights, Preflight{Name: "host-load", Run: func(context.Context, *Gate, *Plan) error {
		return exitErr(ExitPreconditionFailed, "host too loaded")
	}})
	if code := g.Run(context.Background(), nil); code != ExitPreconditionFailed {
		t.Fatalf("exit %d", code)
	}
	if len(rf.order) != 0 {
		t.Errorf("a failing probe must stop the run before the pre-gate: %s", rf.milestones())
	}
}

func TestRunCancelledReturns130(t *testing.T) {
	rf := newRunFake()
	g, _ := newRunGate(t, rf, "E2E_SKIP_PREGATE=1", "E2E_SKIP_PREP=1", "E2E_TRAIN_MODE=off")
	ctx, cancel := context.WithCancel(context.Background())
	rf.suiteHook = func(Cmd) { cancel() }
	if code := g.Run(ctx, nil); code != 130 {
		t.Errorf("exit %d", code)
	}
}
